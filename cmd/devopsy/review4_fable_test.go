package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Review 4 (Fable), a failing test proving a bug: a project user's `docker compose exec` or
// `run` whose own command holds -h or --help (df -h, du -h, drush --help) never reaches the
// door. dockerCompose looks for those two words anywhere after the verb and hands the whole
// command to the real docker, which has no socket for a project user: the command fails on
// Docker's permission error instead of running in the service. Only compose's own help, a
// flag before the service's name, has no daemon behind it.
func TestReview4DoorClientKeepsACommandsOwnHelpFlag(t *testing.T) {
	t.Parallel()
	if syscall.Access(dockerSocket, 2) == nil {
		t.Skip("this user reaches Docker's socket: its docker is no door")
	}
	dir := t.TempDir()
	bin, realBin := filepath.Join(dir, "bin"), filepath.Join(dir, "real")
	asked, direct := filepath.Join(dir, "asked"), filepath.Join(dir, "direct")
	// The real docker renders, and records any other call: one that went past the door.
	write(t, filepath.Join(realBin, "docker"), "#!/bin/sh\ncase \" $* \" in *' config '*) echo '{\"name\":\"shop-prod\",\"services\":{}}';; *) printf '%s\\n' \"$*\" >>"+direct+";; esac\n", 0o755)
	write(t, filepath.Join(bin, "sudo"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >>"+asked+"\n", 0o755)
	if err := os.Symlink(binary, filepath.Join(bin, "docker")); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (door, real, out string) {
		os.Remove(asked)
		os.Remove(direct)
		cmd := exec.Command(filepath.Join(bin, "docker"), args...)
		cmd.Env = []string{"PATH=" + bin + ":" + realBin + ":/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir}
		cmd.Dir = dir
		o, _ := cmd.CombinedOutput()
		a, _ := os.ReadFile(asked)
		d, _ := os.ReadFile(direct)
		return strings.TrimSpace(string(a)), strings.TrimSpace(string(d)), string(o)
	}
	for _, args := range [][]string{
		{"compose", "exec", "-T", "web", "df", "-h"},
		{"compose", "run", "--rm", "web", "drush", "--help"},
	} {
		door, real, out := run(args...)
		want := " " + strings.Join(args[1:], " ")
		if !strings.Contains(door, "--door compose ") || !strings.HasSuffix(door, want) || real != "" {
			t.Errorf("docker %s: the door was asked %q and the real docker %q, the command's own flag taken for compose's help:\n%s", strings.Join(args, " "), door, real, out)
		}
	}
	// The control: the same command without the flag reaches the door, and compose's own help,
	// before the service, the real docker.
	if door, real, out := run("compose", "exec", "-T", "web", "df"); !strings.HasSuffix(door, " exec -T web df") || real != "" {
		t.Fatalf("the control: a plain exec did not reach the door: %q, %q\n%s", door, real, out)
	}
	if door, real, out := run("compose", "exec", "--help"); door != "" || !strings.Contains(real, "compose exec --help") {
		t.Fatalf("the control: compose's own help did not go to the real docker: %q, %q\n%s", door, real, out)
	}
}
