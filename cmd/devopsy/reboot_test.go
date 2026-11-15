package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// rebootServer is a server for --reboot: the deploy user's home (its release root) with one
// environment, shop/prod, a host root with systemd and a boot id, and a docker that answers as
// this host's, logs each call, and runs the reboot as the state files say (state/<name>):
// shutdown kills the act as a shutdown would, after giving the host a new boot id; runfail
// fails the root run; rootless answers as a rootless daemon.
type rebootServer struct {
	t                                   *testing.T
	home, host, state, log, bin, config string
	env                                 []string
}

func newRebootServer(t *testing.T) *rebootServer {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &rebootServer{t: t, home: filepath.Join(tmp, "home"), host: filepath.Join(tmp, "host"), state: filepath.Join(tmp, "state"), log: filepath.Join(tmp, "docker.log"), config: filepath.Join(tmp, "config")}
	for _, d := range []string{s.state, s.config, filepath.Join(s.host, "run", "systemd", "system"), filepath.Join(s.home, "shop", "prod", "releases")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(s.host, "proc", "sys", "kernel", "random", "boot_id"), "boot-1\n", 0o644)
	bin := t.TempDir()
	s.bin = bin
	write(t, filepath.Join(bin, "fakessh"), "#!/bin/sh\nfor a; do s=$a; done\nexec sh -c \"$s\"\n", 0o755)
	write(t, filepath.Join(bin, "docker"), `#!/bin/sh
echo "$*" >>`+s.log+`
case "$*" in
  version) ;;
  'info --format {{json .}}')
    sec='"name=seccomp,profile=builtin"'; [ -e `+s.state+`/rootless ] && sec="$sec"',"name=rootless"'
    echo '{"Name":"'"$(hostname)"'","SecurityOptions":['"$sec"']}' ;;
  'ps -q') echo c1; echo c2 ;;
  'image inspect -f {{.Id}} {{.Created}} '*) echo "sha256:u $(date -u +%Y-%m-%dT%H:%M:%SZ)" ;;
  'run --rm --network none --pid host -v /:/host:ro --pull never '*)
    [ -e `+s.state+`/runfail ] && { echo "Failed to reboot: Access denied" >&2; exit 1; }
    if [ -e `+s.state+`/shutdown ]; then echo boot-2 >`+s.host+`/proc/sys/kernel/random/boot_id; kill -TERM $PPID; fi ;;
  *) echo "unexpected: $*" >&2; exit 9 ;;
esac
`, 0o755)
	s.env = []string{"DEVOPSY_SSH_COMMAND=" + filepath.Join(bin, "fakessh"), "PATH=" + bin + ":" + fakeBin(t) + ":/usr/bin:/bin", "HOME=" + s.home, remote.HostRootEnv + "=" + s.host, "DEVOPSY_HOME=" + s.config}
	return s
}

// set raises the state file name.
func (s *rebootServer) set(name string) { write(s.t, filepath.Join(s.state, name), "", 0o644) }

// cmd is devopsy with args, as the server or the operator runs it.
func (s *rebootServer) cmd(args ...string) *exec.Cmd {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	s.t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.WaitDelay = time.Second
	cmd.Dir = s.home
	cmd.Env = s.env
	return cmd
}

// run is --reboot with args from the operator, its output and exit.
func (s *rebootServer) run(args ...string) (string, int) {
	s.t.Helper()
	out, err := s.cmd(append([]string{"@devopsy@server:", "--reboot"}, args...)...).CombinedOutput()
	return string(out), exitCode(err)
}

// step is the hidden step with args, as a session runs it: its stdout, stderr and exit.
func (s *rebootServer) step(args ...string) (string, string, int) {
	s.t.Helper()
	cmd := s.cmd(append([]string{remote.RebootStepFlag}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), exitCode(err)
}

// rebooted says whether the root run went, and with what.
func (s *rebootServer) rebooted() string {
	b, _ := os.ReadFile(s.log)
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "run ") {
			return l
		}
	}
	return ""
}

