package main

import (
	"path/filepath"
	"strings"
	"testing"

)

// Review 2026-10-23 (container hardening): a rollback reads the restored
// release's data labels now (--mnt-owners, right after its switch), so a
// release whose labels an older devopsy wrote (here devopsy.sync, from
// before devopsy.data) can no longer be rolled back to: the rollback
// fails and current goes back. AGENTS.md (Labels and the child guard)
// says those labels are errors only where labels are read, "never in
// Build: a rollback to an older release, or the restart after a failed
// one, runs a plain up", and "Rollbacks never check roles or imports"
// for the same reason: a rollback must not fail exactly when it is
// needed. A release without devopsy.mnt.owner has nothing to own, so its
// labels should not decide whether it can come back.
func TestReviewRollbackToOldLabels(t *testing.T) {
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "config.yaml"), "project: shop\ndefaults:\n  mode: image\n  release: deploy\n  rollback: deploy\nenvironments:\n  prod: {}\n", 0o644)
	s.releaseProd(nil)
	first := s.current("prod")
	s.releaseProd(nil)
	if s.current("prod") == first {
		t.Fatalf("second release did not go live")
	}
	// The release rolled back to carries a label from before devopsy.data
	// (the fake answers for every release alike).
	write(t, s.log+".shop-prod", "FAKE_APP=old\n", 0o644)
	out, code := s.run(nil, "@prod", "--rollback")
	if code != 0 || s.current("prod") != first {
		t.Errorf("rollback to a release with older labels (%d), current %q, want %q:\n%s", code, s.current("prod"), first, out)
	}
	if strings.Contains(out, "devopsy.sync=rsync: that label is now") {
		t.Errorf("the rollback read the restored release's labels:\n%s", out)
	}
}

// Review 2026-10-23: a service that bind-mounts a directory below an
// entry (./mnt/storage/files:/app/files) owns the entry, storage, and a
// first release creates only that: the bind's own source, storage/files,
// is still missing when the run step's up starts the container, so
// Docker creates it as root, and a service running as its owner from its
// first process cannot write its mount, which is what the label is for
// (README, "Data and mounts"). The next release's scan would fix it,
// after a first release that failed. (The fake's owners container makes
// what it is given, as ownEntries does: mkdir of each entry.)
func TestReviewOwnersSubdirectoryMount(t *testing.T) {
	s := newSyncSetup(t)
	write(t, s.log+".shop-prod", "FAKE_APP_OWNER=10001:10001 FAKE_APP_MOUNT=/storage/files\n", 0o644)
	out, code := s.run(nil, "@prod", "--release")
	if code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	if !fileExists(filepath.Join(s.mnt("prod"), "storage", "files")) {
		t.Errorf("mnt/storage/files, app's bind source, not made before the run step: Docker makes it root's:\n%s", out)
	}
}

// Review 2026-10-23: the mounts check runs before prepare, and a compose
// configuration it cannot read is no refusal, so a project whose compose
// file requires a secret its prepare step generates (drupal11's and the
// registry template's ${DB_PASSWORD:?...}, made by their secrets command)
// is never checked on its first release on an environment: compose config
// fails there for the missing variable, --check-mounts says nothing, and
// the release goes live with a mount the check exists to refuse. That
// first release is the case the check was made for (a child's first
// release going live on empty data). Here app mounts ./.devopsy/cache
// writable, which the same release without the secret refuses
// (TestCheckMountsRelease).
func TestReviewCheckMountsBeforeSecrets(t *testing.T) {
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "config.yaml"), "project: shop\ndefaults:\n  mode: image\n  release: {prepare: [secrets], run: deploy}\nenvironments:\n  prod: {}\n", 0o644)
	write(t, filepath.Join(s.dot, "commands", "secrets"), "#!/bin/sh\n## Description: generates the secrets\ngrep -q '^SECRET=' \"$DEVOPSY_PROJECT_DIR/.env\" 2>/dev/null || echo SECRET=generated >> \"$DEVOPSY_PROJECT_DIR/.env\"\n", 0o755)
	// compose config fails while SECRET is unset, as for ${SECRET:?}.
	write(t, s.log+".shop-prod", "FAKE_APP_MOUNT=/../cache\ncase \"$*\" in *\" config --format json\") [ -n \"${SECRET:-}\" ] || { echo 'required variable SECRET is missing a value' >&2; exit 1; } ;; esac\n", 0o644)
	out, code := s.run(nil, "@prod", "--release")
	if code == 0 || s.current("prod") != "" || !strings.Contains(out, "app mounts ./.devopsy/cache writable") {
		t.Errorf("a first release whose secrets prepare makes (%d), current %q: the mounts check skipped:\n%s", code, s.current("prod"), out)
	}
}
