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
// config under dir (doorHome), its locks' directory, and a docker whose builder's container
// runs, recording its stop in dir/stopped.
func stopperAt(t *testing.T) (dir, space string, env []string) {
	t.Helper()
	dir = t.TempDir()
	space = filepath.Join(dir, "space")
	bin := filepath.Join(dir, "bin")
	write(t, filepath.Join(bin, "docker"), `#!/bin/sh
case "$*" in
  'ps -q --filter name=^/buildx_buildkit_devopsy-shop0$ --filter status=running') echo c0ffee ;;
  'stop buildx_buildkit_devopsy-shop0') echo stopped >>`+filepath.Join(dir, "stopped")+` ;;
  *) exit 9 ;;
esac
`, 0o755)
	env = []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "DEVOPSY_HOME=" + doorHome(dir)}
	return dir, space, env
}

// A use of the builder (useBuilder: a build, --gc) starts its stopper, which answers before the
// use goes on and stops the builder once the use lets the lock go, killed or refused before
// its exec as much as done: one per project, ten uses beside a held one leaving one. A
// stopper for a project that is no door user's ends saying why, and the use is refused with it.
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
	if _, err := useBuilder(space, "blog", os.Args[0], env, testBuilderTiming); err == nil || !strings.Contains(err.Error(), "blog's builder's stopper: it ended with no answer: blog is no project user's") {
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
	c := exec.Command(os.Args[0], builderIdleFlag, "shop", answerFdFlag, "3")
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
  'container inspect --format {{.HostConfig.RestartPolicy.Name}} buildx_buildkit_devopsy-shop0'|'volume inspect buildx_buildkit_devopsy-shop0_state') ;;
  'network ls -q --no-trunc --filter name=^devopsy-shop-build$') echo netid ;;
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
	write(t, filepath.Join(door.BuildxConfigOf(space), "instances", "devopsy-shop"), `{"Name":"devopsy-shop","Driver":"docker-container"}`, 0o644)
	start := time.Now()
	if err := removeBuilder(docker, nil, space, "devopsy-shop"); err != nil {
		t.Fatal(err)
	}
	if err := dropBuilderLocks(space, "shop"); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the removal took %s beside a build", took)
	}
	want := "buildx rm devopsy-shop\ncontainer inspect --format {{.HostConfig.RestartPolicy.Name}} buildx_buildkit_devopsy-shop0\nrm -f buildx_buildkit_devopsy-shop0\n" +
		"volume inspect buildx_buildkit_devopsy-shop0_state\nvolume rm buildx_buildkit_devopsy-shop0_state\nnetwork ls -q --no-trunc --filter name=^devopsy-shop-build$\nnetwork rm devopsy-shop-build\n"
	if got := readString(log); got != want {
		t.Errorf("docker was asked:\n%s\nwant:\n%s", got, want)
	}
	for _, f := range []string{door.BuilderLockOf(space, "shop"), door.BuilderIdleLockOf(space, "shop")} {
		if _, err := os.Stat(f); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s stays: %v", f, err)
		}
	}
}

