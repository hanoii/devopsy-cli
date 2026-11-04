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

// Bug (review 1): --vars set's follow-up reads compose's required
// variables without the env capability (cli.MissingVarsIn loads the
// project but never runs computeEnv), where --missing-vars, a release's
// and a reload's, runs it first. A project whose compose requires a
// variable its env capability computes gets, on every --vars set, the
// warning that the next reload or release refuses until it is set, and
// no --reload line; the reload then goes through.
func TestVarsHintComputedRequired(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "compose.yaml"), "services:\n  app:\n    image: x\n    environment: [\"URL=${SITE_URL:?computed by the env capability}\"]\n  db:\n    image: y\n", 0o644)
	write(t, filepath.Join(s.dot, "capabilities", "env", "compute"), "#!/bin/sh\necho SITE_URL=https://shop.example.com\n", 0o755)
	s.releaseProd(nil)

	out, code := s.run([]string{"SITE_NAME=shop"}, "@prod", "--vars", "set", "SITE_NAME")
	if code != 0 {
		t.Fatalf("--vars set (%d):\n%s", code, out)
	}
	reload, rcode := s.run(nil, "@prod", "--reload")
	if rcode != 0 {
		t.Fatalf("the reload the hint should name failed (%d):\n%s", rcode, reload)
	}
	if strings.Contains(out, "SITE_URL") || !strings.Contains(out, "--reload") {
		t.Errorf("--vars set says SITE_URL, which the env capability computes, is missing, though the reload goes through:\n%s", out)
	}
}

// Bug (review 1): --vars set on a plain directory, or on an environment
// with nothing released yet, ends with "devopsy @<environment> --reload
// applies it" (editVars' default case), a placeholder for an address it
// knows, naming a command that refuses there: a reload needs a current
// release. The plan says nothing on a plain directory.
func TestVarsHintWithoutRelease(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	out, code := s.run([]string{"SITE_NAME=shop"}, "@solo", "--vars", "set", "SITE_NAME")
	if code != 0 {
		t.Fatalf("--vars set before a release (%d):\n%s", code, out)
	}
	reload, rcode := s.run(nil, "@solo", "--reload")
	if rcode == 0 {
		t.Fatalf("a reload before any release went through:\n%s", reload)
	}
	if strings.Contains(out, "--reload") {
		t.Errorf("before any release, --vars set names --reload, which refuses there (%q):\n%s", strings.TrimSpace(reload), out)
	}

	// A plain directory: nothing, as the plan says.
	plain := filepath.Join(s.root, "shop", "nosnap")
	if err := os.MkdirAll(filepath.Join(plain, ".devopsy"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(plain, ".devopsy", "compose.yaml"), "services:\n  app:\n    image: x\n", 0o644)
	out, code = s.run([]string{"SITE_NAME=shop"}, "@nosnap", "--vars", "set", "SITE_NAME")
	if code != 0 {
		t.Fatalf("--vars set on a plain directory (%d):\n%s", code, out)
	}
	if reload, rcode := s.run(nil, "@nosnap", "--reload"); rcode == 0 {
		t.Fatalf("a reload of a plain directory went through:\n%s", reload)
	}
	if strings.Contains(out, "--reload") {
		t.Errorf("a plain directory's --vars set names --reload, which refuses there:\n%s", out)
	}
}

// fakeRunningProxy makes the fake docker answer, for shop-prod's calls, a
// running proxy exporting BASE_DOMAIN=base (cli.Running: docker ps, then
// inspect), through the file the fake sources per compose project.
func fakeRunningProxy(s *syncSetup, base string) {
	s.t.Helper()
	write(s.t, s.log+".shop-prod", `case "$*" in
  "ps -q --no-trunc") echo proxyid; exit 0 ;;
  "inspect --format {{.Id}} {{json .Config.Labels}} proxyid") echo 'proxyid {"com.docker.compose.project":"traefik-prod","devopsy.role":"proxy","devopsy.export.BASE_DOMAIN":"`+base+`"}'; exit 0 ;;
esac
`, 0o644)
}

// Bug (review 1): --vars unset of a variable the environment set over
// an import (so its release imported nothing for it) warns that compose
// now requires it and the next reload refuses: the hint reads the
// release's imports as they were, where a reload reads them again and
// imports it.
func TestVarsHintImportUnset(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "compose.yaml"), "services:\n  app:\n    image: x\n    labels: [devopsy.import.BASE=proxy/BASE_DOMAIN]\n    environment: [\"BASE=${BASE:?}\"]\n  db:\n    image: y\n", 0o644)
	fakeRunningProxy(s, "vm1.example.com")
	write(t, filepath.Join(s.root, "shop", "prod", "shared", ".env"), "BASE=mine.example.com\n", 0o600)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--vars", "unset", "BASE")
	if code != 0 {
		t.Fatalf("--vars unset (%d):\n%s", code, out)
	}
	reload, rcode := s.run(nil, "@prod", "--reload")
	if rcode != 0 {
		t.Fatalf("the reload failed (%d):\n%s", rcode, reload)
	}
	rel := filepath.Join(s.root, "shop", "prod", s.current("prod"))
	if got := readString(filepath.Join(rel, "devopsy-target.env")); !strings.Contains(got, "BASE='vm1.example.com'") {
		t.Errorf("the reload did not import BASE:\n%s\n%s", got, reload)
	}
	if strings.Contains(out, "refuses until") {
		t.Errorf("--vars unset says the next reload refuses until BASE is set, though the reload imports it and goes through:\n%s", out)
	}
}

