package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// Review 4 (Fable), a failing test proving a bug: a project user's key removal whose end did
// not run to its end (EndProjectUserFlag failing: Docker restarting, the util image not
// building, a builder that does not go; or the session cut between the key's mv and the end)
// leaves the key gone and what it left running alive, with nothing on the server saying so
// and no way to ask again: the same removal finds no such key (exit 3, and the operator's
// devopsy stops before the script, the key no longer listed), so the end never runs unless
// someone logs in and runs the hidden flag by hand. With another key left the door still
// answers the user, the processes of the removed key included. Principles ask that what a
// cut-off session leaves is marked and settled by whatever takes the lock next; this asserts
// the plainest form, a removal asked again ending the user, and a mark the next edit settles
// would do as well (then this test changes with it).
func TestReview4KeysRemoveEndIsAskedAgain(t *testing.T) {
	t.Parallel()
	home, bin := t.TempDir(), t.TempDir()
	calls, fails := filepath.Join(bin, "calls"), filepath.Join(bin, "fails")
	for name, body := range map[string]string{
		"flock":   "exit 0",
		"id":      `case "$1" in -nG) echo devopsy ;; -u) [ "$2" = devopsy-shop ] && echo 10002 || /usr/bin/id -u ;; *) exit 1 ;; esac`,
		"devopsy": `echo "devopsy $*" >>` + calls + `; [ ! -e ` + fails + ` ]`,
	} {
		if err := testfile.Write(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	keys := filepath.Join(home, "door", "keys", "devopsy-shop")
	remove := func(blob string) (int, string) {
		t.Helper()
		os.Remove(calls)
		cmd := exec.Command("sh", "-c", KeysRemoveScript("devopsy-shop"))
		cmd.Env = []string{"HOME=" + home, "DEVOPSY_HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin"}
		cmd.Stdin = strings.NewReader(blob + "\n")
		out, _ := cmd.CombinedOutput()
		got, _ := os.ReadFile(calls)
		return cmd.ProcessState.ExitCode(), string(out) + string(got)
	}
	ended := func(out string) bool { return strings.Contains(out, "devopsy "+EndProjectUserFlag+" devopsy-shop\n") }
	write(t, keys, "ssh-ed25519 AAAAleaked ci\nssh-ed25519 AAAAkept laptop\n")
	write(t, fails, "")
	// The control: the end is asked, fails, and the key is gone with the other kept.
	if code, out := remove("AAAAleaked"); code == 0 || !ended(out) || strings.Contains(readFile(keys), "AAAAleaked") || !strings.Contains(readFile(keys), "AAAAkept") {
		t.Fatalf("the control: an end that failed (%d):\n%s\n%s", code, out, readFile(keys))
	}
	// What failed is over (Docker is back): the operator asks again.
	os.Remove(fails)
	if code, out := remove("AAAAleaked"); !ended(out) {
		t.Errorf("the removal asked again (%d) ends nothing: what the removed key left running stays, behind a kept key's door:\n%s", code, out)
	}
}
