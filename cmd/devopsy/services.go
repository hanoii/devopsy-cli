package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// upFor brings name up (up -d --wait) for an import and returns what puts
// it back as it was: stopped again when it was not running before (a
// service down before stays down). keep leaves it up whatever it was (a
// child's first release); noDeps starts none of its dependencies (a
// sync's: one it stopped stays down through the imports).
func upFor(ctx context.Context, cwd, name string, keep, noDeps bool) (down func() error, was string, err error) {
	down = func() error { return nil }
	states, err := serviceStates(cwd)
	if err != nil {
		return down, "", err
	}
	was = states[name]
	up := []string{"up", "-d", "--wait", name}
	if noDeps {
		up = []string{"up", "-d", "--wait", "--no-deps", name}
	}
	if states[name] != "running" && !keep {
		down = func() error {
			report.Info("stopping " + name + " again: it was down before")
			if err := compose(cwd, errOut, "stop", name); err != nil {
				return fmt.Errorf("docker compose stop: %w", err)
			}
			return nil
		}
	}
	// Said first: compose's lines that follow would read as the step
	// before's (a sync's export, on the parent).
	if states[name] == "running" {
		report.Info("waiting for " + name + ", running, to import into it")
	} else {
		report.Info("starting " + name + " to import into it")
	}
	// An up that fails may have started it all the same (unhealthy, or
	// late): down then too.
	if err := composeContext(ctx, cwd, errOut, up...); err != nil {
		return down, was, fmt.Errorf("docker compose up %s: %w", name, err)
	}
	return down, was, nil
}

// compose runs docker compose args in the project at cwd, with its
// environment, to its end: in a process group of its own, so a
// terminal's Ctrl-C, which devopsy handles, never stops it halfway (a
// restore leaving a stopped app down without a word).
func compose(cwd string, stdout io.Writer, args ...string) error {
	return composeRun(context.Background(), cwd, stdout, true, args...)
}

// composeContext is compose, ended with ctx, in devopsy's process group.
func composeContext(ctx context.Context, cwd string, stdout io.Writer, args ...string) error {
	return composeRun(ctx, cwd, stdout, false, args...)
}

func composeRun(ctx context.Context, cwd string, stdout io.Writer, detach bool, args ...string) error {
	plan, err := cli.Build(cwd, append([]string{"--"}, args...), os.Environ())
	if err != nil {
		return err
	}
	if plan.StartsInRelease {
		if err := checkStartBinds(cwd); err != nil {
			return err
		}
	}
	// A start under the release lock too holds its start locks: a --destroy frees a claim
	// after its own release lock has gone with the environment's directory.
	if plan.StartsUIDs && !behindDoor() {
		locks, err := holdStartLocks(plan)
		if err != nil {
			return err
		}
		defer closeLocks(locks)
	}
	return runCompose(ctx, plan.Args, plan.Env, "", stdout, detach)
}

// composeDownProject takes the project at cwd down by its name alone, -p and no
// -f, whatever else args say: compose then finds by its own label what any
// release made, where with files it goes only by what they name. Its directory
// is none of the project's, whose .env compose would read. detach as in
// composeRun.
func composeDownProject(ctx context.Context, cwd string, stdout io.Writer, detach bool, args ...string) error {
	plan, err := cli.Build(cwd, []string{"--", "down"}, os.Environ())
	if err != nil {
		return err
	}
	name := envValue(plan.Env, "COMPOSE_PROJECT_NAME")
	if name == "" {
		return errors.New("no compose project name")
	}
	argv := []string{"docker", "compose"}
	if slices.Contains(plan.Args, "--ansi") {
		argv = append(argv, "--ansi", "always")
	}
	argv = append(append(argv, "-p", name, "down"), args...)
	return runCompose(ctx, argv, plan.Env, "/", stdout, detach)
}

