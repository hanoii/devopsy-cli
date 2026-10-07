package remote

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/dotenv"
)

// ProxyFactsVersion is the newest version of the domains capability's facts
// this devopsy reads.
const ProxyFactsVersion = 1

// ProxyFacts is what the proxy's domains capability reports (`facts`): see
// README, "The domains capability".
type ProxyFacts struct {
	Version int    `json:"version"`
	IP      string `json:"ip"`
	// Retries are the names with a pending retry.
	Retries []string    `json:"retries"`
	Hosts   []HostFacts `json:"hosts"`
}

// HostFacts is what the proxy knows about one host.
type HostFacts struct {
	Host string `json:"host"`
	// Probe is the name to look up and connect to for a wildcard: one its
	// router matches.
	Probe  string `json:"probe"`
	Routed bool   `json:"routed"`
	// Resolver is informative only, shown in the report.
	Resolver string `json:"resolver"`
	// Method is how the certificate is issued: MethodHTTP, MethodDNSCNAME or
	// MethodDNSAPI ("" when unknown).
	Method   string     `json:"method"`
	Record   *DNSRecord `json:"record"`
	Wildcard string     `json:"wildcard"`
}

// DNSRecord is a record to create.
type DNSRecord struct {
	Name   string `json:"name"`
	Target string `json:"target"`
}

// Certificate issuing methods.
const (
	// MethodHTTP is HTTP-01: the host must reach the server.
	MethodHTTP = "http"
	// MethodDNSCNAME is DNS-01 through a delegated record, HostFacts.Record.
	MethodDNSCNAME = "dns-cname"
	// MethodDNSAPI is DNS-01 through the DNS provider's API.
	MethodDNSAPI = "dns-api"
)

// ParseProxyFacts reads the facts capability's output.
func ParseProxyFacts(out []byte) (*ProxyFacts, error) {
	var f ProxyFacts
	if err := json.Unmarshal(out, &f); err != nil {
		return nil, fmt.Errorf("the proxy's domains facts are not JSON: %w\n%s", err, out)
	}
	switch {
	case f.Version == 0:
		return nil, fmt.Errorf("the proxy's domains facts have no version:\n%s", out)
	case f.Version > ProxyFactsVersion:
		return nil, fmt.Errorf("the proxy reports domains facts version %d, this devopsy reads up to %d: upgrade devopsy (devopsy --upgrade)", f.Version, ProxyFactsVersion)
	}
	return &f, nil
}

// Host returns the facts about host, or an unrouted host.
func (f *ProxyFacts) Host(host string) HostFacts {
	for _, h := range f.Hosts {
		if strings.EqualFold(h.Host, host) {
			return h
		}
	}
	return HostFacts{Host: host}
}

// HasRetry reports whether name has a pending retry.
func (f *ProxyFacts) HasRetry(name string) bool {
	for _, r := range f.Retries {
		if r == name {
			return true
		}
	}
	return false
}

// AllHosts is every host the proxy reported, in its order.
func (f *ProxyFacts) AllHosts() []string {
	hosts := make([]string, len(f.Hosts))
	for i, h := range f.Hosts {
		hosts[i] = h.Host
	}
	return hosts
}