// Bug (review 2): the fix a sync's images check names does not fix it
// when the variable that needs another image is an inherited one (the
// parent's inherit:, the usual case: the check reads the pending inherit
// file): "devopsy @<child> --release" fetches under the child's
// shared/inherit.env as it is (a plain release of a released child
// inherits nothing: inheritVars runs for a release that syncs and for
// --inherit), so the next --sync is refused again, in a loop; on a marked
// child the plain release is refused outright. What applies the parent's
// value is --release-sync, or --inherit then --release.
func TestSyncChecksImagesFix(t *testing.T) {
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
	if code == 0 || !strings.Contains(out, "db's image is mariadb:12 now") {
		t.Fatalf("the sync (%d):\n%s", code, out)
	}
	fix := "$ devopsy @devopsy@one:dev --release-sync"
	if !strings.Contains(out, fix) {
		t.Fatalf("the sync's fix is not %q:\n%s", fix, out)
	}
	// A --from stays in the fix: copied, it syncs from the same source.
	s.logs()
	if out, code := s.run(nil, "@dev", "--sync", "--from", "prod", "--yes"); code == 0 || !strings.Contains(out, "--release-sync --from prod") {
		t.Fatalf("the sync --from (%d):\n%s", code, out)
	}
	s.logs()
	// The fix as named: a release that syncs, which inherits first.
	if out, code := s.run(nil, "@dev", "--release-sync", "--yes"); code != 0 {
		t.Fatalf("dev's release again (%d):\n%s", code, out)
	}
	s.logs()
	if out, code := s.run(nil, "@dev", "--sync", "--yes"); code != 0 || strings.Contains(out, "db's image is mariadb:12 now") {
		t.Errorf("the sync after the fix the check named (%d):\n%s", code, out)
	}
}