// envValue is key in env, a list of KEY=value, "" when unset.
func envValue(env []string, key string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

func runCompose(ctx context.Context, argv, env []string, dir string, stdout io.Writer, detach bool) error {
	trace(argv)
	cmd := exec.CommandContext(ctx, "docker", argv[1:]...)
	cmd.Env, cmd.Dir = env, dir
	cmd.Stdout, cmd.Stderr = stdout, errOut
	if composeLock != nil {
		// As fd 9 (ExtraFiles[i] is fd 3+i; nil ones are closed).
		cmd.ExtraFiles = []*os.File{nil, nil, nil, nil, nil, nil, composeLock}
	}
	if detach {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	return cmd.Run()
}

// composeLock is the release lock (fd 9) once an import made it
// close-on-exec so its writer never holds it: composeRun gives it back
// to compose, which must hold it as in every other script.
var composeLock *os.File

// composeProfiles is COMPOSE_PROFILES as compose sees it in the project
// at cwd: the caller's, else the project's .env.
func composeProfiles(cwd string) string {
	plan, err := cli.Build(cwd, []string{"--", "ps"}, os.Environ())
	if err != nil {
		return ""
	}
	for _, kv := range plan.Env {
		if v, ok := strings.CutPrefix(kv, "COMPOSE_PROFILES="); ok {
			return v
		}
	}
	return ""
}

// composeServices lists the services docker compose ps prints for args.
// servicesIn are the services whose container is in one of states
// (serviceStates: one docker compose ps), sorted.
func servicesIn(cwd string, states ...string) ([]string, error) {
	all, err := serviceStates(cwd)
	if err != nil {
		return nil, err
	}
	var names []string
	for name, state := range all {
		if slices.Contains(states, state) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// upServices are the services running or restarting: a container in a
// restart loop (a database on a datadir an aborted copy left half
// written) is up as far as a copy is concerned.
func upServices(cwd string) ([]string, error) {
	return servicesIn(cwd, "running", "restarting")
}

func intersect(a, b []string) []string {
	var out []string
	for _, s := range a {
		if slices.Contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}

// stopServices stops what of a sync's side's set runs (cli.DataStops),
// in one docker compose stop, so Compose orders them by depends_on
// (dependents first), saying what and why, and records them first
// (syncState): restoreServices brings back only those. stopped is what
// it stopped (what of the set ran).
func stopServices(cwd string, set []cli.Stopped) (stopped []string, err error) {
	stop := cli.StoppedNames(set)
	if len(stop) == 0 {
		return nil, nil
	}
	// The record exists before anything can fail (startSyncState).
	rec, _ := readSyncRecord(cwd)
	if err := rec.write(cwd); err != nil {
		return nil, err
	}
	// The guard on the parent's side counts a restarting one as running:
	// stop it too.
	up, err := upServices(cwd)
	if err != nil {
		return nil, err
	}
	stop = intersect(stop, up)
	// What this sync stops, by its id: a sync that writes nothing starts
	// it again, and only it (restoreOwnStops).
	if id := os.Getenv(remote.SyncIDEnv); rec.ownID != id {
		rec.own, rec.ownID = nil, id
	}
	for _, name := range stop {
		if !slices.Contains(rec.stops, name) {
			rec.stops = append(rec.stops, name)
			rec.own = append(rec.own, name)
		}
	}
	if err := rec.write(cwd); err != nil {
		return nil, err
	}
	if len(stop) == 0 {
		return nil, nil
	}
	report.Info(cli.StoppingLine(set, stop))
	if err := compose(cwd, errOut, append([]string{"stop"}, stop...)...); err != nil {
		return stop, fmt.Errorf("docker compose stop: %w", err)
	}
	return stop, nil
}

// healthyTimeout is how long restoreServices waits for a started service.
const healthyTimeout = 5 * time.Minute

// restoreOpts are restoreServices' choices.
type restoreOpts struct {
	// create makes a child's first release's services.
	create bool
	// recreate brings what was written back on its own image.
	recreate bool
	// fallback is what to start when there is no record.
	fallback []string
	// written are the services whose data the copy wrote.
	written []string
}

// restoreServices starts what a sync or an import stopped and waits
// until it runs and is healthy, whatever happened before: what the
// record holds (syncState), else o.fallback (a sync's side's set; nothing
// for an import, whose mark and record are written together). It reads
// the containers' state, so it also recovers after an interrupted sync.
// With o.create, a child's first release gets its data services and the
// child side's set created (finish go): never on an abort, which leaves
// it not live.
func restoreServices(cwd string, services []cli.SyncService, o restoreOpts) error {
	stop := o.fallback
	var errs []error
	// A child's first release had no containers: nothing was stopped,
	// and the finish hook expects the services up (an app to sanitize
	// through): those with data and the child side's set, by labels (an
	// unlabeled app following its own database, an import app without
	// data), the closure because up creates dependencies but never
	// dependents. Created with one up, as the run step's up would a
	// moment later, so not in a profile that is not up (an up naming one
	// starts it); only then: a service without a container in a
	// project that has others was removed on purpose. The record says so
	// (imports created some since); without one, no containers.
	rec, recorded := readSyncRecord(cwd)
	first := rec.first
	if !recorded {
		states, err := serviceStates(cwd)
		if err != nil {
			return err
		}
		first = len(states) == 0
	}
	var created []string
	if first && o.create {
		children := cli.StoppedNames(cli.DataStops(services, cli.SyncChild, ""))
		profiles := composeProfiles(cwd)
		for _, s := range services {
			if (s.Copies() || slices.Contains(children, s.Name)) && s.Active(profiles) {
				created = append(created, s.Name)
			}
		}
	}
	if len(created) > 0 {
		report.Info("creating " + strings.Join(created, " "))
		if err := compose(cwd, errOut, append([]string{"up", "-d", "--wait"}, created...)...); err != nil {
			errs = append(errs, fmt.Errorf("docker compose up: %w", err))
		}
	}
	stateFile := filepath.Join(filepath.Dir(cwd), syncState)
	// What a sync stopped comes back whatever its labels say now (an
	// inherited variable may have changed them since a cut one), if the
	// project still has it: compose start refuses a service it lacks.
	if recorded {
		var names []string
		for _, s := range services {
			names = append(names, s.Name)
		}
		stop = intersect(rec.stops, names)
	}
	var start []string
	if len(stop) > 0 {
		exited, err := servicesIn(cwd, "exited")
		if err != nil {
			errs = append(errs, err)
		}
		start = intersect(stop, exited)
	}
	// On a complete copy (recreate: a sync's finish go, a complete
	// import), a service whose image devopsy's compose file changed since
	// its container was made (another activation's container, a hold) is
	// made again, with no other: compose start would run the old image on
	// the new data, and one the copy's data landed under as it ran
	// (devopsy.data.stop never: written, what this copy wrote) would run on
	// with it, never one it did not touch (review: an import made a release
	// cut past its switch live on another service, without its run step).
	// Never otherwise: an abort in a release that syncs restores the
	// child's own data, which the release's images, not live, must not
	// touch (the release goes back).
	var again []string
	if o.recreate {
		changed := images.Recreate(images.CLI{}, composeName(cwd), remote.ReleaseFile(filepath.Join(cwd, cli.ProjectDirName), ""))
		again = intersect(start, changed)
		if through := intersect(o.written, changed); len(through) > 0 {
			running, err := servicesIn(cwd, "running")
			if err != nil {
				errs = append(errs, err)
			}
			for _, s := range intersect(through, running) {
				if !slices.Contains(again, s) {
					again = append(again, s)
				}
			}
		}
	}
	if len(again) > 0 {
		report.Info("recreating " + strings.Join(again, " ") + " on the image its release gives it now")
		if err := compose(cwd, errOut, append([]string{"up", "-d", "--no-deps", "--wait"}, again...)...); err != nil {
			errs = append(errs, fmt.Errorf("docker compose up: %w", err))
		}
		var rest []string
		for _, s := range start {
			if !slices.Contains(again, s) {
				rest = append(rest, s)
			}
		}
		start = rest
	}
	if len(start) > 0 {
		report.Info("starting " + strings.Join(start, " "))
		if err := compose(cwd, errOut, append([]string{"start"}, start...)...); err != nil {
			errs = append(errs, fmt.Errorf("docker compose start: %w", err))
		} else if err := waitHealthy(cwd, start); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		_ = os.Remove(stateFile)
	}
	return errors.Join(errs...)
}

// psEntry is a container in docker compose ps --format json.
type psEntry struct {
	Name, Service, State, Health, Status, Labels string
	ExitCode                                     int
}

// serviceEntries is parsePS without one-off containers: a stray run --rm
// (an import's, still running) must not stand for its service, or a
// stopped service would read as running and never be started again.
func serviceEntries(out []byte) ([]psEntry, error) {
	entries, err := parsePS(out)
	return slices.DeleteFunc(entries, psEntry.oneOff), err
}

// parsePS reads docker compose ps --format json: one object per line, or
// an array (older compose).
func parsePS(out []byte) ([]psEntry, error) {
	out = bytes.TrimSpace(out)
	var entries []psEntry
	if len(out) == 0 {
		return nil, nil
	}
	if out[0] == '[' {
		err := json.Unmarshal(out, &entries)
		return entries, err
	}
	for _, line := range bytes.Split(out, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e psEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// serviceStates is the state of each service's container (docker compose
// ps: running, paused, exited...), a running one winning when a service
// has several.
func serviceStates(cwd string) (map[string]string, error) {
	var out bytes.Buffer
	if err := compose(cwd, &out, "ps", "--all", "--format", "json"); err != nil {
		return nil, fmt.Errorf("docker compose ps: %w", err)
	}
	entries, err := serviceEntries(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("docker compose ps: %w", err)
	}
	states := map[string]string{}
	for _, e := range entries {
		if states[e.Service] != "running" {
			states[e.Service] = e.State
		}
	}
	return states, nil
}

// waitHealthy waits until every container of services runs and, when it
// has a healthcheck, is healthy.
func waitHealthy(cwd string, services []string) error {
	deadline := time.Now().Add(healthyTimeout)
	for {
		var out bytes.Buffer
		if err := compose(cwd, &out, append([]string{"ps", "--all", "--format", "json"}, services...)...); err != nil {
			return fmt.Errorf("docker compose ps: %w", err)
		}
		entries, err := serviceEntries(out.Bytes())
		if err != nil {
			return fmt.Errorf("docker compose ps: %w", err)
		}
		// Every service must have a container: one gone cannot be started.
		ready := true
		for _, name := range services {
			if !slices.ContainsFunc(entries, func(e psEntry) bool { return e.Service == name }) {
				ready = false
			}
		}
		for _, e := range entries {
			switch {
			case e.State == "exited" || e.State == "dead" || e.Health == "unhealthy":
				return fmt.Errorf("%s is %s after starting", e.Service, cmp.Or(e.Health, e.State))
			case e.State != "running" || (e.Health != "" && e.Health != "healthy"):
				ready = false
			}
		}
		if ready {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not healthy after %s", strings.Join(services, " "), healthyTimeout)
		}
		time.Sleep(time.Second)
	}
}

// oneOff reports a compose run container (a hook's run --rm): ps lists
// it under its service, but it is not the service's container.
func (e psEntry) oneOff() bool {
	return strings.Contains(","+e.Labels+",", ",com.docker.compose.oneoff=True,")
}