// ProjectHosts is a project's public host followed by its DEVOPSY_DOMAINS,
// lowercase, without duplicates, from its environment.
func ProjectHosts(env map[string]string) []string {
	seen := map[string]bool{}
	var hosts []string
	add := func(h string) {
		h = strings.ToLower(strings.TrimSpace(h))
		if h != "" && !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	add(env["DEVOPSY_PUBLIC_HOST"])
	for _, h := range strings.FieldsFunc(env["DEVOPSY_DOMAINS"], func(r rune) bool {
		return r == ' ' || r == ',' || r == '\t' || r == '\n'
	}) {
		add(h)
	}
	return hosts
}

// ParseEnv reads EnvScript's output.
func ParseEnv(out string) (map[string]string, error) {
	env, err := dotenv.UnmarshalWithLookup(out, nil)
	if err != nil {
		return nil, fmt.Errorf("the target's environment: %w", err)
	}
	return env, nil
}

// DefaultProxyDir is where the server's proxy lives unless a target's
// DEVOPSY_PROXY_DIR says otherwise.
const DefaultProxyDir = "/srv/traefik"

// EnvScript prints the target's environment, as devopsy computes it there.
// print-env: the old name of --env, which any server version answers.
func EnvScript(t *Target, projectName string) string {
	return "set -eu\n" + enter(t) + devopsyCall(projectName, []string{"print-env"}, true) + "\n"
}

// CapabilityScript runs a capability's action in the project at dir (its
// current release, if any): devopsy --capability, in a fresh session, so no
// other project's environment applies. Its messages on stderr only show when
// it fails.
func CapabilityScript(dir, name, action string, args []string) string {
	call := devopsyCall("", append([]string{"--capability", name, action}, args...), false)
	s := "set -eu\ndir=" + Quote(dir) + `
cd "$dir" 2>/dev/null || { echo "devopsy: no project at $dir" >&2; exit 1; }
[ ! -d current ] || cd current
`
	if Verbose {
		return s + "exec " + call + "\n"
	}
	return s + `err=$(mktemp)
status=0
` + call + ` 2>"$err" || status=$?
[ "$status" = 0 ] || cat "$err" >&2
rm -f "$err"
exit "$status"
`
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
	Method   string
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
	// dns-cname: the CNAME the challenge needs, what it is now.
	ChallengeWant string
	ChallengeHave string
	Cert          CertInfo
	CertError     string
	Next          string
	Live          bool
}

// Check builds a report per host (ProjectHosts for a project, f.AllHosts()
// for the server's proxy). serverIPs are the addresses that count as this
// server.
func Check(ctx context.Context, f *ProxyFacts, c Checker, serverIPs []string, hosts []string) []DomainReport {
	var reports []DomainReport
	connectIP := ""
	if len(serverIPs) > 0 {
		connectIP = serverIPs[0]
	}
	for _, host := range hosts {
		r := checkHost(ctx, f, c, serverIPs, connectIP, host)
		reports = append(reports, r)
	}
	return reports
}

// checkHost checks one host, with its own time limit so an unreachable one
// cannot use up the others' time.
func checkHost(parent context.Context, f *ProxyFacts, c Checker, serverIPs []string, connectIP, host string) DomainReport {
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	{
		hf := f.Host(host)
		r := DomainReport{Host: host, Resolver: hf.Resolver, Method: hf.Method, Routed: hf.Routed}
		// A wildcard is checked through a name it covers; its challenge is the
		// domain's.
		name, base := host, strings.TrimPrefix(host, "*.")
		if hf.Probe != "" {
			name = hf.Probe
		} else if name != base {
			name = "devopsy-wildcard." + base
		}

		ips, err := c.LookupIP(ctx, name)
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
				status, err := c.Get(ctx, ips[0], name)
				r.ProxyStatus = status
				if err != nil {
					r.ProxyError = err.Error()
				}
			}
		}

		if r.Method == MethodDNSCNAME {
			record := "_acme-challenge." + base
			if hf.Record != nil {
				record, r.ChallengeWant = strings.TrimSuffix(hf.Record.Name, "."), strings.TrimSuffix(hf.Record.Target, ".")
			}
			if cname, err := c.LookupCNAME(ctx, record); err == nil && strings.TrimSuffix(cname, ".") != record {
				r.ChallengeHave = strings.TrimSuffix(cname, ".")
			}
		}

		if connectIP != "" {
			info, err := c.Cert(ctx, connectIP, name)
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
		return "the server's proxy does not route it: is the site running, with its rule from DEVOPSY_HOST_RULE?", false
	case r.Cert.Valid && r.OnServer:
		return "live", true
	case r.Proxy != "":
		return proxyStep(r, ip)
	case r.Cert.Valid:
		return fmt.Sprintf("certificate ready: point its DNS at %s (A record) to switch", ip), false
	}
	switch r.Method {
	case MethodDNSCNAME:
		switch {
		case r.ChallengeWant == "":
			return "its challenge record is not known yet (not registered): run with --retry", false
		case !strings.EqualFold(r.ChallengeHave, r.ChallengeWant):
			return fmt.Sprintf("create, where its DNS is hosted: _acme-challenge.%s. CNAME %s.", strings.TrimPrefix(r.Host, "*."), r.ChallengeWant), false
		default:
			return "challenge CNAME in place: run with --retry", false
		}
	case MethodDNSAPI:
		return "run with --retry; the proxy's DNS API token must cover this domain's zone", false
	case MethodHTTP:
		if !r.OnServer {
			return fmt.Sprintf("point its DNS at %s (HTTP-01 needs it), then run with --retry", ip), false
		}
		return "DNS points here: run with --retry", false
	default:
		return fmt.Sprintf("the proxy does not say how it issues this certificate (resolver %q): run with --retry", r.Resolver), false
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
		// The proxy got an answer from the server despite its invalid
		// certificate, so its SSL mode is not Full (strict).
		return fmt.Sprintf("served through %s (its SSL mode is not Full (strict)), but the server has no certificate for it yet: the proxy requests one once the site is routed, check again in a minute, else run with --retry; get one before switching to Full (strict)", r.Proxy), false
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
		if r.Method == MethodDNSCNAME && (!r.Cert.Valid || r.ChallengeHave != "") {
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

// AllCertified reports whether every routed host has a valid certificate.
func AllCertified(reports []DomainReport) bool {
	for _, r := range reports {
		if r.Routed && !r.Cert.Valid {
			return false
		}
	}
	return true
}
