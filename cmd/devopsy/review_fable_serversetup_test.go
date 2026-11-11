package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// An apply that loaded the firewall holds, whatever the timer it just started does: on a
// host up longer than the timer's OnBootSec, the timer's first start runs the unit at once,
// beside the rest of the install and the apply's check, and that second load is no failure
// of the first.
//
// Bug: every load removes the mark at its start, so the check after the apply, made while
// the timer's load is under way, reads a firewall "to load again" and the apply fails with
// "what was applied did not hold", the rules in all along; --server-info and --upgrade read
// the same in that window.
//
// The fake systemctl runs the loader when the timer starts, as systemd does there, and the
// install goes on once that load began; the fake iptables-restore holds that load until the
// apply ended, where a real one takes as long as its iptables calls.
func TestServerSetupHoldsBesideItsTimersFirstLoad(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	bin := filepath.Join(filepath.Dir(host), "bin")
	moveAside(t, bin, "systemctl")
	write(t, filepath.Join(bin, "systemctl"), `#!/bin/sh
h=$DEVOPSY_INTERNAL_HOST_ROOT
if [ "$*" = "start devopsy-firewall.timer" ]; then
  ( FAKE_TIMERS_LOAD=1 "$h`+remote.FirewallScript+`" >"$h/timers-load.out" 2>&1 </dev/null; : >"$h/timers-load.done" ) >/dev/null 2>&1 </dev/null &
  n=0
  while [ ! -e "$h/timers-load.began" ] && [ "$n" -lt 3000 ]; do sleep 0.02; n=$((n + 1)); done
fi
exec "$(dirname "$0")/orig/systemctl" "$@"
`, 0o755)
	moveAside(t, bin, "iptables-restore")
	write(t, filepath.Join(bin, "iptables-restore"), `#!/bin/sh
h=$DEVOPSY_INTERNAL_HOST_ROOT
if [ -n "${FAKE_TIMERS_LOAD:-}" ]; then
  : >"$h/timers-load.began"
  n=0
  while [ ! -e "$h/timers-load.go" ] && [ "$n" -lt 3000 ]; do sleep 0.02; n=$((n + 1)); done
fi
exec "$(dirname "$0")/orig/iptables-restore" "$@"
`, 0o755)
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	began := fileExists(filepath.Join(host, "timers-load.began"))
	// The timer's load goes on and ends, whatever the apply said.
	write(t, filepath.Join(host, "timers-load.go"), "", 0o644)
	waitFile(t, filepath.Join(host, "timers-load.done"), "the timer's load ending", startWait, func() string { return readString(filepath.Join(host, "timers-load.out")) })
	if !began {
		t.Fatalf("the timer's start ran no load (%d):\n%s", code, out)
	}
	// The control: the apply's own load went in, and the timer's after it.
	if !strings.Contains(out, "loaded devopsy's firewall") || strings.TrimSpace(readString(filepath.Join(host, remote.FirewallMark))) != remote.FirewallContainers {
		t.Fatalf("the firewall not loaded (%d), the mark %q:\n%s\n%s", code, readString(filepath.Join(host, remote.FirewallMark)), out, readString(filepath.Join(host, "timers-load.out")))
	}
	if code != 0 || strings.Contains(out, "did not hold") {
		t.Errorf("an apply beside its timer's first load (%d):\n%s", code, out)
	}
}