// A key's removal goes through a buildx rm that takes longer than a wait on the builder's lock:
// the rm removes the builder's state volume, its whole build cache, which on a large cache and
// a slow disk takes minutes, and the removal waits for it as it did.
//
// The bug this proves (removeBuilder, enduser.go, its docker calls through dockerRunner): every
// call of the removal is bounded by builderTimes.lock, the bound of a build's wait on the
// builder's lock (a minute), a bound the removal never had and its fix did not mean to add (it
// meant --gc's looks before a boot), so a buildx rm, a container's rm -f or the state volume's
// rm past a minute is killed (outputWithin) and the key's removal fails ("the key is removed,
// but ...: docker buildx rm failed"), buildx's record of the builder left, while the daemon
// goes on removing what it was asked to. The fake's buildx rm takes longer than the bound,
// shortened for the test.
//
// Not parallel: it sets builderTimes, the one timing removeBuilder reads.
func TestBuilderRemovalOutlastsALockWait(t *testing.T) {
	saved := builderTimes
	t.Cleanup(func() { builderTimes = saved })
	builderTimes.lock = 200 * time.Millisecond
	dir := t.TempDir()
	space := filepath.Join(dir, "space")
	log := filepath.Join(dir, "docker.log")
	record := filepath.Join(door.BuildxConfigOf(space), "instances", "devopsy-shop")
	write(t, record, `{"Name":"devopsy-shop","Driver":"docker-container"}`, 0o644)
	docker := filepath.Join(dir, "docker")
	write(t, docker, `#!/bin/sh
echo "$*" >>`+log+`
case "$*" in
  'buildx rm devopsy-shop') sleep 1; rm -f `+record+` ;;
  'container inspect '*|'volume inspect '*) exit 1 ;;
  'network ls -q --no-trunc --filter name=^devopsy-shop-build$') echo netid ;;
  'network rm devopsy-shop-build') ;;
  *) exit 9 ;;
esac
`, 0o755)
	if err := removeBuilder(docker, nil, space, "devopsy-shop"); err != nil {
		t.Errorf("a removal whose buildx rm outlasts a lock's wait failed: %v; docker was asked:\n%s", err, readString(log))
	}
}

// A key's removal whose look at the builder failed has not removed it, and says so.
//
// The bug this proves (removeBuilder, enduser.go): any failure of docker buildx inspect is
// taken for no builder there, so buildx rm is skipped, and the container, the state volume and
// the network then go by their names, with no error: the removal reports the builder gone
// while buildx's record of it stays, on a network that is no more. The next build finds that
// record as devopsy makes it (its network named, no container), makes nothing, and its boot
// fails on the missing network, until someone removes the builder by hand.
func TestBuilderRemovalFailsWhenItsInspectFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	space := filepath.Join(dir, "space")
	log := filepath.Join(dir, "docker.log")
	// The builder is there by buildx's record of it and its listing; only the inspect fails.
	write(t, filepath.Join(door.BuildxConfigOf(space), "instances", "devopsy-shop"), `{"Name":"devopsy-shop","Driver":"docker-container"}`, 0o644)
	docker := filepath.Join(dir, "docker")
	write(t, docker, `#!/bin/sh
echo "$*" >>`+log+`
case "$*" in
  'buildx inspect devopsy-shop') echo 'signal: killed' >&2; exit 137 ;;
  'buildx ls --format json') echo '{"Name":"devopsy-shop","Driver":"docker-container","Nodes":[{"Name":"devopsy-shop0","Status":"stopped"}]}' ;;
esac
`, 0o755)
	if err := removeBuilder(docker, nil, space, "devopsy-shop"); err == nil && !strings.Contains(readString(log), "buildx rm devopsy-shop") {
		t.Errorf("a removal whose inspect failed left the builder's record and said nothing, asked:\n%s", readString(log))
	}
}

// A key's removal looks at the builder's network by its exact listing: one gone is no failure
// and is not removed, and a look that fails fails the removal, which never reads it as a
// network gone, nor removes by name what it did not see.
func TestBuilderRemovalGoesByItsNetworksListing(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		ls     string
		failed bool
	}{
		"gone":           {ls: ":", failed: false},
		"a look failing": {ls: "echo 'Cannot connect to the Docker daemon' >&2; exit 1", failed: true},
	} {
		dir := t.TempDir()
		log := filepath.Join(dir, "docker.log")
		docker := filepath.Join(dir, "docker")
		write(t, docker, `#!/bin/sh
echo "$*" >>`+log+`
case "$*" in
  'container inspect '*|'volume inspect '*) exit 1 ;;
  'network ls -q --no-trunc --filter name=^devopsy-shop-build$') `+c.ls+` ;;
  *) exit 9 ;;
esac
`, 0o755)
		err := removeBuilder(docker, nil, filepath.Join(dir, "space"), "devopsy-shop")
		if (err != nil) != c.failed || strings.Contains(readString(log), "network rm") {
			t.Errorf("a removal, its builder's network %s: %v; docker was asked:\n%s", name, err, readString(log))
		}
	}
}

