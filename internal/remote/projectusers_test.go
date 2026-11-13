package remote

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
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
		// The session's hints alone come through the door's sudo, by name.
		`Defaults!DEVOPSY_DOOR env_reset, !env_keep, env_keep = "DEVOPSY_INTERNAL_ADDRESS DEVOPSY_INTERNAL_HINT_ADDRESS", !setenv`,
		"%devopsy-projects ALL=(deploy : devopsy-door) NOPASSWD: DEVOPSY_DOOR",
		"Cmnd_Alias DEVOPSY_DOOR_READ = /opt/bin/devopsy --door-read *",
		"Defaults!DEVOPSY_DOOR_READ env_reset, !env_keep, !setenv",
		"deploy ALL=(%devopsy-projects) NOPASSWD: DEVOPSY_DOOR_READ",
		"--authorized-keys",
		"sshd -T -C",
		"for f in /etc/cron.deny /etc/at.deny; do",
		`rm -f "/var/lib/systemd/linger/$user"`,
		// No user manager, and a login shell that reads nothing of the home.
		`ln -sfn /dev/null "/etc/systemd/system/user@$uid.service"`,
		"--shell " + DoorShell + " ",
		"usermod -s " + DoorShell + ` "$user"`,
		"Match Group devopsy-projects",
		"AuthorizedKeysFile none",
		"AuthorizedKeysCommandUser deploy",
		"visudo -cqf",
		"sshd -t",
		// No subordinate ids, for a user made here or one that was there; its uid and its
		// group's gid the one id the deploy user gave it, never one useradd picks.
		`groupadd -g "$id" "$user"`,
		`useradd -K SUB_UID_COUNT=0 -K SUB_GID_COUNT=0 -u "$id" -g "$id" `,
		"for f in /etc/subuid /etc/subgid; do",
		// The door's entrance for a project user, never over a real docker.
		"ln -sfn '/opt/bin/devopsy' /usr/local/bin/docker",
		"/usr/local/bin/docker is a real docker",
		// sshd takes the drop-in at a reboot, which empties /run (hostRestartFlag).
		"restart_flag /run/devopsy-door-reboot\n", hostRestartFlag,
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
		"if put /etc/ssh/sshd_config.d/devopsy.conf 644 true", "if ! sshd -t; then", "put back as it was", "no id given for $user", "groupadd -g", "useradd ", "sshd -T -C", "/etc/cron.deny",
		`if [ -n "$made" ] && [ -n "$root" ]; then`, `runuser -u "$user" -- sh -c`,
	} {
		i := strings.Index(s, step)
		if i < last {
			t.Errorf("%q comes too early, or is not there (%d, after %d)", step, i, last)
		}
		last = max(last, i)
	}
}

