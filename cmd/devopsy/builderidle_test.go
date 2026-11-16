package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/pathlock"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// testBuilderTiming is the builders' waits a test runs with: short, read-only.
var testBuilderTiming = builderTiming{lock: 3 * time.Second, answer: 5 * time.Second, every: 20 * time.Millisecond, bound: 10 * time.Second, stop: 3 * time.Second}

// waitStoppersGone waits, bounded by startWait, until no stopper of project's builder in space
// is left (its stopper lock free, or gone), so that none writes into a test's directory past
// its end.
func waitStoppersGone(t *testing.T, space, project string) {
	t.Helper()
	idle, err := pathlock.Open(door.BuilderIdleLockOf(space, project))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	for deadline := time.Now().Add(startWait); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if ok, err := idle.Try(syscall.LOCK_EX); ok || err != nil {
			return
		}
	}
	t.Errorf("%s's builder's stopper still runs after %s", project, startWait)
}

// takeSoon takes l (how) within startWait: a parallel test's fork holds the descriptors this
// one closed until its exec, so a lock let go may be held a moment longer.
func takeSoon(l *pathlock.Lock, how int) (bool, error) {
	for deadline := time.Now().Add(startWait); ; time.Sleep(10 * time.Millisecond) {
		ok, err := l.Try(how)
		if ok || err != nil || time.Now().After(deadline) {
			return ok, err
		}
	}
}

