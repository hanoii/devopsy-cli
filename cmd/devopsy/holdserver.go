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
	// Look takes every lock shared and lets nothing be created: a file not there has no
	// holder. A look changes nothing, and two looks never refuse each other.
	Look bool
}

// holdServer takes, at once and never waiting, the release lock of every deploy-user
// environment in root and, with how.Setup, the apply lock: what a change of the whole server
// (Docker's restart, a reboot) must never cut. busy says what holds one, "" when it took them
// all, the caller's words following it; a look lets go of them at once, its caller only
// asking whether it could. Releases wait for their lock (lockWait), so the instant a look
// holds delays none.
func holdServer(root string, how holdHow) (hold serverHold, busy string, err error) {
	defer func() {
		if busy != "" || err != nil {
			hold.release()
			hold = nil
		}
	}()
	lockHow, flags := syscall.LOCK_EX, os.O_CREATE|os.O_WRONLY
	if how.Look {
		lockHow, flags = syscall.LOCK_SH, os.O_RDONLY
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
		return tryFlock(l, lockHow)
	}
	if how.Setup {
		// No record's home, or no lock there, is no apply running: an apply creates it.
		if lock := setup.LockPath(setup.RecordPath(remote.UserConfigFile())); lock != "" {
			ok, err := take(lock)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, "", err
			}
			if err == nil && !ok {
				return nil, "a setup of this server runs", nil
			}
		}
	}
	bases, err := deployEnvBases(root)
	if err != nil {
		return nil, "", err
	}
	for _, base := range bases {
		ok, err := take(filepath.Join(base, ".lock"))
		if err != nil {
			return nil, "", err
		}
		if !ok {
			return nil, "a release or another change runs in " + base, nil
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
