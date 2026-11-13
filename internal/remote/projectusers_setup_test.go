package remote

import (
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

// The project users' root script, on a host root of the test's with fakes of systemctl
// (logging to calls, FAKE_SYSTEMCTL_FAILS, FAKE_SYSTEMCTL_FAILS_ON), groupadd and usermod
// (FAKE_GROUPADD_FAILS, FAKE_USERMOD_FAILS) and getent (the host root's etc/group): the space
// made, its unit put, the group made with the deploy user in it, /proc's unit put with the
// group's number, polkit's drop-in, then systemd reloaded, the space's unit enabled and
// started, the drop-in put once the mount holds, systemd reloaded again, and /proc's run again,
// a line per change and nothing of systemd's (its enable's links); a second run changes and says
// nothing but the mount and the remount it is asked for; systemd refusing a mount fails the
// space alone, naming it, and offers journalctl with the unit's name as systemd has it, its
// \x2d kept when the command is copied into a shell; a command of the processes' step that fails
// (groupadd, usermod, a put that cannot write) fails the processes alone, the space mounted and
// named in the drop-in all the same, said in devopsy's words; and a run with a failure exits
// DoorSetupSaid, which no tool's exit reaches.
func TestProjectUsersSetupScript(t *testing.T) {
	t.Parallel()
	if out, err := exec.Command("sh", "-n", "-c", ProjectUsersSetupScript).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
	// The temporary directory as it is, with no link above it (macOS's /var), the space the
	// test's own user's, as the deploy user's is.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	me := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	host, bin := filepath.Join(dir, "host"), filepath.Join(dir, "bin")
	calls := filepath.Join(dir, "calls")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		// systemctl enable says each link it makes, as systemd does, unless --quiet.
		"systemctl": "#!/bin/sh\nprintf '%s\\n' \"$SYSTEMD_IGNORE_CHROOT systemctl $*\" >>\"$CALLS\"\n[ -z \"${FAKE_SYSTEMCTL_FAILS:-}\" ] || exit 1\n[ \"${FAKE_SYSTEMCTL_FAILS_ON:-}\" != \"$*\" ] || exit 1\n" +
			"case \" $* \" in *' --quiet '*) ;; *' enable '*) echo \"Created symlink '/etc/systemd/system/x' -> '/etc/systemd/system/y'.\" >&2 ;; esac\n",
		"groupadd": "#!/bin/sh\n[ -z \"${FAKE_GROUPADD_FAILS:-}\" ] || exit 9\nmkdir -p \"$HOST/etc\"\necho \"$2:x:991:\" >>\"$HOST/etc/group\"\n",
		// usermod exits 6 for a user it does not find, as one from LDAP.
		"usermod": "#!/bin/sh\n[ -z \"${FAKE_USERMOD_FAILS:-}\" ] || { echo \"usermod: user '$3' does not exist\" >&2; exit 6; }\nawk -F: -v OFS=: -v g=\"$2\" -v u=\"$3\" '$1 == g { $4 = ($4 == \"\" ? u : $4 \",\" u) } { print }' \"$HOST/etc/group\" >\"$HOST/etc/group.new\" && mv \"$HOST/etc/group.new\" \"$HOST/etc/group\"\n",
		"getent":  "#!/bin/sh\nawk -F: -v k=\"$2\" '$1 == k || $3 == k { print; f = 1 } END { exit !f }' \"$HOST/etc/group\" 2>/dev/null\n",
		// The unit it is asked for, as it reaches it.
		"journalctl": "#!/bin/sh\nprintf '%s\\n' \"$2\"\n",
	} {
		if err := testfile.Write(filepath.Join(bin, name), []byte(text), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A - in the space's name, which systemd's name for its unit escapes (\x2d).
	space := filepath.Join(dir, "devopsy-door")
	unit := setup.SpaceUnit(space)
	word := QuoteWord(filepath.Base(unit))
	dropin, err := setup.MountsDropInText(filepath.Join(dir, "srv"), space)
	if err != nil {
		t.Fatal(err)
	}
	bare, err := setup.MountsDropInText(filepath.Join(dir, "srv"))
	if err != nil {
		t.Fatal(err)
	}
	run := func(env []string, args ...string) (string, int) {
		t.Helper()
		os.Remove(calls)
		c := exec.Command("sh", append([]string{"-c", ProjectUsersSetupScript, "sh"}, args...)...)
		c.Env = append(append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "HOST="+host, "CALLS="+calls), env...)
		out, _ := c.CombinedOutput()
		return string(out), c.ProcessState.ExitCode()
	}
	read := func(p string) string {
		data, _ := os.ReadFile(p)
		return string(data)
	}
	all := []string{host, me, space, "1", "", unit, setup.SpaceUnitText(space), "1", dropin, "1", "deploy", setup.ProcUnitText(setup.GIDMark), setup.PolkitDropInText(), word, bare}
	out, code := run(nil, all...)
	want := "made " + space + "\nput " + unit + "\ngroup devopsy-proc\nmember deploy\nput " + setup.ProcUnit + "\nput " + setup.PolkitDropIn + "\nmounted " + space + "\nput " + setup.MountsDropIn + "\nhidden\npolkit\n"
	if code != 0 || out != want {
		t.Errorf("a first run (%d):\n%s\nwant:\n%s", code, out, want)
	}
	if fi, err := os.Stat(space); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o755 {
		t.Errorf("the space: %v", err)
	}
	if read(filepath.Join(host, unit)) != setup.SpaceUnitText(space) || read(filepath.Join(host, setup.MountsDropIn)) != dropin ||
		read(filepath.Join(host, setup.ProcUnit)) != setup.ProcUnitText("991") || read(filepath.Join(host, setup.PolkitDropIn)) != setup.PolkitDropInText() {
		t.Errorf("the files:\n%s\n%s\n%s", read(filepath.Join(host, unit)), read(filepath.Join(host, setup.ProcUnit)), read(filepath.Join(host, setup.PolkitDropIn)))
	}
	if g := read(filepath.Join(host, "etc", "group")); g != "devopsy-proc:x:991:deploy\n" {
		t.Errorf("the group file: %q", g)
	}
	name := filepath.Base(unit)
	wantCalls := "1 systemctl daemon-reload\n1 systemctl enable --quiet --now " + name + "\n1 systemctl daemon-reload\n1 systemctl enable --quiet devopsy-proc.service\n1 systemctl restart devopsy-proc.service\n1 systemctl try-restart polkit.service\n"
	if c := read(calls); c != wantCalls {
		t.Errorf("calls:\n%s\nwant:\n%s", c, wantCalls)
	}

	// Again, everything there: no file put, no group made, no reload; the mount and the remount
	// it is asked for go again, each said.
	if out, code := run(nil, all...); code != 0 || out != "mounted "+space+"\nhidden\n" || strings.Contains(read(calls), "daemon-reload") || strings.Contains(read(calls), "polkit") {
		t.Errorf("a second run (%d):\n%s\ncalls:\n%s", code, out, read(calls))
	}
	// Only the mode set back, asked alone.
	if err := os.Chmod(space, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, code := run(nil, host, me, space, "", "1", unit, "", "", "", "", "deploy", "", "", word, ""); code != 0 || out != "mode "+space+"\n" || read(calls) != "" {
		t.Errorf("the mode (%d):\n%s", code, out)
	}
	if fi, _ := os.Stat(space); fi.Mode().Perm() != 0o755 {
		t.Errorf("the mode: %v", fi.Mode())
	}
	// systemd refusing the mount: the run fails, naming it, says no mount, and puts the drop-in
	// without the space, which a mount that fails would keep Docker from starting at boot on.
	out, code = run([]string{"FAKE_SYSTEMCTL_FAILS=1"}, host, me, space, "", "", unit, "", "1", dropin, "", "deploy", "", "", word, bare)
	if code != DoorSetupSaid || !strings.Contains(out, "devopsy: error: systemd did not mount "+space+"\n") || strings.Contains(out, "mounted") ||
		!strings.Contains(out, "put "+setup.MountsDropIn+"\n") || read(filepath.Join(host, setup.MountsDropIn)) != bare {
		t.Errorf("a mount systemd refuses (%d):\n%s\nthe drop-in:\n%s", code, out, read(filepath.Join(host, setup.MountsDropIn)))
	}
	var command string
	for _, l := range strings.Split(out, "\n") {
		if _, after, ok := strings.Cut(l, "$ "); ok && strings.HasPrefix(after, "journalctl") {
			command = after
		}
	}
	sh := exec.Command("sh", "-c", command)
	sh.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
	if got, err := sh.Output(); command == "" || err != nil || strings.TrimSpace(string(got)) != filepath.Base(unit) {
		t.Errorf("the offered %q asks journalctl for %q, not %q (%v):\n%s", command, strings.TrimSpace(string(got)), filepath.Base(unit), err, out)
	}
	// systemd not reloading its units: none of devopsy's is started, said once, the drop-in put
	// without the space.
	out, code = run([]string{"FAKE_SYSTEMCTL_FAILS_ON=daemon-reload"}, filepath.Join(dir, "reload"), me, space, "", "", unit, setup.SpaceUnitText(space), "1", dropin, "1", "deploy", setup.ProcUnitText(setup.GIDMark), "", word, bare)
	if code != DoorSetupSaid || strings.Count(out, "devopsy: error: ") != 1 || !strings.Contains(out, "devopsy: error: systemd could not reload its units, so devopsy started none of its own units\n") ||
		strings.Contains(out, "mounted") || strings.Contains(out, "hidden") || read(filepath.Join(dir, "reload", setup.MountsDropIn)) != bare {
		t.Errorf("a reload systemd refuses (%d):\n%s", code, out)
	}
	// polkit not restarted: a warning with the restart to run, the run going on, never said
	// restarted.
	out, code = run([]string{"FAKE_SYSTEMCTL_FAILS_ON=try-restart polkit.service"}, filepath.Join(dir, "polkit"), me, space, "", "", unit, "", "", "", "1", "deploy", setup.ProcUnitText(setup.GIDMark), setup.PolkitDropInText(), word, "")
	if code != 0 || !strings.Contains(out, "hidden\n") || strings.Contains(out, "\npolkit\n") ||
		!strings.Contains(out, "devopsy: warning: systemd did not restart polkit, which sees only its own processes until it restarts\ndevopsy: | to fix it:\ndevopsy: | restart it, as root:\ndevopsy: | $ systemctl restart polkit.service\n") {
		t.Errorf("polkit not restarted (%d):\n%s", code, out)
	}
	// A command of the processes' step that fails before the mount, each on a host of its own.
	for name, c := range map[string]struct {
		env []string
		// readOnly is a directory under the host root made unwritable: a put there fails.
		readOnly, want string
	}{
		"usermod":  {env: []string{"FAKE_USERMOD_FAILS=1"}, want: "devopsy: error: devopsy could not put deploy in devopsy-proc\n"},
		"groupadd": {env: []string{"FAKE_GROUPADD_FAILS=1"}, want: "devopsy: error: devopsy could not make the group devopsy-proc\n"},
		"a put":    {readOnly: path.Dir(setup.PolkitDropIn), want: setup.PolkitDropIn + " could not be written\n"},
	} {
		h := filepath.Join(dir, "procs-"+strings.ReplaceAll(name, " ", "-"))
		if c.readOnly != "" {
			ro := filepath.Join(h, c.readOnly)
			if err := os.MkdirAll(ro, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(ro, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(ro, 0o755) })
		}
		sp := filepath.Join(dir, "space-"+strings.ReplaceAll(name, " ", "-"))
		with, err := setup.MountsDropInText(filepath.Join(dir, "srv"), sp)
		if err != nil {
			t.Fatal(err)
		}
		u := setup.SpaceUnit(sp)
		out, code := run(append(c.env, "HOST="+h), h, me, sp, "1", "", u, setup.SpaceUnitText(sp), "1", with, "1", "deploy", setup.ProcUnitText(setup.GIDMark), setup.PolkitDropInText(), QuoteWord(filepath.Base(u)), bare)
		if code != DoorSetupSaid || !strings.Contains(out, "mounted "+sp+"\n") || read(filepath.Join(h, setup.MountsDropIn)) != with || !strings.Contains(out, c.want) || strings.Contains(out, "hidden\n") {
			t.Errorf("%s fails before the mount (%d):\n%s\nthe drop-in:\n%s", name, code, out, read(filepath.Join(h, setup.MountsDropIn)))
		}
	}
}

// The root script touches the space only while no user but root and the deploy user can change
// a part of its path, judged again right before it acts: a space under a directory others may
// write, under a link, or itself a link, and one not there yet in a sticky directory others
// may write (/tmp's way), is left as it is, said, nothing made, and the drop-in goes without
// the space; a sticky directory holding the space as the deploy user's lets it through.
func TestProjectUsersSetupScriptSafePath(t *testing.T) {
	t.Parallel()
	// The temporary directory as it is, with no link above it (macOS's /var).
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host, bin := filepath.Join(dir, "host"), filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := testfile.Write(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	me := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	mkdir := func(p string, mode os.FileMode) string {
		t.Helper()
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	link := func(to, p string) string {
		t.Helper()
		if err := os.Symlink(to, p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	real := mkdir(filepath.Join(dir, "real"), 0o755)
	open := mkdir(filepath.Join(dir, "open"), 0o777)
	sticky := mkdir(filepath.Join(dir, "sticky"), 0o777|os.ModeSticky)
	mkdir(filepath.Join(sticky, "mine"), 0o755)
	bare, err := setup.MountsDropInText(filepath.Join(dir, "srv"))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		space string
		safe  bool
	}{
		"others may write a parent":    {filepath.Join(open, "door"), false},
		"a link above it":              {filepath.Join(link(real, filepath.Join(dir, "via")), "door"), false},
		"a link for a space":           {link(real, filepath.Join(dir, "door")), false},
		"not there in a sticky parent": {filepath.Join(sticky, "door"), false},
		"the deploy user's in sticky":  {filepath.Join(sticky, "mine"), true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			hostRoot := filepath.Join(host, strings.ReplaceAll(name, " ", "-"))
			with, err := setup.MountsDropInText(filepath.Join(dir, "srv"), c.space)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", "-c", ProjectUsersSetupScript, "sh", hostRoot, me, c.space, "1", "1", setup.SpaceUnit(c.space), "", "", with, "", "deploy", "", "", "", bare)
			cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
			out, _ := cmd.CombinedOutput()
			code := cmd.ProcessState.ExitCode()
			dropin, _ := os.ReadFile(filepath.Join(hostRoot, setup.MountsDropIn))
			refused := strings.Contains(string(out), "devopsy: error: "+c.space+", or a directory above it, could be changed by another user: devopsy did not touch it\n")
			if c.safe {
				if code != 0 || refused || string(dropin) != with {
					t.Errorf("(%d):\n%s\nthe drop-in:\n%s", code, out, dropin)
				}
				return
			}
			if _, err := os.Lstat(filepath.Join(real, "door")); code != DoorSetupSaid || !refused || strings.Contains(string(out), "made ") || string(dropin) != bare || err == nil {
				t.Errorf("(%d):\n%s\nthe drop-in:\n%s", code, out, dropin)
			}
		})
	}
}
