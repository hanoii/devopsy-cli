// Package pathlock is a lock on a file by its path: a flock on a descriptor opened
// close-on-exec, checked after each take to be on the file the path names now, since one who
// removes the file (a claim's free, a project user's destroy) may do so while another waits
// on it, and a lock on a removed file is on nothing.
//
// Rules: internals/uids.md.
package pathlock

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// ErrReplaced is a take's answer when the path no longer names the file the lock was opened
// on: removed, or removed and made again.
var ErrReplaced = errors.New("the lock's file was removed")

// Lock is a lock's file, open, held or not.
type Lock struct {
	f    *os.File
	path string
}

// Take opens path's lock, making the file (0600) when it is not there, and takes it, shared
// (syscall.LOCK_SH) or exclusive (LOCK_EX), through wait, which waits, announced and bounded,
// until try takes it, what naming it; a nil wait blocks until it is free. A file removed
// while it waited is opened again where the path is now, and taken again.
func Take(path, what string, how int, wait func(what string, try func() (bool, error)) error) (*Lock, error) {
	for {
		// Read-write: flock is fcntl's lock on NFS, where an exclusive one needs it.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_CLOEXEC, 0o600)
		if err != nil {
			return nil, err
		}
		if wait == nil {
			err = syscall.Flock(int(f.Fd()), how)
		} else {
			err = wait(what, func() (bool, error) { return TryFlock(f, how) })
		}
		if err != nil {
			f.Close()
			return nil, err
		}
		if same, err := samePath(f, path); err != nil || !same {
			f.Close()
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			continue
		}
		return &Lock{f: f, path: path}, nil
	}
}

// Open opens path's lock without making it or taking it, for a holder that tries it again and
// again (Try): os.ErrNotExist when the file is not there.
func Open(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return &Lock{f: f, path: path}, nil
}

// Try takes the lock now (how, syscall.LOCK_SH or LOCK_EX), without waiting: false when another
// holds it, ErrReplaced, the lock let go again, when the path no longer names its file.
func (l *Lock) Try(how int) (bool, error) {
	ok, err := TryFlock(l.f, how)
	if !ok || err != nil {
		return ok, err
	}
	if same, err := samePath(l.f, l.path); err != nil || !same {
		l.Unlock()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		return false, ErrReplaced
	}
	return true, nil
}

// Unlock lets the lock go, its file kept open for another Try.
func (l *Lock) Unlock() error { return syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN) }

// File is the lock's open file, for a command that is to hold the lock while it runs
// (exec.Cmd's ExtraFiles): its descriptor there shares the lock.
func (l *Lock) File() *os.File { return l.f }

// Fd is the lock's descriptor.
func (l *Lock) Fd() uintptr { return l.f.Fd() }

// Path is the lock's path.
func (l *Lock) Path() string { return l.path }

// KeepAcrossExec clears the lock's close-on-exec flag, right before an exec, so the command
// exec'd holds the lock for as long as it runs and the kernel drops it when it ends.
func (l *Lock) KeepAcrossExec() error {
	_, err := unix.FcntlInt(l.f.Fd(), unix.F_SETFD, 0)
	return err
}

// Close lets the lock go and closes its file.
func (l *Lock) Close() error { return l.f.Close() }

// TryFlock takes f's lock (how, syscall.LOCK_EX or LOCK_SH) if it is free now: false, with no
// error, when another holds it.
func TryFlock(f *os.File, how int) (bool, error) {
	err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

func samePath(f *os.File, path string) (bool, error) {
	held, err := f.Stat()
	if err != nil {
		return false, err
	}
	now, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return os.SameFile(held, now), nil
}
