package main

import (
	"path/filepath"
	"strings"
	"testing"
)

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
