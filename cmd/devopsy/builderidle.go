package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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

// orServer is t, a test's own, or builderTimes when t is unset.
func (t builderTiming) orServer() builderTiming {
	if t == (builderTiming{}) {
		return builderTimes
	}
	return t
}

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

// Its answer: it holds the stopper's lock and stays (stopperHolds), another stopper does
// (stopperDeclined), and it ends, or it cannot (stopperFailed, why after it), and it ends.
const (
	stopperHolds    = 'h'
	stopperDeclined = 'd'
	stopperFailed   = 'e'
)

// answerFdFlag tells the stopper the descriptor its answer goes to, its starter's pipe: run by
// hand it has none, and answers nothing.
const answerFdFlag = "--answer-fd"

// errNoAnswer is a stopper's end with no answer: it did not start, or a gate refused it; errLate
// one that has not answered in time, which may still hold the stopper's lock.
var (
	errNoAnswer = errors.New("it ended with no answer")
	errLate     = errors.New("it did not answer in time")
)

// detach starts c in a session of its own, for a process that outlives its starter (a
// builder's stopper, --upgrade-check).
func detach(c *exec.Cmd) { c.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

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
	c := exec.Command(self, builderIdleFlag, project, answerFdFlag, "3")
	c.Env, c.Dir, c.ExtraFiles = env, "/", []*os.File{w}
	detach(c)
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
		if n == 1 && answer[0] == stopperFailed {
			// Why it ends, the rest of what it wrote, bounded: its stderr is /dev/null.
			r.SetReadDeadline(time.Now().Add(time.Second))
			why, _ := io.ReadAll(io.LimitReader(r, 512))
			return false, fmt.Errorf("%w: %s", errNoAnswer, strings.TrimSpace(string(why)))
		}
		if n == 1 {
			return true, nil
		}
		return false, errNoAnswer
	}
	// A grace before the wait says anything: it answers within it, every time but a slow one.
	grace := min(t.answer, time.Second)
	got, err := read(grace)
	if err == nil && !got && t.answer <= grace {
		err = errLate
	} else if err == nil && !got {
		err = (boundedWait{what: project + "'s builder's stopper to start", bound: t.answer - grace}).wait(context.Background(), report.Default(), func() (bool, error) {
			return read(100 * time.Millisecond)
		})
		if errors.As(err, new(waitEnded)) {
			err = errLate
		}
	}
	if err != nil {
		// One that ended is reaped; one late is let go, never killed: it may hold the stopper's
		// lock by now, the one a declined stopper of another use counts on.
		if !errors.Is(err, errNoAnswer) {
			return false, errors.Join(err, c.Process.Release())
		}
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
// descriptor (3, when it inherited it: run by hand it has none, and 3 is then the first file it
// opens), takes the project's stopper lock, answering whether it holds it or another stopper
// does, and, holding it, stops the builder once nothing holds the builder's lock
// (stopWhenIdle).
func runBuilderIdle(args []string) int {
	var answer *os.File
	keep := -1
	if len(args) == 3 && args[1] == answerFdFlag {
		if fd, err := strconv.Atoi(args[2]); err == nil && fd > 2 {
			answer, keep = os.NewFile(uintptr(fd), "answer"), fd
		}
		args = args[:1]
	}
	closeInherited(keep)
	fail := func(why string) int {
		if answer != nil {
			_, _ = answer.Write(append([]byte{stopperFailed}, why...))
			answer.Close()
		}
		return 1
	}
	if len(args) != 1 || !remote.ValidName(args[0]) {
		return 2
	}
	project := args[0]
	d, err := remote.ServerDoor()
	if err != nil || d == nil || d.Space == "" {
		return fail(fmt.Sprintf("the server's door does not read: %v", err))
	}
	if u, ok := d.Users[door.UserOf(project)]; !ok || u.Project != project {
		return fail(project + " is no project user's")
	}
	lock, err := pathlock.Open(door.BuilderLockOf(d.Space, project))
	if err != nil {
		return fail(err.Error())
	}
	defer lock.Close()
	idle, err := pathlock.Open(door.BuilderIdleLockOf(d.Space, project))
	if err != nil {
		return fail(err.Error())
	}
	defer idle.Close()
	held, err := idle.Try(syscall.LOCK_EX)
	if err != nil {
		return fail(err.Error())
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

// stopBuilder stops project's builder's container with docker, under env, its caller holding
// the builder's lock exclusive, the whole step bounded by within: nothing when no container of
// its name runs (docker ps, which answers that for a builder gone too, and fails only when
// Docker does), else docker stop, which stops a container left with no builder's record as
// much. Any failure, Docker's not answering included, is the step's. The one stop of the
// stopper and of --gc.
func stopBuilder(docker string, env []string, project string, within time.Duration) error {
	deadline := time.Now().Add(within)
	run := func(args ...string) ([]byte, error) {
		c := exec.Command(docker, args...)
		c.Env = env
		left := time.Until(deadline)
		if left <= 0 {
			return nil, errors.New("past its bound")
		}
		return outputWithin(c, left)
	}
	container := door.BuilderContainerOf(project)
	out, err := run("ps", "-q", "--filter", "name=^/"+container+"$", "--filter", "status=running")
	if err != nil {
		return dockerFailed("docker ps", err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil
	}
	if _, err := run("stop", container); err != nil {
		return dockerFailed("docker stop", err)
	}
	return nil
}

// builderNodes are the builders buildx lists (docker buildx ls --format json, out), each to its
// one node: a builder with several is none of devopsy's.
func builderNodes(out []byte) map[string]builderNode {
	nodes := map[string]builderNode{}
	for _, line := range strings.Split(string(out), "\n") {
		var b struct {
			Name  string
			Nodes []builderNode
		}
		if json.Unmarshal([]byte(line), &b) == nil && len(b.Nodes) == 1 {
			nodes[b.Name] = b.Nodes[0]
		}
	}
	return nodes
}

// builderRecorded says whether buildx has project's builder in space: buildx's own record of
// it (<BUILDX_CONFIG>/instances/<builder>, made by buildx create, removed by buildx rm), the one
// answer that tells a builder gone from a look that failed, which is an error; one project's
// alone, where buildx ls would load every project's builder, and wait on a builder that hangs.
func builderRecorded(space, project string) (bool, error) {
	_, err := os.Stat(filepath.Join(door.BuildxConfigOf(space), "instances", door.BuilderOf(project)))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// builderContainer is project's builder's container's restart policy, and whether it has one
// (made at its first boot), by run's docker: a builder with none was never booted.
func builderContainer(run func(args ...string) ([]byte, error), project string) (policy string, ok bool) {
	out, err := run("container", "inspect", "--format", "{{.HostConfig.RestartPolicy.Name}}", door.BuilderContainerOf(project))
	return strings.TrimSpace(string(out)), err == nil
}

// dropBuilder removes project's builder in space with run's docker: its record when buildx has
// it (builderRecorded), then its container and state volume by their names (removeLeftover). A
// look that fails is an error, never a builder gone.
func dropBuilder(run func(args ...string) ([]byte, error), space, project string) error {
	recorded, err := builderRecorded(space, project)
	if err != nil {
		return err
	}
	if recorded {
		if _, err := run("buildx", "rm", door.BuilderOf(project)); err != nil {
			return fmt.Errorf("its builder %s: %w", door.BuilderOf(project), dockerFailed("docker buildx rm", err))
		}
	}
	return removeLeftover(run, project)
}

// repairUnder repairs project's builder in space under its lock exclusive, its caller holding
// none of its locks: waited for, said and bounded, the wait ending with nothing done once fine
// says so (another use repaired it meanwhile), fine asked again under the lock, and fix run only
// when it still says no. No build boots or runs on the builder meanwhile: each holds the lock
// shared. The one way a builder is repaired (a remake, a leftover's removal, a container on a
// network gone).
func repairUnder(space, project string, t builderTiming, fine func() (bool, error), fix func() error) error {
	lock, err := builderLockWait(door.BuilderLockOf(space, project), "another use of "+project+"'s builder", syscall.LOCK_EX, t.lock, time.Second, func() bool {
		ok, err := fine()
		return err == nil && ok
	})
	if err != nil || lock == nil {
		return err
	}
	defer lock.Close()
	if ok, err := fine(); err != nil || ok {
		return err
	}
	return fix()
}

// builderNetworkID is the id of project's builder's network by run's docker, "" when it is gone,
// an error only when Docker fails (an exact docker network ls, never a failed inspect).
func builderNetworkID(run func(args ...string) ([]byte, error), project string) (string, error) {
	out, err := run("network", "ls", "-q", "--no-trunc", "--filter", "name=^"+door.BuilderNetworkOf(project)+"$")
	if err != nil {
		return "", dockerFailed("docker network ls", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// builderBootable makes project's builder in space bootable, with probe's docker for its looks
// and run's for its changes, its caller holding none of its locks: its network there, made
// again when gone (a prune takes it from a builder stopped between builds, no endpoint on it),
// and its container on that network. A stopped container keeps the id of the network it was
// made on, never the name alone, so one on a network made again cannot start: it is removed,
// its state volume, the cache, kept, for the next boot to make it on the network now
// (repairUnder).
func builderBootable(probe, run func(args ...string) ([]byte, error), space, project string, t builderTiming) error {
	network, container := door.BuilderNetworkOf(project), door.BuilderContainerOf(project)
	// stale says whether the container is on a network that is not the one now, with the
	// network's id ("" when gone).
	look := func() (stale bool, id string, err error) {
		if id, err = builderNetworkID(probe, project); err != nil {
			return false, "", err
		}
		on, err := probe("container", "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.NetworkID}} {{end}}", container)
		if err != nil {
			// No container: its boot makes one on the network as named.
			return false, id, nil
		}
		return !slices.Contains(strings.Fields(string(on)), id), id, nil
	}
	stale, id, err := look()
	if err != nil {
		return err
	}
	if stale {
		err := repairUnder(space, project, t, func() (bool, error) {
			stale, _, err := look()
			return !stale, err
		}, func() error {
			if _, err := run("rm", "-f", container); err != nil {
				return fmt.Errorf("its container on a network gone: %w", dockerFailed("docker rm", err))
			}
			return nil
		})
		if err != nil {
			return err
		}
		if id, err = builderNetworkID(probe, project); err != nil {
			return err
		}
	}
	if id == "" {
		if _, err := run("network", "create", network); err != nil {
			// Another use may have made it meanwhile: Docker makes no second of a name.
			if again, lerr := builderNetworkID(probe, project); lerr == nil && again != "" {
				return nil
			}
			return fmt.Errorf("its network could not be made: %w", dockerFailed("docker network create", err))
		}
	}
	return nil
}

// dockerRunner runs docker at path under env for what it prints, each call bounded by within
// (outputWithin): the builders' looks and changes outside a request.
func dockerRunner(path string, env []string, within time.Duration) func(args ...string) ([]byte, error) {
	return func(args ...string) ([]byte, error) {
		c := exec.Command(path, args...)
		c.Env = env
		return outputWithin(c, within)
	}
}

// leftoverThere says whether a container or state volume of project's builder's names is there,
// by run's docker.
func leftoverThere(run func(args ...string) ([]byte, error), project string) bool {
	if _, ok := builderContainer(run, project); ok {
		return true
	}
	_, err := run("volume", "inspect", door.BuilderStateOf(project))
	return err == nil
}

// removeLeftover removes project's builder's container and state volume by their names, with
// run's docker, when they are there with no builder of devopsy's (a boot racing a key's
// removal): a builder made anew would take them as its own, a removed key's cache with them.
// Its caller holds the builder's lock exclusive, or is the removal: a build may be booting it.
func removeLeftover(run func(args ...string) ([]byte, error), project string) error {
	if _, ok := builderContainer(run, project); ok {
		if _, err := run("rm", "-f", door.BuilderContainerOf(project)); err != nil {
			return fmt.Errorf("its builder's container: %w", dockerFailed("docker rm", err))
		}
	}
	state := door.BuilderStateOf(project)
	if _, err := run("volume", "inspect", state); err == nil {
		if _, err := run("volume", "rm", state); err != nil {
			return fmt.Errorf("its builder's cache: %w", dockerFailed("docker volume rm", err))
		}
	}
	return nil
}

// outputWithin runs c for what it prints on stdout, as Output does, its stderr in the
// error's (exec.ExitError's Stderr), in a process group of its own, killed with every child
// once within has passed: the one bound of the builders' docker calls.
func outputWithin(c *exec.Cmd, within time.Duration) ([]byte, error) {
	var out, said bytes.Buffer
	c.Stdout, c.Stderr = &out, &said
	// A descendant that left the group holding stdout ends the wait a second after the command.
	c.WaitDelay = time.Second
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
