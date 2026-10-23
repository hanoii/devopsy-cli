package main

import (
	"archive/tar"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A release gives its owned entries to their owners right after its
// switch, before the run step: a missing entry made 0755, every entry
// scanned whole (chown -h, batched, never through a link) in a util
// container of its own that mounts that entry alone, without a network
// (review of 2026-10-23, round 2: one container mounted all of mnt, so a
// directory the app swapped for a link during the scan could lead chown
// into another entry).
// A child's first sync rsyncs into an entry the child owns as its owner
// (--chown), from the child's labels only.
func TestMntOwnersRelease(t *testing.T) {
	s := newSyncSetup(t)
	config := filepath.Join(s.dot, "config.yaml")
	write(t, config, strings.Replace(readString(config), "  release: deploy\n", "  release: deploy\n  rollback: deploy\n", 1), 0o644)
	// The run step runs a plain up, as the templates' deploy does: in a
	// release on a server it reads no labels (a rollback's below).
	compose := filepath.Join(s.dot, "compose.yaml")
	write(t, compose, readString(compose)+"    labels: [devopsy.mnt.owner=10001:10001]\n", 0o644)
	deploy := filepath.Join(s.dot, "commands", "deploy")
	write(t, deploy, readString(deploy)+"exec devopsy up -d\n", 0o755)
	write(t, s.log+".shop-prod", "FAKE_APP_OWNER=10001:10001 FAKE_APP_MOUNT2=/cache\n", 0o644)
	if err := os.MkdirAll(filepath.Join(s.mnt("prod"), "storage"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, code := s.run(nil, "@prod", "--release")
	docker, _ := s.logs()
	switched, giving, deployed := strings.Index(out, "current is now"), strings.Index(out, "devopsy: giving mnt/cache to 10001:10001, mnt/storage to 10001:10001"), strings.Index(out, "deploy in shop-prod")
	if code != 0 || switched < 0 || giving < switched || deployed < giving {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	for _, want := range []string{
		"run --rm --name devopsy-owners-shop-prod-", " --network none --mount type=bind,source=" + s.mnt("prod") + "/cache,target=/mnt/cache devopsy-util:2 sh -c set -eu\n",
		`find "/mnt/$1" -ignore_readdir_race \( ! -user "${2%%:*}" -o ! -group "${2#*:}" \) -exec chown -h "$2" {} +`, "chown_entry \"$1\" \"$2\"\n sh cache 10001:10001\n",
		"-1 --network none --mount type=bind,source=" + s.mnt("prod") + "/storage,target=/mnt/storage devopsy-util:2 sh -c set -eu\n", "chown_entry \"$1\" \"$2\"\n sh storage 10001:10001\n",
	} {
		if !strings.Contains(docker, want) {
			t.Errorf("missing %q in:\n%s", want, docker)
		}
	}
	if strings.Contains(docker, ":/mnt devopsy-util") && strings.Contains(docker, "devopsy-owners-") {
		t.Errorf("an owners' container mounts all of mnt:\n%s", docker)
	}
	// The missing entry made 0755, whatever the umask; one there left as
	// it is, for the container to give away.
	for e, mode := range map[string]os.FileMode{"cache": 0o755, "storage": 0o700} {
		if fi, err := os.Lstat(filepath.Join(s.mnt("prod"), e)); err != nil || !fi.IsDir() || fi.Mode().Perm() != mode {
			t.Errorf("mnt/%s: %v %v, want a directory %v", e, fi, err, mode)
		}
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
	if code != 0 || !strings.Contains(docker, "rsync -a --delete --numeric-ids --modify-window=-1 --chown=10001:10001 /src/storage/ /dst/storage/\n") || !strings.Contains(docker, "rsync -a --delete --numeric-ids --modify-window=-1 /src/db/ /dst/db/\n") || strings.Contains(docker, "20000") {
		t.Errorf("a child's first release (%d):\n%s\n%s", code, out, docker)
	}

	// An mnt import on a target gives what it extracted to its owner,
	// before it counts as complete, in a container that mounts that
	// entry alone, named so that a leftover import's end removes it
	// (importContainers). Locally on macOS it gives nothing away
	// (TestExportImport).
	_, errs, code := s.runIO(nil, tarOf(t, map[string]string{"storage/new.txt": "new\n"}), "@dev", "--import", "app", "--yes")
	docker, _ = s.logs()
	if code != 0 || !strings.Contains(errs, "devopsy: importing app: mnt/storage replaced\ndevopsy: giving mnt/storage to 10001:10001\n") || !regexp.MustCompile(`run --rm --name devopsy-import-shop-dev-[0-9]+-owners-0 --network none --mount type=bind,source=`+regexp.QuoteMeta(s.mnt("dev"))+`/storage,target=/mnt/storage devopsy-util:2 sh -c set -eu`).MatchString(docker) {
		t.Errorf("an owned import on a target (%d):\n%s\n%s", code, errs, docker)
	}

	// A rollback gives owners by the restored release's labels; labels
	// this devopsy cannot read there (here a name from before
	// devopsy.data) only warn, before the switch and after it, nothing
	// given away: a rollback never fails for them, as it never checks
	// roles or imports (review of 2026-10-23: it failed, and current
	// went back). Its run step's up reads no labels at all (review of
	// 2026-10-23, round 2: the local owners' step before up failed it on
	// them, and the restart after it too: AGENTS.md, old labels are
	// errors only where labels are read, never in Build).
	first := s.current("prod")
	s.releaseProd(nil)
	if s.current("prod") == first {
		t.Fatalf("second release did not go live")
	}
	// The util image the restored release's owners need, gone since (a
	// prune) and not buildable: the rollback fails before its switch,
	// nothing changed (review of 2026-10-23, round 2: it failed after
	// the switch, and went back).
	second := s.current("prod")
	out, code = s.run([]string{"FAKE_NO_UTIL=1", "FAIL_UTIL_BUILD=1"}, "@prod", "--rollback")
	if docker, _ := s.logs(); code == 0 || s.current("prod") != second || !strings.Contains(out, "building devopsy-util:2") || !strings.Contains(out, "nothing changed: current is still") || strings.Contains(out, "current is now") || strings.Contains(docker, "devopsy-owners-") {
		t.Errorf("a rollback without the util image (%d), current %q, want %q:\n%s\n%s", code, s.current("prod"), second, out, docker)
	}
	// Two owners on one entry in the restored release: labels its
	// owners' step cannot read, so the check before the switch only
	// warns too.
	write(t, s.log+".shop-prod", "FAKE_APP_MOUNT=/db FAKE_APP_OWNER=1:1 FAKE_DB_OWNER=2:2\n", 0o644)
	out, code = s.run(nil, "@prod", "--rollback")
	if code != 0 || s.current("prod") != first || !strings.Contains(out, "devopsy: warning: the restored release's labels cannot be read: the entries of mnt will keep their owners\ndevopsy: | ./mnt/db is written by") {
		t.Errorf("a rollback to a release with two owners on an entry (%d), current %q, want %q:\n%s", code, s.current("prod"), first, out)
	}
	write(t, s.log+".shop-prod", "", 0o644)
	s.releaseProd(nil)
	s.logs()
	write(t, s.log+".shop-prod", "FAKE_APP=old\n", 0o644)
	out, code = s.run(nil, "@prod", "--rollback")
	if code != 0 || s.current("prod") != first || !strings.Contains(out, "devopsy: warning: the restored release's labels cannot be read: the entries of mnt will keep their owners\ndevopsy: | service app: devopsy.sync=rsync") || !strings.Contains(out, "devopsy: warning: the entries of mnt keep their owners: the restored release's labels cannot be read\ndevopsy: | service app: devopsy.sync=rsync: that label is now devopsy.data") || strings.Contains(out, "error") || strings.Contains(out, "the entries of mnt are left as they are") || !strings.Contains(out, "deploy in shop-prod") {
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
// owned, it refuses one that is a symbolic link in shared/mnt, and
// readies the util image the owners' step needs after the switch.
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

	// An owned entry that is a symbolic link: Docker would mount what it
	// points to.
	write(t, s.log+".shop-solo", "FAKE_APP_OWNER=10001:10001\n", 0o644)
	if err := os.MkdirAll(s.mnt("solo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(s.mnt("solo"), "storage")); err != nil {
		t.Fatal(err)
	}
	out, code = s.run(nil, "@solo", "--release")
	if code == 0 || s.current("solo") != "" || !strings.Contains(out, "devopsy: | mnt/storage is a symbolic link") || !strings.Contains(out, "make each a directory of its own in shared/mnt") {
		t.Errorf("an owned entry a link (%d), current %q:\n%s", code, s.current("solo"), out)
	}
	s.logs()

	// The util image missing and its build failing (a registry down):
	// refused before the switch, not failed after it.
	write(t, s.log+".shop-slow", "FAKE_APP_OWNER=10001:10001\n", 0o644)
	out, code = s.run([]string{"FAKE_NO_UTIL=1", "FAIL_UTIL_BUILD=1"}, "@slow", "--release")
	docker, _ := s.logs()
	if code == 0 || s.current("slow") != "" || !strings.Contains(out, "building devopsy-util:2") || !strings.Contains(out, "not made current") || strings.Contains(docker, "devopsy-owners-") {
		t.Errorf("no util image (%d), current %q:\n%s\n%s", code, s.current("slow"), out, docker)
	}

	// What the owners' step refuses after the switch, the check refuses
	// before it (or neither does): here a leftover
	// .devopsy/capabilities/sync/, which SyncServices refuses and
	// the check once parsed past. Review of 2026-10-23 (round 2):
	// the check passed, the release went live and failed in
	// --mnt-owners, for a project with no owner at all (on main, a
	// release that does not sync never read it).
	write(t, filepath.Join(s.dot, "capabilities", "sync", "prepare"), "#!/bin/sh\nexit 0\n", 0o755)
	out, code = s.run(nil, "@prod", "--release")
	if code == 0 || strings.Contains(out, "current is now") || !strings.Contains(out, "devopsy: error: the mounts check refuses the release, whose labels cannot be read: it does not go live\ndevopsy: | .devopsy/capabilities/sync/ is now .devopsy/capabilities/data/") {
		t.Errorf("refused after the switch, not before (%d), current %q:\n%s", code, s.current("prod"), out)
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
	wipe, owners := strings.Index(docker, "-wipe -v "), strings.Index(docker, "-owners-0 --network none --mount type=bind,source="+s.mnt("dev")+"/storage,target=/mnt/storage ")
	if code != 0 || wipe < 0 || owners < wipe || !strings.Contains(docker, "run --rm --name devopsy-sync-") {
		t.Errorf("reset (%d):\n%s\n%s", code, out, docker)
	}
}

// Locally, before compose starts anything, an owned entry missing is
// made, and on Linux an entry not its
// owner's is given away in a util container, only then; on macOS (Docker Desktop, OrbStack) ownership is
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
	// Labels devopsy cannot read (a name from before a change) are a
	// warning, and compose's command goes on: it says what is wrong with
	// its own files (review of 2026-10-23, round 2: up failed on them).
	out, code := s.run(append(slices.Clone(owned), "FAKE_APP=old"), "up", "-d")
	if docker, _ := s.logs(); code != 0 || !strings.Contains(out, "devopsy: warning: the entries of mnt are left as they are: the labels cannot be read\ndevopsy: | service app: devopsy.sync=rsync") || !strings.Contains(docker, "compose -f "+compose+" up -d") {
		t.Errorf("up on labels that cannot be read (%d):\n%s\n%s", code, out, docker)
	}
	// compose's watch starts the services first too (unless --no-up),
	// and scale creates containers (review of 2026-10-23, round 2: both
	// left the entries for Docker to make root's).
	for _, args := range [][]string{{"watch"}, {"scale", "app=2"}, {"up", "-d"}, {"--profile", "web", "run", "--rm", "app"}} {
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
// do in internal/remote), and the scan (chownEntry, a release's and an
// import's) survives a file a running service removes meanwhile: its
// chown fails, find with it, and only what is left with another owner
// fails the scan (review of 2026-10-23: the release failed after its
// switch and went back), or a look again that fails itself, a directory
// find cannot read (review of 2026-10-23, round 2: it passed, 2>/dev/null
// || true). The fake find takes every file for another owner's, as a
// scan of an entry another machine wrote would, then asks the real one
// what is left (GNU's -ignore_readdir_race left out: macOS's has none).
func TestMntScripts(t *testing.T) {
	for name, script := range map[string]string{"ownEntry": ownEntry, "emptyAndExtract": emptyAndExtract} {
		if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
	tmp := t.TempDir()
	mnt, bin := filepath.Join(tmp, "mnt"), filepath.Join(tmp, "bin")
	write(t, filepath.Join(bin, "find"), `#!/bin/sh
p=$1; shift
for a; do shift; [ "$a" = -ignore_readdir_race ] || set -- "$@" "$a"; done
case " $* " in *" -exec "*) for a; do [ "${q:-}" = -h ] && o=$a; q=$a; done; exec /usr/bin/find "$p" -exec chown -h "$o" {} + ;; esac
[ -z "${FIND_FAIL:-}" ] || { echo "find: '$p/private': Permission denied" >&2; exit 1; }
exec /usr/bin/find "$p" "$@"
`, 0o755)
	// The app removes tmp-123 between find's look and the chown.
	write(t, filepath.Join(bin, "chown"), `#!/bin/sh
shift 2
for f; do
  case $f in *tmp-*) rm -f "$f"; echo "chown: cannot access '$f': No such file or directory" >&2; st=1 ;; *) echo "$f" >> "$CHOWNED" ;; esac
done
exit ${st:-0}
`, 0o755)
	script := strings.ReplaceAll(ownEntry, `"/mnt/`, `"`+mnt+`/`)
	own := strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())
	for _, c := range []struct {
		owner, findFail string
		fails           bool
	}{{own, "", false}, {"99999:99999", "", true}, {own, "1", true}} {
		write(t, filepath.Join(mnt, "storage", "keep.txt"), "x\n", 0o644)
		write(t, filepath.Join(mnt, "storage", "tmp-123"), "x\n", 0o644)
		chowned := filepath.Join(tmp, "chowned")
		os.Remove(chowned)
		cmd := exec.Command("sh", "-c", script, "sh", "storage", c.owner)
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "CHOWNED=" + chowned, "FIND_FAIL=" + c.findFail}
		out, err := cmd.CombinedOutput()
		if (err != nil) != c.fails || strings.Contains(string(out), "cannot access") == !c.fails {
			t.Errorf("owner %s, find failing %q, a file the app removed during the scan: %v (fails: %v)\n%s", c.owner, c.findFail, err, c.fails, out)
		}
		if c.findFail != "" && !strings.Contains(string(out), "Permission denied") {
			t.Errorf("find's own failure not said:\n%s", out)
		}
		if got := readString(chowned); !strings.Contains(got, "keep.txt") {
			t.Errorf("the rest not given away: %q", got)
		}
	}

	// A tree of many directories, all of another owner (data from before
	// the label, an import of a tar made elsewhere) is given away in a few
	// chowns, as find -exec ... + batches them, not one per directory.
	// Review of 2026-10-23 (round 2): -execdir runs chown once per
	// directory; measured in devopsy-util:2 on a Docker volume, 20,200
	// directories of 40,201 inodes took 22.7 s against 0.43 s for -exec
	// +, after the switch (a release) or with the services stopped (an
	// import), so a Drupal files tree took minutes.
	os.RemoveAll(filepath.Join(mnt, "storage"))
	for i := range 200 {
		write(t, filepath.Join(mnt, "storage", "d"+strconv.Itoa(i), "f"), "x\n", 0o644)
	}
	calls := filepath.Join(tmp, "calls")
	write(t, filepath.Join(bin, "chown"), "#!/bin/sh\necho >> \"$CALLS\"\n", 0o755)
	cmd := exec.Command("sh", "-c", script, "sh", "storage", own)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "CALLS=" + calls}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("a tree of many directories: %v\n%s", err, out)
	}
	if n := strings.Count(readString(calls), "\n"); n > 10 {
		t.Errorf("a tree of 200 directories given away in %d chowns", n)
	}
}

// The owners' container's mount: its source one CSV field, as Docker
// parses --mount, so an entry's name with a comma stays one path; the
// entry made first when missing, and refused when it is a symbolic link
// (Docker would mount what it points to).
func TestOwnersMount(t *testing.T) {
	if got, err := bindMount("/srv/mnt/a,b", "/mnt/a,b"); err != nil || got != `type=bind,"source=/srv/mnt/a,b","target=/mnt/a,b"` {
		t.Errorf("bindMount: %q %v", got, err)
	}
	mnt := filepath.Join(t.TempDir(), "mnt")
	if err := makeEntry(mnt, "storage"); err != nil || !fileExists(filepath.Join(mnt, "storage")) {
		t.Errorf("a missing entry: %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(mnt, "link")); err != nil {
		t.Fatal(err)
	}
	if err := makeEntry(mnt, "link"); err == nil || !strings.Contains(err.Error(), "mnt/link is a symbolic link") {
		t.Errorf("an entry a link: %v", err)
	}
}

// An mnt import extracts as root in a util container that mounts that
// import's entries alone, never all of mnt (review of 2026-10-23, round
// 3). The service whose entry it is runs meanwhile unless its stop label
// says otherwise (devopsy.data.stop defaults to never: app is not
// stopped here), and it owns that entry (devopsy.mnt.owner), so it can
// swap a directory the extraction just made for a link to ../<another
// entry> (renameat2's RENAME_EXCHANGE, or a link in place of a directory
// a tar implies without a member of its own, as tarOf's do): GNU tar
// follows it and writes the tar's files, as root, into that entry.
// Checked on alpine 3.22's tar with the compromised app as 10001 without
// capabilities, mounting only its entry: auth/htpasswd of another entry
// replaced with the tar's, 2994 files planted there as root. Round 2
// gave each owned entry's chown a container of its own for this very
// reason; the extraction writes content, not only owners.
func TestImportExtractsInItsEntriesAlone(t *testing.T) {
	s := newSyncSetup(t)
	mnt := filepath.Join(s.dot, "mnt")
	write(t, filepath.Join(mnt, "storage", "old.txt"), "local\n", 0o644)
	write(t, filepath.Join(mnt, "auth", "htpasswd"), "admin:secret\n", 0o644)
	_, errs, code := s.runIO([]string{"FAKE_APP_OWNER=10001:10001"}, tarOf(t, map[string]string{"storage/dir/htpasswd": "pwned\n"}), "--import", "app")
	docker, _ := s.logs()
	if code != 0 {
		t.Fatalf("--import app (%d):\n%s\n%s", code, errs, docker)
	}
	if strings.Contains(docker, " stop app") {
		t.Fatalf("app was stopped for its import, which this test needs running:\n%s", docker)
	}
	if strings.Contains(docker, "-v "+mnt+":/mnt devopsy-util") {
		t.Errorf("the extracting container mounts all of mnt (auth included) while app, the entry's owner, runs:\n%s", docker)
	}
}

// An mnt import whose owners' step fails after its extraction keeps its
// mark, and --resolved is the documented way out ("accepts the data as
// it is"); it then starts the services on the entry as the tar's maker
// left it (review of 2026-10-23, round 3): a tar made anywhere carries
// its maker's owners (1000 on a laptop, root), so an app running as
// 10001 from its first process, or MariaDB as 999, cannot write it (or
// read a datadir's mode-600 files) until the next release's
// --mnt-owners. --resolved should give the owned entries to their
// owners before it starts anything, as the import would have.
func TestImportResolvedGivesOwners(t *testing.T) {
	s := newSyncSetup(t)
	write(t, s.log+".shop-prod", "FAKE_APP_OWNER=10001:10001\n", 0o644)
	s.releaseProd(nil)
	_, errs, code := s.runIO([]string{"FAIL_OWNERS=1"}, tarOf(t, map[string]string{"storage/new.txt": "new\n"}), "@prod", "--import", "app", "--yes")
	docker, _ := s.logs()
	if code == 0 || !strings.Contains(docker, "-owners-0 --network none") || !strings.Contains(errs, "the import of app did not complete") {
		t.Fatalf("an import whose owners' step fails (%d):\n%s\n%s", code, errs, docker)
	}
	_, errs, code = s.runIO(nil, nil, "@prod", "--import", "app", "--resolved", "--yes")
	docker, _ = s.logs()
	if code != 0 || !strings.Contains(docker, " start ") {
		t.Fatalf("--resolved (%d):\n%s\n%s", code, errs, docker)
	}
	if !strings.Contains(docker, "--mount type=bind,source="+s.mnt("prod")+"/storage,target=/mnt/storage devopsy-util:2") {
		t.Errorf("--resolved started the services on mnt/storage as the tar left it, never given to 10001:10001:\n%s\n%s", errs, docker)
	}
}

// The local owners' step never stops a compose command (round 2's rule:
// a label devopsy reads must never stop a plain up), yet an owned entry
// that is a symbolic link locally (an entry shared between worktrees, a
// database on another disk) fails every up before compose runs: makeEntry
// refuses it, on macOS and Linux alike (review of 2026-10-23, round 3).
// On a server the mounts check refuses one before the switch; locally
// Docker mounts what it points to, which is what the developer chose, so
// it is at most a warning.
func TestMntOwnersLocalLinkedEntry(t *testing.T) {
	s := newSyncSetup(t)
	compose := filepath.Join(s.dot, "compose.yaml")
	write(t, compose, readString(compose)+"    labels: [devopsy.mnt.owner=10001:10001]\n", 0o644)
	elsewhere := filepath.Join(t.TempDir(), "storage")
	if err := os.MkdirAll(elsewhere, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.dot, "mnt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(s.dot, "mnt", "storage")); err != nil {
		t.Fatal(err)
	}
	out, code := s.run([]string{"FAKE_APP_OWNER=10001:10001"}, "up", "-d")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(docker, "compose -f "+compose+" up -d") {
		t.Errorf("up with a linked owned entry (%d) never ran compose:\n%s\n%s", code, out, docker)
	}
}

// An mnt import refuses device nodes (review of 2026-10-23, round 3):
// GNU tar as root makes them in shared/mnt on the host, where bind mounts
// are not nodev, and the owners' step then gives one to the entry's
// owner, the app's UID. A block device of the server's disk (b 8 0), mode
// 0666 in the tar, is the raw disk for any user on the host, the deploy
// user's root-equivalent made plain root. The member reaches out of its
// entry as surely as a link to ../ does; a tar of an entry never needs
// one (a FIFO or a socket neither).
func TestImportRefusesDevices(t *testing.T) {
	for _, typ := range []byte{tar.TypeBlock, tar.TypeChar} {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		for _, h := range []*tar.Header{
			{Name: "storage/", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: "storage/disk", Typeflag: typ, Mode: 0o666, Devmajor: 8, Devminor: 0},
		} {
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		spool := filepath.Join(t.TempDir(), "spool")
		write(t, spool, b.String(), 0o600)
		if entries, err := tarEntries(spool); err == nil {
			t.Errorf("a tar with a device node (type %q) below its entry passes: %v", typ, entries)
		}
	}
}
