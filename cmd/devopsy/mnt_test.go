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
// through a link), and a bind source below an owned entry made too
// (review of 2026-10-23: Docker made storage/files root's, which a
// service running as its owner from its first process could not write).
// A child's first sync rsyncs into an entry the child owns as its owner
// (--chown), from the child's labels only.
func TestMntOwnersRelease(t *testing.T) {
	s := newSyncSetup(t)
	config := filepath.Join(s.dot, "config.yaml")
	write(t, config, strings.Replace(readString(config), "  release: deploy\n", "  release: deploy\n  rollback: deploy\n", 1), 0o644)
	write(t, s.log+".shop-prod", "FAKE_APP_OWNER=10001:10001 FAKE_APP_MOUNT2=/storage/files/cache\n", 0o644)
	out, code := s.run(nil, "@prod", "--release")
	docker, _ := s.logs()
	switched, giving, deployed := strings.Index(out, "current is now"), strings.Index(out, "devopsy: giving mnt/storage to 10001:10001"), strings.Index(out, "deploy in shop-prod")
	if code != 0 || switched < 0 || giving < switched || deployed < giving {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	for _, want := range []string{"run --rm --name devopsy-owners-shop-prod-", " --network none -v " + s.mnt("prod") + ":/mnt devopsy-util:2 sh -c set -eu\n", `mkdir -m 0755 "$d"`, `find "/mnt/$1" \( ! -user "${2%%:*}" -o ! -group "${2#*:}" \) -execdir chown -h "$2" {} +`, "own_paths \"$@\"\n sh storage=10001:10001 storage/files/cache=10001:10001\n"} {
		if !strings.Contains(docker, want) {
			t.Errorf("missing %q in:\n%s", want, docker)
		}
	}
	if fi, err := os.Stat(filepath.Join(s.mnt("prod"), "storage", "files", "cache")); err != nil || !fi.IsDir() {
		t.Errorf("mnt/storage/files/cache not made: %v", err)
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

	// A rollback gives owners by the restored release's labels; labels
	// this devopsy cannot read there (here a name from before
	// devopsy.data) only warn, nothing given away: a rollback never fails
	// for them, as it never checks roles or imports (review of
	// 2026-10-23: it failed, and current went back).
	first := s.current("prod")
	s.releaseProd(nil)
	if s.current("prod") == first {
		t.Fatalf("second release did not go live")
	}
	write(t, s.log+".shop-prod", "FAKE_APP=old\n", 0o644)
	out, code = s.run(nil, "@prod", "--rollback")
	if code != 0 || s.current("prod") != first || !strings.Contains(out, "devopsy: warning: the entries of mnt keep their owners: the restored release's labels cannot be read\ndevopsy: | service app: devopsy.sync=rsync: that label is now devopsy.data") || strings.Contains(out, "error") {
		t.Errorf("a rollback to a release with older labels (%d), current %q, want %q:\n%s", code, s.current("prod"), first, out)
	}
	if docker, _ := s.logs(); strings.Contains(docker, "devopsy-owners-") {
		t.Errorf("owners given away by labels that cannot be read:\n%s", docker)
	}
}

// The mounts check refuses before the switch: nothing goes live, nothing
// runs (the refusals themselves: internal/cli's TestCheckMounts). It
// runs after the prepare steps, which may make the secrets compose
// requires (review of 2026-10-23: before them, compose config failed on
// a first release and the check passed it unchecked). With an entry
// owned, it readies the util image the owners' step needs after the
// switch.
func TestCheckMountsRelease(t *testing.T) {
	s := newSyncSetup(t)
	out, code := s.run([]string{"FAKE_APP_MOUNT=/../cache"}, "@prod", "--release")
	if code == 0 || s.current("prod") != "" || strings.Contains(out, "deploy in") || !strings.Contains(out, "devopsy: error: the mounts check refuses the release: it does not go live") || !strings.Contains(out, "app mounts ./.devopsy/cache writable: it is in the release, and the next one replaces it") || !strings.Contains(out, "not made current") {
		t.Errorf("a writable mount in the release (%d), current %q:\n%s", code, s.current("prod"), out)
	}

	// A prepare step makes the secret compose requires (drupal11's and
	// the registry's ${DB_PASSWORD:?}): the check still sees the mount.
	config := filepath.Join(s.dot, "config.yaml")
	write(t, config, readString(config)+"  secret: {release: {prepare: [secrets], run: deploy}}\n", 0o644)
	write(t, filepath.Join(s.dot, "commands", "secrets"), "#!/bin/sh\n## Description: generates the secrets\ngrep -q '^SECRET=' \"$DEVOPSY_PROJECT_DIR/.env\" 2>/dev/null || echo SECRET=generated >> \"$DEVOPSY_PROJECT_DIR/.env\"\n", 0o755)
	// compose config fails while SECRET is unset, as for ${SECRET:?}.
	write(t, s.log+".shop-secret", "FAKE_APP_MOUNT=/../cache\ncase \"$*\" in *\" config --format json\") [ -n \"${SECRET:-}\" ] || { echo 'required variable SECRET is missing a value' >&2; exit 1; } ;; esac\n", 0o644)
	out, code = s.run(nil, "@secret", "--release")
	if code == 0 || s.current("secret") != "" || !strings.Contains(out, "app mounts ./.devopsy/cache writable") {
		t.Errorf("a first release whose prepare makes its secrets (%d), current %q:\n%s", code, s.current("secret"), out)
	}

	// The util image missing and its build failing (a registry down):
	// refused before the switch, not failed after it.
	write(t, s.log+".shop-slow", "FAKE_APP_OWNER=10001:10001\n", 0o644)
	out, code = s.run([]string{"FAKE_NO_UTIL=1", "FAIL_UTIL_BUILD=1"}, "@slow", "--release")
	docker, _ := s.logs()
	if code == 0 || s.current("slow") != "" || !strings.Contains(out, "building devopsy-util:2") || !strings.Contains(out, "not made current") || strings.Contains(docker, "devopsy-owners-") {
		t.Errorf("no util image (%d), current %q:\n%s\n%s", code, s.current("slow"), out, docker)
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
// made, and a bind source below it, and on Linux an entry not its
// owner's is given away in a util container, only then; on macOS (Docker Desktop, OrbStack) ownership is
// never checked. A command that starts nothing, or a project whose
// compose files cannot carry the label, reads no labels: its YAML keys
// are read, never its comments (every template's say "included"
// somewhere, which once made every local up pay a compose config).
func TestMntOwnersLocal(t *testing.T) {
	s := newSyncSetup(t)
	compose := filepath.Join(s.dot, "compose.yaml")
	write(t, compose, "# Every request's headers included; extends: nothing.\n"+readString(compose)+"    # devopsy.mnt.owner is not here\n    environment: {A: b} # nor devopsy.mnt.owner\n", 0o644)
	owned := []string{"FAKE_APP_OWNER=10001:10001", "FAKE_APP_MOUNT2=/storage/files"}
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
		if code != 0 || !fileExists(filepath.Join(s.dot, "mnt", "storage", "files")) || container != (runtime.GOOS != "darwin") || !strings.Contains(docker, "compose -f "+compose) {
			t.Errorf("%v (%d), the util container %v:\n%s\n%s", args, code, container, out, docker)
		}
	}
	if runtime.GOOS == "darwin" {
		return
	}
	// The entry there and its owner's (the test's own user): nothing to do.
	uid, gid := os.Getuid(), os.Getgid()
	if out, code := s.run([]string{"FAKE_APP_OWNER=" + strconv.Itoa(uid) + ":" + strconv.Itoa(gid), "FAKE_APP_MOUNT2=/storage/files"}, "start"); code != 0 {
		t.Fatalf("start (%d):\n%s", code, out)
	}
	if docker, _ := s.logs(); strings.Contains(docker, "devopsy-owners-") {
		t.Errorf("an entry its owner's given away again:\n%s", docker)
	}
}

// The util container's scripts parse (sh -n, as the servers' scripts
// do in internal/remote), and the scan (chownEntry, a release's and an
// import's) survives a file a running service removes meanwhile: its
// chown fails, find with it, and only what is left with another owner
// fails the scan (review of 2026-10-23: the release failed after its
// switch and went back). The fake find takes every file for another
// owner's, as a scan of an entry another machine wrote would, then asks
// the real one what is left.
func TestMntScripts(t *testing.T) {
	for name, script := range map[string]string{"ownEntries": ownEntries, "emptyAndExtract": emptyAndExtract} {
		if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
	tmp := t.TempDir()
	mnt, bin := filepath.Join(tmp, "mnt"), filepath.Join(tmp, "bin")
	write(t, filepath.Join(bin, "find"), `#!/bin/sh
case " $* " in *" -exec"*) for a; do [ "${p:-}" = -h ] && o=$a; p=$a; done; exec /usr/bin/find "$1" -execdir chown -h "$o" {} + ;; esac
exec /usr/bin/find "$@"
`, 0o755)
	// The app removes tmp-123 between find's look and the chown.
	write(t, filepath.Join(bin, "chown"), `#!/bin/sh
shift 2
for f; do
  case $f in *tmp-*) rm -f "$f"; echo "chown: cannot access '$f': No such file or directory" >&2; st=1 ;; *) echo "$f" >> "$CHOWNED" ;; esac
done
exit ${st:-0}
`, 0o755)
	script := strings.ReplaceAll(strings.ReplaceAll(ownEntries, `"/mnt/`, `"`+mnt+`/`), "d=/mnt ", "d="+mnt+" ")
	own := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	// Bind sources below an entry: each directory missing made and its
	// owner's, never past a link the app made (it would reach another
	// entry); only entries scanned.
	write(t, filepath.Join(mnt, "db", "x"), "x\n", 0o644)
	write(t, filepath.Join(mnt, "storage", "keep.txt"), "x\n", 0o644)
	if err := os.Symlink(filepath.Join(mnt, "db"), filepath.Join(mnt, "storage", "link")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", script, "sh", "storage="+own, "storage/files/cache="+own, "storage/link/y="+own, "logs/a="+own)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	if out, err := cmd.CombinedOutput(); err != nil || !fileExists(filepath.Join(mnt, "storage", "files", "cache")) || !fileExists(filepath.Join(mnt, "logs", "a")) || fileExists(filepath.Join(mnt, "db", "y")) {
		t.Errorf("paths (%v):\n%s", err, out)
	}
	os.Remove(filepath.Join(mnt, "storage", "link"))
	for owner, fails := range map[string]bool{own: false, "99999:99999": true} {
		write(t, filepath.Join(mnt, "storage", "keep.txt"), "x\n", 0o644)
		write(t, filepath.Join(mnt, "storage", "tmp-123"), "x\n", 0o644)
		chowned := filepath.Join(tmp, "chowned")
		os.Remove(chowned)
		cmd := exec.Command("sh", "-c", script, "sh", "storage="+owner)
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "CHOWNED=" + chowned}
		out, err := cmd.CombinedOutput()
		if (err != nil) != fails || strings.Contains(string(out), "cannot access") == !fails {
			t.Errorf("owner %s, a file the app removed during the scan, nothing else left: %v (fails: %v)\n%s", owner, err, fails, out)
		}
		if got := readString(chowned); !strings.Contains(got, "keep.txt") {
			t.Errorf("the rest not given away: %q", got)
		}
	}
}
