package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// runSyncKey is the hidden --sync-key (remote.SyncKeyFlag) add <id> <base> <to> <reset|->
// <blob> | remove <id>: a sync's key on the child's server, the holder's, whatever its user,
// so no script branches on it. The deploy user writes its own authorized_keys; a project
// user, who never edits its keys, asks the door, which writes its sync keys
// (remote.DoorSyncKeysFile).
func runSyncKey(args []string) int {
	if behindDoor() {
		if _, err := doorAsk(true, append([]string{"sync-key"}, args...)...); err != nil {
			if !doorRefusedIt(err) {
				return fail("the door: " + err.Error())
			}
			return 1
		}
		return 0
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail(err.Error())
	}
	if err := editSyncKeys(filepath.Join(home, ".ssh", "authorized_keys"), selfPath(), args, time.Now()); err != nil {
		return fail(err.Error())
	}
	return 0
}

// selfPath is this devopsy's absolute path, links resolved: what a sync key's forced
// command runs, since sshd runs it with a PATH of its own.
func selfPath() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	if p, err := filepath.EvalSymlinks(self); err == nil {
		return p
	}
	return self
}

// editSyncKeys does --sync-key's args to file, an authorized_keys, under the lock the
// --ssh-keys scripts take beside it (keysLock), so neither loses the other's edit: add
// writes the sync's line (remote.SyncKeyLine, devopsy the forced command's), one per sync,
// sweeping the sync keys whose expiry passed; remove drops a sync's, if any. Every other
// line stays as it is.
func editSyncKeys(file, devopsy string, args []string, now time.Time) error {
	usage := errors.New("usage: devopsy " + remote.SyncKeyFlag + " add <id> <base> <to> <reset|-> <blob> | remove <id>")
	switch {
	case len(args) == 6 && args[0] == "add" && (args[4] == "reset" || args[4] == "-"):
		line, err := remote.SyncKeyLine(devopsy, args[1], args[2], args[3], args[4] == "reset", args[5], now.Add(remote.SyncKeyLife))
		if err != nil {
			return err
		}
		return lockedKeys(file, func(lines []string) ([]string, error) {
			var kept []string
			for _, l := range lines {
				id, exp, sync := syncKeyOf(l)
				if sync && id == args[1] {
					return nil, fmt.Errorf("sync %s has a key already", id)
				}
				if sync && !exp.After(now) {
					continue
				}
				kept = append(kept, l)
			}
			return append(kept, line), nil
		})
	case len(args) == 2 && args[0] == "remove" && remote.SyncIDPattern.MatchString(args[1]):
		return lockedKeys(file, func(lines []string) ([]string, error) {
			var kept []string
			for _, l := range lines {
				if id, _, sync := syncKeyOf(l); !sync || id != args[1] {
					kept = append(kept, l)
				}
			}
			return kept, nil
		})
	}
	return usage
}

// syncKeyOf says whether line is a sync's key, and which sync's, until when.
func syncKeyOf(line string) (id string, expiry time.Time, ok bool) {
	k, err := remote.ParseAuthorizedKey(line)
	if err != nil || k == nil {
		return "", time.Time{}, false
	}
	if id, ok = remote.SyncKeyID(*k); !ok {
		return "", time.Time{}, false
	}
	expiry, _ = remote.SyncKeyExpiry(*k)
	return id, expiry, true
}

// lockedKeys rewrites file's lines through edit under the lock beside it (the --ssh-keys
// scripts' keysLock: <dir>/.<name>.lock), into a file of mode 0600 renamed over it: an edit
// cut halfway leaves the old file. A file that ends up empty is removed.
func lockedKeys(file string, edit func([]string) ([]string, error)) error {
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "."+filepath.Base(file)+".lock"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for deadline := time.Now().Add(time.Minute); ; time.Sleep(100 * time.Millisecond) {
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			break
		} else if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			return fmt.Errorf("another edit of %s did not end in a minute", file)
		}
	}
	data, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var lines []string
	if s := strings.TrimRight(string(data), "\n"); s != "" {
		lines = strings.Split(s, "\n")
	}
	lines, err = edit(lines)
	if err != nil {
		return err
	}
	if len(lines) == 0 {
		if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}
