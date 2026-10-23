package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// writersWait is how long endWriters waits for the imports it signalled
// before killing them: runDataHook's own WaitDelay, and some.
var writersWait = 15 * time.Second

// endWriters ends whatever the sync's record (remote.SyncTakenFile, in
// the child's directory base) names that may still write into the
// child, before anything is decided on its data: the holder's partial,
// settle. A write runs on past its sync when its devopsy is killed
// outright, or inside a container, which never gets the signal: an
// import's load through docker exec, a one-off run --rm, devopsy's own
// rsync. So, in order: the recorded imports' devopsy (SIGTERM, only a
// process still a devopsy import), the sync's own containers (by name,
// remote.SyncContainerPrefix and its id), the imported services and
// those depending on them (services, cli.WithDependents; stopped: the
// only way to end what docker exec started in them; one running before
// the sync is recorded, so the next completing sync starts it, and a
// dependent stopped here is) and their one-off containers (removed);
// then the imports are waited for, and killed after writersWait. Without
// services (labels it could not read) only the imported ones stop, said.
func endWriters(cwd, base string, services []cli.SyncService) error {
	data, err := os.ReadFile(filepath.Join(base, remote.SyncTakenFile))
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	first := strings.Fields(lines[0])
	var pids []int
	var imported []string
	was := map[string]string{}
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) != 5 || f[0] != "writer" || f[1] != "import" {
			continue
		}
		if pid, err := strconv.Atoi(f[3]); err == nil {
			pids = append(pids, pid)
		}
		if !slices.Contains(imported, f[2]) {
			imported = append(imported, f[2])
		}
		was[f[2]] = f[4]
	}
	// What depends on an imported service stops with it.
	stopping := imported
	if len(imported) > 0 && services == nil {
		report.Warn("the project's labels could not be read: stopping only " + strings.Join(imported, " ") + ", not what depends on them")
	} else if len(imported) > 0 {
		stopping = cli.StoppedNames(cli.WithDependents(services, imported, "imported"))
	}
	var errs []error
	for _, pid := range pids {
		if isSyncImport(pid, base) {
			report.Info(fmt.Sprintf("ending the import that may still write (pid %d)", pid))
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	}
	if len(first) == 2 {
		if err := removeContainers("name=" + remote.SyncContainerPrefix + first[1] + "-"); err != nil {
			errs = append(errs, err)
		}
	}
	if len(stopping) > 0 {
		var out bytes.Buffer
		if err := compose(cwd, &out, append([]string{"ps", "--all", "--format", "json"}, stopping...)...); err != nil {
			errs = append(errs, fmt.Errorf("docker compose ps: %w", err))
		} else if entries, err := parsePS(out.Bytes()); err != nil {
			errs = append(errs, fmt.Errorf("docker compose ps: %w", err))
		} else {
			var stop, oneOffs []string
			for _, e := range entries {
				switch {
				case !slices.Contains(stopping, e.Service):
				case e.oneOff():
					if slices.Contains(imported, e.Service) {
						oneOffs = append(oneOffs, e.Name)
					}
				case e.State == "running" || e.State == "restarting":
					if !slices.Contains(stop, e.Service) {
						stop = append(stop, e.Service)
					}
				}
			}
			if len(oneOffs) > 0 {
				report.Info("removing " + strings.Join(oneOffs, " ") + ": an import may still write through it")
				if err := dockerRm(oneOffs); err != nil {
					errs = append(errs, err)
				}
			}
			if len(stop) > 0 {
				report.Info("stopping " + strings.Join(stop, " ") + ": an import may still write; a sync that completes starts them again")
				// What depends on an imported service was running when
				// stopped here: recorded.
				for _, s := range stop {
					if !slices.Contains(imported, s) {
						was[s] = "running"
					}
				}
				if err := compose(cwd, errOut, append([]string{"stop"}, stop...)...); err != nil {
					errs = append(errs, fmt.Errorf("docker compose stop: %w", err))
				} else if err := recordStops(cwd, stop, was); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	waitImports(pids, base)
	if len(errs) > 0 {
		return fmt.Errorf("ending the sync's writers: %w", errors.Join(errs...))
	}
	return nil
}

// recordStops adds to the sync's record (syncState) the services it
// stopped that ran before the sync (was), so a completing sync starts
// them; a service the import brought up stays down, as it was.
func recordStops(cwd string, stopped []string, was map[string]string) error {
	rec, _ := readSyncRecord(cwd)
	for _, s := range stopped {
		if was[s] == "running" && !slices.Contains(rec.stops, s) {
			rec.stops = append(rec.stops, s)
		}
	}
	return rec.write(cwd)
}

// isSyncImport reports whether pid is still a devopsy sync import
// (--sync-services import) into the environment at base, by its working
// directory (devopsy never changes it): a pid recorded long ago may be
// another process's now, another environment's import after a reboot
// included. Linux's /proc only: elsewhere nothing is signalled.
func isSyncImport(pid int, base string) bool {
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	args := strings.Split(string(cmdline), "\x00")
	if !slices.Contains(args, "--sync-services") || !slices.Contains(args, "import") {
		return false
	}
	dir, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil {
		return false
	}
	if b, err := filepath.EvalSymlinks(base); err == nil {
		base = b
	}
	return dir == base || strings.HasPrefix(dir, base+string(filepath.Separator))
}

// waitImports waits for the imports into base to exit, and kills those
// still running after writersWait.
func waitImports(pids []int, base string) {
	deadline := time.Now().Add(writersWait)
	for {
		alive := slices.DeleteFunc(slices.Clone(pids), func(pid int) bool { return !isSyncImport(pid, base) })
		if len(alive) == 0 {
			return
		}
		if time.Now().After(deadline) {
			for _, pid := range alive {
				report.Warn(fmt.Sprintf("the import (pid %d) did not end: killing it", pid))
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// removeContainers removes every container matching docker ps's filter.
func removeContainers(filter string) error {
	out, err := exec.Command("docker", "ps", "-aq", "--filter", filter).Output()
	if err != nil {
		return fmt.Errorf("docker ps: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil
	}
	report.Info("removing the containers " + strings.Join(ids, " ") + ": they may still write")
	return dockerRm(ids)
}

// dockerRm removes containers, bounded: a hung Docker never holds the
// lock for good.
func dockerRm(names []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", append([]string{"rm", "-f"}, names...)...).CombinedOutput(); err != nil {
		return fmt.Errorf("docker rm -f %s: %v: %s", strings.Join(names, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// endImport ends an import into name that failed or was cut, which may
// still write (a docker exec never gets the cut, a killed devopsy's
// container extracts on): name's one-off containers and the project's
// import containers (remote.ImportContainerPrefix) go, and name and what
// depends on it (cli.WithDependents) are stopped where they run, never
// started again: the data may be partial. With record (on a target) what
// it stops is recorded (syncState), name only when wasRunning (it ran
// before the import: a service down on purpose stays so). It returns
// what it stopped.
func endImport(cwd, name string, services []cli.SyncService, record, wasRunning bool) ([]string, error) {
	var errs []error
	closure := cli.StoppedNames(cli.WithDependents(services, []string{name}, "imported"))
	if !slices.Contains(closure, name) {
		closure = append([]string{name}, closure...)
	}
	if err := removeContainers("name=^/?" + remote.ImportContainerPrefix + composeName(cwd) + "-[0-9]+$"); err != nil {
		errs = append(errs, err)
	}
	var stop []string
	var out bytes.Buffer
	if err := compose(cwd, &out, append([]string{"ps", "--all", "--format", "json"}, closure...)...); err != nil {
		errs = append(errs, fmt.Errorf("docker compose ps: %w", err))
	} else if entries, err := parsePS(out.Bytes()); err != nil {
		errs = append(errs, fmt.Errorf("docker compose ps: %w", err))
	} else {
		var oneOffs []string
		for _, e := range entries {
			switch {
			case !slices.Contains(closure, e.Service):
			case e.oneOff():
				if e.Service == name {
					oneOffs = append(oneOffs, e.Name)
				}
			case e.State == "running" || e.State == "restarting":
				if !slices.Contains(stop, e.Service) {
					stop = append(stop, e.Service)
				}
			}
		}
		if len(oneOffs) > 0 {
			report.Info("removing " + strings.Join(oneOffs, " ") + ": the import may still write through it")
			if err := dockerRm(oneOffs); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(stop) > 0 {
		report.Info("stopping " + strings.Join(stop, " ") + ": the import may still write, and its data may be partial")
		if err := compose(cwd, errOut, append([]string{"stop"}, stop...)...); err != nil {
			errs = append(errs, fmt.Errorf("docker compose stop: %w", err))
		} else if record {
			was := map[string]string{}
			for _, s := range stop {
				if s != name || wasRunning {
					was[s] = "running"
				}
			}
			if err := recordStops(cwd, stop, was); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return stop, errors.Join(errs...)
}

// endLeftoverImport ends what an import of name a killed devopsy left may
// still write, before anything else under the release lock (a retry of
// it, --resolved, a sync's prepare): the hook's process group the record
// names (its writer lines, SIGTERM, only while /proc shows the data
// capability's import there, then SIGKILL after writersWait), then
// endImport, recording what it stops. A reboot ended it all already.
func endLeftoverImport(cwd, base string, services []cli.SyncService, name string) error {
	rec, _ := readSyncRecord(cwd)
	compose := composeName(cwd)
	var alive []int
	for _, pgid := range rec.writers {
		if isDataImport(pgid, compose) {
			report.Info(fmt.Sprintf("ending the import of %s a devopsy killed outright left (process group %d)", name, pgid))
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
			alive = append(alive, pgid)
		}
	}
	deadline := time.Now().Add(writersWait)
	for len(alive) > 0 {
		alive = slices.DeleteFunc(alive, func(p int) bool { return !isDataImport(p, compose) })
		if len(alive) == 0 {
			break
		}
		if time.Now().After(deadline) {
			for _, p := range alive {
				report.Warn(fmt.Sprintf("the import (process group %d) did not end: killing it", p))
				_ = syscall.Kill(-p, syscall.SIGKILL)
			}
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if len(rec.writers) > 0 {
		if err := recordWriter(cwd, 0, false, false); err != nil {
			return err
		}
	}
	// What it brought up for itself was down before: it stays down.
	_, err := endImport(cwd, name, services, true, !rec.writerDown)
	return err
}

// isDataImport reports whether pid is still the data capability's import
// (the hook's script, or the devopsy about to exec it) of the compose
// project compose, by the environment it started with (a hook may change
// its directory): a pid recorded long ago may be another process's now,
// another environment's import after a reboot included. Linux's /proc
// only.
func isDataImport(pid int, compose string) bool {
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
	if err != nil || !slices.Contains(strings.Split(string(environ), "\x00"), "COMPOSE_PROJECT_NAME="+compose) {
		return false
	}
	args := strings.Split(string(cmdline), "\x00")
	hook := "capabilities/" + cli.DataCapability + "/import"
	return slices.ContainsFunc(args, func(a string) bool { return strings.HasSuffix(a, hook) }) ||
		slices.Contains(args, "--capability") && slices.Contains(args, cli.DataCapability) && slices.Contains(args, "import")
}

// recordWriter adds the import hook's process group (pgid, add) to the
// record (syncState), down when its service was not running before the
// import, or with add false removes every writer line: the next
// lock-taker ends a hook a killed devopsy left by it, and starts the
// service again later only if it ran.
func recordWriter(cwd string, pgid int, add, down bool) error {
	rec, _ := readSyncRecord(cwd)
	if add {
		rec.writers = append(rec.writers, pgid)
		rec.writerDown = down
	} else {
		rec.writers, rec.writerDown = nil, false
	}
	return rec.write(cwd)
}
