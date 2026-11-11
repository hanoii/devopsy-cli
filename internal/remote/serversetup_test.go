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

// setupHostRoot is a host root for SetupRootScript: the live buffers low, fakes of dockerd
// (FAKE_DOCKERD_REFUSES), sysctl and systemctl (FAKE_SYSTEMCTL_FAILS) logging to calls.
func setupHostRoot(t *testing.T) (host, bin string) {
	t.Helper()
	dir := t.TempDir()
	host, bin = filepath.Join(dir, "host"), filepath.Join(dir, "bin")
	for _, d := range []string{"proc/sys/net/core", "run", "etc"} {
		if err := os.MkdirAll(filepath.Join(host, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range setup.BufferKeys {
		if err := os.WriteFile(filepath.Join(host, setup.BufferPath(k)), []byte("212992\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fakes := map[string]string{
		"dockerd":   "#!/bin/sh\necho \"dockerd $*\" >>\"$CALLS\"\n[ -z \"${FAKE_DOCKERD_REFUSES:-}\" ] || { echo 'unknown key' >&2; exit 1; }\necho 'configuration OK'\n",
		"sysctl":    "#!/bin/sh\necho \"sysctl $*\" >>\"$CALLS\"\nk=${2%%=*}\necho \"${2#*=}\" >\"$HOST/proc/sys/$(echo \"$k\" | tr . /)\"\n",
		"systemctl": "#!/bin/sh\necho \"$SYSTEMD_IGNORE_CHROOT systemctl $*\" >>\"$CALLS\"\n[ -z \"${FAKE_SYSTEMCTL_FAILS:-}\" ] || exit 1\n",
	}
	for name, text := range fakes {
		if err := testfile.Write(filepath.Join(bin, name), []byte(text), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return host, bin
}

// The root script puts only what differs, each file after its check, daemon.json's by dockerd
// itself, and a failed check leaves the file as it was; the live buffers rise only when lower;
// the release root is made; the restart marker and the system's flag rise only when
// daemon.json changed and Docker is not restarted now, and a restart is queued, daemon-reload
// first, or marked when systemctl refuses.
func TestSetupRootScript(t *testing.T) {
	t.Parallel()
	if out, err := exec.Command("sh", "-n", "-c", SetupRootScript).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v\n%s", err, out)
	}
	host, bin := setupHostRoot(t)
	calls := filepath.Join(host, "calls")
	root := filepath.Join(filepath.Dir(host), "srv", "releases")
	dropin, err := setup.MountsDropInText(root)
	if err != nil {
		t.Fatal(err)
	}
	run := func(env []string, args ...string) (string, int) {
		t.Helper()
		os.Remove(calls)
		c := exec.Command("sh", append([]string{"-c", SetupRootScript, "sh"}, args...)...)
		c.Env = append(append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "HOST="+host, "CALLS="+calls), env...)
		out, _ := c.CombinedOutput()
		return string(out), c.ProcessState.ExitCode()
	}
	read := func(p string) string {
		data, _ := os.ReadFile(filepath.Join(host, p))
		return string(data)
	}
	owner := "0:0" // never used: nothing here is root's
	daemon := "{\n  \"live-restore\": true\n}\n"
	all := []string{host, owner, "1", root, daemon, setup.SysctlText(), dropin, "1", "", "inv-1"}
	out, code := run(nil, all...)
	for _, want := range []string{"made " + root + "\n", "put " + setup.DaemonFile + "\n", "put " + setup.SysctlFile + "\n", "raised net.core.rmem_max\n", "raised net.core.wmem_max\n", "put " + setup.MountsDropIn + "\n", "marked\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("first run: no %q (%d):\n%s", want, code, out)
		}
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		t.Errorf("the root not made: %v", err)
	}
	if read(setup.DaemonFile) != daemon || read(setup.SysctlFile) != setup.SysctlText() || read(setup.MountsDropIn) != dropin {
		t.Errorf("the files:\n%s\n%s\n%s", read(setup.DaemonFile), read(setup.SysctlFile), read(setup.MountsDropIn))
	}
	if read(setup.RestartMarker) != "inv-1\n" || read(RebootRequired+".pkgs") != "devopsy\n" {
		t.Errorf("the marker %q, the flag's packages %q", read(setup.RestartMarker), read(RebootRequired+".pkgs"))
	}
	if c := read("calls"); !strings.Contains(c, "dockerd --validate --config-file ") || !strings.Contains(c, "sysctl -qw net.core.rmem_max=7500000") || strings.Contains(c, "systemctl") {
		t.Errorf("calls:\n%s", c)
	}

	// Again: nothing differs, so nothing is said, marked or raised.
	if err := os.Remove(filepath.Join(host, setup.RestartMarker)); err != nil {
		t.Fatal(err)
	}
	if out, code := run(nil, all...); code != 0 || out != "" || read(setup.RestartMarker) != "" || strings.Contains(read("calls"), "sysctl") {
		t.Errorf("a second run (%d):\n%s\ncalls:\n%s", code, out, read("calls"))
	}

	// A daemon.json dockerd refuses: left as it was, nothing marked.
	refused := []string{host, owner, "", root, "{\"bad\": 1}\n", "", "", "", "", "inv-1"}
	if out, code := run([]string{"FAKE_DOCKERD_REFUSES=1"}, refused...); code != 4 || !strings.Contains(out, "did not pass its check") || read(setup.DaemonFile) != daemon || read(setup.RestartMarker) != "" {
		t.Errorf("a refused daemon.json (%d):\n%s\nthe file: %s", code, out, read(setup.DaemonFile))
	}

	// A restart: daemon-reload first, the restart queued, the marker gone.
	write := func(p, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(host, p), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(setup.RestartMarker, "inv-0\n")
	changed := []string{host, owner, "", root, "{\"live-restore\": false}\n", "", "", "", "restart", "inv-1"}
	out, code = run(nil, changed...)
	c := read("calls")
	if code != 0 || !strings.HasSuffix(out, "queued\n") || read(setup.RestartMarker) != "" ||
		!strings.HasPrefix(c, "dockerd --validate --config-file ") || !strings.HasSuffix(c, "1 systemctl daemon-reload\n1 systemctl restart --no-block docker.service\n") {
		t.Errorf("a restart (%d):\n%s\ncalls:\n%s", code, out, read("calls"))
	}

	// systemctl refused from the chroot: said, and marked for the next restart.
	out, code = run([]string{"FAKE_SYSTEMCTL_FAILS=1"}, host, owner, "", root, "", "", "", "", "restart", "inv-2")
	if code != 0 || !strings.Contains(out, "systemctl could not restart Docker") || !strings.Contains(out, "nosystemctl\nmarked\n") || read(setup.RestartMarker) != "inv-2\n" {
		t.Errorf("systemctl refused (%d):\n%s", code, out)
	}
}
