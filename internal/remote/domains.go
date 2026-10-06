package remote

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/dotenv"
)

// DomainsScript prints facts about a target's domains, as `key<TAB>base64`
// lines: the effective environment (devopsy print-env), the server's IP,
// Traefik's routers (its local API) and the acme-dns registrations. The
// checks themselves run locally, so the server needs nothing extra.
func DomainsScript(t *Target, projectName string) string {
	return fmt.Sprintf(`set -eu
cd %s/current 2>/dev/null || { echo "devopsy: no release on %s yet: run 'devopsy @%s release' first" >&2; exit 1; }
traefik=/srv/traefik
v=$(sed -n 's/^DEVOPSY_TRAEFIK_DIR=//p' /etc/devopsy/devopsy.env 2>/dev/null | tail -n 1)
[ -z "$v" ] || traefik=$v
api=$(sed -n 's/^DEVOPSY_API_PORT=//p' "$traefik/.devopsy/.env" 2>/dev/null | tail -n 1)
api=${api:-127.0.0.1:8080}
case $api in *:*) ;; *) api=127.0.0.1:$api ;; esac
fact() { printf '%%s\t' "$1"; base64 | tr -d '\n'; echo; }
%s print-env | fact env
ip -4 route get 1.1.1.1 | awk '{ for (i = 1; i < NF; i++) if ($i == "src") { print $(i + 1); exit } }' | fact ip
curl -fsS "http://$api/api/http/routers?per_page=1000" 2>/dev/null | fact routers || echo "routers	"
cat "$traefik/.devopsy/mnt/letsencrypt/acme-dns-accounts.json" 2>/dev/null | fact accounts || echo "accounts	"
printf '%%s' "$traefik" | fact traefik
`, Quote(t.Path), t.Path, t.Name, devopsyCall(projectName, nil, false))
}

// Facts is what DomainsScript reports.
type Facts struct {
	Env      map[string]string
	ServerIP string
	Routers  []Router
	// Accounts maps a domain to its acme-dns CNAME target.
	Accounts   map[string]string
	TraefikDir string
}

// Router is the part of Traefik's API router this needs.
type Router struct {
	Name string `json:"name"`
	Rule string `json:"rule"`
	TLS  *struct {
		CertResolver string `json:"certResolver"`
	} `json:"tls"`
}

// ParseFacts reads DomainsScript output.
func ParseFacts(out string) (*Facts, error) {
	raw := map[string][]byte{}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			if k != "" {
				raw[k] = nil
			}
			continue
		}
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("server output for %s: %w", k, err)
		}
		raw[k] = b
	}
	if _, ok := raw["env"]; !ok {
		return nil, fmt.Errorf("unexpected server output:\n%s", out)
	}
	f := &Facts{
		ServerIP:   strings.TrimSpace(string(raw["ip"])),
		TraefikDir: string(raw["traefik"]),
		Accounts:   map[string]string{},
	}
	env, err := dotenv.UnmarshalWithLookup(string(raw["env"]), nil)
	if err != nil {
		return nil, fmt.Errorf("server environment: %w", err)
	}
	f.Env = env
	if len(raw["routers"]) > 0 {
		if err := json.Unmarshal(raw["routers"], &f.Routers); err != nil {
			return nil, fmt.Errorf("Traefik routers: %w", err)
		}
	}
	if len(raw["accounts"]) > 0 {
		var accounts map[string]struct {
			FullDomain string `json:"fulldomain"`
		}
		if err := json.Unmarshal(raw["accounts"], &accounts); err != nil {
			return nil, fmt.Errorf("acme-dns registrations: %w", err)
		}
		for d, a := range accounts {
			f.Accounts[strings.TrimPrefix(d, "*.")] = a.FullDomain
		}
	}
	return f, nil
}

