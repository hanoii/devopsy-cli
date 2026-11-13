package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// atTerminalRun runs devopsy with args on s at a terminal, util-linux's script giving it a
// pseudo-terminal fed answers, one line each, with its output and exit; the test is skipped
// where there is no such script (macOS's takes other flags).
func atTerminalRun(t *testing.T, s *syncSetup, answers string, args ...string) (string, int) {
	t.Helper()
	path, err := exec.LookPath("script")
	if err != nil || runtime.GOOS != "linux" {
		t.Skip("needs util-linux's script")
	}
	words := []string{remote.Quote(binary)}
	for _, a := range args {
		words = append(words, remote.Quote(a))
	}
	cmd := exec.Command(path, "-qec", strings.Join(words, " "), "/dev/null")
	cmd.Dir = s.project
	cmd.Env = append(append([]string{}, s.env...), "NO_COLOR=1")
	cmd.Stdin = strings.NewReader(answers)
	out, _ := cmd.CombinedOutput()
	return strings.ReplaceAll(string(out), "\r\n", "\n"), cmd.ProcessState.ExitCode()
}

// At a terminal, --server-setup asks where releases live before it shows anything, the home
// first and Enter's: a volume chosen by its number (an answer naming none asked again) is
// judged by a second facts session, facts --root, and named by the apply, with no volume hint,
// the question answered; Enter keeps the home and names nothing, with no hint either.
func TestServerSetupAsksWhereReleasesLiveAtATerminal(t *testing.T) {
	t.Parallel()
	s, host, vol, config := rootChoice(t)
	before := readString(config)
	home := filepath.Dir(vol)
	question := "devopsy: where do releases live?\n"
	choices := "devopsy: | 1. " + home + ", the deploy user's home\ndevopsy: | 2. " + vol + " ("

	// Enter: the home, nothing named, the rest applied as confirmed.
	out, code := atTerminalRun(t, s, "\ny\nn\n", "@devopsy@one:", "--server-setup")
	if !strings.Contains(out, question) || !strings.Contains(out, choices) || readString(config) != before ||
		strings.Contains(out, "to keep releases on") || strings.Contains(out, "named ") || strings.Count(out, "where do releases live?") != 1 {
		t.Errorf("Enter (%d), the config %q:\n%s", code, readString(config), out)
	}

	// 3 names no choice and is asked again; 2 is the volume, named by the apply.
	out, code = atTerminalRun(t, s, "3\n2\ny\nn\n", "@devopsy@one:", "--server-setup")
	if code != 0 || !strings.Contains(out, "answer with a number from 1 to 2") || !strings.Contains(out, "--server-setup names it as releases: root in "+config) ||
		!strings.Contains(out, "devopsy: named "+vol+" the release root in "+config) || strings.Contains(out, "to keep releases on") {
		t.Fatalf("a volume chosen (%d):\n%s", code, out)
	}
	if got := readString(config); got != before+"releases:\n  root: "+vol+"\n" {
		t.Errorf("the config:\n%s", got)
	}
	if !strings.Contains(readString(filepath.Join(host, setup.MountsDropIn)), vol) {
		t.Errorf("the mounts drop-in names no %s", vol)
	}
	// Named: nothing asked again.
	if out, _ := atTerminalRun(t, s, "\n\n\n", "@devopsy@one:", "--server-setup"); strings.Contains(out, "where do releases live?") {
		t.Errorf("asked again once named:\n%s", out)
	}
}

// A root on a filesystem in memory (tmpfs, as Debian 13's /tmp, which tmp.mount mounts at every
// boot from a unit file of its own) is refused, whatever mounts it: after a reboot the
// filesystem is empty again, so every release on it would be gone.
func TestServerSetupRefusesARootInMemory(t *testing.T) {
	t.Parallel()
	s, host, vol, _ := rootChoice(t)
	tmp := filepath.Join(filepath.Dir(vol), "tmp")
	write(t, filepath.Join(host, "proc", "self", "mountinfo"), "1 0 8:1 / / rw - ext4 /dev/sda1 rw\n2 1 8:16 / "+vol+" rw,relatime - xfs /dev/sdb rw\n3 1 0:30 / "+tmp+" rw,nosuid,nodev - tmpfs tmpfs rw\n", 0o644)
	unit := setup.MountUnitName(tmp)
	write(t, filepath.Join(host, "fragment."+unit), "/usr/lib/systemd/system/tmp.mount\n", 0o644)
	if f, out, code := setupFactsOf(s, "--root", filepath.Join(tmp, "releases")); code != 1 || !strings.Contains(out, "devopsy: error: "+tmp+" is a tmpfs, in memory: it keeps nothing across a reboot") {
		t.Errorf("facts --root in a tmpfs (%d), names %v:\n%s", code, f.NamesRoot, out)
	}
}
