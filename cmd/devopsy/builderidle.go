package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/pathlock"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// builderIdleFlag is the hidden stopper of a project user's builder (runBuilderIdle), started
// detached by whoever takes the builder's lock shared (a build, --gc): it stops the builder
// once nothing holds that lock, so an idle builder holds no memory.
const builderIdleFlag = "--builder-idle"

// builderTiming is how long the builders' waits are: a build's or --gc's wait on the lock
// (lock), the stopper's answer (answer), its tries (every), its whole life (bound), a stop
// (stop). Tests shorten them; a server takes builderTimes.
type builderTiming struct {
	lock, answer, every, bound, stop time.Duration
}

var builderTimes = builderTiming{lock: time.Minute, answer: 5 * time.Second, every: time.Second, bound: 24 * time.Hour, stop: 30 * time.Second}

// builderEnv is the environment of a docker command on the project users' builders: base and
// the door's BUILDX_CONFIG.
func builderEnv(base []string, space string) []string {
	return append(append([]string(nil), base...), "BUILDX_CONFIG="+door.BuildxConfigOf(space))
}

// madeRight says whether a builder, as inspect printed it (inspected) and its container's
// restart policy (policy, "" with no container: never booted), is one devopsy makes: on its
// own network, and restarted by nothing, so a reboot or a restart of Docker mid-build leaves it
// stopped. A builder with no container is judged by its network alone: a claim never boots
// it, and would otherwise remake it at every claim.
func madeRight(inspected, network, policy string, container bool) bool {
	if !strings.Contains(inspected, `network="`+network+`"`) {
		return false
	}
	return !container || policy == "no" || policy == ""
}

// builderLockWait takes the builder's lock at path (how, shared or exclusive) waiting for it
// as wait waits, said as what and bounded by within; done, when set, ends the wait with no lock
// (nil, nil) once it says so, tried before each try of the lock.
func builderLockWait(path, what string, how int, within, every time.Duration, done func() bool) (*pathlock.Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	ended := false
	l, err := pathlock.Take(path, what, how, func(what string, try func() (bool, error)) error {
		return (boundedWait{what: what, bound: within, every: every}).wait(context.Background(), report.Default(), func() (bool, error) {
			if done != nil && done() {
				ended = true
				return true, nil
			}
			return try()
		})
	})
	if err != nil {
		return nil, err
	}
	if ended {
		l.Close()
		return nil, nil
	}
	return l, nil
}

// useBuilder takes project's builder's lock shared, as a build or --gc uses the builder, and
// starts its stopper (startStopper), which stops the builder once every user lets the lock go.
// A stopper that does not answer refuses the use, before anything runs on the builder: the
// builder would stay up.
func useBuilder(space, project, self string, env []string, t builderTiming) (*pathlock.Lock, error) {
	l, err := builderLockWait(door.BuilderLockOf(space, project), project+"'s builder being made or stopped", syscall.LOCK_SH, t.lock, 0, nil)
	if err != nil {
		return nil, err
	}
	// The stopper's own lock is the starter's to make, so that the stopper makes none.
	idle, err := os.OpenFile(door.BuilderIdleLockOf(space, project), os.O_CREATE|os.O_RDONLY|syscall.O_CLOEXEC, 0o600)
	if err == nil {
		idle.Close()
		_, err = startStopper(self, project, env, t)
	}
	if err != nil {
		l.Close()
		return nil, fmt.Errorf("%s's builder's stopper: %w", project, err)
	}
	return l, nil
}

// Its answer: it holds the stopper's lock and stays (stopperHolds), or another stopper does
// (stopperDeclined), and it ends.
const (
	stopperHolds    = 'h'
	stopperDeclined = 'd'
)

// startStopper starts self's builderIdleFlag for project, detached in a session of its own,
// stdin, stdout and stderr /dev/null, in /, with env, its answer on a pipe (descriptor 3),
// read within t.answer: none, it did not start or a gate refused it. One that declined is
// reaped; one that holds goes on past its starter's exec. It says whether it holds.
func startStopper(self, project string, env []string, t builderTiming) (bool, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return false, err
	}
	defer r.Close()
	c := exec.Command(self, builderIdleFlag, project)
	c.Env, c.Dir, c.ExtraFiles = env, "/", []*os.File{w}
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = c.Start()
	w.Close()
	if err != nil {
		return false, err
	}
	answer := make([]byte, 1)
	read := func(within time.Duration) (bool, error) {
		if err := r.SetReadDeadline(time.Now().Add(within)); err != nil {
			return false, err
		}
		n, err := r.Read(answer)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return false, nil
		}
		if n == 1 {
			return true, nil
		}
		return false, errors.New("it ended with no answer")
	}
	// A grace before the wait says anything: it answers within it, every time but a slow one.
	grace := min(t.answer, time.Second)
	got, err := read(grace)
	if err == nil && !got {
		err = (boundedWait{what: project + "'s builder's stopper to start", bound: t.answer - grace}).wait(context.Background(), report.Default(), func() (bool, error) {
			return read(100 * time.Millisecond)
		})
	}
	if err != nil {
		_ = c.Process.Kill()
		_ = c.Wait()
		return false, err
	}
	if answer[0] == stopperDeclined {
		// It ends right after its answer: reaped, so a build's compose keeps no zombie of it.
		done := make(chan struct{})
		go func() { _ = c.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		return false, nil
	}
	return true, c.Process.Release()
}

