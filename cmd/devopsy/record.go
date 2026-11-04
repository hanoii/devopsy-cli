package main

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// syncState records, next to syncLog, the services a sync or an import
// stopped ("stop db" lines), so restoring brings back those and leaves a
// service that was down before down (a maintenance stop). Written before
// the first stop, removed by a restore that brought everything back;
// absent, a sync restores its side's set (cli.DataStops), an import or
// --resolved nothing (its mark and record are written together).
const syncState = remote.SyncStateFile

// markIncomplete marks the environment at base as one no sync completed
// on, or whose import did not complete (remote.SyncIncompleteFile,
// remote.IncompleteLine): code and why say what happened, from the
// parent's address (a sync's: "" takes it from the sync's record or
// environment) or the imported service. An earlier mark gives its time
// (since when); keep leaves a sync's as it is, and replaces an import's:
// the data is partly a sync's now.
func markIncomplete(base, from, code, why string, keep bool) error {
	path := filepath.Join(base, remote.SyncIncompleteFile)
	since := time.Now().UTC().Format("2006-01-02 15:04:05")
	if old, err := os.ReadFile(path); err == nil {
		m := remote.ParseMark(string(old))
		if keep && !m.IsImport() {
			return nil
		}
		// An import's mark is timed at its latest write: --resolved keeps a
		// window that landed since, never one from an import before that a
		// retry then wrote over (review).
		if m.Since != "" && code != remote.IncompleteImport {
			since = m.Since
		}
	}
	if from == "" {
		// Who wrote it: the parent the gate named (remote.SyncOpenFile's
		// line, carried into the record), else this sync's.
		record, err := os.ReadFile(filepath.Join(base, remote.SyncTakenFile))
		if err != nil {
			record, _ = os.ReadFile(filepath.Join(base, remote.SyncStartedFile))
		}
		from, _, _ = strings.Cut(strings.TrimSpace(string(record)), " ")
		from = cmp.Or(from, os.Getenv(remote.SyncFromTargetEnv), "unknown")
	}
	line := remote.IncompleteLine(since, from, code, why)
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		return fmt.Errorf("marking %s as partial: %w", base, err)
	}
	return nil
}

// claimWrite claims the right to write into the child at base before
// the first write: the holder's gate of this sync (remote.SyncOpenFile,
// named by remote.SyncIDEnv) renamed to remote.SyncStartedFile, one
// rename or the holder's unlink winning (first: this is the sync's
// first write, which a reset wipes before), or a write of this sync
// claimed it already. Neither: the holder decided, and nothing may be
// written into it.
func claimWrite(base string) (first bool, err error) {
	id := os.Getenv(remote.SyncIDEnv)
	open, started := filepath.Join(base, remote.SyncOpenFile(id)), filepath.Join(base, remote.SyncStartedFile)
	if id != "" && os.Rename(open, started) == nil {
		return true, nil
	}
	if recordOf(started, id) {
		return false, nil
	}
	return false, fmt.Errorf("the child's side ended already (%s): nothing copied", base)
}

// recordOf reports whether the gate or record at path is sync id's: its
// first line is "<from> <id>".
func recordOf(path, id string) bool {
	data, err := os.ReadFile(path)
	first, _, _ := strings.Cut(string(data), "\n")
	f := strings.Fields(first)
	return err == nil && len(f) == 2 && f[1] == id
}

// errDecided is a write into a child whose holder decided already: its
// record (remote.SyncStartedFile) is taken, or another sync's.
var errDecided = errors.New("the child's side decided already: this write does not count")

