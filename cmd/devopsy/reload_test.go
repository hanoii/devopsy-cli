package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// A reload runs, under the release lock and in the current release,
// devopsy's own up of what the release fetched, never the run step; the
// release's reload: once it has one (the checkout's alone is not what
// runs); its log goes to the release's. The owners follow a label a
// variable changed. A variable that gives a service another image, a
// marked environment and a required variable unset refuse it, nothing
// started and nothing of the reload left.
func TestReload(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	base := filepath.Join(s.root, "shop", "prod")
	rel := filepath.Join(base, s.current("prod"))
	targetEnv := readString(filepath.Join(rel, remote.TargetEnvFile))

	s.labels("FAKE_APP_OWNER=20002:20002")
	out, code := s.run(nil, "@prod", "--reload")
	docker, _ := s.logs()
	locked := readString(s.log + ".locked")
	if code != 0 || strings.Contains(out, "deploy in") || !strings.Contains(out, "giving mnt/storage to 20002:20002") || !strings.Contains(locked, "shop-prod compose up -d --wait --no-build --pull never") {
		t.Fatalf("reload (%d):\n%s\ndocker:\n%s\nlocked:\n%s", code, out, docker, locked)
	}
	if got := readString(filepath.Join(rel, remote.TargetEnvFile)); got != targetEnv || fileExists(filepath.Join(rel, remote.TargetReloadFile)) {
		t.Errorf("devopsy-target.env after a reload with nothing to import:\n%s", got)
	}
	if log, _ := s.run(nil, "@prod", "--log"); !strings.Contains(log, "reloading devopsy@one:prod") || !strings.Contains(log, "running 'devopsy -- up -d --wait --no-build --pull never' (reload)") {
		t.Errorf("--log:\n%s", log)
	}
	s.labels("")

	// The checkout's reload: is not the release's.
	write(t, filepath.Join(s.dot, "config.yaml"), strings.Replace(readString(filepath.Join(s.dot, "config.yaml")), "  release: deploy\n", "  release: deploy\n  reload: deploy\n", 1), 0o644)
	if out, code := s.run(nil, "@prod", "--reload"); code != 0 || strings.Contains(out, "deploy in") {
		t.Errorf("a reload: only in the checkout ran (%d):\n%s", code, out)
	}
	s.releaseProd(nil)
	if out, code := s.run(nil, "@prod", "--reload"); code != 0 || !strings.Contains(out, "deploy in shop-prod") {
		t.Errorf("the release's reload: did not run (%d):\n%s", code, out)
	}
	s.logs()

	refused := func(what string, want ...string) {
		t.Helper()
		out, code := s.run(nil, "@prod", "--reload")
		docker, _ := s.logs()
		if code == 0 || !strings.Contains(out, "nothing reloaded") || strings.Contains(out, "deploy in") || strings.Contains(docker, " up ") || fileExists(filepath.Join(base, s.current("prod"), remote.TargetReloadFile)) {
			t.Errorf("%s (%d):\n%s\ndocker:\n%s", what, code, out, docker)
		}
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Errorf("%s: no %q in:\n%s", what, w, out)
			}
		}
	}
	s.labels("FAKE_DB_IMAGE=mariadb:12")
	refused("another image", "db's image is mariadb:12 now, the release fetched shop-prod-db", "$ devopsy @devopsy@one:prod --release")
	s.labels("")

	write(t, filepath.Join(base, remote.SyncIncompleteFile), remote.IncompleteLine("2026-10-27 10:00:00", "db", remote.IncompleteImport, "cut"), 0o644)
	refused("a marked environment", "the import of db into @devopsy@one:prod did not complete")
	os.Remove(filepath.Join(base, remote.SyncIncompleteFile))

	write(t, filepath.Join(base, s.current("prod"), remote.CodeDir, ".devopsy", "compose.yaml"), "services:\n  app:\n    image: x\n    environment: [\"KEY=${REQUIRED:?}\"]\n  db:\n    image: y\n", 0o644)
	refused("a required variable unset", "REQUIRED")

	os.Remove(filepath.Join(base, "current"))
	if out, code := s.run(nil, "@prod", "--reload"); code == 0 || !strings.Contains(out, "has no current release: nothing to reload") {
		t.Errorf("no current release (%d):\n%s", code, out)
	}
}

