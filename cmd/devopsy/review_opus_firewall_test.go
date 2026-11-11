package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// A release says W0001 when devopsy's firewall is installed and its last load failed (a boot
// whose loader failed once, a start of Docker whose drop-in's load failed): with no timer,
// nothing loads it again, and docs/codes.md names a failed load among W0001's causes.
// --server-info says W0001 there (the control).
//
// Bug: a load that fails removes the mark (FirewallLoader's EXIT trap), and the release reads
// the chains only where the mark is (releaseFirewall), so the deploy user's release says
// nothing and the door answers a project user's "unknown": the release is silent in the one
// state where the firewall is surely not in.
func TestReviewOpusServerReadyWarnsAfterAFailedLoad(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		t.Fatal(err)
	}
	firewallLoaded(t, host, false)
	// The load that failed: no mark, the jump not in.
	for _, p := range []string{remote.FirewallMark, "fw.iptables.jumps"} {
		if err := os.Remove(filepath.Join(host, p)); err != nil {
			t.Fatal(err)
		}
	}
	if out, _ := s.run(nil, "@devopsy@one:", "--server-info"); !strings.Contains(out, "W0001") {
		t.Fatalf("the control: --server-info says no W0001 of a failed load:\n%s", out)
	}
	out, code := s.run(nil, remote.ServerReadyFlag, "one:")
	if code != 0 || !strings.Contains(out, "devopsy: warning: W0001: the server's firewall is not loaded as devopsy loads it") {
		t.Errorf("a release beside a firewall whose load failed (%d):\n%q", code, out)
	}
}