// Hosts is the public host followed by DEVOPSY_DOMAINS, without duplicates.
func (f *Facts) Hosts() []string {
	seen := map[string]bool{}
	var hosts []string
	add := func(h string) {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" && !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	add(f.Env["DEVOPSY_PUBLIC_HOST"])
	for _, h := range strings.FieldsFunc(f.Env["DEVOPSY_DOMAINS"], func(r rune) bool {
		return r == ' ' || r == ',' || r == '\t' || r == '\n'
	}) {
		add(h)
	}
	return hosts
}

// Resolver returns the cert resolver of the router serving host, "" when no
// router serves it. Routers with TLS win over their HTTP twins.
func (f *Facts) Resolver(host string) (resolver string, routed bool) {
	needle := "Host(`" + host + "`)"
	for _, r := range f.Routers {
		if !strings.Contains(strings.ToLower(r.Rule), strings.ToLower(needle)) {
			continue
		}
		routed = true
		if r.TLS != nil && r.TLS.CertResolver != "" {
			return r.TLS.CertResolver, true
		}
	}
	return "", routed
}

// CertInfo describes the certificate a server presents for a name.
type CertInfo struct {
	Valid   bool
	Problem string
	Issuer  string
	Expires time.Time
}

// Checker does the network checks; replaceable in tests.
type Checker struct {
	LookupIP    func(ctx context.Context, host string) ([]string, error)
	LookupCNAME func(ctx context.Context, host string) (string, error)
	Cert        func(ctx context.Context, ip, host string) (CertInfo, error)
	// Proxy reports which CDN proxy an IP belongs to, "" for none.
	Proxy func(ip string) string
	// Get requests https://host/ through ip, as a browser would reach it, and
	// returns the HTTP status.
	Get func(ctx context.Context, ip, host string) (int, error)
}

// cloudflareRanges is Cloudflare's published IPv4 list
// (https://www.cloudflare.com/ips-v4), used when it cannot be fetched.
var cloudflareRanges = []string{
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
}

// cloudflareNets fetches Cloudflare's current ranges, falling back to the
// built-in list.
func cloudflareNets(ctx context.Context) []*net.IPNet {
	ranges := cloudflareRanges
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://www.cloudflare.com/ips-v4", nil)
	client := &http.Client{Timeout: 5 * time.Second}
	if resp, err := client.Do(req); err == nil {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if fetched := strings.Fields(string(body)); resp.StatusCode == 200 && len(fetched) > 0 {
			ranges = fetched
		}
	}
	var nets []*net.IPNet
	for _, r := range ranges {
		if _, n, err := net.ParseCIDR(r); err == nil {
			nets = append(nets, n)
		}
	}
	return nets
}

// PublicChecker resolves through 1.1.1.1, as the rest of the Internet sees
// DNS, and verifies certificates against the system's trusted roots.
func PublicChecker(ctx context.Context) Checker {
	cf := cloudflareNets(ctx)
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, "1.1.1.1:53")
		},
	}
	return Checker{
		LookupIP: func(ctx context.Context, host string) ([]string, error) {
			ips, err := r.LookupIP(ctx, "ip4", host)
			out := make([]string, len(ips))
			for i, ip := range ips {
				out[i] = ip.String()
			}
			return out, err
		},
		LookupCNAME: func(ctx context.Context, host string) (string, error) {
			c, err := r.LookupCNAME(ctx, host)
			return strings.TrimSuffix(c, "."), err
		},
		Proxy: func(ip string) string {
			parsed := net.ParseIP(ip)
			for _, n := range cf {
				if parsed != nil && n.Contains(parsed) {
					return "Cloudflare"
				}
			}
			return ""
		},
		Get: func(ctx context.Context, ip, host string) (int, error) {
			dialer := &net.Dialer{Timeout: 10 * time.Second}
			client := &http.Client{
				Timeout: 15 * time.Second,
				Transport: &http.Transport{
					DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
						return dialer.DialContext(ctx, network, net.JoinHostPort(ip, "443"))
					},
					TLSClientConfig: &tls.Config{ServerName: host},
				},
				CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			}
			req, err := http.NewRequestWithContext(ctx, "GET", "https://"+host+"/", nil)
			if err != nil {
				return 0, err
			}
			resp, err := client.Do(req)
			if err != nil {
				return 0, err
			}
			resp.Body.Close()
			return resp.StatusCode, nil
		},
		Cert: func(ctx context.Context, ip, host string) (CertInfo, error) {
			d := tls.Dialer{
				NetDialer: &net.Dialer{Timeout: 10 * time.Second},
				Config:    &tls.Config{ServerName: host, InsecureSkipVerify: true},
			}
			conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, "443"))
			if err != nil {
				return CertInfo{}, err
			}
			defer conn.Close()
			certs := conn.(*tls.Conn).ConnectionState().PeerCertificates
			if len(certs) == 0 {
				return CertInfo{}, fmt.Errorf("no certificate")
			}
			leaf := certs[0]
			info := CertInfo{Issuer: issuerName(leaf), Expires: leaf.NotAfter}
			pool := x509.NewCertPool()
			for _, c := range certs[1:] {
				pool.AddCert(c)
			}
			if _, err := leaf.Verify(x509.VerifyOptions{DNSName: host, Intermediates: pool}); err != nil {
				info.Problem = err.Error()
			} else {
				info.Valid = true
			}
			return info, nil
		},
	}
}