// In build mode, a variable that changes a build argument refuses a
// reload, naming it: the image runs what the release built it with.
func TestReloadBuildArgument(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "config.yaml"), "project: shop\ndefaults:\n  release: deploy\nenvironments:\n  prod: {}\n", 0o644)
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.email=a@example.com", "-c", "user.name=a", "commit", "-qm", "x"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = s.project
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	s.labels("FAKE_APP_BUILD=1")
	s.releaseProd(nil)
	if out, code := s.run(nil, "@prod", "--reload"); code != 0 {
		t.Fatalf("an unchanged build (%d):\n%s", code, out)
	}
	s.logs()
	s.labels("FAKE_APP_BUILD=1\nFAKE_APP_UID=10002")
	out, code := s.run(nil, "@prod", "--reload")
	if docker, _ := s.logs(); code == 0 || !strings.Contains(out, "app's build changed since the release built its image: APP_UID") || strings.Contains(out, "10002") || strings.Contains(docker, " up ") {
		t.Errorf("APP_UID changed (%d):\n%s\ndocker:\n%s", code, out, docker)
	}
}

// A session gone silent during a reload's checks ends it, nothing done:
// the watcher's signal (Linux, where the script leads a group) or the
// operator's quiet time.
func TestReloadSessionGone(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	start := time.Now()
	out, code := s.run([]string{remote.HeartbeatEveryEnv + "=1", "FAKE_SILENT=1"}, "@prod", "--reload")
	if docker, _ := s.logs(); code == 0 || strings.Contains(docker, " up ") || !strings.Contains(out, "interrupted: nothing reloaded") && !strings.Contains(out, "lost before the reload's checks passed, or its devopsy on the server was killed: nothing changed there") {
		t.Errorf("a silent session's reload (%d) in %s:\n%s\ndocker:\n%s", code, time.Since(start), out, docker)
	}
}

// --reload without a target runs defaults:' reload step in place, else
// devopsy's own up, once compose's required variables are set.
func TestLocalReload(t *testing.T) {
	t.Parallel()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, ".devopsy")
	write(t, filepath.Join(dir, "compose.yaml"), "services: {app: {image: x}}\n", 0o644)
	write(t, filepath.Join(dir, "config.yaml"), "project: app\ndefaults: {release: deploy}\nenvironments: {prod: {}}\n", 0o644)
	write(t, filepath.Join(dir, "commands", "deploy"), "#!/bin/sh\n## Description: d\necho DEPLOYED\n", 0o755)
	write(t, filepath.Join(dir, "commands", "apply"), "#!/bin/sh\n## Description: a\necho APPLIED in $PWD\n", 0o755)
	out, code := runDevopsy(t, tmp, nil, "--reload")
	if code != 0 || strings.Contains(out, "DEPLOYED") || !strings.Contains(out, "[up][-d][--wait][--no-build][--pull][never]") {
		t.Errorf("--reload without a step (%d):\n%s", code, out)
	}
	write(t, filepath.Join(dir, "config.yaml"), "project: app\ndefaults: {release: deploy, reload: apply}\nenvironments: {prod: {}}\n", 0o644)
	if out, code := runDevopsy(t, tmp, nil, "--reload"); code != 0 || !strings.Contains(out, "APPLIED in "+tmp) {
		t.Errorf("--reload with a step (%d):\n%s", code, out)
	}
	write(t, filepath.Join(dir, "compose.yaml"), "services: {app: {image: \"x:${TAG:?}\"}}\n", 0o644)
	if out, code := runDevopsy(t, tmp, nil, "--reload"); code == 0 || strings.Contains(out, "APPLIED") || !strings.Contains(out, "TAG") {
		t.Errorf("--reload with a required variable unset (%d):\n%s", code, out)
	}
}