// Bug (review 3): a --no-code restore --with-env refused by the images
// check names "devopsy @<target> --release" as the fix, which does not
// fix it: the check reads the snapshot's shared.env, a release fetches
// under the environment's own shared/.env as it is, so the same restore
// is refused again after it, in a loop (as the sync's --release was,
// review 2). What applies the snapshot's variables is a full restore
// (its code, its images, --with-env), or the restore without --with-env.
func TestRestoreWithEnvRefusalFix(t *testing.T) {
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
	fix := "$ devopsy @devopsy@one:prod --snapshots restore " + m[1] + " --with-env"
	if !strings.Contains(out, fix+"\n") {
		t.Fatalf("the restore names another fix than %q:\n%s", fix, out)
	}
	s.logs()
	// The fix as named: the full restore, the snapshot's code, images and
	// variables together; the environment then reloads as it is.
	if out, code := s.run(nil, "@prod", "--snapshots", "restore", m[1], "--with-env", "--yes"); code != 0 {
		t.Fatalf("the restore the check named (%d):\n%s", code, out)
	}
	s.logs()
	if out, code := s.run(nil, "@prod", "--reload"); code != 0 || strings.Contains(out, "now, the release fetched") {
		t.Errorf("a reload after the fix it named is refused (%d):\n%s", code, out)
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
func TestFullRestoreWithEnvChecksImages(t *testing.T) {
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
func TestVarsHintMarkedNoReload(t *testing.T) {
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
func TestRestoreWithEnvComposeReadsDotenv(t *testing.T) {
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

// Bug (review 4): --vars set, --vars unset and --inherit on a marked
// environment (a sync, an import or a restore that did not complete)
// name --reload as what applies the change, and the reload refuses there
// (markRefusal, before its checks), as review 1's plain directory and
// unreleased environment did: varsApply only tells plain, first and the
// images check's exit, never a mark, and runInherit says --reload
// unconditionally. The follow-up should be the mark's way out (the
// import again, --resolved, --sync, --reset), which clears it; a release
// (the images check's fix) refuses there too (UploadScript).
func TestVarsHintMarkedWayOut(t *testing.T) {
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
func TestReloadNeverReleased(t *testing.T) {
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

// Bug (review 5): --vars set and --inherit on an environment whose
// activation was cut off past its switch (ActivatingFile, nothing holding
// the lock) name --reload as what applies the change, and the reload
// refuses there now (cutRefusal), as reviews 1, 3 and 4 found for a plain
// directory, an unreleased and a marked environment: varsApply tells
// plain, first, marked and the images check's exit, never a cut
// activation, and runInherit says --reload whatever the child is. The
// follow-up should be what finishes the cut activation (--release here),
// as the reload's refusal and --status say.
func TestVarsHintCut(t *testing.T) {
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
		id := strings.TrimPrefix(s.current(env), "releases/")
		write(t, filepath.Join(base, remote.ActivatingFile), id+"\trelease\t\t\n", 0o644)
	}
	reload, rcode := s.run(nil, "@prod", "--reload")
	if rcode == 0 || !strings.Contains(reload, "cut off past its switch") {
		t.Fatalf("a cut release's reload went through (%d):\n%s", rcode, reload)
	}
	out, code := s.run([]string{"SITE_NAME=shop"}, "@prod", "--vars", "set", "SITE_NAME")
	if code != 0 {
		t.Fatalf("--vars set (%d):\n%s", code, out)
	}
	if strings.Contains(out, "--reload") {
		t.Errorf("--vars set on a cut release names --reload, which refuses there:\n%s", out)
	}
	write(t, filepath.Join(s.root, "shop", "prod", "shared", ".env"), "SITE_NAME=shop\n", 0o600)
	out, code = s.run(nil, "@dev", "--inherit")
	if code != 0 {
		t.Fatalf("--inherit (%d):\n%s", code, out)
	}
	if strings.Contains(out, "--reload") {
		t.Errorf("--inherit into a cut child names --reload, which refuses there:\n%s", out)
	}
}

// Bug (review 5): devopsy --reload typed in a release on a server checks
// its reload: step's command as one run locally without an address
// (cli.Bare in runLocalReload), so a step whose command runs only on a
// target (## Run: target) is refused there ("runs only on a target:
// devopsy @<target> apply"), though that is where it is: the release's
// own --reload (--release-steps reload run) runs it, a release checks it
// as a remote step (checkStepScopes, cli.OnServer), and where a command
// runs is decided by the directory (cli.InRelease).
func TestLocalReloadInReleaseRunTarget(t *testing.T) {
	t.Parallel()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(tmp, "releases", "20261027000000")
	dir := filepath.Join(rel, remote.CodeDir, ".devopsy")
	write(t, filepath.Join(dir, "compose.yaml"), "services: {app: {image: x}}\n", 0o644)
	write(t, filepath.Join(dir, "config.yaml"), "project: app\ndefaults: {release: deploy}\nenvironments:\n  prod: {reload: apply}\n", 0o644)
	write(t, filepath.Join(dir, "commands", "apply"), "#!/bin/sh\n## Description: a\n## Run: target\necho APPLIED\n", 0o755)
	write(t, filepath.Join(rel, remote.TargetEnvFile), "COMPOSE_PROJECT_NAME='app-prod'\nDEVOPSY_ENVIRONMENT='prod'\n", 0o644)
	if out, code := runDevopsy(t, filepath.Join(rel, remote.CodeDir), nil, "--reload"); code != 0 || !strings.Contains(out, "APPLIED") {
		t.Errorf("--reload in a release, its step a target's command (%d):\n%s", code, out)
	}
}
