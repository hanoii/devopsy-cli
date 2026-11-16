package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// serverHold is what a change of the whole server holds (holdServer), let go by release.
type serverHold []*os.File

// release lets go of every lock h holds.
func (h serverHold) release() {
	for _, l := range h {
		l.Close()
	}
}

// holdHow is how holdServer takes its locks.
type holdHow struct {
	// Setup takes the apply lock beside the server's record too (setup.LockPath): a caller
	// inside an apply holds it already, and a second flock on a new open of the file would
	// refuse itself.
	Setup bool
	// Look lets nothing be created, a file not there having no holder, and lets go at once: a
	// look changes nothing. It takes each lock exclusively as the act does, since a sync's
	// push and an export hold an environment's shared, and the act refuses them.
	Look bool
}

// holdServer takes, at once and never waiting, the release lock of every deploy-user
// environment in root and, with how.Setup, the apply lock: what a change of the whole server
// (Docker's restart, a reboot) must never cut. Its string says what holds one, "" when it took
// them all, the caller's words following it; a busy or failed hold lets go of what it took, and
// a look lets go of them at once, its caller only asking whether it could. Releases wait for
// their lock (lockWait), so the instant a look holds delays none.
func holdServer(root string, how holdHow) (serverHold, string, error) {
	var hold serverHold
	// What was taken before a busy or failed lock is let go: the caller gets nothing to hold.
	refuse := func(busy string, err error) (serverHold, string, error) {
		hold.release()
		return nil, busy, err
	}
	flags := os.O_CREATE | os.O_WRONLY
	if how.Look {
		flags = os.O_RDONLY
	}
	take := func(path string) (bool, error) {
		l, err := os.OpenFile(path, flags, 0o644)
		if how.Look && errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		hold = append(hold, l)
		return tryFlock(l, syscall.LOCK_EX)
	}
	if how.Setup {
		// For a look, no record's home or no lock there is no apply running: an apply makes them.
		if lock := setup.LockPath(setup.RecordPath(remote.UserConfigFile())); lock != "" {
			// A hold that keeps its locks makes the record's home, as an apply does
			// (applyLock): one made after the hold would let an apply start under it.
			if !how.Look {
				if err := os.MkdirAll(filepath.Dir(lock), 0o700); err != nil {
					return refuse("", err)
				}
			}
			ok, err := take(lock)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return refuse("", err)
			}
			if err == nil && !ok {
				return refuse("a setup of this server runs", nil)
			}
		}
	}
	bases, err := deployEnvBases(root)
	if err != nil {
		return refuse("", err)
	}
	for _, base := range bases {
		ok, err := take(filepath.Join(base, ".lock"))
		if err != nil {
			return refuse("", err)
		}
		if !ok {
			return refuse("a release or another change runs in "+base, nil)
		}
	}
	if how.Look {
		hold.release()
		hold = nil
	}
	return hold, "", nil
}

// projectUsersNotWaited says, when the server has project users, that a change of the whole
// server (what) cuts their releases: holdServer holds the deploy user's alone.
func projectUsersNotWaited(what string) {
	if d, err := remote.ServerDoor(); err == nil && d != nil && len(d.Users) > 0 {
		report.Info("project users' releases are not waited for: one under way is cut by " + what)
	}
}
