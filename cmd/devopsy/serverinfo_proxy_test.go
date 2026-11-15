package main

import (
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// The warning for one proxy without the other reads the proxy the server's own daemon's
// containers get, config.json's proxies.default: an entry for another daemon (a tcp:// one the
// deploy user builds on) gives this server's containers nothing.
//
// Bug: any entry counts, so a server with no proxy whose deploy user names one for another
// daemon is warned that its pulls go direct, and a proxied daemon whose containers get none
// passes for set up.
func TestServerInfoProxyWarningReadsThisDaemonsEntry(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	other := []remote.ClientProxy{{For: "tcp://build.example.com:2376", File: "/home/devopsy/.docker/config.json", Proxy: "http://proxy.example.com:3128", NoProxy: "localhost,127.0.0.1,::1"}}
	warned := func(daemon, words string) bool {
		info := remote.ServerInfo{
			Version: "0.53.0", Root: "/home/devopsy", Keep: 5, MaxKeep: 5, SnapshotMaxKeep: 5,
			OS: "Debian GNU/Linux 13 (trixie)", Kernel: "6.12.48+deb13-amd64", Arch: "amd64", CPUs: 2,
			MemTotal: 4 << 30, MemAvailable: 2 << 30, Load: "0.10 0.20 0.30", Uptime: 3 * 86400, NTP: "yes",
			AppArmor: true, AppArmorParser: true,
			Docker: remote.DockerFacts{Version: "29.9.1", Compose: "5.5.0", Buildx: "0.38.2", Containerd: true, Driver: "overlayfs", Cgroup: "v2",
				RootDir: "/var/lib/docker", Security: []string{"apparmor", "seccomp"}, LogDriver: "json-file",
				LogOpts: map[string]string{"max-size": "20m", "max-file": "5"}, LogOptsKnown: true,
				Proxy: daemon, ClientProxies: other},
		}
		_, ws := serverInfoSays(info, "vm1", "vm1:", "0.53.0", now)
		for _, w := range ws {
			if !w.note && strings.Contains(w.msg, words) {
				return true
			}
		}
		return false
	}
	if warned("", "Docker's daemon none") {
		t.Error("a proxy for another daemon alone, and no proxy on this server: warned that its daemon has none")
	}
	if !warned("http://proxy.example.com:3128", "containers get none") {
		t.Error("a proxied daemon whose containers get a proxy only on another daemon: no warning that they get none")
	}
}