// A sync reruns the child's run step on its images: a variable that now
// needs another image refuses it before anything stops, the parent's
// inherited ones included (the pending file the sync reads).
func TestSyncChecksImages(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "config.yaml"), strings.Replace(readString(filepath.Join(s.dot, "config.yaml")), "  prod: {}\n", "  prod: {inherit: [FAKE_DB_IMAGE]}\n", 1), 0o644)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("dev's release (%d):\n%s", code, out)
	}
	s.logs()
	write(t, filepath.Join(s.root, "shop", "prod", "shared", ".env"), "FAKE_DB_IMAGE=mariadb:12\n", 0o644)
	out, code := s.run(nil, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code == 0 || !strings.Contains(out, "db's image is mariadb:12 now") || !strings.Contains(out, "nothing changed") || strings.Contains(docker, " stop ") || strings.Contains(out, "deploy in") {
		t.Errorf("a sync whose inherited variable needs another image (%d):\n%s\ndocker:\n%s", code, out, docker)
	}
}

// A --no-code restore reruns the run step on the current release's
// images: with --with-env, under the snapshot's variables, refused before
// anything stops when they need another image; without, the current
// ones, which do not.
func TestRestoreNoCodeChecksImages(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	shared := filepath.Join(s.root, "shop", "prod", "shared", ".env")
	write(t, shared, "FAKE_DB_IMAGE=mariadb:12\n", 0o600)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	write(t, shared, "", 0o600)
	s.logs()
	out, code = s.run(nil, "@prod", "--snapshots", "restore", m[1], "--no-code", "--with-env", "--yes")
	docker, _ := s.logs()
	if code == 0 || !strings.Contains(out, "db's image is mariadb:12 now") || !strings.Contains(out, "nothing restored") || strings.Contains(docker, " stop ") || readString(shared) != "" {
		t.Errorf("--with-env needing another image (%d):\n%s\ndocker:\n%s", code, out, docker)
	}
	if out, code := s.run(nil, "@prod", "--snapshots", "restore", m[1], "--no-code", "--yes"); code != 0 || !strings.Contains(out, "deploy in shop-prod") {
		t.Errorf("without --with-env (%d):\n%s", code, out)
	}
}

// --vars set and unset on an environment say what applies the change: a
// reload, a release when the variables now need another image, or
// nothing until compose's required variables are set; refusing none.
// The project's file, several environments', gets the plain line.
func TestVarsApply(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	vars := func(extra []string, args ...string) string {
		t.Helper()
		out, code := s.run(extra, append([]string{"@prod", "--vars"}, args...)...)
		if code != 0 {
			t.Fatalf("--vars %v (%d):\n%s", args, code, out)
		}
		return out
	}
	if out := vars([]string{"SITE_NAME=shop"}, "set", "SITE_NAME"); !strings.Contains(out, "to apply it now,\ndevopsy: | $ devopsy @devopsy@one:prod --reload") {
		t.Errorf("a plain value:\n%s", out)
	}
	if out := vars([]string{"FAKE_DB_IMAGE=mariadb:12"}, "set", "FAKE_DB_IMAGE"); !strings.Contains(out, "db's image is mariadb:12 now") || !strings.Contains(out, "$ devopsy @devopsy@one:prod --release") || strings.Contains(out, "--reload") {
		t.Errorf("an image's name:\n%s", out)
	}
	vars(nil, "unset", "FAKE_DB_IMAGE")
	write(t, filepath.Join(s.root, "shop", "prod", s.current("prod"), remote.CodeDir, ".devopsy", "compose.yaml"), "services:\n  app:\n    image: x\n    environment: [\"KEY=${REQUIRED:?}\"]\n  db:\n    image: y\n", 0o644)
	vars([]string{"REQUIRED=1"}, "set", "REQUIRED")
	if out := vars(nil, "unset", "REQUIRED"); !strings.Contains(out, "warning: unset REQUIRED in") || !strings.Contains(out, "refuses until they are: REQUIRED") {
		t.Errorf("a required variable unset:\n%s", out)
	}
	if out, code := s.run([]string{"X=1"}, "@prod", "--vars-project", "set", "X"); code != 0 || !strings.Contains(out, "each environment's --reload applies it") {
		t.Errorf("the project's file (%d):\n%s", code, out)
	}
}

