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

	"github.com/hanoii/devopsy-cli/internal/remote"
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
	// sudo runs this binary by its own path, which add names in sudo's rules, never a devopsy
	// PATH finds first (fakeBin's, here).
	self, err := filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{"-n", "-u", me.Username, "-g", remote.DoorRunGroup, self, doorFlag, "request", filepath.Join(home, ".docker", "config.json"), "ps", "-a"}, "\n") + "\n"
	if got := readString(log); code != 0 || got != want || strings.Contains(out, "[ps]") {
		t.Errorf("with no socket (%d), sudo was asked:\n%s\nwant:\n%s\nand docker said:\n%s", code, got, want, out)
	}
}

// A project user's docker compose hands the door only the verb and what follows it, so a
// global flag of compose's that changes what the command does (--dry-run, which would take a
// project down for real, or one compose adds later) is refused, never dropped; one that
// changes what renders goes to the render, its = form too.
func TestDoorClientDropsNoGlobalFlag(t *testing.T) {
	t.Parallel()
	if syscall.Access(dockerSocket, 2) == nil {
		t.Skip("this user reaches Docker's socket: its docker is no door")
	}
	dir := t.TempDir()
	bin, realBin, asked := filepath.Join(dir, "bin"), filepath.Join(dir, "real"), filepath.Join(dir, "asked")
	write(t, filepath.Join(realBin, "docker"), "#!/bin/sh\necho '{\"name\":\"shop-prod\",\"services\":{}}'\n", 0o755)
	write(t, filepath.Join(bin, "sudo"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >>"+asked+"\n", 0o755)
	if err := os.Symlink(binary, filepath.Join(bin, "docker")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"compose", "--dry-run", "-p", "shop-prod", "down", "-v"},
		{"compose", "--dry-run", "up", "-d"},
		{"compose", "--dry-run=true", "up", "-d"},
		{"compose", "--no-such-flag", "up", "-d"},
	} {
		os.Remove(asked)
		cmd := exec.Command(filepath.Join(bin, "docker"), args...)
		cmd.Env = []string{"PATH=" + bin + ":" + realBin + ":/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir}
		cmd.Dir = dir
		out, _ := cmd.CombinedOutput()
		got, _ := os.ReadFile(asked)
		if strings.Contains(string(got), "--door") || !strings.Contains(string(out), "not taken for a project user") {
			t.Errorf("docker %s asks the door for the real thing:\n%s%s", strings.Join(args, " "), got, out)
		}
	}
	// The control: without the flag, the same command reaches the door.
	os.Remove(asked)
	cmd := exec.Command(filepath.Join(bin, "docker"), "compose", "-p", "shop-prod", "down", "-v")
	cmd.Env = []string{"PATH=" + bin + ":" + realBin + ":/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir}
	cmd.Dir = dir
	out, _ := cmd.CombinedOutput()
	if got, _ := os.ReadFile(asked); !strings.Contains(string(got), "--door down shop-prod -v") {
		t.Fatalf("the control: a down by name did not reach the door:\n%s%s", got, out)
	}
	os.Remove(asked)
	cmd = exec.Command(filepath.Join(bin, "docker"), "compose", "--project-name=shop-prod", "--compatibility", "ps")
	cmd.Env = []string{"PATH=" + bin + ":" + realBin + ":/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir}
	cmd.Dir = dir
	out, _ = cmd.CombinedOutput()
	if got, _ := os.ReadFile(asked); !strings.Contains(string(got), "--door compose ") || !strings.HasSuffix(strings.TrimSpace(string(got)), " ps") {
		t.Errorf("a render's flags, one in its = form, did not reach the door:\n%s%s", got, out)
	}
}

// A render that a sudo which ran and refused left goes at the next render, an hour old: it
// holds the project's variables. A newer one, another's or another file stays.
func TestSweepRenders(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	now := time.Now()
	for name, age := range map[string]time.Duration{"devopsy-door-old": 2 * time.Hour, "devopsy-door-new": time.Minute, "other-old": 2 * time.Hour} {
		write(t, filepath.Join(dir, name), "{}\n", 0o600)
		if err := os.Chtimes(filepath.Join(dir, name), now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	sweepRenders(dir, now)
	for name, kept := range map[string]bool{"devopsy-door-old": false, "devopsy-door-new": true, "other-old": true} {
		if fileExists(filepath.Join(dir, name)) != kept {
			t.Errorf("%s: kept %v", name, !kept)
		}
	}
}

// A project user's compose asks for compose's own help only by -h or --help among compose's
// flags, the verb's (here) or the global ones before it (dockerCompose's loop): past exec's or
// run's service they are the service's command's (`exec web df -h`), which goes to the door
// like any command.
func TestComposeHelpIsComposesOwn(t *testing.T) {
	t.Parallel()
	for args, want := range map[string]bool{
		"up --help": true, "up -d web -h": true, "exec --help": true, "exec -T -h web sh": true,
		"exec -T web df -h": false, "run --rm web drush --help": false, "exec -u 10001 web ls --help": false,
		"exec -e A=-h web true": false, "logs -f": false, "": false,
	} {
		if got := composeHelp(strings.Fields(args)); got != want {
			t.Errorf("%q: %v, want %v", args, got, want)
		}
	}
}

// A -h or --help past exec's or run's service is that command's (`exec web df -h`, `run web
// drush --help`): the client sends it to the door like any command, never to the real docker,
// which has no socket for a project user.
func TestDoorClientKeepsACommandsOwnHelpFlag(t *testing.T) {
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

// A project user's `docker compose --help` (or -h) before the verb is compose's own help,
// which no daemon is behind: it goes to the real docker, as a verb's own help does.
func TestDoorClientGivesComposesOwnHelp(t *testing.T) {
	t.Parallel()
	if syscall.Access(dockerSocket, 2) == nil {
		t.Skip("this user reaches Docker's socket: its docker is no door")
	}
	dir := t.TempDir()
	bin, realBin := filepath.Join(dir, "bin"), filepath.Join(dir, "real")
	direct := filepath.Join(dir, "direct")
	write(t, filepath.Join(realBin, "docker"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >>"+direct+"\n", 0o755)
	write(t, filepath.Join(bin, "sudo"), "#!/bin/sh\nexit 1\n", 0o755)
	if err := os.Symlink(binary, filepath.Join(bin, "docker")); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, string) {
		os.Remove(direct)
		cmd := exec.Command(filepath.Join(bin, "docker"), args...)
		cmd.Env = []string{"PATH=" + bin + ":" + realBin + ":/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir}
		cmd.Dir = dir
		out, _ := cmd.CombinedOutput()
		return strings.TrimSpace(readString(direct)), string(out)
	}
	// The control: a verb's help goes to the real docker.
	if real, out := run("compose", "up", "--help"); real != "compose up --help" {
		t.Fatalf("the control: a verb's help did not go to the real docker: %q\n%s", real, out)
	}
	for _, flag := range []string{"--help", "-h"} {
		if real, out := run("compose", flag); real != "compose "+flag {
			t.Errorf("docker compose %s: the real docker was asked %q:\n%s", flag, real, out)
		}
	}
}
