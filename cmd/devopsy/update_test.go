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

// An upgrade applies the server's setup once per devopsy version, from a release build, on a
// server whose record is there: another version's record (or one with none) is applied, the
// same version's left alone, and a laptop, a server never set up and a development build
// apply nothing.
func TestUpgradeApplies(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		version, recorded string
		exists            bool
		want              bool
	}{
		{"0.65.0", "0.64.0", true, true},
		{"0.65.0", "", true, true},
		{"0.65.0", "0.65.0", true, false},
		{"0.65.0", "", false, false},
		{"dev", "0.64.0", true, false},
	} {
		if got := upgradeApplies(c.version, c.recorded, c.exists); got != c.want {
			t.Errorf("%s, recorded %q, there %v: %v", c.version, c.recorded, c.exists, got)
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

// --match-server-version reads the server's devopsy --version in one session: a server of this
// devopsy's version downloads nothing, a server's development build is refused, and so is a
// local development build, before connecting.
func TestMatchServerVersion(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	match := func(exe string) (string, int) {
		cmd := exec.Command(exe, "@prod", "--match-server-version")
		cmd.Dir, cmd.Env = s.project, s.env
		out, _ := cmd.CombinedOutput()
		return string(out), cmd.ProcessState.ExitCode()
	}
	rel, err := releaseBinary()
	if err != nil {
		t.Fatal(err)
	}
	s.logs()
	if out, code := match(binary); code == 0 || !strings.Contains(out, "development build: update its checkout instead") {
		t.Errorf("a local development build (%d):\n%s", code, out)
	}
	if _, ssh := s.logs(); ssh != "" {
		t.Errorf("a local development build connected:\n%s", ssh)
	}
	if out, code := match(rel); code == 0 || !strings.Contains(out, "devopsy: error: the server's devopsy dev is a development build, which no release matches\n") {
		t.Errorf("the server's development build (%d):\n%s", code, out)
	}
	asRelease(t, s)
	if out, code := match(rel); code != 0 || out != "devopsy: devopsy "+releaseVersion+" is the server's already\n" {
		t.Errorf("the server's version already (%d):\n%s", code, out)
	}
}

// The server's devopsy --version names its release on its first line, and anything else names
// none: no download is tried.
func TestServerRelease(t *testing.T) {
	t.Parallel()
	for out, want := range map[string]string{
		"devopsy 0.69.0\ndocker 29.9.1\ndocker compose 5.5.0\n": "0.69.0",
		"devopsy v0.69.0-2-g1a2b3c4-dirty\n":                    "is a development build",
		"devopsy dev\n":                                         "is a development build",
		"":                                                      "answered no version",
		"sh: 1: devopsy: not found\n":                           "answered no version",
	} {
		got, err := serverRelease(out)
		if err != nil {
			got = err.Error()
		}
		if !strings.Contains(got, want) {
			t.Errorf("%q: %q, want %q", out, got, want)
		}
	}
}
