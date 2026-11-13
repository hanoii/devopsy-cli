package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Review round 1 (Fable). Bug: --project-users add's host step makes the user's group
// (groupadd -g) and then the user (useradd -g) as two commands, the group whether or not it
// is there. A step cut or failed between the two, or a group the user's removal left (an
// admin's userdel on a host that keeps a user's group), leaves a group of the user's name
// with no user: every add after it fails at groupadd, which refuses a name that is there,
// and takes one more id of the range each time (takeProjectID). The add must go on from a
// group that is there, as it does from a user that is.
func TestReview1AddGoesOnFromAGroupLeftWithNoUser(t *testing.T) {
	t.Parallel()
	script := DoorHostScript("deploy", "/opt/bin/devopsy")
	from, to := strings.Index(script, "made=\n"), strings.Index(script, "  made=1\nfi\n")
	if from < 0 || to < from {
		t.Fatal("the script's making of the user was not found")
	}
	block := script[from : to+len("  made=1\nfi\n")]
	bin, state := t.TempDir(), t.TempDir()
	for name, body := range map[string]string{
		// As the real ones: id fails for a user not there, groupadd refuses a name there (9).
		"id":       `[ "$1" = -u ] && [ -e "` + state + `/user-$2" ]`,
		"groupadd": `for a; do g=$a; done; [ ! -e "` + state + `/group-$g" ] || { echo "groupadd: group '$g' already exists" >&2; exit 9; }; : >"` + state + `/group-$g"`,
		"useradd":  `for a; do u=$a; done; : >"` + state + `/user-$u"`,
		"usermod":  `:`,
	} {
		write(t, filepath.Join(bin, name), "#!/bin/sh\n"+body+"\n")
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The group an add cut after groupadd left.
	write(t, filepath.Join(state, "group-devopsy-shop"), "")
	c := exec.Command("sh", "-c", "set -eu\nuser=$1 id=$2 group="+DoorGroup+"\n"+block+"echo \"made=$made\"\n", "sh", "devopsy-shop", "50002")
	c.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	out, _ := c.CombinedOutput()
	if code := c.ProcessState.ExitCode(); code != 0 || !strings.Contains(string(out), "made=1") || !fileThere(filepath.Join(state, "user-devopsy-shop")) {
		t.Errorf("an add over a group left with no user (%d), its user made %v:\n%s", code, fileThere(filepath.Join(state, "user-devopsy-shop")), out)
	}
}