// The restart flag marks what waits for a restart, empty or holding what it is given, and
// raises the system's flag with devopsy in its .pkgs once, keeping the packages there.
func TestHostRestartFlag(t *testing.T) {
	t.Parallel()
	if out, err := exec.Command("sh", "-n", "-c", hostRestartFlag).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
	host := t.TempDir()
	if err := os.Mkdir(filepath.Join(host, "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	pkgs := filepath.Join(host, RebootRequired+".pkgs")
	if err := os.WriteFile(pkgs, []byte("linux-image-amd64\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("sh", "-c", "set -eu\n"+hostRestartFlag+`restart_flag /run/door-mark
restart_flag /run/docker-mark "$1"
restart_flag /run/docker-mark "$1"`, "sh", "id-1")
	c.Env = append(os.Environ(), "host_root="+host)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for file, want := range map[string]string{
		"/run/door-mark": "", "/run/docker-mark": "id-1\n", RebootRequired: "", RebootRequired + ".pkgs": "linux-image-amd64\ndevopsy\n",
	} {
		if data, err := os.ReadFile(filepath.Join(host, file)); err != nil || string(data) != want {
			t.Errorf("%s holds %q (%v), want %q", file, data, err, want)
		}
	}
}

// A host piece lands only when it is missing or differs, and only once it passed its check:
// a sudoers file or a sshd drop-in that does not parse breaks every sudo, or every login, on
// the server. One that is the same is left as it is, which is how add knows nothing changed
// (no reboot asked), and one whose mode alone differs gets its mode back with nothing
// reported (keep: leaving the admin's); one refused leaves what was there, and nothing
// beside it.
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
	// The same text in another mode (a loader that lost its exec bit) gets its mode back,
	// with no check run, and reports no new text, which callers read as a reboot or a reload.
	if err := os.Chmod(f, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out := put("exit 9", "rule two\n"); code != 1 {
		t.Errorf("the same piece in another mode (%d): %s", code, out)
	}
	if fi, err := os.Stat(f); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("its mode put back: %v (%v)", fi.Mode(), err)
	}
	holds("a piece whose mode was put back", "rule two\n")
	// A link with the same text is left as it is, the file it names keeping its mode: a file
	// devopsy may not own.
	target := filepath.Join(t.TempDir(), "named")
	write(t, target, "rule two\n")
	if err := os.Chmod(target, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("sh", "-c", "set -eu\n"+hostPut+`put "$1" 640 true "$2"`, "sh", link, "rule two\n")
	if out, _ := c.CombinedOutput(); c.ProcessState.ExitCode() != 1 {
		t.Errorf("a link to the same text (%d): %s", c.ProcessState.ExitCode(), out)
	}
	if fi, err := os.Stat(target); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the linked file's mode: %v (%v)", fi.Mode(), err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link replaced: %v (%v)", fi.Mode(), err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	// keep: leaves the mode its admin gave it.
	if err := os.Chmod(f, 0o600); err != nil {
		t.Fatal(err)
	}
	c = exec.Command("sh", "-c", "set -eu\n"+hostPut+`put "$1" keep:640 true "$2"`, "sh", f, "rule two\n")
	if out, _ := c.CombinedOutput(); c.ProcessState.ExitCode() != 1 {
		t.Errorf("keep: on the same text (%d): %s", c.ProcessState.ExitCode(), out)
	}
	if fi, err := os.Stat(f); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("keep: changed the admin's mode: %v (%v)", fi.Mode(), err)
	}
	if err := os.Chmod(f, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f); err != nil {
		t.Fatal(err)
	}
	if code, _ := put("false", "broken\n"); code != 4 {
		t.Errorf("a first piece its check refuses: exit %d", code)
	}
	holds("a first piece its check refuses", "")
	// A move that fails (a full disk, a read-only file system) inside an if is the run's
	// failure, never the same text.
	bin := t.TempDir()
	write(t, filepath.Join(bin, "mv"), "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(filepath.Join(bin, "mv"), 0o755); err != nil {
		t.Fatal(err)
	}
	c = exec.Command("sh", "-c", "set -eu\n"+hostPut+`if put "$1" 640 true "$2"; then echo put; else echo same; fi`, "sh", f, "rule three\n")
	c.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
	moved, _ := c.CombinedOutput()
	if code := c.ProcessState.ExitCode(); code != 5 || !strings.Contains(string(moved), "could not be written") || strings.Contains(string(moved), "same") {
		t.Errorf("a move that fails (%d): %s", code, moved)
	}
	holds("a move that failed", "")
}

// add refuses a host whose sshd would not take its drop-in for the project user: a sshd_config
// with no Include of sshd_config.d, or with it below a Match, parses (sshd -t) and leaves the
// user on its own ~/.ssh/authorized_keys, where a leaked key adds a second one that outlives
// its rotation and remove, and one whose login settings (no ~/.ssh/rc, no forwarding) another
// value read first overrides. What sshd resolves for that user says (sshd -T).
func TestDoorSSHDTakes(t *testing.T) {
	t.Parallel()
	taken := "authorizedkeyscommand " + DoorKeysCommand + " %u\nauthorizedkeyscommanduser deploy\nauthorizedkeysfile none\n" +
		"permituserrc no\nallowagentforwarding no\nx11forwarding no\npermittunnel no\n"
	for name, c := range map[string]struct {
		effective string
		ok        bool
	}{
		"taken":            {taken, true},
		"no include":       {"authorizedkeyscommand none\nauthorizedkeyscommanduser none\nauthorizedkeysfile .ssh/authorized_keys .ssh/authorized_keys2\n", false},
		"another's user":   {strings.Replace(taken, "user deploy", "user nobody", 1), false},
		"its own keys too": {strings.Replace(taken, "authorizedkeysfile none", "authorizedkeysfile none .ssh/authorized_keys", 1), false},
		// A value sshd took first from elsewhere: the user's ~/.ssh/rc, an agent forwarded.
		"its rc":   {strings.Replace(taken, "permituserrc no", "permituserrc yes", 1), false},
		"an agent": {strings.Replace(taken, "allowagentforwarding no", "allowagentforwarding yes", 1), false},
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
	for p, ok := range map[string]bool{"/home/devopsy/.local/bin/devopsy": true, "/opt/dev opsy/devopsy": false, "/opt/a,b/devopsy": false, "devopsy": false, "/opt/x=y/devopsy": false,
		// A glob's characters match other paths, # starts a comment and ! negates.
		"/opt/*/devopsy": false, "/opt/dev?psy": false, "/opt/[a]/devopsy": false, "/opt/#x/devopsy": false, "/opt/!x/devopsy": false} {
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

// --project-users add asks a reboot for the sshd drop-in only when sshd has a new text to
// take: a drop-in with devopsy's text in another mode (a hardening script's 600 on
// sshd_config.d) changes nothing sshd reads, so its mode is put back with no reboot marker,
// no restart flag and no word that project users wait for a reboot. The script's own lines
// for the drop-in run here, its paths moved into a directory.
func TestDoorHostScriptAsksNoRebootForADropInsMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := DoorHostScript("deploy", "/opt/bin/devopsy")
	from, to := strings.Index(s, "old=$(cat "+DoorSSHDropIn), strings.Index(s, "made=\n")
	if from < 0 || to < from {
		t.Fatalf("the drop-in's lines not found in the host script:\n%s", s)
	}
	dropIn := filepath.Join(dir, "devopsy.conf")
	lines := strings.ReplaceAll(s[from:to], DoorSSHDropIn, dropIn)
	write(t, filepath.Join(dir, "bin", "sshd"), "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(dir, "bin", "sshd"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func() string {
		t.Helper()
		cmd := exec.Command("sh", "-c", "set -eu\n"+hostPut+"restart_flag() { echo \"flagged $1\"; }\n"+lines)
		cmd.Env = []string{"PATH=" + filepath.Join(dir, "bin") + ":/usr/bin:/bin"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return string(out)
	}
	// The control: a drop-in not there is put, and a reboot asked.
	if out := run(); !strings.Contains(out, "reboot") || !strings.Contains(out, "flagged "+DoorRebootMarker) {
		t.Fatalf("a new drop-in asked no reboot:\n%s", out)
	}
	if out := run(); out != "" {
		t.Fatalf("the same drop-in again:\n%s", out)
	}
	if err := os.Chmod(dropIn, 0o600); err != nil {
		t.Fatal(err)
	}
	if out := run(); out != "" {
		t.Errorf("the drop-in's text as devopsy's, its mode 600: a reboot asked for nothing:\n%s", out)
	}
}

// Each piece --project-users add's host step puts ends the step when it cannot be written (a
// full disk, a read-only file system), as each is called: put's exit 5 goes through its
// `|| :` and its if, never read as the same text, so no step after it runs on a piece missing.
func TestDoorHostScriptStopsAtAPieceNotWritten(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	for name, body := range map[string]string{"mv": "#!/bin/sh\nexit 1\n", "visudo": "#!/bin/sh\n", "sshd": "#!/bin/sh\n"} {
		write(t, filepath.Join(bin, name), body)
		if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Each call from its start to its end, its quoted text across lines: put's own lines and
	// what it is called by.
	script := DoorHostScript("deploy", "/opt/bin/devopsy")
	n := 0
	for _, at := range regexp.MustCompile(`(?m)^\s*(if )?put `).FindAllStringIndex(script, -1) {
		call := script[at[0]:]
		cut := min(strings.Index(call+" || :\n", " || :\n"), strings.Index(call+"; then\n", "; then\n"))
		if cut == len(call) {
			t.Fatalf("a put called otherwise than in an if or with || : (a pipeline or $(...) keeps its exit): %.80s", call)
		}
		ifCall := strings.HasPrefix(strings.TrimSpace(call), "if ")
		target, args, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(call[:cut]), "if "), " ")
		target, args, _ = strings.Cut(args, " ")
		n++
		piece := `put "$1" ` + args
		if ifCall {
			piece = "if " + piece + "; then :; fi"
		} else {
			piece += " || :"
		}
		f := filepath.Join(t.TempDir(), filepath.Base(target))
		c := exec.Command("sh", "-c", "set -eu\n"+hostPut+piece+"\necho after", "sh", f)
		c.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
		out, _ := c.CombinedOutput()
		if code := c.ProcessState.ExitCode(); code != 5 || !strings.Contains(string(out), "could not be written") || strings.Contains(string(out), "after") {
			t.Errorf("%s, not written (%d): %s", target, code, out)
		}
	}
	if n < 3 {
		t.Errorf("%d pieces put, want the sudoers rule, the keys command and the sshd drop-in at least", n)
	}
}

// A destroy's host step deletes the project user only while it is there and in the project
// users' group (a rerun finds it gone), then the tree and the home by their paths, each only as
// a directory: a link the user made there is left, and so is what it points to.
func TestDoorUserDeleteScript(t *testing.T) {
	t.Parallel()
	if out, err := exec.Command("sh", "-n", "-c", DoorUserDeleteScript).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
	// groupLine is the user's group as getent gives it once the user is deleted, "" for none:
	// a host that keeps a user's group leaves it.
	groupLine := ""
	groupGone := func() bool { return true }
	run := func(group string, tree, home string) (string, int, string) {
		t.Helper()
		bin, users := t.TempDir(), t.TempDir()
		write(t, filepath.Join(users, "devopsy-shop"), "")
		if groupLine != "" {
			write(t, filepath.Join(users, "group-devopsy-shop"), groupLine)
		}
		groupGone = func() bool { return !fileThere(filepath.Join(users, "group-devopsy-shop")) }
		for name, body := range map[string]string{
			"id":       `case "$1" in -u) [ -e "` + users + `/$2" ] ;; -nG) echo "$2 ` + group + `" ;; esac`,
			"userdel":  `[ "$1" = -r ] && rm "` + users + `/$2"`,
			"getent":   `[ "$1" = group ] && cat "` + users + `/group-$2" 2>/dev/null`,
			"groupdel": `rm "` + users + `/group-$1"`,
		} {
			write(t, filepath.Join(bin, name), "#!/bin/sh\n"+body+"\n")
			if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		c := exec.Command("sh", "-c", DoorUserDeleteScript, "sh", "devopsy-shop", "59999", tree, home)
		c.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
		out, _ := c.CombinedOutput()
		gone := "deleted"
		if _, err := os.Stat(filepath.Join(users, "devopsy-shop")); err == nil {
			gone = "kept"
		}
		return string(out), c.ProcessState.ExitCode(), gone
	}
	dir := t.TempDir()
	tree, home := filepath.Join(dir, "shop"), filepath.Join(dir, "devopsy-shop")
	write(t, filepath.Join(tree, "prod", "db"), "x")
	write(t, filepath.Join(home, ".profile"), "x")
	if out, code, user := run(DoorGroup, tree, home); code != 0 || user != "deleted" || fileThere(tree) || fileThere(home) {
		t.Errorf("a project user (%d): user %s, tree %v, home %v:\n%s", code, user, fileThere(tree), fileThere(home), out)
	}
	// Run again with nothing left, nothing fails.
	if out, code, _ := run(DoorGroup, tree, home); code != 0 {
		t.Errorf("run again (%d):\n%s", code, out)
	}
	if out, code, user := run("users", "", ""); code != 3 || user != "kept" || !strings.Contains(out, "is no project user") {
		t.Errorf("no project user (%d): %s:\n%s", code, user, out)
	}
	// The group of its name userdel left goes when it is the project user's (in the range, no
	// member), so an add takes no id for it again; another stays, said.
	groupLine = "devopsy-shop:x:59999:\n"
	if out, code, _ := run(DoorGroup, "", ""); code != 0 || !groupGone() {
		t.Errorf("a group left by userdel (%d), gone %v:\n%s", code, groupGone(), out)
	}
	groupLine = "devopsy-shop:x:59999:alice\n"
	if out, code, _ := run(DoorGroup, "", ""); code != 0 || groupGone() || !strings.Contains(out, "left as it is") {
		t.Errorf("a group with a member (%d), gone %v:\n%s", code, groupGone(), out)
	}
	groupLine = ""
	target := t.TempDir()
	write(t, filepath.Join(target, "keep"), "x")
	if err := os.Symlink(target, tree); err != nil {
		t.Fatal(err)
	}
	if out, code, _ := run(DoorGroup, tree, ""); code != 3 || !strings.Contains(out, "is a link, left") || !fileThere(filepath.Join(target, "keep")) || !fileThere(tree) {
		t.Errorf("a tree that is a link (%d):\n%s", code, out)
	}
}