// --gc's removals boot each project user's builder once, one at a time, and stop it before the
// next: the listed removal prunes it; --yes reads it first and prunes only one with something
// to remove, said only then.
//
// Not parallel: it checks that --gc takes a lock it let go at once, which a parallel test's
// fork holds until its exec.
func TestGCPrunesEachBuilderBootedOnce(t *testing.T) {
	dir := t.TempDir()
	space := filepath.Join(dir, "space")
	log := filepath.Join(dir, "docker.log")
	old := `[{"id":"x","inUse":false,"size":3000,"createdAt":"2026-09-01T00:00:00Z","lastUsedAt":"2026-09-02T00:00:00Z","parents":null,"shared":false}]`
	docker := filepath.Join(dir, "docker")
	write(t, docker, `#!/bin/sh
case "$*" in
  'ps -q '*) echo c0ffee ;;
  'container inspect --format {{range .NetworkSettings.Networks}}'*|'network ls -q --no-trunc --filter '*) echo netid ;;
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
		"remove-listed": {"buildx inspect --bootstrap devopsy-shop", "prune", "stop buildx_buildkit_devopsy-shop0", "buildx inspect --bootstrap devopsy-wiki", "prune", "stop buildx_buildkit_devopsy-wiki0"},
		"remove":        {"buildx inspect --bootstrap devopsy-shop", "du", "prune", "stop buildx_buildkit_devopsy-shop0", "buildx inspect --bootstrap devopsy-wiki", "du", "stop buildx_buildkit_devopsy-wiki0"},
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

// A stop goes by the builder's container running, and by nothing else: none running (none at
// all, a builder gone) is nothing to stop, and every failure, a Docker that does not answer
// within the bound included, is the stop's, so its stopper tries again (stopWhenIdle) rather
// than ending as if it had stopped it. The bound is the whole step's: a look that took most of
// it leaves the stop the rest, so a stop holds the builder's lock exclusive no longer than it.
func TestStopGoesByTheContainerWithinOneBound(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log := filepath.Join(dir, "docker.log")
	stop := func(ps, stopped string) (string, error) {
		t.Helper()
		os.Remove(log)
		docker := filepath.Join(dir, "docker")
		write(t, docker, `#!/bin/sh
echo "$*" >>`+log+`
case "$*" in
  'ps -q --filter name=^/buildx_buildkit_devopsy-shop0$ --filter status=running') `+ps+` ;;
  'stop buildx_buildkit_devopsy-shop0') `+stopped+` ;;
  *) exit 9 ;;
