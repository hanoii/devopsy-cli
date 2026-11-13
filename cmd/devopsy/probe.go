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
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

const probeUsage = "usage: devopsy --probe [--ip <server ip>] <host>..."

// runProbe implements `devopsy --probe [--ip <server ip>] <host>...`: DNS,
// the certificate and HTTPS of each host, checked locally. Exits 1 when any host
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
line to run, like devopsy-template-traefik's domains.
`)
			return 0
		case a == "--ip" && i+1 < len(args):
			ip = args[i+1]
			i++
		case strings.HasPrefix(a, "--ip="):
			ip = strings.TrimPrefix(a, "--ip=")
		case strings.HasPrefix(a, "-"):
			report.Error(probeUsage)
			return 1
		default:
			hosts = append(hosts, strings.ToLower(a))
		}
	}
	if len(hosts) == 0 || (ip != "" && net.ParseIP(ip) == nil) {
		report.Error(probeUsage)
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
func runPrepareRelease(cwd string, args []string, color bool) int {
	reload := len(args) == 1 && args[0] == "reload"
	if len(args) > 0 && !reload {
		report.Error("usage: devopsy " + cli.PrepareReleaseFlag + " [reload]")
		return 2
	}
	cli.Progress = func(s string) { report.Info(s) }
	said, err := cli.PrepareRelease(cwd, os.Environ(), reload)
	for _, s := range said {
		report.Info(s)
	}
	if err != nil {
		return failErr(err)
	}
	return 0
}

// runReleaseSettings implements the hidden --release-settings [<project> <compose name>],
// run first by every script on a server: the machine's releases: and snapshots: settings
// and the features it has (remote.FeaturesList), as key=value lines. Given an environment's
// project and compose name, it refuses the deploy user one whose name is a project user's
// (doorNameOwner), before the script
// changes anything, with remote.SettingsRefusedExit: compose acts on a project by its name
// alone, so a --destroy there takes that user's containers and volumes down.
func runReleaseSettings(args []string) int {
	if len(args) != 0 && len(args) != 2 {
		report.Error("usage: devopsy --release-settings [<project> <compose name>]")
		return 2
	}
	r, err := remote.ReleaseSettings()
	if err != nil {
		return failErr(err)
	}
	if len(args) == 2 && !behindDoor() {
		// A door that does not read stops the script: its names are then no one's to tell.
		d, err := remote.ServerDoor()
		if err != nil {
			failErr(err)
			return remote.SettingsRefusedExit
		}
		who, err := doorNameOwner(d, r.Root, args[0], args[1])
		if err != nil {
			failErr(fmt.Errorf("cannot tell whether compose project %s is under a project user's names: %w", args[1], err))
			return remote.SettingsRefusedExit
		}
		if who != "" {
			failErr(&report.Failure{Msg: "compose project " + args[1] + " is under the names of project user " + who + " here: as the deploy user, devopsy would act on that user's containers and volumes",
				Remedies: []report.Remedy{report.Fix("address it as that user, for example:", "DEVOPSY_SERVER="+who+"@<server> devopsy @<target> --release")}})
			return remote.SettingsRefusedExit
		}
	}
	images, err := imagesDir()
	if err != nil {
		failErr(err)
		return remote.SettingsRefusedExit
	}
	fmt.Printf("root=%s\nimages=%s\nkeep=%d\nmax_keep=%d\nsnapshot_root=%s\nsnapshot_max_keep=%d\nsnapshot_mirror=%t\nfeatures=%s\n", r.Root, images, r.Keep, r.MaxKeep, r.Snapshots.Root, r.Snapshots.MaxKeep, !r.Snapshots.MirrorsOff(),
		remote.FeaturesList())
	return 0
}