// hold takes path's lock, as a release holds its environment's, until the test ends.
func (s *rebootServer) hold(path string) {
	s.t.Helper()
	l, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { l.Close() })
	if ok, err := tryFlock(l, syscall.LOCK_EX); !ok || err != nil {
		s.t.Fatal("cannot take", path, err)
	}
}

// TestRebootLook keeps the look's facts what the prompt says (the hostname, the boot id, the
// running containers, the reboot flag and its packages once each), the look creating no lock
// file, and the prompt refused without a terminal before any act, naming --yes.
func TestRebootLook(t *testing.T) {
	t.Parallel()
	s := newRebootServer(t)
	write(t, filepath.Join(s.host, remote.RebootRequired), "", 0o644)
	write(t, filepath.Join(s.host, remote.RebootRequired+".pkgs"), "linux-image-amd64\ndevopsy\nlinux-image-amd64\n", 0o644)
	out, errs, code := s.step("look")
	host, _ := os.Hostname()
	if code != 0 || strings.TrimSpace(out) != `{"hostname":"`+host+`","boot_id":"boot-1","running":2,"reboot_required":true,"packages":["linux-image-amd64","devopsy"]}` {
		t.Fatalf("look (%d): %s%s", code, out, errs)
	}
	if fileExists(filepath.Join(s.home, "shop", "prod", ".lock")) || fileExists(setup.LockPath(setup.RecordPath(filepath.Join(s.config, remote.ConfigFile)))) {
		t.Error("the look created a lock file")
	}
	said, code := s.run()
	if code != 1 || !strings.Contains(said, "devopsy: error: nothing rebooted: no terminal to confirm it on\n") ||
		!strings.Contains(said, "devopsy: | this reboots "+host+" (devopsy@server): its 2 running containers stop") || !strings.Contains(said, "add --yes to go on without asking") || s.rebooted() != "" {
		t.Errorf("no terminal (%d):\n%s", code, said)
	}
}

// TestRebootRefuses keeps every refusal before anything, in the look and in the act each: an
// environment's lock held (a release), the setup's lock held (an apply), a Docker that is not
// this host's, a project user; nothing rebooted, no lock left held.
func TestRebootRefuses(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, says string
		set        func(s *rebootServer)
		// script: refused by the session's script, before the step runs.
		script bool
	}{
		{"release", "devopsy: error: a release or another change runs in ", func(s *rebootServer) { s.hold(filepath.Join(s.home, "shop", "prod", ".lock")) }, false},
		{"setup", "devopsy: error: a setup of this server runs: nothing rebooted", func(s *rebootServer) { s.hold(filepath.Join(s.config, setup.LockFile)) }, false},
		{"project user", "is a project user, and a reboot is the server's, its deploy user's", func(s *rebootServer) {
			write(t, filepath.Join(s.bin, "id"), "#!/bin/sh\necho "+remote.DoorGroup+"\n", 0o755)
		}, true},
		{"rootless", "devopsy: error: Docker: the Docker devopsy reaches is not this host's docker.service (rootless, or another host's: ", func(s *rebootServer) { s.set("rootless") }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newRebootServer(t)
			c.set(s)
			for _, step := range [][]string{{"look"}, {"act", "1"}} {
				if c.script {
					break
				}
				out, errs, code := s.step(step...)
				if code != 1 || out != "" || !strings.Contains(errs, c.says) {
					t.Errorf("%s (%d): %q\n%s", step[0], code, out, errs)
				}
			}
			if said, code := s.run(); c.script && (code != 1 || !strings.Contains(said, c.says)) {
				t.Errorf("look (%d):\n%s", code, said)
			}
			if said, code := s.run("--yes"); code != 1 || !strings.Contains(said, c.says) || s.rebooted() != "" {
				t.Errorf("--yes (%d):\n%s", code, said)
			}
		})
	}
}