// A release refuses, before its upload, a reload: step naming a command
// that only runs locally: the server runs it, past a reload's checks.
func TestReloadStepScope(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "commands", "here"), "#!/bin/sh\n## Description: local only\n## Run: local\necho here\n", 0o755)
	write(t, filepath.Join(s.dot, "config.yaml"), strings.Replace(readString(filepath.Join(s.dot, "config.yaml")), "  release: deploy\n", "  release: deploy\n  reload: here\n", 1), 0o644)
	out, code := s.run(nil, "@prod", "--release")
	if code == 0 || !strings.Contains(out, "a remote step") || s.current("prod") != "" {
		t.Errorf("a release with a local-only reload: step (%d):\n%s", code, out)
	}
}

// A release cut off past its switch (its run step may not have run) is
// no release to reload: an up alone would start its code without it. The
// reload refuses, in --status' words (Activating.Cut), naming what
// finishes it by the kind of activation cut: a failed run step's restart
// of current too.
func TestReloadCut(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	base := filepath.Join(s.root, "shop", "prod")
	id := strings.TrimPrefix(s.current("prod"), "releases/")
	for line, want := range map[string][]string{
		id + "\trelease\t\t\n":               {"the release of " + id + " was cut off past its switch", "devopsy @devopsy@one:prod --release releases it again"},
		id + "\trollback\t\t\n":              {"the rollback to " + id + " was cut off past its switch", "devopsy @devopsy@one:prod --rollback finishes it"},
		id + "\trestart\t29990101000000\t\n": {"the restart of " + id + " after 29990101000000 failed was cut off", "devopsy @devopsy@one:prod --release releases it again"},
		id + "\trestore\t\t20261027000000\n": {"the restore of snapshot 20261027000000 as " + id + " was cut off past its switch", "devopsy @devopsy@one:prod --snapshots restore 20261027000000 runs it again"},
	} {
		write(t, filepath.Join(base, remote.ActivatingFile), line, 0o644)
		out, code := s.run(nil, "@prod", "--reload")
		docker, _ := s.logs()
		if code == 0 || !strings.Contains(out, want[0]) || !strings.Contains(out, want[1]) || !strings.Contains(out, "nothing reloaded") || strings.Contains(docker, " up ") {
			t.Errorf("a cut %q (%d), want %q:\n%s\ndocker:\n%s", line, code, want, out, docker)
		}
	}
}

// devopsy --reload typed in a release on a server runs that release's
// environment's reload: step, as --destroy there runs its destroy:, never
// defaults:' alone.
func TestLocalReloadInRelease(t *testing.T) {
	t.Parallel()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(tmp, "releases", "20261027000000")
	dir := filepath.Join(rel, remote.CodeDir, ".devopsy")
	write(t, filepath.Join(dir, "compose.yaml"), "services: {app: {image: x}}\n", 0o644)
	write(t, filepath.Join(dir, "config.yaml"), "project: app\ndefaults: {release: deploy, reload: general}\nenvironments:\n  prod: {reload: apply}\n", 0o644)
	write(t, filepath.Join(dir, "commands", "apply"), "#!/bin/sh\n## Description: a\necho APPLIED\n", 0o755)
	write(t, filepath.Join(dir, "commands", "general"), "#!/bin/sh\n## Description: g\necho GENERAL\n", 0o755)
	write(t, filepath.Join(rel, remote.TargetEnvFile), "COMPOSE_PROJECT_NAME='app-prod'\nDEVOPSY_ENVIRONMENT='prod'\n", 0o644)
	if out, code := runDevopsy(t, filepath.Join(rel, remote.CodeDir), nil, "--reload"); code != 0 || !strings.Contains(out, "APPLIED") || strings.Contains(out, "GENERAL") {
		t.Errorf("--reload in a release (%d):\n%s", code, out)
	}
}
