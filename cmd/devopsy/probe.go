package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/probe"
)

const probeUsage = "usage: devopsy --probe [--ip <server ip>] <host>..."

// runProbe implements `devopsy --probe [--ip <server ip>] <host>...`: DNS,
// the certificate and HTTPS of each host, from here. Exits 1 when any host
// has a problem, so it works as a smoke test.
func runProbe(args []string, color bool) int {
	ip := ""
	var hosts []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--help" || a == "-h":
			fmt.Println(probeUsage)
			fmt.Print(`
For each host, from where devopsy runs: DNS through 1.1.1.1, IPv4 and IPv6
(and whether it points at --ip, either kind, or at a CDN proxy like
Cloudflare: IPv4 ranges only), the certificate the
server presents (at --ip, else where the host resolves; verified against
the system's roots) and an HTTPS request as visitors make it. Exits 1 when
a host has a problem. It knows no proxy: proxies' own commands print the
line to run, like devopsy-traefik's domains.
`)
			return 0
		case a == "--ip" && i+1 < len(args):
			ip = args[i+1]
			i++
		case strings.HasPrefix(a, "--ip="):
			ip = strings.TrimPrefix(a, "--ip=")
		case strings.HasPrefix(a, "-"):
			cli.Fprint(os.Stderr, red, probeUsage, color)
			return 1
		default:
			hosts = append(hosts, strings.ToLower(a))
		}
	}
	if len(hosts) == 0 || (ip != "" && net.ParseIP(ip) == nil) {
		cli.Fprint(os.Stderr, red, probeUsage, color)
		return 1
	}
	if ip != "" {
		ip = net.ParseIP(ip).String()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	checker := probe.Public(ctx)
	var reports []probe.Report
	ok := true
	for _, h := range hosts {
		r := probe.Host(ctx, checker, ip, h)
		ok = ok && r.OK()
		reports = append(reports, r)
	}
	fmt.Print(probe.Format(reports))
	if !ok {
		return 1
	}
	return 0
}

// runPrepareRelease implements the hidden --prepare-release, run by the
// release script on the server (cli.PrepareRelease).
func runPrepareRelease(cwd string, color bool) int {
	said, err := cli.PrepareRelease(cwd, os.Environ())
	for _, s := range said {
		fmt.Fprintln(os.Stderr, s)
	}
	if err != nil {
		cli.Fprint(os.Stderr, red, "devopsy: "+err.Error(), color)
		return 1
	}
	return 0
}
