package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/cli"
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

// Review 2026-10-23: the owners' scan (chownEntry, in ownEntries and
// emptyAndExtract) runs while the services still run (a release's old
// containers until its run step; an mnt import's service unless its
// labels stop it), and fails when one of them removes a file during the
// scan: find hands the file to chown, which no longer finds it, find exits
// 1, set -e ends the script, and the release fails after its switch and
// goes back. Measured in devopsy-util:2 (alpine, GNU findutils and
// coreutils) on a 10,000-file entry all 10001's, with a container running
// as 10001 creating and removing temporary files there: 31 of 200 scans
// failed with "chown: cannot access '/mnt/storage/d10/t26396': No such
// file or directory". A sync's rsync takes the same race as a notice
// (RsyncVanished); a vanished file has no owner to fix. Here a chown that
// finds one of its files gone, as in the race (the rest it changes).
func TestReviewOwnersScanVanishedFile(t *testing.T) {
	tmp := t.TempDir()
	mnt, bin := filepath.Join(tmp, "mnt"), filepath.Join(tmp, "bin")
	write(t, filepath.Join(mnt, "storage", "keep.txt"), "x\n", 0o644)
	write(t, filepath.Join(mnt, "storage", "tmp-123"), "x\n", 0o644)
	// The app removes tmp-123 between find's look and the chown.
	write(t, filepath.Join(bin, "chown"), `#!/bin/sh
shift 2
for f; do
  case $f in */tmp-*) rm -f "$f"; echo "chown: cannot access '$f': No such file or directory" >&2; st=1 ;; *) echo "$f" >> "$CHOWNED" ;; esac
done
exit ${st:-0}
`, 0o755)
	script := strings.ReplaceAll(ownEntries, `"/mnt/`, `"`+mnt+`/`)
	cmd := exec.Command("sh", "-c", script, "sh", "storage=99999:99999")
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "CHOWNED=" + filepath.Join(tmp, "chowned")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("a file the app removed during the scan failed it (%v):\n%s", err, out)
	}
	if got := readString(filepath.Join(tmp, "chowned")); !strings.Contains(got, "keep.txt") {
		t.Errorf("the rest not given away: %q", got)
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

// Review 2026-10-23: Plan.Starts reads compose files as text and takes
// any "include" or "extends" for a file that includes or extends others
// (cli.mayOwnMnt), so a comment saying "included" (every template has
// one: Traefik's "projects' included", whoami's "headers included",
// drupal11's "its app included") makes every local up, start, run and
// create pay a compose config (and a second Build, the env capability
// included) for a project without devopsy.mnt.owner, which AGENTS.md
// says it never does ("only when the compose files mention the label or
// include or extend others: no compose config for every command").
func TestReviewStartsIncludedInAComment(t *testing.T) {
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "compose.yaml"), "# Every request's headers included.\nservices:\n  app:\n    image: x\n  db:\n    image: y\n", 0o644)
	if out, code := s.run(nil, "up", "-d"); code != 0 {
		t.Fatalf("up (%d):\n%s", code, out)
	}
	if docker, _ := s.logs(); strings.Contains(docker, "config --format json") {
		t.Errorf("labels read for a project without the label, its compose file saying \"included\":\n%s", docker)
	}
}

// Review 2026-10-23: the child guard no longer sees a service that
// mounts ./mnt whole. Without data or an owner such a mount is ignored
// now (parseSyncServices drops "."), so the child's list names no entry
// for it, and a sync rsyncs storage under it while it runs, writable:
// before this change "." was its entry and syncConflicts refused ("mnt/.
// would change under it"). The guard's rule is "a child service mounting
// a copied entry is running and not mnt there" (AGENTS.md, Labels and
// the child guard); a writable tool over all of mnt (a file manager, a
// backup agent) mounts every copied entry. The README's exception is
// only for a read-only tool.
func TestReviewChildGuardMntWhole(t *testing.T) {
	dot := filepath.Join(t.TempDir(), "code", ".devopsy")
	child, err := cli.ParseSyncServices([]byte(`{"services":{"files":{"volumes":[{"type":"bind","source":"`+dot+`/mnt","target":"/data"}]}}}`), dot)
	if err != nil {
		t.Fatal(err)
	}
	list := fmt.Sprintf("%s\t%s\trunning\t%s\t%s\n", child[0].Name, syncListLabel(child[0]), strings.Join(child[0].Dirs, " "), listOwners(child[0]))
	got, _ := parseChildServices(list)
	parent := []cli.SyncService{{Name: "app", Transfer: cli.DataMnt, Dirs: []string{"storage"}, HasData: true}}
	if conflicts := syncConflicts(parent, got); len(conflicts) == 0 {
		t.Errorf("the child's files runs, mounting ./mnt whole writable, and the sync would rsync storage under it: no conflict (its list line %q)", list)
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