func issuerName(c *x509.Certificate) string {
	parts := []string{}
	if len(c.Issuer.Organization) > 0 {
		parts = append(parts, c.Issuer.Organization[0])
	}
	if c.Issuer.CommonName != "" {
		parts = append(parts, c.Issuer.CommonName)
	}
	return strings.Join(parts, " ")
}

// DomainReport is the state of one host and what to do next.
type DomainReport struct {
	Host     string
	Resolver string
	Routed   bool
	// IPs the host resolves to, and whether one is the server.
	IPs      []string
	DNSError string
	OnServer bool
	// Proxy names the CDN proxy the host resolves to, like Cloudflare, and
	// ProxyStatus is the HTTP status through it (0 when unreachable).
	Proxy       string
	ProxyStatus int
	ProxyError  string
	// acme-dns: the CNAME the challenge needs, what it is now.
	ChallengeWant string
	ChallengeHave string
	Cert          CertInfo
	CertError     string
	Next          string
	Live          bool
}

// Check builds a report per host. serverIPs are the addresses that count as
// this server.
func Check(ctx context.Context, f *Facts, c Checker, serverIPs []string) []DomainReport {
	var reports []DomainReport
	connectIP := ""
	if len(serverIPs) > 0 {
		connectIP = serverIPs[0]
	}
	for _, host := range f.Hosts() {
		r := checkHost(ctx, f, c, serverIPs, connectIP, host)
		reports = append(reports, r)
	}
	return reports
}

// checkHost checks one host, with its own time limit so an unreachable one
// cannot use up the others' time.
func checkHost(parent context.Context, f *Facts, c Checker, serverIPs []string, connectIP, host string) DomainReport {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	{
		r := DomainReport{Host: host}
		r.Resolver, r.Routed = f.Resolver(host)

		ips, err := c.LookupIP(ctx, host)
		r.IPs = ips
		if err != nil {
			r.DNSError = err.Error()
		}
		for _, ip := range ips {
			for _, s := range serverIPs {
				if ip == s {
					r.OnServer = true
				}
			}
		}
		if !r.OnServer && len(ips) > 0 && c.Proxy != nil {
			proxy := c.Proxy(ips[0])
			for _, ip := range ips {
				if c.Proxy(ip) != proxy {
					proxy = ""
				}
			}
			if proxy != "" && c.Get != nil {
				r.Proxy = proxy
				status, err := c.Get(ctx, ips[0], host)
				r.ProxyStatus = status
				if err != nil {
					r.ProxyError = err.Error()
				}
			}
		}

		if r.Resolver == "acmedns" {
			r.ChallengeWant = f.Accounts[host]
			if cname, err := c.LookupCNAME(ctx, "_acme-challenge."+host); err == nil && cname != "_acme-challenge."+host {
				r.ChallengeHave = cname
			}
		}

		if connectIP != "" {
			info, err := c.Cert(ctx, connectIP, host)
			r.Cert = info
			if err != nil {
				r.CertError = err.Error()
			}
		}

		r.Next, r.Live = nextStep(r, connectIP)
		return r
	}
}

