package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --release-settings prints the server's settings the scripts read, as the config writes them,
// and nothing else of it (gc:'s ages are --gc's own to read).
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
	for _, want := range []string{"root=" + home + "\n", "keep=3\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "\ngc_") {
		t.Errorf("gc: printed, which no script reads:\n%s", out)
	}
}
