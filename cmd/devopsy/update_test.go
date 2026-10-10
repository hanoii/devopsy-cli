package main

import (
	"os"
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
