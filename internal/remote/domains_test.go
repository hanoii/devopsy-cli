package remote

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func parseFacts(t *testing.T, out string) *ProxyFacts {
	t.Helper()
	f, err := ParseProxyFacts([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// What the domains capability reports for a project's hosts.
const sampleFacts = `{
  "version": 1,
  "ip": "10.0.0.5",
  "retries": ["shop"],
  "hosts": [
    {"host": "shop.vm1.example.com", "routed": true, "resolver": "letsencrypt1", "method": "http", "wildcard": "*.vm1.example.com"},
    {"host": "live.org", "routed": true, "resolver": "letsencrypt1", "method": "http"},
    {"host": "ready.org", "routed": true, "resolver": "letsencrypt1", "method": "http"},
    {"host": "nocname.org", "routed": true, "resolver": "acmedns", "method": "dns-cname",
     "record": {"name": "_acme-challenge.nocname.org", "target": "abc.acme-vm1.example.com"}},
    {"host": "unregistered.org", "routed": true, "resolver": "acmedns", "method": "dns-cname"},
    {"host": "notpointing.org", "routed": true, "resolver": "letsencrypt1", "method": "http"},
    {"host": "api.org", "routed": true, "resolver": "cloudflare", "method": "dns-api"},
    {"host": "odd.org", "routed": true, "resolver": "custom"},
    {"host": "gone.org", "routed": false}
  ],
  "future": "ignored"
}`

func TestParseProxyFacts(t *testing.T) {
	f := parseFacts(t, sampleFacts)
	if f.IP != "10.0.0.5" || len(f.Hosts) != 9 || !f.HasRetry("shop") || f.HasRetry("other") {
		t.Fatalf("%+v", f)
	}
	if h := f.Host("Live.org"); !h.Routed || h.Method != MethodHTTP {
		t.Fatalf("live.org %+v", h)
	}
	if h := f.Host("other.org"); h.Routed || h.Host != "other.org" {
		t.Fatalf("unknown host %+v", h)
	}
	for in, want := range map[string]string{
		"garbage":                        "not JSON",
		`{"hosts": []}`:                  "no version",
		`{"version": 2, "hosts": []}`:    "upgrade devopsy",
		`{"version": 99, "ip": "x.y.z"}`: "version 99",
	} {
		if _, err := ParseProxyFacts([]byte(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want it to contain %q", in, err, want)
		}
	}
}

func TestProjectHosts(t *testing.T) {
	env, err := ParseEnv("DEVOPSY_WILDCARD_HOST='shop.vm1.example.com'\nDEVOPSY_DOMAINS='live.org, Live.org ready.org'\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ProjectHosts(env), " "); got != "shop.vm1.example.com live.org ready.org" {
		t.Fatalf("hosts %s", got)
	}
	if got := ProjectHosts(map[string]string{}); len(got) != 0 {
		t.Fatalf("no hosts: %v", got)
	}
}

func TestCheckNextSteps(t *testing.T) {
	f := parseFacts(t, sampleFacts)
	server := "203.0.113.10"
	dns := map[string][]string{
		"shop.vm1.example.com": {server},
		"live.org":             {server},
		"ready.org":            {"198.51.100.1"},
		"nocname.org":          {"198.51.100.1"},
		"unregistered.org":     {"198.51.100.1"},
		"notpointing.org":      {"198.51.100.1"},
		"api.org":              {"198.51.100.1"},
		"odd.org":              {server},
	}
	valid := map[string]bool{"shop.vm1.example.com": true, "live.org": true, "ready.org": true}
	c := Checker{
		LookupIP: func(_ context.Context, h string) ([]string, error) {
			if ips, ok := dns[h]; ok {
				return ips, nil
			}
			return nil, errors.New("no such host")
		},
		LookupCNAME: func(_ context.Context, h string) (string, error) {
			return h, nil // no CNAME anywhere
		},
		Cert: func(_ context.Context, ip, h string) (CertInfo, error) {
			if ip != server {
				t.Errorf("connected to %s", ip)
			}
			if valid[h] {
				return CertInfo{Valid: true, Issuer: "Let's Encrypt R13", Expires: time.Date(2027, 1, 3, 0, 0, 0, 0, time.UTC)}, nil
			}
			return CertInfo{Issuer: "TRAEFIK DEFAULT CERT"}, nil
		},
	}
	hosts := []string{"shop.vm1.example.com", "live.org", "ready.org", "nocname.org", "unregistered.org", "notpointing.org", "api.org", "odd.org", "gone.org", "missing.org"}
	reports := Check(context.Background(), f, c, []string{server, f.IP}, hosts)
	got := map[string]DomainReport{}
	for _, r := range reports {
		got[r.Host] = r
	}
	for host, want := range map[string]string{
		"shop.vm1.example.com": "live",
		"live.org":             "live",
		"ready.org":            "certificate ready: point its DNS at 203.0.113.10",
		"nocname.org":          "_acme-challenge.nocname.org. CNAME abc.acme-vm1.example.com.",
		"unregistered.org":     "not known yet",
		"notpointing.org":      "point its DNS at 203.0.113.10 (HTTP-01 needs it)",
		"api.org":              "DNS API token",
		"odd.org":              `resolver "custom"`,
		"gone.org":             "does not route it",
		"missing.org":          "does not route it",
	} {
		if !strings.Contains(got[host].Next, want) {
			t.Errorf("%s: next %q, want it to contain %q", host, got[host].Next, want)
		}
	}
	if !got["live.org"].Live || got["ready.org"].Live {
		t.Error("live flags wrong")
	}
	text := FormatReports(reports)
	for _, want := range []string{"live.org  [live]", "(this server)", "challenge    missing", "expires 2027-01-03", "resolver     letsencrypt1"} {
		if !strings.Contains(text, want) {
			t.Errorf("report missing %q:\n%s", want, text)
		}
	}

	reports = []DomainReport{{Routed: true, Cert: CertInfo{Valid: true}}, {Routed: false}}
	if !AllCertified(reports) {
		t.Fatal("all routed hosts are certified")
	}
	reports = append(reports, DomainReport{Routed: true})
	if AllCertified(reports) {
		t.Fatal("a routed host has no certificate")
	}
}

func TestCheckProxied(t *testing.T) {
	f := parseFacts(t, `{"version": 1, "ip": "203.0.113.10", "hosts": [
	  {"host": "shop.vm1.example.com", "routed": true, "resolver": "acmedns", "method": "dns-cname"},
	  {"host": "cf-ok.org", "routed": true, "resolver": "acmedns", "method": "dns-cname"},
	  {"host": "cf-526.org", "routed": true, "resolver": "acmedns", "method": "dns-cname"},
	  {"host": "cf-down.org", "routed": true, "resolver": "acmedns", "method": "dns-cname"},
	  {"host": "cf-pending.org", "routed": true, "resolver": "acmedns", "method": "dns-cname"}]}`)
	status := map[string]int{"cf-ok.org": 200, "cf-526.org": 526, "cf-down.org": 522, "cf-pending.org": 401}
	c := Checker{
		LookupIP: func(_ context.Context, h string) ([]string, error) {
			if h == "shop.vm1.example.com" {
				return []string{"203.0.113.10"}, nil
			}
			return []string{"104.21.74.195", "172.67.162.100"}, nil
		},
		LookupCNAME: func(_ context.Context, h string) (string, error) { return h, nil },
		Cert: func(_ context.Context, _, h string) (CertInfo, error) {
			return CertInfo{Valid: h != "cf-526.org" && h != "cf-pending.org", Issuer: "Let's Encrypt YR2"}, nil
		},
		Proxy: func(ip string) string {
			if strings.HasPrefix(ip, "104.") || strings.HasPrefix(ip, "172.67.") {
				return "Cloudflare"
			}
			return ""
		},
		Get: func(_ context.Context, _, h string) (int, error) { return status[h], nil },
	}
	got := map[string]DomainReport{}
	for _, r := range Check(context.Background(), f, c, []string{"203.0.113.10"}, f.AllHosts()) {
		got[r.Host] = r
	}
	if r := got["cf-ok.org"]; !r.Live || r.Proxy != "Cloudflare" {
		t.Errorf("cf-ok.org: %+v", r)
	}
	if r := got["cf-526.org"]; r.Live || !strings.Contains(r.Next, "526") {
		t.Errorf("cf-526.org: %q", r.Next)
	}
	if r := got["cf-down.org"]; r.Live || !strings.Contains(r.Next, "cannot reach the server (522)") {
		t.Errorf("cf-down.org: %q", r.Next)
	}
	// The app's own 401 through the proxy: served, but no certificate yet.
	if r := got["cf-pending.org"]; r.Live || !strings.Contains(r.Next, "not Full (strict)") || !strings.Contains(r.Next, "--retry") {
		t.Errorf("cf-pending.org: %q", r.Next)
	}
	text := FormatReports([]DomainReport{got["shop.vm1.example.com"], got["cf-ok.org"]})
	if strings.Contains(text, "challenge") {
		t.Errorf("challenge line for hosts with valid certificates and no record:\n%s", text)
	}
	if !strings.Contains(text, "(Cloudflare proxy, HTTP 200 through it)") {
		t.Errorf("proxy not shown:\n%s", text)
	}
}

// On the proxy's own target, a wildcard is checked through the name its
// router matches, and its challenge is the domain's.
func TestCheckWildcard(t *testing.T) {
	f := parseFacts(t, `{"version": 1, "ip": "203.0.113.10", "hosts": [
	  {"host": "*.vm1.example.com", "probe": "devopsy-wildcard.vm1.example.com", "routed": true, "resolver": "acmedns", "method": "dns-cname",
	   "record": {"name": "_acme-challenge.vm1.example.com", "target": "wild.acme-vm1.example.com"}},
	  {"host": "*.other.example.com", "routed": true, "resolver": "cloudflare", "method": "dns-api"}]}`)
	server := "203.0.113.10"
	var connected, challenged []string
	c := Checker{
		LookupIP: func(_ context.Context, h string) ([]string, error) { return []string{server}, nil },
		LookupCNAME: func(_ context.Context, h string) (string, error) {
			challenged = append(challenged, h)
			return "wild.acme-vm1.example.com.", nil
		},
		Cert: func(_ context.Context, ip, h string) (CertInfo, error) {
			connected = append(connected, h)
			return CertInfo{Valid: true, Issuer: "Let's Encrypt R13"}, nil
		},
	}
	reports := Check(context.Background(), f, c, []string{server}, f.AllHosts())
	if r := reports[0]; r.Host != "*.vm1.example.com" || r.Resolver != "acmedns" || !r.Live || r.ChallengeWant != "wild.acme-vm1.example.com" {
		t.Errorf("wildcard: %+v", r)
	}
	if strings.Join(connected, " ") != "devopsy-wildcard.vm1.example.com devopsy-wildcard.other.example.com" || strings.Join(challenged, " ") != "_acme-challenge.vm1.example.com" {
		t.Errorf("wildcards checked through %v, challenge %v", connected, challenged)
	}
	text := FormatReports(reports)
	if !strings.Contains(text, "challenge    ok") {
		t.Errorf("challenge not ok:\n%s", text)
	}
}

func TestDomainsScripts(t *testing.T) {
	tg := &Target{Name: "prod", Host: "h", Path: "/srv/shop", Mode: ModeImage}
	for name, script := range map[string]string{
		"env":        EnvScript(tg, "shop"),
		"capability": CapabilityScript("/srv/traefik", "domains", "facts", []string{"a.org", "b c"}),
	} {
		if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
			t.Fatalf("%s script: %v\n%s", name, err, out)
		}
	}
	s := CapabilityScript("/srv/traefik", "domains", "retry", []string{"shop", "--done"})
	if !strings.Contains(s, "devopsy '--capability' 'domains' 'retry' 'shop' '--done'") || strings.Contains(s, "COMPOSE_PROJECT_NAME") {
		t.Fatalf("capability script:\n%s", s)
	}
}
