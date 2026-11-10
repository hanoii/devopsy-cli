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
		"Cmnd_Alias DEVOPSY_DOOR = /opt/bin/devopsy --door *",
		"%devopsy-projects ALL=(deploy) NOPASSWD: DEVOPSY_DOOR",
		"deploy ALL=(%devopsy-projects) NOPASSWD: /opt/bin/devopsy --door-read *",
		"--authorized-keys",
		"sshd -T -C",
		"for f in /etc/cron.deny /etc/at.deny; do",
		`rm -f "/var/lib/systemd/linger/$user"`,
		"Match Group devopsy-projects",
		"AuthorizedKeysFile none",
		"AuthorizedKeysCommandUser deploy",
		"visudo -cqf",
		"sshd -t",
		// No subordinate ids, for a user made here or one that was there.
		"useradd -K SUB_UID_COUNT=0 -K SUB_GID_COUNT=0",
		"for f in /etc/subuid /etc/subgid; do",
		// The door's entrance for a project user, never over a real docker.
		"ln -sfn '/opt/bin/devopsy' /usr/local/bin/docker",
		"/usr/local/bin/docker is a real docker",
		// sshd takes the drop-in at a reboot, which empties /run.
		"touch /run/devopsy-door-reboot /run/reboot-required",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("no %q", want)
		}
	}
	// Each check comes before what it protects: who is in the group and whose the user and
	// the tree are before anything lands, each piece's own check inside put, sshd's reading
	// of the user before its cron ban and its home, and the user's config written as that
	// user, only for one made here.
	last := -1
	for _, step := range []string{
		"sshd would ignore its own keys", "is there already, and no project user", "is there already, and not $1's",
		"is a real docker", "put /etc/sudoers.d/devopsy-projects 440 'visudo -cqf \"$1\"'", "put /usr/local/libexec/devopsy-authorized-keys 755 'sh -n \"$1\"'",
		"if put /etc/ssh/sshd_config.d/devopsy.conf 644 true", "if ! sshd -t; then", "put back as it was", "useradd ", "sshd -T -C", "/etc/cron.deny",
		`if [ -n "$made" ] && [ -n "$root" ]; then`, `runuser -u "$user" -- sh -c`,
	} {
		i := strings.Index(s, step)
		if i < last {
			t.Errorf("%q comes too early, or is not there (%d, after %d)", step, i, last)
		}
		last = max(last, i)
	}
}

// A host piece lands only when it is missing or differs, and only once it passed its check:
// a sudoers file or a sshd drop-in that does not parse breaks every sudo, or every login, on
// the server. One that is the same is left as it is, which is how add knows nothing changed
// (no reboot asked); one refused leaves what was there, and nothing beside it.
func TestHostPut(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := filepath.Join(dir, "piece")
	put := func(check, content string) (int, string) {
		t.Helper()
		c := exec.Command("sh", "-c", "set -eu\n"+hostPut+`put "$1" 640 "$2" "$3"`, "sh", f, check, content)
		out, _ := c.CombinedOutput()
		return c.ProcessState.ExitCode(), string(out)
	}
	holds := func(what, want string) {
		t.Helper()
		data, _ := os.ReadFile(f)
		entries, _ := os.ReadDir(dir)
		if string(data) != want || len(entries) > 1 || (want == "" && len(entries) != 0) {
			t.Errorf("%s: the piece holds %q, want %q, among %d files", what, data, want, len(entries))
		}
	}
	// The check reads the new content, at the path it is given, before it lands.
	seen := `grep -qx "rule one" "$1" && [ "$1" != ` + Quote(f) + ` ]`
	if code, out := put(seen, "rule one\n"); code != 0 {
		t.Fatalf("a missing piece (%d): %s", code, out)
	}
	holds("a missing piece", "rule one\n")
	if fi, err := os.Stat(f); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("its mode: %v (%v)", fi.Mode(), err)
	}
	// The same again changes nothing, and says so by failing, with no check run.
	before, _ := os.Stat(f)
	if code, out := put("exit 9", "rule one\n"); code != 1 {
		t.Errorf("the same piece again (%d): %s", code, out)
	}
	if after, _ := os.Stat(f); !os.SameFile(before, after) {
		t.Error("the same piece was written again")
	}
	holds("the same piece again", "rule one\n")
	if code, out := put("false", "rule two, broken\n"); code != 4 || !strings.Contains(out, "did not pass its check") {
		t.Errorf("a piece its check refuses (%d): %s", code, out)
	}
	holds("a piece its check refuses", "rule one\n")
	if code, out := put("true", "rule two\n"); code != 0 {
		t.Errorf("a piece that differs (%d): %s", code, out)
	}
	holds("a piece that differs", "rule two\n")
	if err := os.Remove(f); err != nil {
		t.Fatal(err)
	}
	if code, _ := put("false", "broken\n"); code != 4 {
		t.Errorf("a first piece its check refuses: exit %d", code)
	}
	holds("a first piece its check refuses", "")
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

// The --ssh-keys scripts find the door's directory as DoorDir does, by the same variables: a
// script that ran without the server's devopsy would otherwise edit keys sshd never reads.
func TestDoorDirShell(t *testing.T) {
	t.Parallel()
	for _, env := range []map[string]string{
		{"HOME": "/home/devopsy"},
		{"HOME": "/home/devopsy", "XDG_CONFIG_HOME": "/xdg"},
		{"HOME": "/home/devopsy", "XDG_CONFIG_HOME": "/xdg", "DEVOPSY_HOME": "/dh"},
	} {
		c := exec.Command("sh", "-c", "set -u\n"+doorDirShell+"\nprintf '%s' \"$door\"")
		c.Env = nil
		for k, v := range env {
			c.Env = append(c.Env, k+"="+v)
		}
		out, err := c.Output()
		want := filepath.Join(filepath.Dir(userConfigFile(func(k string) string { return env[k] }, env["HOME"])), "door")
		if err != nil || string(out) != want {
			t.Errorf("%v: %q %v, want %q", env, out, err, want)
		}
	}
}

// add takes a Unix user only when it made it: one there already that is no project user (not
// in the group) is someone's account, the deploy user's if it is named devopsy-<project>, and
// adopting it would put it in the group (sshd then ignores its own keys, the firewall holds
// it) and hand its home to the door. One add made before, and remove kept, is taken again.
func TestDoorUserCheck(t *testing.T) {
	t.Parallel()
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	write(t, filepath.Join(bin, "id"), `#!/bin/sh
case "$1 $2" in
  "-u devopsy-shop") echo 10002 ;;
  "-nG devopsy-shop") echo "devopsy-shop `+DoorGroup+`" ;;
  *) exec /usr/bin/id "$@" ;;
esac
`)
	if err := os.Chmod(filepath.Join(bin, "id"), 0o755); err != nil {
		t.Fatal(err)
	}
	for who, ok := range map[string]bool{"devopsy-nosuchuser": true, "devopsy-shop": true, me.Username: false} {
		c := exec.Command("sh", "-c", doorUserCheck, "sh", who)
		c.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
		if out, err := c.CombinedOutput(); (err == nil) != ok {
			t.Errorf("%s: %v %s", who, err, out)
		}
	}
}
