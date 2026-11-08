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
		"--authorized-keys",
		"sshd -T -C",
		"for f in /etc/cron.deny /etc/at.deny; do",
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

// add refuses a host whose sshd would not take its drop-in for the project user: a sshd_config
// with no Include of sshd_config.d, or with it below a Match, parses (sshd -t) and leaves the
// user on its own ~/.ssh/authorized_keys, where a leaked key adds a second one that outlives
// its rotation and remove. What sshd resolves for that user says (sshd -T).
func TestDoorSSHDTakes(t *testing.T) {
	t.Parallel()
	taken := "authorizedkeyscommand " + DoorKeysCommand + " %u\nauthorizedkeyscommanduser deploy\nauthorizedkeysfile none\n"
	for name, c := range map[string]struct {
		effective string
		ok        bool
	}{
		"taken":            {taken, true},
		"no include":       {"authorizedkeyscommand none\nauthorizedkeyscommanduser none\nauthorizedkeysfile .ssh/authorized_keys .ssh/authorized_keys2\n", false},
		"another's user":   {strings.Replace(taken, "user deploy", "user nobody", 1), false},
		"its own keys too": {strings.Replace(taken, "authorizedkeysfile none", "authorizedkeysfile none .ssh/authorized_keys", 1), false},
	} {
		script := "sshd() { [ \"$1 $2\" = \"-T -C\" ] && [ \"$3\" = user=devopsy-shop,host=devopsy,addr=127.0.0.1 ] && printf '" + strings.ReplaceAll(c.effective, "%", "%%") + "'; }\nuser=devopsy-shop\n" + doorSSHDTakes("deploy")
		out, err := exec.Command("sh", "-c", script).CombinedOutput()
		if (err == nil) != c.ok {
			t.Errorf("%s: %v %s", name, err, out)
		}
	}
}

// devopsy's path goes into sudoers, which has no quoting, so add takes only a plain one, and
// into scripts quoted.
func TestDoorDevopsyPath(t *testing.T) {
	t.Parallel()
	for p, ok := range map[string]bool{"/home/devopsy/.local/bin/devopsy": true, "/opt/dev opsy/devopsy": false, "/opt/a,b/devopsy": false, "devopsy": false, "/opt/x=y/devopsy": false} {
		if (DoorDevopsyPath(p) == nil) != ok {
			t.Errorf("%q: %v", p, !ok)
		}
	}
	if s := doorKeysCommand("/opt/bin/devopsy"); !strings.Contains(s, "exec '/opt/bin/devopsy' --authorized-keys") {
		t.Errorf("the keys command:\n%s", s)
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
