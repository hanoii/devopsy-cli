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
