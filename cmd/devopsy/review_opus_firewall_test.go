package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// unitSystemctl wraps setupServer's systemctl with what systemd does for devopsy's firewall
// units: devopsy-firewall.service's Result stays what its last run left (fwresult in the host
// root, success when absent) until the unit runs again or is reset; a start of the service
// runs the loader, as systemd's oneshot start does, before it returns; a start of the timer
// on a host booted past its OnBootSec (every host but one just booted) fires the service at
// once, which the fake notes in timer-fired. Every other call goes to setupServer's fake.
func unitSystemctl(t *testing.T, host string) {
	t.Helper()
	bin := filepath.Join(filepath.Dir(host), "bin")
	moveAside(t, bin, "systemctl")
	write(t, filepath.Join(bin, "systemctl"), `#!/bin/sh
h=$DEVOPSY_INTERNAL_HOST_ROOT
case "$*" in
  "show -p Result --value devopsy-firewall.service") cat "$h/fwresult" 2>/dev/null || echo success; exit 0 ;;
  "reset-failed devopsy-firewall.service") rm -f "$h/fwresult"; exit 0 ;;
  "start devopsy-firewall.service") FAKE_VIA_UNIT=1 "$h`+remote.FirewallScript+`" && rm -f "$h/fwresult"; exit ;;
  "start devopsy-firewall.timer") : >"$h/timer-fired" ;;
esac
exec "$(dirname "$0")/orig/systemctl" "$@"
`, 0o755)
}

// moveAside moves bin's fake name into bin/orig, under the same name, for a wrapper to call:
// setupServer's iptables keeps its state by its own name.
func moveAside(t *testing.T, bin, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(bin, "orig"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(bin, name), filepath.Join(bin, "orig", name)); err != nil {
		t.Fatal(err)
	}
}

// Review finding: --server-setup judges the metadata firewall by its unit's Result, which stays
// failed until the unit runs again, while its apply runs the loader directly, outside the unit.
// A server whose loader failed once by its unit (a boot, a timer run) reads "to load again"; the
// apply then loads it fine, and its check after reads the same stale Result and fails with
// "what was applied did not hold": --server-setup, its own remedy, keeps failing until the
// timer's next run of the unit, up to five minutes later. The apply would run the loader
// through the unit (systemctl start devopsy-firewall.service) where systemd is reachable, or
// reset its failure once the loader ran.
func TestReviewFirewallApplyHeldByAStaleUnitResult(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	unitSystemctl(t, host)
	firewallLoaded(t, host, false)
	if err := os.Chmod(filepath.Join(host, remote.FirewallScript), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(host, "fwresult"), "exit-code\n", 0o644)
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if code != 0 || strings.Contains(out, "did not hold") {
		t.Errorf("a loader that ran fine judged by the unit's last result (%d):\n%s", code, out)
	}
}

// Review finding: the install starts devopsy-firewall.timer before it runs the loader itself.
// A timer started on a host up longer than its OnBootSec=5min elapses at once, so systemd runs
// the loader by the unit while the install runs it too: two loaders each insert the jump at the
// top of raw's PREROUTING and then delete "the other copies" by their line numbers, read before
// the other's delete, so the second delete takes another tool's rule (Docker's own raw rules
// there) or fails under set -e (no such line, or iptables-legacy's xtables lock held), failing
// the install or the unit (whose stale Result then fails the check, as the test above shows).
// A loader the install runs must come before the timer's start, or go through the unit, which
// systemd runs once at a time.
func TestReviewFirewallTimerStartedBeforeTheInstallsLoad(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	unitSystemctl(t, host)
	bin := filepath.Join(filepath.Dir(host), "bin")
	// iptables noting a loader's insert run outside the unit once the timer fired: the two
	// loaders' runs overlap there.
	for _, c := range []string{"iptables", "ip6tables"} {
		moveAside(t, bin, c)
		write(t, filepath.Join(bin, c), `#!/bin/sh
h=$DEVOPSY_INTERNAL_HOST_ROOT
case "$*" in
  "-t raw -I PREROUTING 1 "*) [ ! -e "$h/timer-fired" ] || [ -n "${FAKE_VIA_UNIT:-}" ] || echo "$(basename "$0") $*" >>"$h/overlap" ;;
esac
exec "$(dirname "$0")/orig/$(basename "$0")" "$@"
`, 0o755)
	}
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if code != 0 {
		t.Fatalf("(%d):\n%s", code, out)
	}
	if !fileExists(filepath.Join(host, "timer-fired")) {
		t.Fatalf("the install never started the timer:\n%s", readString(filepath.Join(host, "systemctl.log")))
	}
	if o := readString(filepath.Join(host, "overlap")); o != "" {
		t.Errorf("the install loaded the firewall itself after the timer's start fired the unit's loader:\n%s", o)
	}
}