// runBuilderIdle implements builderIdleFlag <project>: the stopper of a project user's
// builder, the deploy user's (deployUserSteps). It closes what it inherited but its answer's
// descriptor (3), takes the project's stopper lock, answering whether it holds it or another
// stopper does, and, holding it, stops the builder once nothing holds the builder's lock
// (stopWhenIdle).
func runBuilderIdle(args []string) int {
	closeInherited(3)
	answer := os.NewFile(3, "answer")
	if len(args) != 1 || !remote.ValidName(args[0]) {
		return 2
	}
	project := args[0]
	d, err := remote.ServerDoor()
	if err != nil || d == nil || d.Space == "" {
		return 1
	}
	if u, ok := d.Users[door.UserOf(project)]; !ok || u.Project != project {
		return 1
	}
	lock, err := pathlock.Open(door.BuilderLockOf(d.Space, project))
	if err != nil {
		return 1
	}
	defer lock.Close()
	idle, err := pathlock.Open(door.BuilderIdleLockOf(d.Space, project))
	if err != nil {
		return 1
	}
	defer idle.Close()
	held, err := idle.Try(syscall.LOCK_EX)
	if err != nil {
		return 1
	}
	say := byte(stopperDeclined)
	if held {
		say = stopperHolds
	}
	if answer != nil {
		_, _ = answer.Write([]byte{say})
		answer.Close()
	}
	if !held {
		return 0
	}
	env := builderEnv(os.Environ(), d.Space)
	stopWhenIdle(lock, idle, func() error { return stopBuilder(realDocker(), env, project, builderTimes.stop) }, builderTimes)
	return 0
}

// stopWhenIdle tries lock, the builder's, exclusive every t.every until it takes it, every
// user of the builder gone, then stops the builder (stop) and lets idle go, then lock: let go
// in that order, a build waiting on the stop takes the stopper's lock for its own stopper. A
// stop that fails keeps idle, lets lock go and tries again, backing off up to a minute, so a
// stop that fails at once (Docker down) does not run every second. It gives up past t.bound,
// and at once when lock's file is no longer the one it opened (a destroy).
func stopWhenIdle(lock, idle *pathlock.Lock, stop func() error, t builderTiming) {
	deadline := time.Now().Add(t.bound)
	wait := t.every
	for time.Now().Before(deadline) {
		taken, err := lock.Try(syscall.LOCK_EX)
		if err != nil {
			return
		}
		if !taken {
			time.Sleep(t.every)
			continue
		}
		if stop() == nil {
			idle.Unlock()
			lock.Unlock()
			return
		}
		lock.Unlock()
		time.Sleep(wait)
		wait = min(2*wait, time.Minute)
	}
}

// stopBuilder stops project's builder with docker, under env (the door's BUILDX_CONFIG), its
// caller holding the builder's lock exclusive: nothing when it is gone (an inspect, never
// buildx's words) or stopped already, a stop bounded by within, its process group killed past
// it. The one stop of the stopper and of --gc.
func stopBuilder(docker string, env []string, project string, within time.Duration) error {
	name := door.BuilderOf(project)
	run := func(args ...string) ([]byte, error) {
		c := exec.Command(docker, args...)
		c.Env = env
		return outputWithin(c, within)
	}
	out, err := run("buildx", "inspect", name)
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		if v, ok := strings.CutPrefix(line, "Status:"); ok && strings.TrimSpace(v) != "running" {
			return nil
		}
	}
	_, err = run("buildx", "stop", name)
	return err
}

// outputWithin runs c for what it prints on stdout, as Output does, its stderr in the
// error's (exec.ExitError's Stderr), in a process group of its own, killed with every child
// once within has passed: the one bound of the builders' docker calls.
func outputWithin(c *exec.Cmd, within time.Duration) ([]byte, error) {
	var out, said bytes.Buffer
	c.Stdout, c.Stderr = &out, &said
	ownGroup(c)
	if err := c.Start(); err != nil {
		return nil, err
	}
	defer killGroupAfter(c, within)()
	err := c.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		ee.Stderr = said.Bytes()
	}
	return out.Bytes(), err
}

// closeInherited closes every descriptor the process inherited open across its exec, from 3
// up, but keep: those /dev/fd lists without close-on-exec, which Go's own never lack.
func closeInherited(keep int) {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return
	}
	for _, e := range entries {
		fd, err := strconv.Atoi(e.Name())
		if err != nil || fd < 3 || fd == keep {
			continue
		}
		if flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err == nil && flags&unix.FD_CLOEXEC == 0 {
			unix.Close(fd)
		}
	}
}
