package main

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// TestHoldServerBusyLetsGoReview proves a bug: holdServer's deferred release runs on the named
// result hold, which `return nil, busy, nil` (and `return nil, "", err`) has already set to
// nil, so the locks taken before the busy one stay held by files nobody closes until the
// process ends (or a finalizer runs). Its doc promises a busy or failed hold lets go of them.
func TestHoldServerBusyLetsGoReview(t *testing.T) {
	t.Parallel()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"prod", "stage"} {
		if err := os.MkdirAll(filepath.Join(root, "shop", env, "releases"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	bases, err := deployEnvBases(root)
	if err != nil || len(bases) != 2 {
		t.Fatalf("bases %v: %v", bases, err)
	}
	// The last environment's lock held by another, as a release holds it.
	other, err := os.OpenFile(filepath.Join(bases[1], ".lock"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if ok, err := tryFlock(other, syscall.LOCK_EX); !ok || err != nil {
		t.Fatal("cannot take", bases[1], err)
	}
	hold, busy, err := holdServer(root, holdHow{})
	if err != nil || busy == "" || hold != nil {
		t.Fatalf("hold %v, busy %q, err %v", hold, busy, err)
	}
	// The first environment's lock, which holdServer took before it found the busy one, must be
	// free again.
	first, err := os.OpenFile(filepath.Join(bases[0], ".lock"), os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if ok, err := tryFlock(first, syscall.LOCK_EX); !ok || err != nil {
		t.Errorf("a busy holdServer left %s locked (%v)", bases[0], err)
	}
}

// TestRebootActHoldsSetupWithNoRecordReview proves a gap: the act holds the locks until the
// shutdown so that nothing starts in between (internals/server-setup.md "Rebooting"), but on a
// server whose record's home does not exist yet holdServer takes no setup lock, and a
// --server-setup apply started after the reboot was asked creates the home and its lock and
// runs, to be cut by the shutdown.
func TestRebootActHoldsSetupWithNoRecordReview(t *testing.T) {
	t.Parallel()
	s := newRebootServer(t)
	if err := os.RemoveAll(s.config); err != nil {
		t.Fatal(err)
	}
	cmd := s.cmd(remote.RebootStepFlag, "act", "3")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	sc := bufio.NewScanner(out)
	for sc.Scan() && sc.Text() != remote.RebootAsked {
	}
	// What applyLock does, as an apply started now would.
	record := setup.RecordPath(filepath.Join(s.config, remote.ConfigFile))
	if err := os.MkdirAll(filepath.Dir(record), 0o700); err != nil {
		t.Fatal(err)
	}
	l, err := os.OpenFile(setup.LockPath(record), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if ok, _ := tryFlock(l, syscall.LOCK_EX); ok {
		t.Error("a setup took its lock while the reboot's act held the server")
	}
	_, _ = io.Copy(io.Discard, out)
}
