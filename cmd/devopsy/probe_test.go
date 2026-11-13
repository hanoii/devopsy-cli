package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// --release-settings prints the server's settings the scripts read, as the config writes them,
// and nothing else of it (gc:'s ages are --gc's own to read), and where this user's image lock
// is (images=): the release root's for the deploy user, the home's for a project user, whose
// release root under releases: root is the deploy user's directory, where it can create nothing.
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
	root := t.TempDir()
	write(t, filepath.Join(home, "config.yaml"), "releases:\n  root: "+root+"\n", 0o600)
	link := t.TempDir()
	if err := os.Symlink(binary, filepath.Join(link, "docker")); err != nil {
		t.Fatal(err)
	}
	settings := func(path string) string {
		t.Helper()
		cmd := exec.Command(binary, "--release-settings")
		cmd.Env = []string{"PATH=" + path, "HOME=" + home, "DEVOPSY_HOME=" + home}
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if out := settings(fakeBin(t) + ":/usr/bin:/bin"); !strings.Contains(out, "\nimages="+root+"\n") {
		t.Errorf("the deploy user's image lock not in its release root:\n%s", out)
	}
	if syscall.Access(dockerSocket, 2) == nil {
		t.Skip("this user reaches " + dockerSocket + ": it has no door here")
	}
	if out := settings(link + ":/usr/bin:/bin"); !strings.Contains(out, "root="+root+"\n") || !strings.Contains(out, "\nimages="+home+"\n") {
		t.Errorf("a project user's image lock not in its home:\n%s", out)
	}
}