func nextStep(r DomainReport, ip string) (string, bool) {
	switch {
	case !r.Routed:
		return "no Traefik router serves it: is the site running, with its rule from DEVOPSY_HOST_RULE?", false
	case r.Cert.Valid && r.OnServer:
		return "live", true
	case r.Proxy != "":
		return proxyStep(r, ip)
	case r.Cert.Valid:
		return fmt.Sprintf("certificate ready: point its DNS at %s (A record) to switch", ip), false
	}
	switch r.Resolver {
	case "acmedns":
		switch {
		case r.ChallengeWant == "":
			return "not registered with acme-dns yet: run with --retry", false
		case !strings.EqualFold(r.ChallengeHave, r.ChallengeWant):
			return fmt.Sprintf("create, where its DNS is hosted: _acme-challenge.%s. CNAME %s.", r.Host, r.ChallengeWant), false
		default:
			return "challenge CNAME in place: run with --retry", false
		}
	case "cloudflare":
		return "run with --retry; the Cloudflare token must cover this domain's zone", false
	default:
		if !r.OnServer {
			return fmt.Sprintf("point its DNS at %s (HTTP-01 needs it), then run with --retry", ip), false
		}
		return "DNS points here: run with --retry", false
	}
}

// proxyStep judges a host behind a CDN proxy. The proxy reaches the server
// itself, so DNS pointing elsewhere is expected. Its 52x statuses are
// Cloudflare's origin errors.
func proxyStep(r DomainReport, ip string) (string, bool) {
	switch {
	case r.ProxyStatus == 0:
		return fmt.Sprintf("%s proxy did not answer: %s", r.Proxy, r.ProxyError), false
	case r.ProxyStatus == 521 || r.ProxyStatus == 522 || r.ProxyStatus == 523:
		return fmt.Sprintf("%s cannot reach the server (%d): check its DNS origin is %s and ports 80 and 443 are open", r.Proxy, r.ProxyStatus, ip), false
	case r.ProxyStatus == 525:
		return fmt.Sprintf("%s's TLS handshake with the server fails (525): does the server have a certificate for this name?", r.Proxy), false
	case r.ProxyStatus == 526:
		return fmt.Sprintf("%s rejects the server's certificate (526): get a valid one first, or set its SSL mode to Full", r.Proxy), false
	case r.ProxyStatus >= 520 && r.ProxyStatus <= 530:
		return fmt.Sprintf("%s reports an origin error (%d)", r.Proxy, r.ProxyStatus), false
	case !r.Cert.Valid:
		return fmt.Sprintf("served through %s, but the server has no valid certificate for it: with SSL mode Full (strict) that fails; run with --retry", r.Proxy), false
	}
	return "live", true
}

