package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Bug (review 4): --vars set, --vars unset and --inherit on a marked
// environment (a sync, an import or a restore that did not complete)
// name --reload as what applies the change, and the reload refuses there
// (markRefusal, before its checks), as review 1's plain directory and
// unreleased environment did: varsApply only tells plain, first and the
// images check's exit, never a mark, and runInherit says --reload
// unconditionally. The follow-up should be the mark's way out (the
// import again, --resolved, --sync, --reset), which clears it; a release
// (the images check's fix) refuses there too (UploadScript).
func TestReview4VarsHintMarked(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "config.yaml"), strings.Replace(readString(filepath.Join(s.dot, "config.yaml")), "  prod: {}\n", "  prod: {inherit: [SITE_NAME]}\n", 1), 0o644)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("dev's release (%d):\n%s", code, out)
	}
	s.logs()
	for _, env := range []string{"prod", "dev"} {
		base := filepath.Join(s.root, "shop", env)
		write(t, filepath.Join(base, remote.SyncIncompleteFile), remote.IncompleteLine("2026-10-27 10:00:00", "db", remote.IncompleteImport, "cut"), 0o644)
	}
	reload, rcode := s.run(nil, "@prod", "--reload")
	if rcode == 0 || !strings.Contains(reload, "did not complete") {
		t.Fatalf("a marked environment's reload went through (%d):\n%s", rcode, reload)
	}
	out, code := s.run([]string{"SITE_NAME=shop"}, "@prod", "--vars", "set", "SITE_NAME")
	if code != 0 {
		t.Fatalf("--vars set (%d):\n%s", code, out)
	}
	if strings.Contains(out, "--reload") || !strings.Contains(out, "did not complete") {
		t.Errorf("--vars set on a marked environment names --reload, which refuses there, and not the mark's way out:\n%s", out)
	}
	out, code = s.run(nil, "@prod", "--vars", "unset", "SITE_NAME")
	if code != 0 {
		t.Fatalf("--vars unset (%d):\n%s", code, out)
	}
	if strings.Contains(out, "--reload") || !strings.Contains(out, "did not complete") {
		t.Errorf("--vars unset on a marked environment names --reload, which refuses there:\n%s", out)
	}
	write(t, filepath.Join(s.root, "shop", "prod", "shared", ".env"), "SITE_NAME=shop\n", 0o600)
	out, code = s.run(nil, "@dev", "--inherit")
	if code != 0 {
		t.Fatalf("--inherit (%d):\n%s", code, out)
	}
	if strings.Contains(out, "--reload") || !strings.Contains(out, "did not complete") {
		t.Errorf("--inherit into a marked child names --reload, which refuses there:\n%s", out)
	}
}

// Bug (review 4): a reload of an environment never released (no
// directory on the server yet) fails with "<path> is gone", the release
// lock's words for a directory removed while it waited (releaseLock
// without create, in prelude): ReloadScript checks for a current release
// only past the lock. The holder's words apply, before the lock: no
// current release, release it first.
func TestReview4ReloadNeverReleased(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	out, code := s.run(nil, "@solo", "--reload")
	if code == 0 {
		t.Fatalf("a reload of an environment never released went through:\n%s", out)
	}
	if strings.Contains(out, "is gone") || !strings.Contains(out, "no current release") {
		t.Errorf("a reload of an environment never released says it is gone:\n%s", out)
	}
}
