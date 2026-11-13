package remote

import (
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/setup"
	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// A command of the processes' step that fails before the space's mount (usermod on a deploy
// user the local group file cannot take, groupadd, a put that cannot write) fails the processes
// alone: the space is mounted and named in the drop-in all the same, the failure is said in
// devopsy's words, and the run exits DoorSetupSaid, which no tool returns through it.
func TestProjectUsersSetupScriptProcessesFailureLeavesTheSpace(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		fake map[string]string
		// readOnly is a directory under the host root made unwritable: a put there fails.
		readOnly, want string
	}{
		// usermod exits 6 for a user it does not find.
		"usermod": {fake: map[string]string{"usermod": "#!/bin/sh\necho \"usermod: user '$3' does not exist\" >&2\nexit 6\n"},
			want: "devopsy: error: devopsy could not put deploy in devopsy-proc\n"},
		"groupadd": {fake: map[string]string{"groupadd": "#!/bin/sh\nexit 9\n"},
			want: "devopsy: error: devopsy could not make the group devopsy-proc\n"},
		"a put": {readOnly: path.Dir(setup.PolkitDropIn), want: setup.PolkitDropIn + " could not be written\n"},
	} {
		t.Run(name, func(t *testing.T) {
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
			fakes := map[string]string{
				"systemctl": "#!/bin/sh\nexit 0\n",
				"groupadd":  "#!/bin/sh\nmkdir -p \"$HOST/etc\"\necho \"$2:x:991:\" >>\"$HOST/etc/group\"\n",
				"usermod":   "#!/bin/sh\nexit 0\n",
				"getent":    "#!/bin/sh\nawk -F: -v k=\"$2\" '$1 == k || $3 == k { print; f = 1 } END { exit !f }' \"$HOST/etc/group\" 2>/dev/null\n",
			}
			maps.Copy(fakes, c.fake)
			for name, text := range fakes {
				if err := testfile.Write(filepath.Join(bin, name), []byte(text), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if c.readOnly != "" {
				ro := filepath.Join(host, c.readOnly)
				if err := os.MkdirAll(ro, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(ro, 0o555); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
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
			cmd := exec.Command("sh", "-c", ProjectUsersSetupScript, "sh", host, me, space, "1", "", unit, setup.SpaceUnitText(space), "1", dropin, "1", "deploy",
				setup.ProcUnitText(setup.GIDMark), setup.PolkitDropInText(), QuoteWord(filepath.Base(unit)), bare)
			cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "HOST="+host)
			out, _ := cmd.CombinedOutput()
			code := cmd.ProcessState.ExitCode()
			got, _ := os.ReadFile(filepath.Join(host, setup.MountsDropIn))
			if code != DoorSetupSaid || !strings.Contains(string(out), "mounted "+space+"\n") || string(got) != dropin || !strings.Contains(string(out), c.want) || strings.Contains(string(out), "hidden\n") {
				t.Errorf("a processes' step that fails before the mount (%d):\n%s\nthe drop-in:\n%s", code, out, got)
			}
		})
	}
}