esac
`, 0o755)
		return readString(log), stopBuilder(docker, nil, "shop", 1500*time.Millisecond)
	}
	if _, err := stop("echo c0ffee", ":"); err != nil {
		t.Errorf("a running builder's stop: %v", err)
	}
	if got, err := stop(":", "exit 9"); err != nil || strings.Contains(readString(log), "stop buildx") {
		t.Errorf("a builder not running (%v), asked:\n%s", err, got)
	}
	if _, err := stop("echo 'Cannot connect to the Docker daemon' >&2; exit 1", ":"); err == nil || !strings.Contains(err.Error(), "Cannot connect to the Docker daemon") {
		t.Errorf("a look Docker failed: %v", err)
	}
	start := time.Now()
	if _, err := stop("exec sleep 30", ":"); err == nil {
		t.Error("a look past its bound says the builder is stopped")
	}
	start = time.Now()
	if _, err := stop("sleep 1; echo c0ffee", "exec sleep 30"); err == nil {
		t.Error("a stop past its bound says the builder is stopped")
	}
	if took := time.Since(start); took >= 2500*time.Millisecond {
		t.Errorf("the stop took %s, its bound 1.5s: the look and the stop each had the whole bound", took.Round(10*time.Millisecond))
	}
}

// A stopper started with no answer's descriptor (by hand: only its starters hand it one) still
// stops an idle builder, writing nothing anywhere: descriptor 3 is its answer only when it
// inherited it, else the first file it opens, the builder's lock, would take the answer and be
// closed under it.
func TestStopperWithNoAnswerStillStops(t *testing.T) {
	t.Parallel()
	dir, space, env := stopperAt(t)
	l, err := builderLockWait(door.BuilderLockOf(space, "shop"), "", syscall.LOCK_SH, time.Second, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	if f, err := os.OpenFile(door.BuilderIdleLockOf(space, "shop"), os.O_CREATE|os.O_RDONLY, 0o600); err == nil {
		f.Close()
	}
	c := exec.Command(os.Args[0], builderIdleFlag, "shop")
	c.Env, c.Dir = env, "/"
	out, err := c.CombinedOutput()
	if err != nil {
		t.Errorf("the stopper (%v): %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "stopped")); err != nil {
		t.Error("an idle builder left running by a stopper with no answer's descriptor")
	}
	if b := readString(door.BuilderLockOf(space, "shop")); b != "" {
		t.Errorf("the builder's lock file holds %q", b)
	}
}

// --gc reads and prunes a project user's builder whose network a prune took: a stopped
// builder's container has no endpoint on its network, so docker network prune removes it, the
// container kept (docker system prune takes the stopped container too, and --gc then skips the
// builder as one never booted).
//
// The bug this proves (onBuilder, gc.go): --gc boots the builder as it finds it (buildx inspect
// --bootstrap), its network never looked at, where only a build's ensureBuilder makes it again,
// and that by name alone, the container left on the old network's id. So after a prune every
// --gc warns that the cache of each stopped builder stays, and exits as if something was left,
// until each project builds again (and, the container kept, after that too). The fake boots as
// Docker does: a container that is there starts only on the network whose id it keeps, one that
// is not there is made on the network as named.
func TestGCBootsABuilderWhoseNetworkWasPruned(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	space := filepath.Join(dir, "space")
	log := filepath.Join(dir, "docker.log")
	// net holds the network's id while it is there; attached the id the builder's container
	// (booted once, stopped) was made on.
	net, attached := filepath.Join(dir, "net"), filepath.Join(dir, "attached")
	write(t, attached, "old\n", 0o644)
	old := `[{"id":"x","inUse":false,"size":3000,"createdAt":"2026-09-01T00:00:00Z","lastUsedAt":"2026-09-02T00:00:00Z","parents":null,"shared":false}]`
	docker := filepath.Join(dir, "docker")
	write(t, docker, `#!/bin/sh
echo "$*" >>`+log+`
case "$*" in
  'ps -q '*) ;;
  'buildx inspect --bootstrap devopsy-shop')
    [ -e `+net+` ] || { echo 'network devopsy-shop-build not found' >&2; exit 1; }
    [ -e `+attached+` ] || cp `+net+` `+attached+`
    cmp -s `+net+` `+attached+` || { echo "failed to set up container networking: network $(cat `+attached+`) not found" >&2; exit 1; } ;;
  'exec buildx_buildkit_devopsy-shop0 buildctl du --format {{json .}}') echo '`+old+`' ;;
  'buildx prune '*) printf 'Total:\t3kB\n' ;;
  'container inspect --format {{range .NetworkSettings.Networks}}{{.NetworkID}} {{end}} buildx_buildkit_devopsy-shop0') [ -e `+attached+` ] && cat `+attached+` ;;
  'network ls -q --no-trunc --filter name=^devopsy-shop-build$') if [ -e `+net+` ]; then cat `+net+`; fi ;;
  'container inspect '*) [ -e `+attached+` ] && echo no ;;
  'rm -f buildx_buildkit_devopsy-shop0'|'rm buildx_buildkit_devopsy-shop0') rm -f `+attached+` ;;
  'network inspect devopsy-shop-build') [ -e `+net+` ] ;;
  'network create devopsy-shop-build') echo new >`+net+` ;;
  'network disconnect '*) ;;
  'network connect devopsy-shop-build buildx_buildkit_devopsy-shop0') cp `+net+` `+attached+` ;;
  *) ;;
