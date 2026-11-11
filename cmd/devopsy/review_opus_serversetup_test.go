package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Bug (review round 2): the install's check races the load its own timer starts. The install
// loads by the unit, then starts the timer, which on a server up longer than its OnBootSec
// (any server past its first five minutes) runs the unit again at once, as the history says;
// that load clears the mark at its start, and the check after the apply reads the mark while
// it runs: --server-setup says "what was applied did not hold: the metadata firewall is still
// to load again" for a load that went fine, whenever the second load outlasts the install's
// read of the chains (a door's load, a busy xtables lock). The fake systemctl starts the
// service in the background at the timer's start, as systemd does for a timer whose
// OnBootSec has passed, and that run's restore takes a second, a slow load (the one wait
// here models a load's length, not an event to poll for).
func TestReviewServerSetupCheckBesideTheTimersFirstRun(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	bin := filepath.Join(filepath.Dir(host), "bin")
	moveAside(t, bin, "systemctl")
	write(t, filepath.Join(bin, "systemctl"), `#!/bin/sh
h=$DEVOPSY_INTERNAL_HOST_ROOT
if [ "$*" = "start devopsy-firewall.timer" ] && grep -q '^OnBootSec=' "$h/etc/systemd/system/devopsy-firewall.timer"; then
  ( FAKE_TIMER_RUN=1 "$(dirname "$0")/orig/systemctl" start devopsy-firewall.service; : >"$h/timer-run-done" ) </dev/null >/dev/null 2>&1 &
fi
exec "$(dirname "$0")/orig/systemctl" "$@"
`, 0o755)
	for _, c := range []string{"iptables-restore", "ip6tables-restore"} {
		moveAside(t, bin, c)
		write(t, filepath.Join(bin, c), `#!/bin/sh
[ -z "${FAKE_TIMER_RUN:-}" ] || sleep 1
exec "$(dirname "$0")/orig/$(basename "$0")" "$@"
`, 0o755)
	}
	t.Cleanup(func() {
		// The timer's run ends before the test's directories go.
		for start := time.Now(); time.Since(start) < startWait; time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(host, "timer-run-done")); err == nil || !fileExists(filepath.Join(host, "etc", "systemd", "system", "devopsy-firewall.timer")) {
				return
			}
		}
	})
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if code != 0 || strings.Contains(out, "did not hold") {
		t.Errorf("a load that went through, checked while the timer's first run loads again (%d):\n%s", code, out)
	}
}
