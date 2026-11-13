package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/setup"
	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// The project users' root script, on a host root of the test's with fakes of systemctl
// (logging to calls, FAKE_SYSTEMCTL_FAILS), groupadd, usermod and getent (the host root's
// etc/group): the space made, its unit put, the group made with the deploy user in it, /proc's
// unit put with the group's number, polkit's drop-in, then systemd reloaded, the space's unit
// enabled and started, the drop-in put once the mount holds, systemd reloaded again, and
// /proc's run again, a line per change; a second run
// changes and says nothing but the mount and the remount it is asked for; systemd refusing a
// mount fails the run, naming it, and offers journalctl with the unit's name as systemd has it,
// its \x2d kept when the command is copied into a shell.
func TestProjectUsersSetupScript(t *testing.T) {
	t.Parallel()
	if out, err := exec.Command("sh", "-n", "-c", ProjectUsersSetupScript).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
	dir := t.TempDir()
	host, bin := filepath.Join(dir, "host"), filepath.Join(dir, "bin")
	calls := filepath.Join(dir, "calls")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"systemctl": "#!/bin/sh\nprintf '%s\\n' \"$SYSTEMD_IGNORE_CHROOT systemctl $*\" >>\"$CALLS\"\n[ -z \"${FAKE_SYSTEMCTL_FAILS:-}\" ] || exit 1\n",
		"groupadd":  "#!/bin/sh\nmkdir -p \"$HOST/etc\"\necho \"$2:x:991:\" >>\"$HOST/etc/group\"\n",
		"usermod":   "#!/bin/sh\nawk -F: -v OFS=: -v g=\"$2\" -v u=\"$3\" '$1 == g { $4 = ($4 == \"\" ? u : $4 \",\" u) } { print }' \"$HOST/etc/group\" >\"$HOST/etc/group.new\" && mv \"$HOST/etc/group.new\" \"$HOST/etc/group\"\n",
		"getent":    "#!/bin/sh\nawk -F: -v k=\"$2\" '$1 == k || $3 == k { print; f = 1 } END { exit !f }' \"$HOST/etc/group\" 2>/dev/null\n",
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
	all := []string{host, "0:0", space, "1", "", unit, setup.SpaceUnitText(space), "1", dropin, "1", "deploy", setup.ProcUnitText(setup.GIDMark), setup.PolkitDropInText(), word, bare}
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
	wantCalls := "1 systemctl daemon-reload\n1 systemctl enable --now " + name + "\n1 systemctl daemon-reload\n1 systemctl enable devopsy-proc.service\n1 systemctl restart devopsy-proc.service\n1 systemctl try-restart polkit.service\n"
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
	if out, code := run(nil, host, "0:0", space, "", "1", unit, "", "", "", "", "deploy", "", "", word, ""); code != 0 || out != "mode "+space+"\n" || read(calls) != "" {
		t.Errorf("the mode (%d):\n%s", code, out)
	}
	if fi, _ := os.Stat(space); fi.Mode().Perm() != 0o755 {
		t.Errorf("the mode: %v", fi.Mode())
	}
	// systemd refusing the mount: the run fails, naming it, says no mount, and puts the drop-in
	// without the space, which a mount that fails would keep Docker from starting at boot on.
	out, code = run([]string{"FAKE_SYSTEMCTL_FAILS=1"}, host, "0:0", space, "", "", unit, "", "1", dropin, "", "deploy", "", "", word, bare)
	if code != 6 || !strings.Contains(out, "devopsy: error: systemd did not mount "+space+"\n") || strings.Contains(out, "mounted") ||
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
}
