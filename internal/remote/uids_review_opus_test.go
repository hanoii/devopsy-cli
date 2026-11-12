package remote

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Bug: --destroy frees the uid claim after removeBase, when $base/.lock went with $base, and
// with nothing at $base it holds no release lock at all. A release opening $base/.lock then
// makes a fresh one and runs beside the free: its load reads the claim still there, the free
// releases it, and its compose (runCompose, which takes no start lock because the release
// lock is said to keep --destroy out) starts containers on a uid no claim holds.
func TestDestroyFreesUIDUnderReleaseLockReviewOpus(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	root := t.TempDir()
	// The fake's free does what a release starting meanwhile does: make $base, lock its .lock.
	write(t, filepath.Join(bin, "devopsy"), `#!/bin/sh
if [ "$1" = --release-settings ]; then printf 'root=%s\nkeep=1\nmax_keep=5\nprotocol=8\n' "$FAKE_ROOT"; exit; fi
case "$*" in
  "--stdin-guard") exec cat ;;
  "-- config --format json"|"-- --profile * config --format json") printf '{"name":"app","services":{}}' ;;
  --free-uids*)
    mkdir -p "$FAKE_ROOT/app/prod"
    if flock -n "$FAKE_ROOT/app/prod/.lock" true; then echo "$*" >>"$FAKE_ROOT/beside"; fi
    rmdir "$FAKE_ROOT/app/prod" 2>/dev/null || true ;;
esac
exit 0
`)
	fakeImageTools(t, bin)
	tg := &Target{Name: "prod", Path: "app/prod", Mode: ModeImage, Project: &Project{Name: "app"}}
	run := func() string {
		t.Helper()
		cmd := exec.Command("sh", "-c", DestroyScript(tg, ""))
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}
		cmd.Stdin = strings.NewReader(AckInput)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("destroy (%v):\n%s", err, out)
		}
		return string(out)
	}
	write(t, filepath.Join(root, tg.Path, "releases", "1", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
	writeRecord(t, filepath.Join(root, tg.Path), "1", ModeImage)
	if out := run(); !strings.Contains(out, "removed") {
		t.Fatalf("no removal:\n%s", out)
	}
	if out := run(); !strings.Contains(out, "nothing at") {
		t.Fatalf("no second destroy:\n%s", out)
	}
	if got := readFile(filepath.Join(root, "beside")); got != "" {
		t.Errorf("a release took the environment's release lock while its uid was freed:\n%s", got)
	}
}
