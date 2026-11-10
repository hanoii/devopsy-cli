package door

import (
	"net/netip"
	"regexp"
	"strings"
)

// Metadata4 and Metadata6 are the cloud metadata services' addresses, which hand out an
// instance's credentials: in IPv4 link-local (AWS's IMDS, DigitalOcean's, Tencent's,
// Scaleway's, ECS's), then Alibaba's and Azure's WireServer; in IPv6, AWS's, Google's,
// OpenStack's and Oracle's. The project users' firewall closes them (internal/remote), and no
// registry is at one.
var (
	Metadata4 = []string{"169.254.0.0/16", "100.100.100.200/32", "168.63.129.16/32"}
	Metadata6 = []string{"fd00:ec2::254/128", "fd20:ce::254/128", "fe80::a9fe:a9fe/128", "fd00:c1::a9fe:a9fe/128"}
)

// registryHost is the host a ref pulls from, as Docker reads it: the first part when it holds a
// dot, a colon or a capital, or is localhost, else Docker Hub's (""), its port and brackets
// dropped.
func registryHost(ref string) string {
	first, _, ok := strings.Cut(ref, "/")
	if !ok || (!strings.ContainsAny(first, ".:") && first != "localhost" && strings.ToLower(first) == first) {
		return ""
	}
	return hostOf(first)
}

// hostOf is a registry's host, its port and an IPv6 address's brackets dropped.
func hostOf(registry string) string {
	if rest, ok := strings.CutPrefix(registry, "["); ok {
		h, _, _ := strings.Cut(rest, "]")
		return h
	}
	h, _, _ := strings.Cut(registry, ":")
	return h
}

// ownHost says whether host is the server's own or a metadata service's: loopback (an
// address, localhost, a name under .localhost), link-local, unspecified, a metadata address,
// or a name a C resolver reads as an address (127.1, 0x7f.1, 0x7f000001: its last label a
// number, hexadecimal included), in any case.
// Docker's daemon contacts a registry as root, outside the project users' firewall, so a
// project user's pull or login of one would reach for it what the firewall closes.
func ownHost(host string) bool {
	if host == "" {
		return false
	}
	host = strings.ToLower(host)
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if a, err := netip.ParseAddr(host); err == nil {
		a = a.Unmap()
		if a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsUnspecified() {
			return true
		}
		for _, m := range append(Metadata4, Metadata6...) {
			if p, err := netip.ParsePrefix(m); err == nil && p.Contains(a) {
				return true
			}
		}
		return false
	}
	labels := strings.Split(host, ".")
	return addressLabel.MatchString(labels[len(labels)-1])
}

// addressLabel is a label inet_aton reads as a number: decimal, octal or hexadecimal.
var addressLabel = regexp.MustCompile(`^(0x[0-9a-f]*|[0-9]+)$`)