// FormatReports renders reports for people.
func FormatReports(reports []DomainReport) string {
	var b strings.Builder
	for _, r := range reports {
		state := "pending"
		if r.Live {
			state = "live"
		}
		fmt.Fprintf(&b, "%s  [%s]\n", r.Host, state)
		resolver := r.Resolver
		if resolver == "" {
			resolver = "-"
		}
		fmt.Fprintf(&b, "  resolver     %s\n", resolver)
		dns := strings.Join(r.IPs, ", ")
		switch {
		case r.DNSError != "" && dns == "":
			dns = "does not resolve"
		case r.OnServer:
			dns += " (this server)"
		case r.Proxy != "":
			dns += fmt.Sprintf(" (%s proxy, HTTP %d through it)", r.Proxy, r.ProxyStatus)
		default:
			dns += " (not this server)"
		}
		fmt.Fprintf(&b, "  dns          %s\n", dns)
		// The challenge only matters while there is no valid certificate, or
		// when the record exists (renewals use it).
		if r.Resolver == "acmedns" && (!r.Cert.Valid || r.ChallengeHave != "") {
			have := r.ChallengeHave
			if have == "" {
				have = "missing"
			} else if strings.EqualFold(have, r.ChallengeWant) {
				have = "ok"
			} else {
				have = "points to " + have
			}
			fmt.Fprintf(&b, "  challenge    %s\n", have)
		}
		switch {
		case r.CertError != "":
			fmt.Fprintf(&b, "  certificate  error: %s\n", r.CertError)
		case r.Cert.Valid:
			fmt.Fprintf(&b, "  certificate  valid, %s, expires %s\n", r.Cert.Issuer, r.Cert.Expires.Format("2006-01-02"))
		case r.Cert.Issuer != "":
			fmt.Fprintf(&b, "  certificate  not valid for this name (%s)\n", r.Cert.Issuer)
		}
		if !r.Live {
			fmt.Fprintf(&b, "  next         %s\n", r.Next)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// RetryScript writes a Traefik dynamic configuration file that requests the
// certificates of hosts again, grouped by resolver. Every run uses new router
// names, which Traefik sees as a configuration change: no restart, no
// downtime. The file is replaced on the next retry.
func RetryScript(traefikDir, project string, byResolver map[string][]string, now time.Time) string {
	stamp := now.UTC().Format("20060102150405")
	var y strings.Builder
	y.WriteString("# Written by `devopsy @<target> domains --retry`: asks Traefik to request\n# these certificates again. Safe to delete.\nhttp:\n  routers:\n")
	resolvers := make([]string, 0, len(byResolver))
	for r := range byResolver {
		resolvers = append(resolvers, r)
	}
	sort.Strings(resolvers)
	for _, res := range resolvers {
		fmt.Fprintf(&y, "    devopsy-retry-%s-%s-%s:\n", project, res, stamp)
		fmt.Fprintf(&y, "      rule: 'Host(`devopsy-retry-%s.invalid`)'\n", stamp)
		y.WriteString("      entryPoints: [websecure]\n      service: noop@internal\n      tls:\n")
		fmt.Fprintf(&y, "        certResolver: %s\n        domains:\n", res)
		for _, h := range byResolver[res] {
			fmt.Fprintf(&y, "          - main: '%s'\n", h)
		}
	}
	file := RetryFile(traefikDir, project)
	return fmt.Sprintf("set -eu\nprintf '%%s' %s > %s.tmp\nmv %s.tmp %s\n",
		Quote(y.String()), Quote(file), Quote(file), Quote(file))
}

// RetryFile is where RetryScript writes, for a project.
func RetryFile(traefikDir, project string) string {
	return traefikDir + "/.devopsy/mnt/dynamic/devopsy-retry-" + project + ".yaml"
}

// CleanupRetryScript removes a project's retry file, if any, and prints
// "removed" when it did. Once the certificates exist the file is only
// clutter: Traefik keeps and renews them without it.
func CleanupRetryScript(traefikDir, project string) string {
	f := Quote(RetryFile(traefikDir, project))
	return "set -eu\nif [ -e " + f + " ]; then rm -f " + f + "; echo removed; fi\n"
}

// AllCertified reports whether every routed host has a valid certificate.
func AllCertified(reports []DomainReport) bool {
	for _, r := range reports {
		if r.Routed && !r.Cert.Valid {
			return false
		}
	}
	return true
}
