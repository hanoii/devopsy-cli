// Package probe checks hosts from where devopsy runs, as visitors reach
// them: DNS through a public resolver, the certificate a server presents,
// and an HTTPS request. It knows no proxy: `devopsy --probe [--ip <server
// ip>] <host>...`, which a proxy's own commands can print for people to run.
package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// CertInfo describes the certificate a server presents for a name.
type CertInfo struct {
	Valid   bool
	Problem string
	Issuer  string
	Expires time.Time
}

// Checker does the network checks; replaceable in tests.
type Checker struct {
	LookupIP func(ctx context.Context, host string) ([]string, error)
	Cert     func(ctx context.Context, ip, host string) (CertInfo, error)
	// CDN names the CDN proxy an IP belongs to, "" for none.
	CDN func(ip string) string
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

// Public resolves through 1.1.1.1, as the rest of the Internet sees DNS, and
// verifies certificates against the system's trusted roots.
func Public(ctx context.Context) Checker {
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
		CDN: func(ip string) string {
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

// Report is what a probe found about one host.
type Report struct {
	Host string
	// IPs the host resolves to; OnServer when one is the --ip.
	IPs      []string
	DNSError string
	OnServer bool
	// CDN names the CDN proxy the host resolves to, like Cloudflare.
	CDN string
	// CertIP is where the certificate was checked: the server (--ip) when
	// given, else what the host resolves to.
	CertIP    string
	Cert      CertInfo
	CertError string
	// Status is the HTTPS status through what the host resolves to, as
	// visitors get it (0 when unreachable).
	Status    int
	HTTPError string
	Problems  []string
}

// OK reports whether nothing is wrong.
func (r Report) OK() bool { return len(r.Problems) == 0 }

// Host probes one host. ip is the server it should reach, "" when unknown.
func Host(parent context.Context, c Checker, ip, host string) Report {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	r := Report{Host: host}
	if strings.HasPrefix(host, "*.") {
		r.Problems = append(r.Problems, "a wildcard cannot be probed: name a host it covers")
		return r
	}

	ips, err := c.LookupIP(ctx, host)
	r.IPs = ips
	if err != nil || len(ips) == 0 {
		r.DNSError = "does not resolve"
		if err != nil {
			r.DNSError += ": " + err.Error()
		}
	}
	for _, a := range ips {
		if a == ip {
			r.OnServer = true
		}
	}
	if !r.OnServer && len(ips) > 0 && c.CDN != nil {
		cdn := c.CDN(ips[0])
		for _, a := range ips {
			if c.CDN(a) != cdn {
				cdn = ""
			}
		}
		r.CDN = cdn
	}

	r.CertIP = ip
	if r.CertIP == "" && len(ips) > 0 {
		r.CertIP = ips[0]
	}
	if r.CertIP != "" {
		info, err := c.Cert(ctx, r.CertIP, host)
		r.Cert = info
		if err != nil {
			r.CertError = err.Error()
		}
	}
	if len(ips) > 0 {
		status, err := c.Get(ctx, ips[0], host)
		r.Status = status
		if err != nil {
			r.HTTPError = err.Error()
		}
	}
	r.Problems = append(r.Problems, problems(r, ip)...)
	return r
}

func problems(r Report, ip string) []string {
	var out []string
	switch {
	case r.DNSError != "":
		if ip != "" {
			out = append(out, fmt.Sprintf("DNS: point it at %s", ip))
		} else {
			out = append(out, "DNS: it does not resolve")
		}
	case ip != "" && !r.OnServer && r.CDN == "":
		out = append(out, fmt.Sprintf("DNS: it points elsewhere, not at %s", ip))
	}
	switch {
	case r.CertError != "":
		out = append(out, fmt.Sprintf("certificate: no TLS connection to %s: %s", r.CertIP, r.CertError))
	case r.CertIP != "" && !r.Cert.Valid:
		out = append(out, fmt.Sprintf("certificate: %s presents none valid for this name (%s)", r.CertIP, r.Cert.Problem))
	}
	switch {
	case len(r.IPs) == 0:
	case r.Status == 0:
		out = append(out, "https: no answer: "+r.HTTPError)
	case r.CDN != "" && (r.Status == 521 || r.Status == 522 || r.Status == 523):
		out = append(out, fmt.Sprintf("https: %s cannot reach the server (%d): check its origin and ports 80 and 443", r.CDN, r.Status))
	case r.CDN != "" && r.Status == 525:
		out = append(out, fmt.Sprintf("https: %s's TLS handshake with the server fails (525): does the server have a certificate for it?", r.CDN))
	case r.CDN != "" && r.Status == 526:
		out = append(out, fmt.Sprintf("https: %s rejects the server's certificate (526): get a valid one, or set its SSL mode to Full", r.CDN))
	case r.Status >= 500:
		out = append(out, fmt.Sprintf("https: %d", r.Status))
	}
	return out
}

// Format renders reports for people.
func Format(reports []Report) string {
	var b strings.Builder
	for _, r := range reports {
		state := "ok"
		if !r.OK() {
			state = "problem"
		}
		fmt.Fprintf(&b, "%s  [%s]\n", r.Host, state)
		if len(r.IPs) > 0 || r.DNSError != "" {
			dns := strings.Join(r.IPs, ", ")
			switch {
			case r.DNSError != "":
				dns = r.DNSError
			case r.OnServer:
				dns += " (the server)"
			case r.CDN != "":
				dns += " (" + r.CDN + " proxy)"
			}
			fmt.Fprintf(&b, "  dns          %s\n", dns)
		}
		switch {
		case r.CertError != "":
			fmt.Fprintf(&b, "  certificate  error: %s\n", r.CertError)
		case r.Cert.Valid:
			fmt.Fprintf(&b, "  certificate  valid, %s, expires %s (from %s)\n", r.Cert.Issuer, r.Cert.Expires.Format("2006-01-02"), r.CertIP)
		case r.Cert.Issuer != "":
			fmt.Fprintf(&b, "  certificate  not valid for this name, %s (from %s)\n", r.Cert.Issuer, r.CertIP)
		}
		if r.Status != 0 {
			fmt.Fprintf(&b, "  https        %d\n", r.Status)
		}
		for _, p := range r.Problems {
			fmt.Fprintf(&b, "  problem      %s\n", p)
		}
		b.WriteString("\n")
	}
	return b.String()
}