// builderLocks are a stopper's two locks for project shop in a test's own space, as a starter
// makes them, the stopper's taken as runBuilderIdle takes it.
func builderLocks(t *testing.T) (space string, lock, idle *pathlock.Lock) {
	t.Helper()
	space = t.TempDir()
	for _, f := range []string{door.BuilderLockOf(space, "shop"), door.BuilderIdleLockOf(space, "shop")} {
		l, err := builderLockWait(f, "", syscall.LOCK_SH, time.Second, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		l.Close()
	}
	var err error
	if lock, err = pathlock.Open(door.BuilderLockOf(space, "shop")); err != nil {
		t.Fatal(err)
	}
	if idle, err = pathlock.Open(door.BuilderIdleLockOf(space, "shop")); err != nil {
		t.Fatal(err)
	}
	if ok, err := takeSoon(idle, syscall.LOCK_EX); !ok || err != nil {
		t.Fatalf("the stopper's lock: %v, %v", ok, err)
	}
	t.Cleanup(func() { lock.Close(); idle.Close() })
	return space, lock, idle
}

// The stopper stops the builder once nothing holds the builder's lock, and only then: a second
// build's shared lock keeps it from stopping until that build ends too, and it stops once. It
// lets its own lock go before the builder's, so a build waiting on the stop finds it free for
// a stopper of its own; a stop that fails sends it back to trying, its own lock kept. It stops
// nothing on a lock's file a destroy removed, nor past its bound.
func TestStopperStopsOnceTheBuildsAreGone(t *testing.T) {
	t.Parallel()
	space, lock, idle := builderLocks(t)
	path := door.BuilderLockOf(space, "shop")
	take := func() *pathlock.Lock {
		t.Helper()
		l, err := pathlock.Take(path, "", syscall.LOCK_SH, nil)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	first, second := take(), take()
	var stops atomic.Int32
	release, stopping := make(chan struct{}), make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		stopWhenIdle(lock, idle, func() error {
			stops.Add(1)
			stopping <- struct{}{}
			<-release
			return nil
		}, testBuilderTiming)
		close(done)
	}()
	first.Close()
	time.Sleep(10 * testBuilderTiming.every)
	if stops.Load() != 0 {
		t.Fatal("stopped beside a second build")
	}
	second.Close()
	<-stopping
	// During the stop a build waits for the lock shared; once it has it, the stopper's own lock
	// is free for that build's stopper.
	waited := make(chan *pathlock.Lock)
	go func() { waited <- take() }()
	time.Sleep(5 * testBuilderTiming.every)
	select {
	case <-waited:
		t.Fatal("a build took the lock during the stop")
	default:
	}
	close(release)
	l := <-waited
	other, err := pathlock.Open(door.BuilderIdleLockOf(space, "shop"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if ok, err := takeSoon(other, syscall.LOCK_EX); !ok || err != nil {
		t.Errorf("the stopper's lock held once the build waiting on the stop has the builder's: %v, %v", ok, err)
	}
	l.Close()
	<-done
	if n := stops.Load(); n != 1 {
		t.Errorf("%d stops, want 1", n)
	}
}

func TestStopperTriesAgainAfterAFailedStop(t *testing.T) {
	t.Parallel()
	space, lock, idle := builderLocks(t)
	var stops atomic.Int32
	stopWhenIdle(lock, idle, func() error {
		if stops.Add(1) == 1 {
			return errors.New("Docker down")
		}
		return nil
	}, testBuilderTiming)
	if n := stops.Load(); n != 2 {
		t.Errorf("%d stops, want a failed one and one that went", n)
	}
	other, err := pathlock.Open(door.BuilderIdleLockOf(space, "shop"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if ok, _ := takeSoon(other, syscall.LOCK_EX); !ok {
		t.Error("the stopper's lock kept past its stop")
	}
}

func TestStopperStopsNothingOnAReplacedLockOrPastItsBound(t *testing.T) {
	t.Parallel()
	space, lock, idle := builderLocks(t)
	path := door.BuilderLockOf(space, "shop")
	held, err := pathlock.Take(path, "", syscall.LOCK_SH, nil)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	short := testBuilderTiming
	short.bound = 10 * short.every
	stopWhenIdle(lock, idle, func() error { stopped = true; return nil }, short)
	if stopped {
		t.Error("stopped past its bound beside a build")
	}
	// A destroy removed the file, an add made it again: its stopper stops nothing.
	held.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	again, err := pathlock.Take(path, "", syscall.LOCK_SH, nil)
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
	stopWhenIdle(lock, idle, func() error { stopped = true; return nil }, testBuilderTiming)
	if stopped {
		t.Error("stopped on a lock a destroy removed")
	}
}

// stopperAt is what a stopper started through the binary needs for project shop: a door
// config under dir (doorHome), its locks' directory, and a docker that answers inspect for a
// running builder and records its stop in dir/stopped.
func stopperAt(t *testing.T) (dir, space string, env []string) {
	t.Helper()
	dir = t.TempDir()
	space = filepath.Join(dir, "space")
	bin := filepath.Join(dir, "bin")
	write(t, filepath.Join(bin, "docker"), `#!/bin/sh
case "$*" in
  'buildx inspect devopsy-shop') [ "$BUILDX_CONFIG" = '`+space+`/buildx' ] && echo 'Status: running' ;;
  'buildx stop devopsy-shop') echo stopped >>`+filepath.Join(dir, "stopped")+` ;;
  *) exit 9 ;;
esac
`, 0o755)
	env = []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "DEVOPSY_HOME=" + doorHome(dir)}
	return dir, space, env
}

// A use of the builder (useBuilder: a build, --gc) starts its stopper, which answers before the
// use goes on and stops the builder once the use lets the lock go, killed or refused before
// its exec as much as done: one per project, ten uses beside a held one leaving one. A
// stopper for a project that is no door user's answers nothing, and the use is refused.
func TestUseOfTheBuilderStartsItsStopper(t *testing.T) {
	t.Parallel()
	dir, space, env := stopperAt(t)
	held, err := useBuilder(space, "shop", os.Args[0], env, testBuilderTiming)
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		holds, err := startStopper(os.Args[0], "shop", env, testBuilderTiming)
		if err != nil || holds {
			t.Fatalf("a second stopper: holds %v, %v", holds, err)
		}
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "stopped")); err == nil {
		t.Fatal("stopped while a use holds the builder")
	}
	held.Close()
	waitFile(t, filepath.Join(dir, "stopped"), "the builder's stop", startWait, func() string { return "" })
	waitStoppersGone(t, space, "shop")
	if got := readString(filepath.Join(dir, "stopped")); got != "stopped\n" {
		t.Errorf("stops: %q, want one", got)
	}
	if _, err := useBuilder(space, "blog", os.Args[0], env, testBuilderTiming); err == nil || !strings.Contains(err.Error(), "blog's builder's stopper: it ended with no answer") {
		t.Errorf("a use of a builder no door user's: %v", err)
	}
}

// A stopper keeps nothing it inherited but its answer: a descriptor left open across its exec
// (a session's lock, a door's rendered file) would be held for as long as it waits.
func TestStopperClosesWhatItInherits(t *testing.T) {
	t.Parallel()
	_, space, env := stopperAt(t)
	l, err := builderLockWait(door.BuilderLockOf(space, "shop"), "", syscall.LOCK_SH, time.Second, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if f, err := os.OpenFile(door.BuilderIdleLockOf(space, "shop"), os.O_CREATE|os.O_RDONLY, 0o600); err == nil {
		f.Close()
	}
	answerR, answerW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	inheritedR, inheritedW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command(os.Args[0], builderIdleFlag, "shop")
	c.Env, c.ExtraFiles = env, []*os.File{answerW, inheritedW}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	answerW.Close()
	inheritedW.Close()
	defer func() { l.Close(); c.Wait() }()
	b := make([]byte, 1)
	if _, err := answerR.Read(b); err != nil || b[0] != stopperHolds {
		t.Fatalf("its answer: %q, %v", b, err)
	}
	// Its write end closed in the stopper too, while it still waits: the pipe ends.
	inheritedR.SetReadDeadline(time.Now().Add(startWait))
	if _, err := inheritedR.Read(b); !errors.Is(err, io.EOF) {
		t.Errorf("the stopper keeps a descriptor it inherited: %v", err)
	}
}

// A key's or user's removal takes no lock: beside a build holding the builder's lock and a
// stopper holding its own, it removes a stopped builder, a container and state volume left
// with no builder (a boot racing a removal), and the network, at once; a destroy then unlinks
// both locks without waiting on either.
func TestBuilderRemovalWaitsOnNoBuild(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	space := filepath.Join(dir, "space")
	log := filepath.Join(dir, "docker.log")
	docker := filepath.Join(dir, "docker")
	write(t, docker, `#!/bin/sh
echo "$*" >>`+log+`
case "$*" in
  'buildx inspect devopsy-shop'|'container inspect buildx_buildkit_devopsy-shop0'|'volume inspect buildx_buildkit_devopsy-shop0_state'|'network inspect devopsy-shop-build') ;;
  'buildx rm devopsy-shop'|'rm -f buildx_buildkit_devopsy-shop0'|'volume rm buildx_buildkit_devopsy-shop0_state'|'network rm devopsy-shop-build') ;;
  *) exit 9 ;;
esac
`, 0o755)
	build, err := builderLockWait(door.BuilderLockOf(space, "shop"), "", syscall.LOCK_SH, time.Second, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer build.Close()
	stopper, err := builderLockWait(door.BuilderIdleLockOf(space, "shop"), "", syscall.LOCK_EX, time.Second, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stopper.Close()
	start := time.Now()
	if err := removeBuilder(docker, builderEnv(nil, space), "devopsy-shop"); err != nil {
		t.Fatal(err)
	}
	if err := dropBuilderLocks(space, "shop"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the removal took %s beside a build", took)
	}
	want := "buildx inspect devopsy-shop\nbuildx rm devopsy-shop\ncontainer inspect buildx_buildkit_devopsy-shop0\nrm -f buildx_buildkit_devopsy-shop0\n" +
		"volume inspect buildx_buildkit_devopsy-shop0_state\nvolume rm buildx_buildkit_devopsy-shop0_state\nnetwork inspect devopsy-shop-build\nnetwork rm devopsy-shop-build\n"
	if got := readString(log); got != want {
		t.Errorf("docker was asked:\n%s\nwant:\n%s", got, want)
	}
	for _, f := range []string{door.BuilderLockOf(space, "shop"), door.BuilderIdleLockOf(space, "shop")} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s stays: %v", f, err)
		}
	}
}

// --gc's removals boot each project user's builder once, one at a time, and stop it before the
// next: the listed removal prunes it; --yes reads it first and prunes only one with something
// to remove, said only then.
func TestGCPrunesEachBuilderBootedOnce(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	space := filepath.Join(dir, "space")
	log := filepath.Join(dir, "docker.log")
	old := `[{"id":"x","inUse":false,"size":3000,"createdAt":"2026-09-01T00:00:00Z","lastUsedAt":"2026-09-02T00:00:00Z","parents":null,"shared":false}]`
	docker := filepath.Join(dir, "docker")
	write(t, docker, `#!/bin/sh
case "$*" in
  'buildx inspect devopsy-'*) echo 'Status: running' ;;
  'exec buildx_buildkit_devopsy-shop0 buildctl du --format {{json .}}') echo "$*" >>`+log+`; echo '`+old+`' ;;
  'exec buildx_buildkit_devopsy-wiki0 buildctl du --format {{json .}}') echo "$*" >>`+log+`; echo '[]' ;;
  'buildx prune '*) [ "$BUILDX_CONFIG" = '`+space+`/buildx' ] || exit 7; echo "$*" >>`+log+`; printf 'Total:\t3kB\n' ;;
  *) echo "$*" >>`+log+` ;;
esac
`, 0o755)
	stopper := filepath.Join(dir, "stopper")
	write(t, stopper, "#!/bin/sh\nprintf d >&3\n", 0o755)
	users := map[string]remote.DoorUser{door.UserOf("shop"): {Project: "shop"}, door.UserOf("wiki"): {Project: "wiki"}}
	plan := gcPlan{Builders: []gcBuilder{{Name: "devopsy-shop", Project: "shop"}, {Name: "devopsy-wiki", Project: "wiki"}}}
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	for mode, want := range map[string][]string{
		"remove-listed": {"buildx inspect --bootstrap devopsy-shop", "prune", "buildx stop devopsy-shop", "buildx inspect --bootstrap devopsy-wiki", "prune", "buildx stop devopsy-wiki"},
		"remove":        {"buildx inspect --bootstrap devopsy-shop", "du", "prune", "buildx stop devopsy-shop", "buildx inspect --bootstrap devopsy-wiki", "du", "buildx stop devopsy-wiki"},
	} {
		os.Remove(log)
		g := &gcScope{docker: docker, now: now, listedAt: time.Now(), ages: remote.ServerGC{BuildCache: 30 * 24 * time.Hour}, door: &remote.Door{Space: space, Users: users},
			mode: mode, self: stopper, timing: testBuilderTiming}
		if code := g.remove(plan); code != 0 {
			t.Errorf("%s: exit %d", mode, code)
		}
		var got []string
		for _, l := range strings.Split(strings.TrimSpace(readString(log)), "\n") {
			switch {
			case strings.HasPrefix(l, "buildx prune "):
				l = "prune"
			case strings.Contains(l, "buildctl du"):
				l = "du"
			}
			got = append(got, l)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s asked:\n%s\nwant:\n%s", mode, strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	}
}

// BUG (review): stopBuilder takes any inspect that fails for an absent builder, one killed past
// its bound included (a builder that runs but does not answer: inspect asks its BuildKit), and
// answers that nothing is left to stop, so its stopper lets both locks go and ends, the
// builder left running with no stopper. A stop whose inspect does not end must fail, as one
// whose buildx stop does not, so the stopper tries again (stopWhenIdle).
func TestStopFailsWhenItsInspectDoesNotEnd(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	write(t, docker, `#!/bin/sh
echo "$*" >>`+filepath.Join(dir, "docker.log")+`
case "$*" in
  'buildx inspect devopsy-shop') exec sleep 30 ;;
esac
`, 0o755)
	start := time.Now()
	err := stopBuilder(docker, builderEnv(os.Environ(), filepath.Join(dir, "space")), "shop", 300*time.Millisecond)
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("the stop took %s, its inspect bounded by 300ms", took)
	}
	if err == nil {
		t.Errorf("a stop whose inspect was killed past its bound says the builder is stopped; docker was asked:\n%s", readString(filepath.Join(dir, "docker.log")))
	}
}

// A stop whose inspect does not answer within its bound (a daemon slow or hung) has not found
// the builder gone: it failed, and the stopper tries again (stopWhenIdle), its own lock kept.
// Bug: stopBuilder takes any failure of its inspect, its kill past the bound included, for a
// builder gone and returns nil, so the stopper ends as if it stopped it, the builder left
// running with no stopper until the next build.
func TestStopBuilderFailsOnAnInspectPastItsBound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	docker := filepath.Join(dir, "docker")
	write(t, docker, `#!/bin/sh
case "$*" in
  'buildx inspect devopsy-shop') sleep 10; echo 'Status: running' ;;
  'buildx stop devopsy-shop') ;;
  *) exit 9 ;;
esac
`, 0o755)
	if err := stopBuilder(docker, nil, "shop", 200*time.Millisecond); err == nil {
		t.Error("a stop whose inspect was killed past its bound said the builder is stopped")
	}
}
