package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// TestRebootLookRefusesSharedHoldReview proves a bug: a sync from an environment (its push) and
// an --export hold its release lock shared for as long as they run (syncSourceLock). The look
// takes every lock shared too (holdHow.Look), so it finds nothing held, the hostname is asked
// and typed, and only then the act, which takes the locks exclusively, refuses: the look does
// not refuse what refuses the reboot.
func TestRebootLookRefusesSharedHoldReview(t *testing.T) {
	t.Parallel()
	s := newRebootServer(t)
	base := filepath.Join(s.home, "shop", "prod")
	// As syncSourceLock holds them: the release lock shared, the sync lock exclusively.
	for name, how := range map[string]int{".lock": syscall.LOCK_SH, remote.SyncLockFile: syscall.LOCK_EX} {
		l, err := os.OpenFile(filepath.Join(base, name), os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { l.Close() })
		if ok, err := tryFlock(l, how); !ok || err != nil {
			t.Fatal("cannot take", name, err)
		}
	}
	const says = "devopsy: error: a release or another change runs in "
	// The act refuses it, which the look must say before the prompt.
	if out, errs, code := s.step("act", "1"); code != 1 || out != "" || !strings.Contains(errs, says) {
		t.Fatalf("act (%d): %q\n%s", code, out, errs)
	}
	if out, errs, code := s.step("look"); code != 1 || out != "" || !strings.Contains(errs, says) {
		t.Errorf("the look found nothing held under an export's shared hold, and the act refuses (%d): %q\n%s", code, out, errs)
	}
}
