package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestUpgradeCommand(t *testing.T) {
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
