package remote

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// Bug (deploy-user review round 1): --upgrade's script runs no prelude, so a login the
// server's record does not name never meets --release-settings' check: it replaces the devopsy
// its PATH finds (its own copy, or fails on the deploy user's) and, where it has a setup record
// of its own from before, applies the setup as itself (apply --upgrade), which on a server not
// recorded yet records it as the deploy user, refusing the real one from then on. The session
// here is such a login: the server's devopsy refuses --release-settings as it does for one.
func TestUpgradeScriptRefusesAnotherLogin(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "calls")
	body := `echo "$*" >> ` + log + `
case "$1" in --release-settings) echo "devopsy: error: W0009: this login is not the server's deploy user" >&2; exit ` + strconv.Itoa(SettingsRefusedExit) + ` ;; esac`
	if err := testfile.Write(filepath.Join(bin, "devopsy"), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", UpgradeScript(nil))
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=" + bin + ":/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err == nil || strings.Contains(readFile(log), "--upgrade") {
		t.Errorf("a login the record does not name upgraded (%v), calls %q:\n%s", err, readFile(log), out)
	}
}
