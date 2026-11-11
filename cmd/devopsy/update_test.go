package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/report"
)

func TestUpgradeCommand(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("root can write everywhere")
	}
	own, other := t.TempDir(), t.TempDir()
	exe := filepath.Join(own, "devopsy")
	if err := os.WriteFile(exe, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(other, "devopsy")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(other, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(other, 0o755) })

	// A link in a directory that is not ours to the binary in one that is,
	// as on servers.
	for _, path := range []string{exe, link} {
		if got := upgradeCommand(path); got != "devopsy --upgrade" {
			t.Errorf("upgradeCommand(%s) = %q", path, got)
		}
	}
	if err := os.Chmod(own, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(own, 0o755) })
	if got := upgradeCommand(link); got != "sudo devopsy --upgrade" {
		t.Errorf("upgradeCommand(%s) = %q in a read-only directory", link, got)
	}
}

// Flags that need no project work where the working directory cannot be
// read, like root's for a deploy user.
func TestNoWorkingDirectory(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("root can read everywhere")
	}
	dir := filepath.Join(t.TempDir(), "closed")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	run := func(args string) (string, error) {
		cmd := exec.Command("/bin/sh", "-c", `cd "$1" && chmod 000 . && exec "$2" `+args, "sh", dir, binary)
		cmd.Env = []string{"PATH=" + fakeBin(t) + ":/usr/bin:/bin", "HOME=" + t.TempDir()}
		out, err := cmd.CombinedOutput()
		os.Chmod(dir, 0o755)
		return string(out), err
	}
	if out, err := run("ps"); err == nil {
		t.Skipf("the working directory is readable here: %s", out)
	}
	for _, args := range []string{"--version", "--release-settings", "--completion bash"} {
		if out, err := run(args); err != nil {
			t.Errorf("devopsy %s: %v\n%s", args, err, out)
		}
	}
}

// An upgrade applies the server's setup only from a release build, on a server whose record
// is there: a laptop, a server never set up and a development build apply nothing.
func TestUpgradeApplies(t *testing.T) {
	t.Parallel()
	there := func(string) bool { return true }
	gone := func(string) bool { return false }
	for _, c := range []struct {
		version, record string
		exists          func(string) bool
		want            bool
	}{
		{"0.60.0", "/home/devopsy/.config/devopsy/server.yaml", there, true},
		{"0.60.0", "/home/devopsy/.config/devopsy/server.yaml", gone, false},
		{"0.60.0", "", there, false},
		{"dev", "/home/devopsy/.config/devopsy/server.yaml", there, false},
	} {
		if got := upgradeApplies(c.version, c.record, c.exists); got != c.want {
			t.Errorf("%s, %q: %v", c.version, c.record, got)
		}
	}
}

// An upgrade whose apply cannot start (its binary gone, say) warns, naming --server-setup, and
// exits 0: the upgrade itself is done.
func TestApplyAfterUpgradeFails(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if code := applyAfterUpgrade(report.New(&out, false, 0), filepath.Join(t.TempDir(), "gone"), true); code != 0 ||
		!strings.Contains(out.String(), "devopsy: warning: the server's setup was not applied: ") || !strings.Contains(out.String(), "run --server-setup for this server") {
		t.Errorf("(%d):\n%s", code, out.String())
	}
	out.Reset()
	if code := applyAfterUpgrade(report.New(&out, false, 0), "/nonexistent", false); code != 0 || out.Len() != 0 {
		t.Errorf("not set up (%d):\n%s", code, out.String())
	}
}
