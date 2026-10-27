package main

import (
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Review 2 (fable): the no-reflinks warning's fix is a setting the
// server refuses. Round 1 moved the fix to the project's
// snapshots: {mirror: true}, which is where a mirror is asked for; but
// the server's own snapshots: {mirror: false} (ServerSnapshots.MirrorsOff,
// takeMirror) wins over every project's, and a take asking goes on
// without one, warned. --server-info knows that setting (info.MirrorsOff:
// its snapshots line says "no mirrors"), yet on such a server its fix
// still tells the operator to set mirror: true in the projects, which
// changes nothing there: the services stay stopped for the whole copy
// of every snapshot and release snapshot. The fix must name the
// server's mirror: false as what to lift first (or offer XFS alone).
func TestReview2NoReflinksMirrorsOff(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	info := remote.ServerInfo{
		Version: "0.53.0", Protocol: 8, Root: "/home/devopsy", Keep: 5, MaxKeep: 5, SnapshotMaxKeep: 5,
		MirrorsOff: true,
		Arch:       "amd64", CPUs: 2, NTP: "yes",
		Docker: remote.DockerFacts{Version: "29.9.1", Compose: "5.5.0", Containerd: true, Driver: "overlayfs",
			RootDir: "/var/lib/docker", Security: []string{"apparmor", "seccomp"}, LogDriver: "local", LogOptsKnown: true},
		Disks: []remote.Disk{{Paths: []string{"releases=/home/devopsy", "docker=/var/lib/docker", "/=/"}, Type: "ext4", Size: 80e9, Free: 40e9, Used: 40e9, Reflinks: "no"}},
	}
	lines, warnings := serverInfoReport(info, "vm1", "vm1:", "0.53.0", now)
	if !strings.Contains(strings.Join(lines, "\n"), "no mirrors") {
		t.Fatalf("the snapshots line does not say the server keeps no mirrors:\n%s", strings.Join(lines, "\n"))
	}
	var fix string
	for _, w := range warnings {
		if strings.Contains(w.msg, "makes no reflinks") {
			fix = fixText(w.fixes)
		}
	}
	if fix == "" {
		t.Fatalf("no reflinks warning: %+v", warnings)
	}
	if !strings.Contains(fix, "mirror: false") {
		t.Errorf("the server's snapshots: {mirror: false} refuses the mirror the fix asks for, and the fix does not say so:\n%s", fix)
	}
}
