package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --release-settings prints the server's settings a script or the operator reads, gc:'s ages
// among them, as the config writes them.
func TestReleaseSettingsPrints(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	write(t, filepath.Join(home, "config.yaml"), "releases:\n  keep: 3\ngc:\n  containers: 6h\n", 0o600)
	cmd := exec.Command(binary, "--release-settings")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "DEVOPSY_HOME=" + home}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"root=" + home + "\n", "keep=3\n", "gc_containers=6h0m0s\n", "gc_build_cache=30d\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}
