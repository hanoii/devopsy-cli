package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// A firewall whose unit failed at its last run (a load that lost the xtables lock to Docker,
// say) is loaded again by --server-setup, and the apply then holds: the check after it reads
// the firewall in sync.
//
// Bug: the apply runs the loader itself, never through its unit, so the unit's result stays
// the failed one until the timer's next run, up to five minutes later, and the check after
// the apply, which reads that result, says "what was applied did not hold" of a firewall the
// apply just loaded and read back as root. The fake systemctl here keeps the unit failed until
// something starts the unit or resets its failure, as systemd does.
func TestServerSetupFirewallHoldsAfterItsUnitFailed(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes"); code != 0 {
		t.Fatalf("the first load (%d):\n%s", code, out)
	}
	systemctl := filepath.Join(filepath.Dir(s.root), "bin", "systemctl")
	if err := os.Rename(systemctl, systemctl+".real"); err != nil {
		t.Fatal(err)
	}
	write(t, systemctl, `#!/bin/sh
h=$DEVOPSY_INTERNAL_HOST_ROOT
case "$*" in
  "show -p Result --value devopsy-firewall.service") cat "$h/firewall-result"; exit 0 ;;
  *"start devopsy-firewall.service"*|*"reset-failed"*) echo success >"$h/firewall-result" ;;
esac
exec "$0.real" "$@"
`, 0o755)
	write(t, filepath.Join(host, "firewall-result"), "exit-code\n", 0o644)
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if code != 0 || !strings.Contains(out, "loaded devopsy's firewall") || strings.Contains(out, "did not hold") {
		t.Errorf("loaded again after its unit failed (%d):\n%s", code, out)
	}
}

// A server whose loader is the door's keeps it through an --upgrade once door: is gone from
// its config: what replaces the project users' firewall with the containers' chain alone is
// shown and confirmed, never done with no question.
//
// Bug: whether a door is there is read from the config alone, and a loader that differs from
// the want is "to load again", which --upgrade applies as any drift of devopsy's own. So a
// door: taken out of the config (its users: with it, their accounts still on the host) has the
// next --upgrade overwrite the door's loader with the containers' one, asking nothing: the
// project users' and the daemon's chains are loaded no more from the next boot on.
func TestServerSetupUpgradeKeepsADoorsLoader(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	firewallLoaded(t, host, true)
	doors := remote.FirewallFiles(true)[remote.FirewallScript]
	out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--upgrade")
	if got := readString(filepath.Join(host, remote.FirewallScript)); got != doors {
		t.Errorf("--upgrade replaced the door's loader with the containers' alone (%d):\n%s", code, out)
	}
}
