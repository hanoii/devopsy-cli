package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// A release gives its owned entries to their owners right after its
// switch, before the run step: in the util container, without a network,
// a missing entry made 0755, every entry scanned whole (chown -h, never
// through a link). A child's first sync rsyncs into an entry the child
// owns as its owner (--chown), from the child's labels only.
func TestMntOwnersRelease(t *testing.T) {
	s := newSyncSetup(t)
	write(t, s.log+".shop-prod", "FAKE_APP_OWNER=10001:10001\n", 0o644)
	out, code := s.run(nil, "@prod", "--release")
	docker, _ := s.logs()
	switched, giving, deployed := strings.Index(out, "current is now"), strings.Index(out, "devopsy: giving mnt/storage to 10001:10001"), strings.Index(out, "deploy in shop-prod")
	if code != 0 || switched < 0 || giving < switched || deployed < giving {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	for _, want := range []string{"run --rm --name devopsy-owners-shop-prod-", " --network none -v " + s.mnt("prod") + ":/mnt devopsy-util:2 sh -c set -eu\n", `mkdir -m 0755 "/mnt/$e"`, `find "/mnt/$1" \( ! -user "${2%%:*}" -o ! -group "${2#*:}" \) -exec chown -h "$2" {} +`, "done\n sh storage=10001:10001\n"} {
		if !strings.Contains(docker, want) {
			t.Errorf("missing %q in:\n%s", want, docker)
		}
	}
	if fi, err := os.Stat(filepath.Join(s.mnt("prod"), "storage")); err != nil || !fi.IsDir() {
		t.Errorf("mnt/storage not made: %v", err)
	}
	// Failing, it is a failed run step: nothing current on a first one.
	write(t, s.log+".shop-solo", "FAKE_APP_OWNER=10001:10001\n", 0o644)
	if out, code := s.run([]string{"FAIL_OWNERS=3"}, "@solo", "--release"); code == 0 || !strings.Contains(out, "giving the entries of mnt to their owners failed (1)") || strings.Contains(out, "deploy in shop-solo") || s.current("solo") != "" {
		t.Errorf("failing owners (%d), current %q:\n%s", code, s.current("solo"), out)
	}
	s.logs()

	// The child's owner, never the parent's: dev owns storage, prod's
	// labels say 10001:10001 too but count for nothing here.
	write(t, filepath.Join(s.mnt("prod"), "storage", "file.txt"), "from prod\n", 0o644)
	write(t, filepath.Join(s.mnt("prod"), "db", "ibdata"), "rows\n", 0o644)
	write(t, s.log+".shop-prod", "FAKE_APP_OWNER=20000:20000\n", 0o644)
	write(t, s.log+".shop-dev", "FAKE_APP_OWNER=10001:10001\n", 0o644)
	out, code = s.run(nil, "@dev", "--release")
	docker, _ = s.logs()
	if code != 0 || !strings.Contains(docker, "rsync -a --delete --numeric-ids --chown=10001:10001 /src/storage/ /dst/storage/\n") || !strings.Contains(docker, "rsync -a --delete --numeric-ids /src/db/ /dst/db/\n") || strings.Contains(docker, "20000") {
		t.Errorf("a child's first release (%d):\n%s\n%s", code, out, docker)
	}
}

// The mounts check refuses before the switch: nothing goes live, nothing
// runs (the refusals themselves: internal/cli's TestCheckMounts).
func TestCheckMountsRelease(t *testing.T) {
	s := newSyncSetup(t)
	out, code := s.run([]string{"FAKE_APP_MOUNT=/../cache"}, "@prod", "--release")
	if code == 0 || s.current("prod") != "" || strings.Contains(out, "deploy in") || !strings.Contains(out, "devopsy: error: the mounts check refuses the release: it does not go live") || !strings.Contains(out, "app mounts ./.devopsy/cache writable: it is in the release, and the next one replaces it") || !strings.Contains(out, "not made current") {
		t.Errorf("a writable mount in the release (%d), current %q:\n%s", code, s.current("prod"), out)
	}
}

// A reset gives the owned entries back to their owners once mnt is
// emptied: the services it then creates, and a stream's import, would
// otherwise find root's entries Docker made.
func TestMntOwnersReset(t *testing.T) {
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	s.logs()
	write(t, s.log+".shop-dev", "FAKE_APP_OWNER=10001:10001\n", 0o644)
	out, code := s.run(nil, "@dev", "--reset", "--yes")
	docker, _ := s.logs()
	wipe, owners := strings.Index(docker, "-wipe -v "), strings.Index(docker, "-owners --network none -v "+s.mnt("dev")+":/mnt ")
	if code != 0 || wipe < 0 || owners < wipe || !strings.Contains(docker, "run --rm --name devopsy-sync-") {
		t.Errorf("reset (%d):\n%s\n%s", code, out, docker)
	}
}

// Locally, before compose starts anything, an owned entry missing is
// made, and on Linux one not its owner's is given away in a util
// container, only then; on macOS (Docker Desktop, OrbStack) ownership is
// never checked. A command that starts nothing, or a project whose
// compose files cannot carry the label, reads no labels: its YAML keys
// are read, never its comments (every template's say "included"
// somewhere, which once made every local up pay a compose config).
func TestMntOwnersLocal(t *testing.T) {
	s := newSyncSetup(t)
	compose := filepath.Join(s.dot, "compose.yaml")
	write(t, compose, "# Every request's headers included; extends: nothing.\n"+readString(compose)+"    # devopsy.mnt.owner is not here\n    environment: {A: b} # nor devopsy.mnt.owner\n", 0o644)
	owned := []string{"FAKE_APP_OWNER=10001:10001"}
	if out, code := s.run(owned, "up", "-d"); code != 0 {
		t.Fatalf("up (%d):\n%s", code, out)
	}
	if docker, _ := s.logs(); strings.Contains(docker, "config --format json") || fileExists(filepath.Join(s.dot, "mnt", "storage")) {
		t.Errorf("labels read for a project without the label:\n%s", docker)
	}
	write(t, compose, readString(compose)+"    labels: [devopsy.mnt.owner=10001:10001]\n", 0o644)
	if out, code := s.run(owned, "ps"); code != 0 {
		t.Fatalf("ps (%d):\n%s", code, out)
	}
	if docker, _ := s.logs(); strings.Contains(docker, "config --format json") {
		t.Errorf("labels read for ps:\n%s", docker)
	}
	for _, args := range [][]string{{"up", "-d"}, {"--profile", "web", "run", "--rm", "app"}} {
		os.RemoveAll(filepath.Join(s.dot, "mnt"))
		out, code := s.run(owned, args...)
		docker, _ := s.logs()
		container := strings.Contains(docker, "run --rm --name devopsy-owners-")
		if code != 0 || !fileExists(filepath.Join(s.dot, "mnt", "storage")) || container != (runtime.GOOS != "darwin") || !strings.Contains(docker, "compose -f "+compose) {
			t.Errorf("%v (%d), the util container %v:\n%s\n%s", args, code, container, out, docker)
		}
	}
	if runtime.GOOS == "darwin" {
		return
	}
	// The entry there and its owner's (the test's own user): nothing to do.
	uid, gid := os.Getuid(), os.Getgid()
	if out, code := s.run([]string{"FAKE_APP_OWNER=" + strconv.Itoa(uid) + ":" + strconv.Itoa(gid)}, "start"); code != 0 {
		t.Fatalf("start (%d):\n%s", code, out)
	}
	if docker, _ := s.logs(); strings.Contains(docker, "devopsy-owners-") {
		t.Errorf("an entry its owner's given away again:\n%s", docker)
	}
}

// The util container's scripts parse (sh -n, as the servers' scripts
// do in internal/remote).
func TestMntScriptsParse(t *testing.T) {
	for name, script := range map[string]string{"ownEntries": ownEntries, "emptyAndExtract": emptyAndExtract} {
		if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
}