esac
`, 0o755)
	stopper := filepath.Join(dir, "stopper")
	write(t, stopper, "#!/bin/sh\nprintf d >&3\n", 0o755)
	g := &gcScope{docker: docker, now: time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC), listedAt: time.Now(), ages: remote.ServerGC{BuildCache: 30 * 24 * time.Hour},
		door: &remote.Door{Space: space, Users: map[string]remote.DoorUser{door.UserOf("shop"): {Project: "shop"}}}, mode: "remove", self: stopper, timing: testBuilderTiming}
	code := g.remove(gcPlan{Builders: []gcBuilder{{Name: "devopsy-shop", Project: "shop"}}})
	if asked := readString(log); code != 0 || !strings.Contains(asked, "buildx prune ") {
		t.Errorf("--gc on a stopped builder whose network a prune took: exit %d, its cache not pruned; docker was asked:\n%s", code, asked)
	}
}

// A build that loses the making of its builder's network to another build of the project goes
// on: the network is there, which is all its boot needs.
//
// The bug this proves (builderBootable, builderidle.go): the network's look and its making
// are two docker calls under no lock, so two builds of one project that both find it gone (a
// project's first builds, two environments released at once; or the first builds after a
// docker network prune) both make it, and Docker refuses the second ("network with name
// devopsy-shop-build already exists", Docker 29.8.2, eight docker network create of one name
// at once: one made it, seven refused). builderBootable takes that refusal for a builder that
// cannot boot and the door refuses the build, where ensureBuilder forgives the same lost race
// on buildx create right after. The fake's create is the loser's: the network is there by
// then, made by the other build, and the create fails as Docker's does.
func TestBuilderBootableBesideAnotherBuildMakingItsNetwork(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	space := filepath.Join(dir, "space")
	net := filepath.Join(dir, "net")
	docker := filepath.Join(dir, "docker")
	write(t, docker, `#!/bin/sh
case "$*" in
  'container inspect '*) echo 'Error response from daemon: No such container' >&2; exit 1 ;;
  'network ls -q --no-trunc --filter name=^devopsy-shop-build$') if [ -e `+net+` ]; then cat `+net+`; fi ;;
  'network create devopsy-shop-build') echo netid >`+net+`; echo 'Error response from daemon: network with name devopsy-shop-build already exists' >&2; exit 1 ;;
  *) exit 9 ;;
esac
`, 0o755)
	run := func(args ...string) ([]byte, error) { return exec.Command(docker, args...).Output() }
	if err := builderBootable(run, space, "shop", testBuilderTiming); err != nil {
		t.Errorf("a build whose builder's network another build made first is refused, the network there: %v", err)
	}
}

// Two builds of one project that find its network gone (a prune took it from the builder
// stopped between them) both get it back: the one whose network create comes second finds the
// network the other made, and builds on it.
//
// The bug this proves (builderBootable, builderidle.go): the network is looked at (network ls)
// and made (network create) with no lock and no look again, so when another build of the
// project, another environment's, makes it between the two, Docker refuses the create
// ("network with name ... already exists", Docker 25 and later, which refuse a name twice),
// and the build is refused ("its network could not be made") though the network is there, as
// devopsy makes it. buildx create beside it looks again on a failure (ensureBuilder: "another
// environment's build may have made it meanwhile"). The fake's other build makes the network
// right after the look.
func TestBuilderBootableWhenAnotherBuildMadeItsNetwork(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log := filepath.Join(dir, "docker.log")
	net := filepath.Join(dir, "net")
	docker := filepath.Join(dir, "docker")
	write(t, docker, `#!/bin/sh
echo "$*" >>`+log+`
case "$*" in
  'container inspect '*) echo 'Error: No such container: buildx_buildkit_devopsy-shop0' >&2; exit 1 ;;
  'network ls -q --no-trunc --filter name=^devopsy-shop-build$')
    if [ -e `+net+` ]; then cat `+net+`; else echo theirs >`+net+`; fi ;;
  'network create devopsy-shop-build')
    if [ -e `+net+` ]; then echo 'Error response from daemon: network with name devopsy-shop-build already exists' >&2; exit 1; fi
    echo ours >`+net+` ;;
  *) exit 9 ;;
esac
`, 0o755)
	run := func(args ...string) ([]byte, error) { return exec.Command(docker, args...).Output() }
	if err := builderBootable(run, filepath.Join(dir, "space"), "shop", testBuilderTiming); err != nil {
		t.Errorf("a build whose builder's network another build made meanwhile is refused: %v; docker was asked:\n%s", err, readString(log))
	}
}
