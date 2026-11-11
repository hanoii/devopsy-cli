package main

import (
	"slices"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Each root run on a host gets what it declares and nothing more: --project-users add's host
// step writes the host's files with no network, the firewall's read and load have the host's
// network, processes, cgroups, NET_ADMIN and SYS_PTRACE, its read only a read-only root, and
// the setup's root run the host's network, NET_ADMIN and pids; none pulls its image.
func TestHostRunArgs(t *testing.T) {
	t.Parallel()
	script := remote.DoorHostScript("deploy", "/opt/bin/devopsy")
	for _, c := range []struct {
		name string
		run  hostRun
		want []string
	}{
		{"add", addUserRun("deploy", "/opt/bin/devopsy", "devopsy-shop", "/srv/shop", "/srv"),
			[]string{"run", "--rm", "--network", "none", "-v", "/:/host", "--pull", "never", remote.UtilImage,
				"chroot", "/host", "/bin/sh", "-c", script, "sh", "devopsy-shop", "/srv/shop", "/srv"}},
		{"add in the home", addUserRun("deploy", "/opt/bin/devopsy", "devopsy-shop", "", ""),
			[]string{"run", "--rm", "--network", "none", "-v", "/:/host", "--pull", "never", remote.UtilImage,
				"chroot", "/host", "/bin/sh", "-c", script, "sh", "devopsy-shop", "", ""}},
		{"firewall read", firewallRun("read it", false),
			[]string{"run", "--rm", "--network", "host", "--pid", "host", "--cgroupns", "host", "--cap-add", "NET_ADMIN", "--cap-add", "SYS_PTRACE", "-v", "/:/host:ro", "--pull", "never", remote.UtilImage,
				"chroot", "/host", "/bin/sh", "-c", "read it"}},
		// The chains' read every check makes, the door's for a project user too: devopsy's
		// script, nothing of a caller's.
		{"firewall chains", firewallReadRun("/usr/bin/docker", remote.FirewallChainsRead),
			[]string{"run", "--rm", "--network", "host", "--pid", "host", "--cgroupns", "host", "--cap-add", "NET_ADMIN", "--cap-add", "SYS_PTRACE", "-v", "/:/host:ro", "--pull", "never", remote.UtilImage,
				"chroot", "/host", "/bin/sh", "-c", remote.FirewallChainsRead}},
		{"firewall load", firewallRun("load it", true),
			[]string{"run", "--rm", "--network", "host", "--pid", "host", "--cgroupns", "host", "--cap-add", "NET_ADMIN", "--cap-add", "SYS_PTRACE", "-v", "/:/host", "--pull", "never", remote.UtilImage,
				"chroot", "/host", "/bin/sh", "-c", "load it"}},
		// The setup's root run: the host's network and NET_ADMIN for sysctl, its pids for
		// systemctl, which refuses a peer it cannot see.
		{"setup", setupRun("", "1000:1000"),
			[]string{"run", "--rm", "--network", "host", "--pid", "host", "--cap-add", "NET_ADMIN", "-v", "/:/host", "--pull", "never", remote.UtilImage,
				"chroot", "/host", "/bin/sh", "-c", remote.SetupRootScript, "sh", "", "1000:1000"}},
	} {
		if got := c.run.dockerArgs(); !slices.Equal(got, c.want) {
			t.Errorf("%s:\n%q\nwant\n%q", c.name, got, c.want)
		}
	}
}
