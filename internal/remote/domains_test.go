package remote

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

func factLine(k, v string) string {
	return k + "\t" + base64.StdEncoding.EncodeToString([]byte(v)) + "\n"
}

func sampleFacts(t *testing.T) *Facts {
	t.Helper()
	out := factLine("env", "DEVOPSY_PUBLIC_HOST='shop.vm1.example.com'\nDEVOPSY_PUBLIC_DOMAIN='vm1.example.com'\nDEVOPSY_PROJECT_NAME='shop'\nDEVOPSY_DOMAINS='live.org ready.org nocname.org unregistered.org notpointing.org Live.org'\n") +
		factLine("ip", "10.0.0.5\n") +
		factLine("routers", `[
  {"name":"shop@docker","rule":"Host(`+"`shop.vm1.example.com`"+`) || Host(`+"`live.org`"+`) || Host(`+"`ready.org`"+`) || Host(`+"`notpointing.org`"+`)","tls":null},
  {"name":"websecure-shop@docker","rule":"Host(`+"`shop.vm1.example.com`"+`) || Host(`+"`live.org`"+`) || Host(`+"`ready.org`"+`) || Host(`+"`notpointing.org`"+`)","tls":{"certResolver":"letsencrypt1"}},
  {"name":"dns@docker","rule":"Host(`+"`nocname.org`"+`) || Host(`+"`unregistered.org`"+`)","tls":{"certResolver":"acmedns"}}
]`) +
		factLine("accounts", `{"nocname.org":{"fulldomain":"abc.acme-vm1.example.com"},"*.vm1.example.com":{"fulldomain":"wild.acme-vm1.example.com"}}`) +
		factLine("traefik", "/srv/traefik")
	f, err := ParseFacts(out)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestParseFacts(t *testing.T) {
	f := sampleFacts(t)
	if f.ServerIP != "10.0.0.5" || f.TraefikDir != "/srv/traefik" || len(f.Routers) != 3 {
		t.Fatalf("%+v", f)
	}
	if f.Accounts["vm1.example.com"] != "wild.acme-vm1.example.com" {
		t.Fatalf("wildcard account key not trimmed: %v", f.Accounts)
	}
	hosts := f.Hosts()
	want := []string{"shop.vm1.example.com", "live.org", "ready.org", "nocname.org", "unregistered.org", "notpointing.org"}
	if strings.Join(hosts, " ") != strings.Join(want, " ") {
		t.Fatalf("hosts %v", hosts)
	}
	if r, ok := f.Resolver("live.org"); r != "letsencrypt1" || !ok {
		t.Fatalf("live.org resolver %q %v", r, ok)
	}
	if _, ok := f.Resolver("other.org"); ok {
		t.Fatal("unrouted host reported routed")
	}
	if _, err := ParseFacts("garbage\n"); err == nil {
		t.Fatal("garbage: want an error")
	}
}

func TestCheckNextSteps(t *testing.T) {
	f := sampleFacts(t)
	server := "203.0.113.10"
	dns := map[string][]string{
		"shop.vm1.example.com": {server},
		"live.org":             {server},
		"ready.org":            {"198.51.100.1"},
		"nocname.org":          {"198.51.100.1"},
		"unregistered.org":     {"198.51.100.1"},
		"notpointing.org":      {"198.51.100.1"},
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
	reports := Check(context.Background(), f, c, []string{server, f.ServerIP})
	got := map[string]DomainReport{}
	for _, r := range reports {
		got[r.Host] = r
	}
	for host, want := range map[string]string{
		"shop.vm1.example.com": "live",
		"live.org":             "live",
		"ready.org":            "certificate ready: point its DNS at 203.0.113.10",
		"nocname.org":          "_acme-challenge.nocname.org. CNAME abc.acme-vm1.example.com.",
		"unregistered.org":     "not registered with acme-dns yet",
		"notpointing.org":      "point its DNS at 203.0.113.10 (HTTP-01 needs it)",
	} {
		if !strings.Contains(got[host].Next, want) {
			t.Errorf("%s: next %q, want it to contain %q", host, got[host].Next, want)
		}
	}
	if !got["live.org"].Live || got["ready.org"].Live {
		t.Error("live flags wrong")
	}
	text := FormatReports(reports)
	for _, want := range []string{"live.org  [live]", "(this server)", "challenge    missing", "expires 2027-01-03"} {
		if !strings.Contains(text, want) {
			t.Errorf("report missing %q:\n%s", want, text)
		}
	}
}

func TestDomainsAndRetryScripts(t *testing.T) {
	tg := &Target{Name: "prod", Host: "h", Path: "/srv/shop", Mode: ModeImage}
	traefik := t.TempDir()
	if err := os.MkdirAll(filepath.Join(traefik, ".devopsy", "mnt", "dynamic"), 0o755); err != nil {
		t.Fatal(err)
	}
	retry := RetryScript(traefik, "shop", map[string][]string{
		"acmedns":      {"nocname.org"},
		"letsencrypt1": {"notpointing.org", "x.org"},
	}, time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	if out, err := exec.Command("sh", "-n", "-c", DomainsScript(tg, "shop")).CombinedOutput(); err != nil {
		t.Fatalf("domains script: %v\n%s", err, out)
	}
	if out, err := exec.Command("sh", "-c", retry).CombinedOutput(); err != nil {
		t.Fatalf("retry script: %v\n%s", err, out)
	}
	out, err := os.ReadFile(filepath.Join(traefik, ".devopsy", "mnt", "dynamic", "devopsy-retry-shop.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		HTTP struct {
			Routers map[string]struct {
				Rule string
				TLS  struct {
					CertResolver string `yaml:"certResolver"`
					Domains      []struct{ Main string }
				}
			}
		}
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	r := doc.HTTP.Routers["devopsy-retry-shop-letsencrypt1-20261005120000"]
	if r.TLS.CertResolver != "letsencrypt1" || len(r.TLS.Domains) != 2 || r.TLS.Domains[1].Main != "x.org" {
		t.Fatalf("%s", out)
	}
}
