package main

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A read as a project user (--door-read) runs in a session of its own: in the caller's, its
// process, which that project user can trace, would have the caller's terminal as its
// controlling one, an admin's under --server-info, to read and to type into.
func TestDoorReadRunsInASessionOfItsOwn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	fake := filepath.Join(dir, "sudo")
	write(t, fake, "#!/bin/sh\necho $$ >"+pidFile+".tmp && mv "+pidFile+".tmp "+pidFile+"\nexec sleep 30\n", 0o755)
	c := doorRead("devopsy-shop", "environments", "/srv/shop")
	// The fake in sudo's place: where no sudo is on PATH, exec.Command recorded its lookup.
	c.Path, c.Args[0], c.Err = fake, fake, nil
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Process.Kill(); _ = c.Wait() }()
	var pid int
	for deadline := time.Now().Add(startWait); pid == 0 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
	}
	if pid == 0 {
		t.Fatal("the read never started")
	}
	mine, err := unix.Getsid(0)
	if err != nil {
		t.Fatal(err)
	}
	if sid, err := unix.Getsid(pid); err != nil || sid == mine {
		t.Errorf("a read as a project user runs in the caller's session %d (%d, %v)", mine, sid, err)
	}
}

// --door, --door-read and --authorized-keys are answered before anything loads a project or
// reads the environment: every word there is a project user's, or anyone's for sshd, on the
// deploy user's rights. In a directory whose project does not load, each still answers; and
// the door reads who asks from SUDO_USER and what it may from the deploy user's own config,
// never one the caller's environment names (DEVOPSY_HOME), which would grant itself.
func TestDoorIsAnsweredBeforeAnyProject(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	write(t, filepath.Join(project, ".devopsy", "compose.yaml"), "services: {}\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", "config.yaml"), "project: [not, a, name\n", 0o644)
	if out, code := runDevopsy(t, project, nil, "--env"); code == 0 || !strings.Contains(out, "config.yaml") {
		t.Fatalf("the control: the project loads (%d):\n%s", code, out)
	}
	// A config of the caller's that names it a project user, which the server's devopsy
	// reads as such (the control), with keys of its own beside it, and a sync's key, which
	// sshd is given after them.
	const who = "devopsy-review4cgrant"
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g op@laptop"
	home := t.TempDir()
	write(t, filepath.Join(home, "config.yaml"), "door:\n  space: "+t.TempDir()+"\n  users:\n    "+who+": {project: review4cgrant}\n", 0o644)
	write(t, filepath.Join(home, "door", "keys", who), "# a comment\n"+key+"\n", 0o600)
	syncKey := `restrict,command="/usr/local/bin/devopsy --sync-child 1762600000.0a1b2c /srv/x vm2:x" ` + strings.TrimSuffix(key, "op@laptop") + "devopsy-sync-1762600000.0a1b2c"
	write(t, filepath.Join(home, "door", "sync-keys", who), syncKey+"\n", 0o600)
	caller := []string{"DEVOPSY_HOME=" + home, "XDG_CONFIG_HOME=" + home, "SUDO_USER=" + who}
	if out, code := runDevopsy(t, t.TempDir(), caller, "--project-users-step", "list"); code != 0 || !strings.Contains(out, who+"\tproject review4cgrant") {
		t.Fatalf("the control: that config names %s a project user (%d):\n%s", who, code, out)
	}
	out, code := runDevopsy(t, project, caller, doorFlag, "request", "/nonexistent", "ps")
	if code != doorNotHere || !strings.Contains(out, "refused: "+who+" is not a project user here") {
		t.Errorf("the door, asked with a config of the caller's (%d):\n%s", code, out)
	}
	if out, code := runDevopsy(t, project, caller, doorReadFlag, "isdir", project); code != 0 {
		t.Errorf("a read as a project user, in a project that does not load (%d):\n%s", code, out)
	}
	if out, code := runDevopsy(t, project, caller, authorizedKeysFlag, who); code != 0 || out != key+"\n"+syncKey+"\n" {
		t.Errorf("sshd's question, in a project that does not load (%d):\n%s", code, out)
	}
}

// Who the socket lets in has no door: found as `docker`, devopsy hands the command to the
// real one as it is, for the deploy user and for a project an admin put in the docker group.
// Anyone else's goes to the door through sudo, as the binary's owner, with nothing run here.
func TestDockerHasNoDoorForWhoReachesTheSocket(t *testing.T) {
	t.Parallel()
	link, log := t.TempDir(), filepath.Join(t.TempDir(), "sudo.log")
	if err := os.Symlink(binary, filepath.Join(link, "docker")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(link, "sudo"), "#!/bin/sh\nprintf '%s\\n' \"$@\" >"+log+"\n", 0o755)
	home := t.TempDir()
	run := func(env ...string) (string, int) {
		t.Helper()
		c := exec.Command(filepath.Join(link, "docker"), "ps", "-a")
		c.Env = append([]string{"PATH=" + link + ":" + fakeBin(t) + ":/usr/bin:/bin", "HOME=" + home}, env...)
		out, err := c.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return string(out), code
	}
	if out, code := run("DOCKER_HOST=unix:///run/user/1000/docker.sock"); code != 0 || !strings.HasPrefix(out, "[ps][-a]\n") || fileExists(log) {
		t.Errorf("with a Docker of its own (%d), sudo asked %v:\n%s", code, fileExists(log), out)
	}
	if syscall.Access(dockerSocket, 2) == nil {
		t.Skip("this user reaches " + dockerSocket + ": it has no door here")
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	out, code := run()
	want := strings.Join([]string{"-n", "-u", me.Username, filepath.Join(fakeBin(t), "devopsy"), doorFlag, "request", filepath.Join(home, ".docker", "config.json"), "ps", "-a"}, "\n") + "\n"
	if got := readString(log); code != 0 || got != want || strings.Contains(out, "[ps]") {
		t.Errorf("with no socket (%d), sudo was asked:\n%s\nwant:\n%s\nand docker said:\n%s", code, got, want, out)
	}
}