// recordWrite appends line (remote.WriterLine, remote.DoneLine) to the
// sync's record in the child's directory base, never creating it: once
// the holder took it, the write is not this sync's to make.
func recordWrite(base, line string) error {
	path := filepath.Join(base, remote.SyncStartedFile)
	if !recordOf(path, os.Getenv(remote.SyncIDEnv)) {
		return errDecided
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if errors.Is(err, os.ErrNotExist) {
		return errDecided
	}
	if err != nil {
		return fmt.Errorf("the sync's record: %w", err)
	}
	_, err = f.WriteString(line + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("the sync's record: %w", err)
	}
	return nil
}

// syncFirst is the record's line for a child's first release: it had no
// container at all when the sync started.
const syncFirst = "first"

// syncRecord is the record a sync or an import keeps (syncState):
// whether a sync started on a child's first release (no container at
// all: syncFirst), the services it stopped ("stop <name>" lines), those
// of them a sync stopped itself, by its id ("own <sync id> <name>": what
// a sync that writes nothing starts again; the rest is an earlier sync's
// or import's, kept down under a mark, else started by the next settle:
// settleStops), an import's hook while it runs ("writer
// <process group>", then " down" when its service was not running
// before the import: writerDown), and what a snapshot's stopped pass
// stopped ("snapshot <id> <name>": started again by its own end, or, a
// take killed outright, by the next settle unless the environment is
// marked: settleStops).
type syncRecord struct {
	first      bool
	stops      []string
	own        []string
	ownID      string
	writers    []int
	writerDown bool
	snaps      []string
	snapID     string
}

// readSyncRecord reads the record; ok is false without one.
func readSyncRecord(cwd string) (rec syncRecord, ok bool) {
	state, err := os.ReadFile(filepath.Join(filepath.Dir(cwd), syncState))
	if err != nil {
		return rec, false
	}
	for _, line := range strings.Split(string(state), "\n") {
		action, name, _ := strings.Cut(line, " ")
		switch {
		case line == syncFirst:
			rec.first = true
		case action == "stop" && name != "" && !slices.Contains(rec.stops, name):
			rec.stops = append(rec.stops, name)
		case action == "own":
			if id, svc, ok := strings.Cut(name, " "); ok && svc != "" {
				rec.ownID = id
				rec.own = append(rec.own, svc)
			}
		case action == "snapshot":
			if id, svc, ok := strings.Cut(name, " "); ok && svc != "" && !slices.Contains(rec.snaps, svc) {
				rec.snapID = id
				rec.snaps = append(rec.snaps, svc)
			}
		case action == "writer":
			pgid, down, _ := strings.Cut(name, " ")
			if n, err := strconv.Atoi(pgid); err == nil {
				rec.writers = append(rec.writers, n)
				rec.writerDown = rec.writerDown || down == "down"
			}
		}
	}
	return rec, true
}

// startSyncState makes sure a record exists (syncState) before a sync's
// prepare changes or asks anything: without one, restoring brings back
// the side's whole set, a service down on purpose included. The
// stops of one left by an earlier sync that could not restore stay: their
// services come back with this one's. first is what the sync finds now:
// an earlier first release's mark goes once containers exist.
func startSyncState(cwd string, first bool) error {
	rec, _ := readSyncRecord(cwd)
	rec.first = first
	return rec.write(cwd)
}

// restoreOwnStops starts what a sync that wrote nothing stopped itself
// (the record's own lines with its id: none when its prepare failed
// before stopping, or another sync's) and keeps the rest recorded.
func restoreOwnStops(cwd string) error {
	rec, ok := readSyncRecord(cwd)
	id := os.Getenv(remote.SyncIDEnv)
	if !ok || id == "" || rec.ownID != id {
		return nil
	}
	own := rec.own
	if len(own) > 0 {
		exited, err := servicesIn(cwd, "exited")
		if err != nil {
			return err
		}
		if start := intersect(own, exited); len(start) > 0 {
			report.Info("starting " + strings.Join(start, " ") + ": nothing was written into their data")
			if err := compose(cwd, errOut, append([]string{"start"}, start...)...); err != nil {
				return fmt.Errorf("docker compose start: %w", err)
			}
			if err := waitHealthy(cwd, start); err != nil {
				return err
			}
		}
	}
	rec.stops = slices.DeleteFunc(rec.stops, func(name string) bool { return slices.Contains(own, name) })
	rec.own, rec.ownID = nil, ""
	return rec.write(cwd)
}

// write replaces the record whole.
func (rec syncRecord) write(cwd string) error {
	var b strings.Builder
	if rec.first {
		b.WriteString(syncFirst + "\n")
	}
	for _, name := range rec.stops {
		b.WriteString("stop " + name + "\n")
	}
	for _, name := range rec.own {
		b.WriteString("own " + rec.ownID + " " + name + "\n")
	}
	for _, name := range rec.snaps {
		b.WriteString("snapshot " + rec.snapID + " " + name + "\n")
	}
	for _, pgid := range rec.writers {
		b.WriteString("writer " + strconv.Itoa(pgid))
		if rec.writerDown {
			b.WriteString(" down")
		}
		b.WriteString("\n")
	}
	path := filepath.Join(filepath.Dir(cwd), syncState)
	if err := os.WriteFile(path+".tmp", []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("recording the services to restore: %w", err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return fmt.Errorf("recording the services to restore: %w", err)
	}
	return nil
}
