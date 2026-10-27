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

// A session gone silent during a reload's checks ends it, nothing done.
func TestReloadSessionGone(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	start := time.Now()
	out, code := s.run([]string{remote.HeartbeatEveryEnv + "=1", "FAKE_SILENT=1"}, "@prod", "--reload")
	if docker, _ := s.logs(); code == 0 || strings.Contains(docker, " up ") || !strings.Contains(out, "lost before the reload's checks passed, or its devopsy on the server was killed: nothing changed there") {
		t.Errorf("a silent session's reload (%d) in %s:\n%s\ndocker:\n%s", code, time.Since(start), out, docker)
	}
}
