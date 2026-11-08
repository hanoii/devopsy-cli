package remote

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// What --project-users add runs as root on the host is POSIX sh, and what it writes names the
// deploy user and the devopsy it found: the door's two sudo rules, the script sshd runs, and
// sshd's block for the project users' group alone.
func TestDoorHostScript(t *testing.T) {
	t.Parallel()
	s := DoorHostScript("deploy", "/opt/bin/devopsy")
	if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
	for _, want := range []string{
		"%devopsy-projects ALL=(deploy) NOPASSWD: /opt/bin/devopsy --door *",
		"deploy ALL=(%devopsy-projects) NOPASSWD: /opt/bin/devopsy --door-read *",
		"exec /opt/bin/devopsy --authorized-keys",
		"Match Group devopsy-projects",
		"AuthorizedKeysFile none",
		"AuthorizedKeysCommandUser deploy",
		"visudo -cqf",
		"sshd -t",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("no %q", want)
		}
	}
}

// A project user's tree is made by add, or is that user's already: a directory there that is
// another's (the deploy user's releases of the project, say) would hand that user what runs
// from it, whose entries it could then rename. A link is never followed.
func TestDoorTreeCheck(t *testing.T) {
	t.Parallel()
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mine := filepath.Join(dir, "mine")
	if err := os.Mkdir(mine, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(mine, link); err != nil {
		t.Fatal(err)
	}
	other := "root"
	if me.Username == "root" {
		t.Skip("as root, every tree is root's")
	}
	for _, c := range []struct {
		user, tree string
		ok         bool
	}{
		{me.Username, "", true},
		{me.Username, filepath.Join(dir, "missing"), true},
		{me.Username, mine, true},
		{other, mine, false},
		{me.Username, link, false},
		{"devopsy-nosuchuser", mine, false},
	} {
		out, err := exec.Command("sh", "-c", doorTreeCheck, "sh", c.user, c.tree).CombinedOutput()
		if (err == nil) != c.ok {
			t.Errorf("%s %s: %v %s", c.user, c.tree, err, out)
		}
	}
}
