package pathlock

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A holder that tries again and again (Open, Try) never makes the file, takes it only when
// it is free, and is told when its path names another file by then, its lock let go: a lock
// on a removed file is on nothing, and the file made anew is another holder's.
func TestTryOnTheFileThePathNames(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "x.lock")
	if _, err := Open(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("opened a lock that is not there: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Open made the file")
	}
	shared, err := Take(path, "x", syscall.LOCK_SH, nil)
	if err != nil {
		t.Fatal(err)
	}
	l, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if ok, err := l.Try(syscall.LOCK_EX); ok || err != nil {
		t.Fatalf("taken exclusive beside a shared holder: %v, %v", ok, err)
	}
	shared.Close()
	if ok, err := l.Try(syscall.LOCK_EX); !ok || err != nil {
		t.Fatalf("not taken once free: %v, %v", ok, err)
	}
	if err := l.Unlock(); err != nil {
		t.Fatal(err)
	}
	// Removed and made again: the path names another file.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	other, err := Take(path, "x", syscall.LOCK_SH, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if ok, err := l.Try(syscall.LOCK_EX); ok || !errors.Is(err, ErrReplaced) {
		t.Fatalf("a replaced file: %v, %v", ok, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if ok, err := l.Try(syscall.LOCK_EX); ok || !errors.Is(err, ErrReplaced) {
		t.Fatalf("a removed file: %v, %v", ok, err)
	}
}

// Release lets a lock go whatever copies of its descriptor are open (a fork in flight holds
// one until its exec): closing alone would keep it held until every copy closes, so a lock
// let go would stay busy for whoever takes it next.
func TestReleaseWithACopyOpen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "lock")
	open := func() *os.File {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_CLOEXEC, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	held := open()
	if ok, err := TryFlock(held, syscall.LOCK_EX); !ok || err != nil {
		t.Fatalf("take: %v, %v", ok, err)
	}
	// A fork's copy of the descriptor: the same open file, and with it the lock.
	copied, err := syscall.Dup(int(held.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(copied)
	if err := Release(held); err != nil {
		t.Fatal(err)
	}
	next := open()
	defer next.Close()
	if ok, err := TryFlock(next, syscall.LOCK_EX); !ok || err != nil {
		t.Errorf("the lock stays held after its release while a copy is open: %v, %v", ok, err)
	}
}