// TestReboot keeps the act's order and how --reboot ends by its lines: the image readied before
// the locks, boot, the root run through systemd's chroot guard, asked, then the shutdown cutting
// the session and the wait ending on the new boot (exit 0); a root run failing says failed, its
// error, and no wait; a server that does not go down is said so once the act's bound ends.
func TestReboot(t *testing.T) {
	t.Parallel()
	host, _ := os.Hostname()
	t.Run("back", func(t *testing.T) {
		t.Parallel()
		s := newRebootServer(t)
		s.set("shutdown")
		said, code := s.run("--yes")
		run := s.rebooted()
		if code != 0 || !strings.Contains(said, "devopsy: "+host+" reboots\n") || !strings.HasSuffix(run, "chroot /host /bin/sh -c SYSTEMD_IGNORE_CHROOT=1 exec systemctl reboot") {
			t.Errorf("back (%d): %s\n%s", code, run, said)
		}
		b, _ := os.ReadFile(s.log)
		if calls := string(b); strings.Index(calls, "image inspect") > strings.Index(calls, "run ") {
			t.Errorf("the image readied after the run:\n%s", calls)
		}
	})
	t.Run("run fails", func(t *testing.T) {
		t.Parallel()
		s := newRebootServer(t)
		s.set("runfail")
		out, errs, code := s.step("act", "1")
		if code != 1 || out != "boot boot-1 "+host+"\nfailed\n" || !strings.Contains(errs, "devopsy: error: the reboot was not asked: systemctl reboot, as root") {
			t.Errorf("act (%d): %q\n%s", code, out, errs)
		}
		if said, code := s.run("--yes"); code != 1 || strings.Contains(said, "reboots\n") || strings.Contains(said, "waiting for") {
			t.Errorf("--yes (%d):\n%s", code, said)
		}
	})
	t.Run("not down", func(t *testing.T) {
		t.Parallel()
		s := newRebootServer(t)
		out, errs, code := s.step("act", "1")
		if code != 1 || out != "boot boot-1 "+host+"\nasked\nfailed\n" || !strings.Contains(errs, "devopsy: error: the reboot was asked, and the server did not go down: the server to go down did not end in a second") {
			t.Errorf("act (%d): %q\n%s", code, out, errs)
		}
	})
}

// TestReadRebootAct keeps how the act's lines read: a line the reboot cut is ignored.
func TestReadRebootAct(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		out  string
		want rebootEnd
	}{
		{"boot b1 vm1\nasked\n", rebootEnd{bootID: "b1", hostname: "vm1", asked: true}},
		{"boot b1 vm1\nask", rebootEnd{bootID: "b1", hostname: "vm1"}},
		{"boot b1 vm1\nfailed\n", rebootEnd{bootID: "b1", hostname: "vm1", failed: true}},
		{"boo", rebootEnd{}},
	} {
		if got := readRebootAct([]byte(c.out)); got != c.want {
			t.Errorf("%q: %+v", c.out, got)
		}
	}
}

// TestAwaitBoot keeps the wait's end: a new boot ends it, a try with no answer never does,
// and at the bound the last try decides the words, the old boot once then silence being a
// server that went down and did not come back.
func TestAwaitBoot(t *testing.T) {
	t.Parallel()
	end := rebootEnd{bootID: "b1", hostname: "vm1"}
	tries := func(answers ...string) func(context.Context) (string, bool) {
		i := 0
		return func(context.Context) (string, bool) {
			a := answers[min(i, len(answers)-1)]
			i++
			return a, a != ""
		}
	}
	for _, c := range []struct {
		name    string
		answers []string
		want    string
	}{
		{"back", []string{"b1", "", "", "b2"}, ""},
		{"same boot", []string{"b1"}, "vm1 still runs since the same boot after a second: it did not reboot"},
		{"no answer", []string{"b1", ""}, "vm1 did not answer in a second since its reboot"},
	} {
		var buf bytes.Buffer
		err := awaitBoot(context.Background(), report.New(&buf, false, 0), end, time.Second, time.Millisecond, tries(c.answers...))
		if got := ""; err != nil {
			got = err.Error()
			if got != c.want {
				t.Errorf("%s: %q", c.name, got)
			}
		} else if c.want != "" || !strings.Contains(buf.String(), "devopsy: done waiting for vm1 to come back") {
			t.Errorf("%s: no error\n%s", c.name, buf.String())
		}
	}
}
