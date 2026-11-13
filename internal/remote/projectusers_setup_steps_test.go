package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/setup"
	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// Bug (review round 2): the root script runs under set -e, so a command of the processes' step
// that fails before the mount (usermod on a deploy user the local group file cannot take, as
// with one from LDAP; groupadd; put's exit 5 on a full disk) ends the whole run at once: the
// space is never mounted and the drop-in never put, though each step should go by its own result
// and the processes' failure fail the processes alone, the run then exiting 6. The space not
// mounted, the Go side keeps door: space: out of the config too. usermod exits 6 for a user it
// does not find, the run's own code for "each failure said by the run" (applyDoorSetup), so the
// operator sees usermod's raw line and no devopsy error for it; another command's exit (1, or
// put's 5) reads as "devopsy could not run its setup as root".
func TestProjectUsersSetupScriptProcessesFailureLeavesTheSpace(t *testing.T) {
	t.Parallel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	me := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	host, bin := filepath.Join(dir, "host"), filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"systemctl": "#!/bin/sh\nexit 0\n",
		"groupadd":  "#!/bin/sh\nmkdir -p \"$HOST/etc\"\necho \"$2:x:991:\" >>\"$HOST/etc/group\"\n",
		// The deploy user is not in the local passwd file: usermod refuses it.
		"usermod": "#!/bin/sh\necho \"usermod: user '$3' does not exist\" >&2\nexit 6\n",
		"getent":  "#!/bin/sh\nawk -F: -v k=\"$2\" '$1 == k || $3 == k { print; f = 1 } END { exit !f }' \"$HOST/etc/group\" 2>/dev/null\n",
	} {
		if err := testfile.Write(filepath.Join(bin, name), []byte(text), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	space := filepath.Join(dir, "door")
	unit := setup.SpaceUnit(space)
	dropin, err := setup.MountsDropInText(filepath.Join(dir, "srv"), space)
	if err != nil {
		t.Fatal(err)
	}
	bare, err := setup.MountsDropInText(filepath.Join(dir, "srv"))
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command("sh", "-c", ProjectUsersSetupScript, "sh", host, me, space, "1", "", unit, setup.SpaceUnitText(space), "1", dropin, "1", "deploy",
		setup.ProcUnitText(setup.GIDMark), "", QuoteWord(filepath.Base(unit)), bare)
	c.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "HOST="+host)
	out, _ := c.CombinedOutput()
	code := c.ProcessState.ExitCode()
	got, _ := os.ReadFile(filepath.Join(host, setup.MountsDropIn))
	if code != 6 || !strings.Contains(string(out), "mounted "+space+"\n") || string(got) != dropin || !strings.Contains(string(out), "devopsy: error: ") {
		t.Errorf("a processes' step that fails before the mount (%d):\n%s\nthe drop-in:\n%s", code, out, got)
	}
}
