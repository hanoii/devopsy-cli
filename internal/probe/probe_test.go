package probe

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fake answers from fixed tables: DNS, certificates per "ip host", HTTPS
// statuses per "ip host".
func fake(dns map[string][]string, certs map[string]CertInfo, status map[string]int) Checker {
	return Checker{
		LookupIP: func(_ context.Context, host string) ([]string, error) {
			if ips, ok := dns[host]; ok {
				return ips, nil
			}
			return nil, errors.New("no such host")
		},
		Cert: func(_ context.Context, ip, host string) (CertInfo, error) {
			if c, ok := certs[ip+" "+host]; ok {
				return c, nil
			}
			return CertInfo{}, errors.New("connection refused")
		},
		CDN: func(ip string) string {
			if strings.HasPrefix(ip, "104.") {
				return "Cloudflare"
			}
			return ""
		},
		Get: func(_ context.Context, ip, host string) (int, error) {
			if s, ok := status[ip+" "+host]; ok {
				return s, nil
			}
			return 0, errors.New("timeout")
		},
	}
}

func TestHost(t *testing.T) {
	valid := CertInfo{Valid: true, Issuer: "Let's Encrypt R11", Expires: time.Date(2027, 1, 3, 0, 0, 0, 0, time.UTC)}
	c := fake(map[string][]string{
		"live.org":      {"10.0.0.5"},
		"elsewhere.org": {"10.9.9.9"},
		"proxied.org":   {"104.16.1.1"},
		"down.org":      {"104.16.1.1"},
		"nocert.org":    {"10.0.0.5"},
	}, map[string]CertInfo{
		"10.0.0.5 live.org":      valid,
		"10.0.0.5 proxied.org":   valid,
		"10.0.0.5 down.org":      valid,
		"10.0.0.5 nocert.org":    {Issuer: "TRAEFIK DEFAULT CERT", Problem: "x509: not valid"},
		"10.9.9.9 elsewhere.org": valid,
	}, map[string]int{
		"10.0.0.5 live.org":      200,
		"104.16.1.1 proxied.org": 302,
		"104.16.1.1 down.org":    522,
		"10.0.0.5 nocert.org":    200,
		"10.9.9.9 elsewhere.org": 200,
	})
	ctx := context.Background()
	for host, want := range map[string][]string{
		"live.org":      nil,
		"proxied.org":   nil,
		"down.org":      {"Cloudflare cannot reach the server (522)"},
		"nocert.org":    {"presents none valid"},
		"elsewhere.org": {"points elsewhere, not at 10.0.0.5"},
		"new.org":       {"DNS: point it at 10.0.0.5", "no TLS connection"},
		"*.vm1.org":     {"name a host it covers"},
	} {
		r := Host(ctx, c, "10.0.0.5", host)
		if len(want) == 0 && !r.OK() {
			t.Errorf("%s: %q", host, r.Problems)
		}
		got := strings.Join(r.Problems, "\n")
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: problems %q, want %q", host, r.Problems, w)
			}
		}
	}

	// Without --ip: the certificate where the host resolves.
	r := Host(ctx, c, "", "elsewhere.org")
	if !r.OK() || r.CertIP != "10.9.9.9" {
		t.Fatalf("no ip: %+v", r)
	}
	out := Format([]Report{Host(ctx, c, "10.0.0.5", "live.org"), Host(ctx, c, "10.0.0.5", "down.org")})
	for _, w := range []string{"live.org  [ok]", "10.0.0.5 (the server)", "valid, Let's Encrypt R11, expires 2027-01-03 (from 10.0.0.5)", "down.org  [problem]", "104.16.1.1 (Cloudflare proxy)", "https        522"} {
		if !strings.Contains(out, w) {
			t.Errorf("format: no %q in\n%s", w, out)
		}
	}
}
