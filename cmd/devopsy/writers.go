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

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// writersWait is how long endWriters waits for the imports it signalled
// before killing them: runSyncHook's own WaitDelay, and some.
var writersWait = 15 * time.Second

// endWriters ends whatever the sync's record (remote.SyncTakenFile, in
// the child's directory base) names that may still write into the
// child, before anything is decided on its data: the holder's partial,
// settle. A write runs on past its sync when its devopsy is killed
// outright, or inside a container, which never gets the signal: an
// import's load through docker exec, a one-off run --rm, devopsy's own
// rsync. So, in order: the recorded imports' devopsy (SIGTERM, only a
// process still a devopsy import), the sync's own containers (by name,
// remote.SyncContainerPrefix and its id), the imported services
// (stopped: the only way to end what docker exec started in them; one
// running before the sync is recorded, so the next completing sync
// starts it) and their one-off containers (removed); then the imports
// are waited for, and killed after writersWait.
func endWriters(cwd, base string) error {
	data, err := os.ReadFile(filepath.Join(base, remote.SyncTakenFile))
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	first := strings.Fields(lines[0])
	var pids []int
	var services []string
	was := map[string]string{}
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) != 5 || f[0] != "writer" || f[1] != "import" {
			continue
		}
		if pid, err := strconv.Atoi(f[3]); err == nil {
			pids = append(pids, pid)
		}
		if !slices.Contains(services, f[2]) {
			services = append(services, f[2])
		}
		was[f[2]] = f[4]
	}
	var errs []error
	for _, pid := range pids {
		if isSyncImport(pid) {
			report.Info(fmt.Sprintf("ending the import that may still write (pid %d)", pid))
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	}
	if len(first) == 2 {
		if err := removeContainers("name=" + remote.SyncContainerPrefix + first[1] + "-"); err != nil {
			errs = append(errs, err)
		}
	}
	if len(services) > 0 {
		var out bytes.Buffer
		if err := compose(cwd, &out, append([]string{"ps", "--all", "--format", "json"}, services...)...); err != nil {
			errs = append(errs, fmt.Errorf("docker compose ps: %w", err))
		} else if entries, err := parsePS(out.Bytes()); err != nil {
			errs = append(errs, fmt.Errorf("docker compose ps: %w", err))
		} else {
			var stop, oneOffs []string
			for _, e := range entries {
				switch {
				case !slices.Contains(services, e.Service):
				case e.oneOff():
					oneOffs = append(oneOffs, e.Name)
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
				report.Info("stopping " + strings.Join(stop, " ") + ": an import into it may still write; a sync that completes starts it again")
				if err := compose(cwd, errOut, append([]string{"stop"}, stop...)...); err != nil {
					errs = append(errs, fmt.Errorf("docker compose stop: %w", err))
				} else if err := recordStops(cwd, stop, was); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	waitImports(pids)
	if len(errs) > 0 {
		return fmt.Errorf("ending the sync's writers: %w", errors.Join(errs...))
	}
	return nil
}

// recordStops adds to the sync's record (syncState) the services it
// stopped that ran before the sync, so a completing sync starts them; a
// service the import brought up stays down, as it was.
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
// (--sync-services import): a pid recorded long ago may be another
// process's now. Linux's /proc only: elsewhere nothing is signalled.
func isSyncImport(pid int) bool {
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return false
	}
	args := strings.Split(string(cmdline), "\x00")
	return slices.Contains(args, "--sync-services") && slices.Contains(args, "import")
}

// waitImports waits for the imports to exit, and kills those still
// running after writersWait.
func waitImports(pids []int) {
	deadline := time.Now().Add(writersWait)
	for {
		alive := slices.DeleteFunc(slices.Clone(pids), func(pid int) bool { return !isSyncImport(pid) })
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
	report.Info("removing the sync's containers " + strings.Join(ids, " ") + ": they may still write")
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
