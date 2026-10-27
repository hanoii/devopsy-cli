package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Bug (review 3): a --no-code restore --with-env refused by the images
// check names "devopsy @<target> --release" as the fix, which does not
// fix it: the check reads the snapshot's shared.env, a release fetches
// under the environment's own shared/.env as it is, so the same restore
// is refused again after it, in a loop (as the sync's --release was,
// review 2). What applies the snapshot's variables is a full restore
// (its code, its images, --with-env), or the restore without --with-env.
func TestReview3RestoreWithEnvFixLoops(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	shared := filepath.Join(s.root, "shop", "prod", "shared", ".env")
	write(t, shared, "FAKE_DB_IMAGE=mariadb:11\n", 0o600)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	write(t, shared, "FAKE_DB_IMAGE=mariadb:12\n", 0o600)
	s.releaseProd(nil)
	restore := []string{"@prod", "--snapshots", "restore", m[1], "--no-code", "--with-env", "--yes"}
	out, code = s.run(nil, restore...)
	if code == 0 || !strings.Contains(out, "db's image is mariadb:11 now") {
		t.Fatalf("the restore (%d):\n%s", code, out)
	}
	fix := "$ devopsy @devopsy@one:prod --release"
	if !strings.Contains(out, fix+"\n") {
		t.Fatalf("the restore names another fix than %q:\n%s", fix, out)
	}
	s.logs()
	// The fix as named.
	if out, code := s.run(nil, "@prod", "--release"); code != 0 {
		t.Fatalf("the release the restore named (%d):\n%s", code, out)
	}
	s.logs()
	if out, code := s.run(nil, restore...); code != 0 || strings.Contains(out, "db's image is mariadb:11 now") {
		t.Errorf("the same restore after the fix it named is refused again (%d):\n%s", code, out)
	}
}

// Bug (review 3): a full restore --with-env starts the restored release
// on variables that need another image than it fetched, unchecked: its
// images are restored (img restore, before the switch) under the
// environment's shared/.env as it is, then its data step puts the
// snapshot's in place and the run step starts the services on them (an
// up pulls the other name outside any release, held by no bookmark).
// The --no-code restore and a sync check this (--check-images); the full
// restore does neither, so the environment it leaves is one its own
// --reload refuses at once.
func TestReview3FullRestoreWithEnvUnchecked(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.labels("FAKE_DB=none")
	shared := filepath.Join(s.root, "shop", "prod", "shared", ".env")
	write(t, shared, "FAKE_DB_IMAGE=mariadb:11\n", 0o600)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	write(t, shared, "FAKE_DB_IMAGE=mariadb:12\n", 0o600)
	s.releaseProd(nil)
	out, code = s.run(nil, "@prod", "--snapshots", "restore", m[1], "--with-env", "--yes")
	docker, _ := s.logs()
	if code != 0 {
		// Refused before anything changed: what the fix does.
		if !strings.Contains(out, "mariadb:11") {
			t.Errorf("the full restore failed for another reason (%d):\n%s\n%s", code, out, docker)
		}
		return
	}
	if reload, rcode := s.run(nil, "@prod", "--reload"); rcode != 0 {
		t.Errorf("a full restore --with-env went live on variables that need another image than it fetched (its run step ran on them); its --reload refuses at once (%d):\n%s\nthe restore:\n%s\ndocker:\n%s", rcode, reload, out, docker)
	}
}

// Bug (review 3): on a marked environment (an import, a sync or a
// restore that did not complete), --vars set says "to apply it now:
// devopsy @<target> --reload", which refuses there before anything
// (markRefusal), as a release that does not sync does: the hint reads
// marks nowhere, where review 1 kept it from naming --reload on a plain
// directory and before any release for the same reason.
func TestReview3VarsHintMarked(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	write(t, filepath.Join(s.root, "shop", "prod", remote.SyncIncompleteFile), remote.IncompleteLine("2026-10-27 10:00:00", "db", remote.IncompleteImport, "cut"), 0o644)
	out, code := s.run([]string{"SITE_NAME=shop"}, "@prod", "--vars", "set", "SITE_NAME")
	if code != 0 {
		t.Fatalf("--vars set (%d):\n%s", code, out)
	}
	reload, rcode := s.run(nil, "@prod", "--reload")
	if rcode == 0 || !strings.Contains(reload, "nothing reloaded") {
		t.Fatalf("the reload of a marked environment went through (%d):\n%s", rcode, reload)
	}
	if strings.Contains(out, "--reload") {
		t.Errorf("on a marked environment --vars set names --reload, which refuses there:\n%s\nthe reload:\n%s", out, reload)
	}
}

// Bug (review 3): a --no-code restore --with-env checks its images with
// the snapshot's shared.env standing in for the environment's .env
// (DotenvPendingEnv, read by devopsy alone), but compose reads the .env
// beside the compose file itself, .devopsy/.env, the environment's
// shared/.env as it is, for every variable devopsy's environment leaves
// unset (checked by hand with compose v5.5.0: `devopsy -- config` with
// DEVOPSY_INTERNAL_DOTENV_FILE naming an empty file still interpolates
// .devopsy/.env's values). A key the environment's file has and the
// snapshot's lacks so passes the check with its current value, then the
// restore puts the snapshot's file in place and the run step starts the
// services without it: on another image than the release fetched. The
// fake reads that .env as compose does (FAKE_DOTENV).
func TestReview3RestoreWithEnvComposeReadsDotenv(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.labels("FAKE_DOTENV=1")
	shared := filepath.Join(s.root, "shop", "prod", "shared", ".env")
	write(t, shared, "OTHER=1\n", 0o600)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	write(t, shared, "FAKE_DB_IMAGE=mariadb:12\n", 0o600)
	s.releaseProd(nil)
	out, code = s.run(nil, "@prod", "--snapshots", "restore", m[1], "--no-code", "--with-env", "--yes")
	docker, _ := s.logs()
	if readString(shared) != "OTHER=1\n" && code == 0 {
		t.Fatalf("the restore did not bring the snapshot's .env back (%d):\n%s", code, out)
	}
	if code == 0 || !strings.Contains(out, "the release fetched mariadb:12") {
		t.Errorf("a --no-code restore --with-env whose snapshot's .env lacks FAKE_DB_IMAGE (the release fetched mariadb:12 under it) went through (%d): the check read the environment's .env through compose:\n%s\ndocker:\n%s", code, out, docker)
	}
}
