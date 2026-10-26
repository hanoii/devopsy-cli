package main

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// tarNames are the member names of the tar at path.
func tarNames(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tr := tar.NewReader(bytes.NewReader(data)); ; {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		names = append(names, strings.TrimSuffix(h.Name, "/"))
	}
}

var snapshotID = regexp.MustCompile(`taking snapshot ([0-9]{14}) of ([0-9]{14})`)

// A manual take: a first pass into the mirror while everything runs,
// then every service with data and its dependents stopped, the mirror
// brought in line, the services started again, and the tars from the
// mirror afterwards; the snapshot holds the release's code and listing,
// a manifest and the environment's variables; its stop lines go once
// the services are back; keep counts manual snapshots; manual: false and
// a marked environment refuse.
func TestSnapshotTake(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	write(t, filepath.Join(s.root, "shop", "prod", "shared", ".env"), "DB_PASSWORD=secret\n", 0o600)

	out, code := s.run(nil, "@prod", "--snapshot")
	docker, _ := s.logs()
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil || !strings.Contains(out, "snapshot "+m[1]+" taken") {
		t.Fatalf("take (%d):\n%s\n%s", code, out, docker)
	}
	id, rel := m[1], m[2]
	snap := filepath.Join(s.root, "shop", "prod", "snapshots", id)
	for _, f := range []string{"manifest", "devopsy-images", "code/.devopsy/compose.yaml", "devopsy-target.env", "data/shared.env", "data/images"} {
		if !fileExists(filepath.Join(snap, f)) {
			t.Errorf("the snapshot lacks %s", f)
		}
	}
	if got := strings.Join(tarNames(t, filepath.Join(snap, "data", "mnt.db.tar.zst")), " "); !strings.Contains(got, "db/ibdata") {
		t.Errorf("db's tar: %s", got)
	}
	if got := strings.Join(tarNames(t, filepath.Join(snap, "data", "mnt.storage.tar.zst")), " "); !strings.Contains(got, "storage/file.txt") {
		t.Errorf("storage's tar: %s", got)
	}
	manifest := readString(filepath.Join(snap, "manifest"))
	if !strings.HasPrefix(manifest, "format\t1\nkind\tmanual\nrelease\t"+rel+"\n") || !strings.Contains(manifest, "service\tdb\tvolumes\tdb\t\n") || !strings.Contains(manifest, "entry\tstorage\tdir\n") {
		t.Errorf("manifest:\n%s", manifest)
	}
	order := []string{"-mirror-", "/src/db/ /dst/db/", "stop db app", "-stopped-", "/src/db/ /dst/db/", "start db app", "-tar "}
	rest := docker
	for _, want := range order {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("missing %q in order:\n%s", want, docker)
		}
		rest = rest[i+len(want):]
	}
	if rec := readString(filepath.Join(s.root, "shop", "prod", ".devopsy-sync.services")); strings.Contains(rec, "snapshot ") {
		t.Errorf("snapshot lines left in the record:\n%s", rec)
	}

	// keep: 3 by default; a fourth take removes the oldest.
	for range 3 {
		if out, code := s.run(nil, "@prod", "--snapshot"); code != 0 {
			t.Fatalf("take (%d):\n%s", code, out)
		}
	}
	s.logs()
	if ids := manualSnapshots(snapshotPaths{snaps: filepath.Join(s.root, "shop", "prod", "snapshots")}); len(ids) != 3 || ids[0] == id {
		t.Errorf("kept: %v (the first %s)", ids, id)
	}

	// Listed newest first; removed by a unique prefix, with --yes; a word
	// naming none refuses.
	out, code = s.run(nil, "@prod", "--snapshots")
	ids := manualSnapshots(snapshotPaths{snaps: filepath.Join(s.root, "shop", "prod", "snapshots")})
	if code != 0 || !strings.HasPrefix(out, "SNAPSHOT        KIND    RELEASE         TAKEN                 SIZE\n"+ids[2]+"  manual  "+rel) || strings.Count(out, " manual ") != 3 {
		t.Errorf("--snapshots (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@prod", "--snapshots", "remove", "1999"); code == 0 || !strings.Contains(out, "has no snapshot 1999: nothing removed") {
		t.Errorf("remove a snapshot that is none (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@prod", "--snapshots", "remove", ids[0], "--yes"); code != 0 || !strings.Contains(out, "removing snapshot "+ids[0]) || fileExists(filepath.Join(s.root, "shop", "prod", "snapshots", ids[0])) {
		t.Errorf("remove (%d):\n%s", code, out)
	}
	s.logs()

	// A take cut off leaves its .tmp: INCOMPLETE, and IN PROGRESS only
	// while a take holds the snapshot lock, never a removal or a restore,
	// which hold it too (no holding file: snapshotLock removes a take's).
	cut := filepath.Join(s.root, "shop", "prod", "snapshots", remote.NextReleaseID(time.Now(), ids)+".tmp")
	write(t, filepath.Join(cut, remote.SnapshotManifest), "format\t1\nkind\tmanual\n", 0o600)
	lock, err := os.OpenFile(filepath.Join(s.root, "shop", "prod", remote.SnapshotLockFile), os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	out, code = s.run(nil, "@prod", "--snapshots")
	lock.Close()
	os.RemoveAll(cut)
	if code != 0 || !strings.Contains(out, "INCOMPLETE") || strings.Contains(out, "IN PROGRESS") {
		t.Errorf("a cut take's snapshot while another holds the snapshot lock (%d):\n%s", code, out)
	}
	s.logs()

	// Off, and on a marked environment: refused, nothing stopped.
	if out, code := s.run(nil, "@nosnap", "--snapshot"); code == 0 || !strings.Contains(out, "manual snapshots are off for nosnap") {
		t.Errorf("manual: false (%d):\n%s", code, out)
	}
	write(t, filepath.Join(s.root, "shop", "prod", ".devopsy-sync.incomplete"), "2026-10-25 00:00:00\tdb\timport\tcut\n", 0o644)
	out, code = s.run(nil, "@prod", "--snapshot")
	docker, _ = s.logs()
	if code == 0 || !strings.Contains(out, "did not complete") || strings.Contains(docker, " stop ") {
		t.Errorf("a marked environment (%d):\n%s\n%s", code, out, docker)
	}
}

// A Ctrl-C during the stopped pass cancels the take: the services start
// again, their snapshot lines go, and no snapshot is left.
func TestSnapshotInterrupt(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	started := filepath.Join(t.TempDir(), "started")
	outFile, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "@prod", "--snapshot")
	cmd.Dir = s.project
	cmd.Env = append(append([]string{}, s.env...), "FAKE_SLOW_STOPPED="+started)
	cmd.Stdin = strings.NewReader("")
	cmd.Stdout, cmd.Stderr = outFile, outFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); !fileExists(started); time.Sleep(50 * time.Millisecond) {
		if time.Since(start) > startWait {
			_ = cmd.Process.Kill()
			t.Fatalf("the stopped pass never started:\n%s", readString(outFile.Name()))
		}
	}
	start := time.Now()
	_ = cmd.Process.Signal(syscall.SIGINT)
	_ = cmd.Wait()
	out := readString(outFile.Name())
	if cmd.ProcessState.ExitCode() != 130 || time.Since(start) > 15*time.Second || !strings.Contains(out, "cancelling the snapshot") {
		t.Fatalf("interrupt (%d, %s):\n%s", cmd.ProcessState.ExitCode(), time.Since(start), out)
	}
	base := filepath.Join(s.root, "shop", "prod")
	for !strings.Contains(readString(s.log), "start db app") || strings.Contains(readString(filepath.Join(base, ".devopsy-sync.services")), "snapshot ") {
		if time.Since(start) > 15*time.Second {
			t.Fatalf("the services not started again, or their snapshot lines left:\n%s\n%s", readString(s.log), readString(filepath.Join(base, ".devopsy-sync.services")))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if left, _ := filepath.Glob(filepath.Join(base, "snapshots", "2*")); len(left) > 0 {
		t.Errorf("snapshots left: %v", left)
	}
}

// A full restore: a new release made from the snapshot's copy of its
// release, its tars checked, the data written back between the images
// and the switch (the services stopped, then started by its run step),
// its record naming the snapshot; the release it leaves stays live, a
// rollback target. --with-env brings shared/.env back; without it a
// difference is said.
func TestSnapshotRestore(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	base := filepath.Join(s.root, "shop", "prod")
	write(t, filepath.Join(base, "shared", ".env"), "DB_PASSWORD=old\n", 0o600)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	id, taken := m[1], m[2]
	write(t, filepath.Join(s.mnt("prod"), "storage", "file.txt"), "changed\n", 0o644)
	write(t, filepath.Join(s.mnt("prod"), "storage", "new.txt"), "new\n", 0o644)
	write(t, filepath.Join(base, "shared", ".env"), "DB_PASSWORD=new\n", 0o600)
	s.logs()

	out, code = s.run(nil, "@prod", "--snapshots", "restore", id[:12], "--yes")
	docker, _ := s.logs()
	r := restoringAs.FindStringSubmatch(out)
	if code != 0 || r == nil || !strings.Contains(out, "deploy in shop-prod") {
		t.Fatalf("restore (%d):\n%s\n%s", code, out, docker)
	}
	if got := readString(filepath.Join(s.mnt("prod"), "storage", "file.txt")); got != "from prod\n" || fileExists(filepath.Join(s.mnt("prod"), "storage", "new.txt")) {
		t.Errorf("storage after the restore: %q, new.txt kept %v", got, fileExists(filepath.Join(s.mnt("prod"), "storage", "new.txt")))
	}
	if s.current("prod") != "releases/"+r[1] || r[1] == taken {
		t.Errorf("current %s, the restore's release %s (the snapshot's %s)", s.current("prod"), r[1], taken)
	}
	if rec := readString(filepath.Join(base, "releases", r[1], "devopsy-release.json")); !strings.Contains(rec, `"restore":"`+id+`"`) {
		t.Errorf("the restore's record: %s", rec)
	}
	if out, code := s.run(nil, "@prod", "--releases"); code != 0 || !strings.Contains(out, ", restore of snapshot "+id) {
		t.Errorf("--releases (%d):\n%s", code, out)
	}
	if fileExists(filepath.Join(base, "releases", taken, "rolled-back")) || fileExists(filepath.Join(base, "releases", taken, "devopsy-rolled-back")) {
		t.Errorf("the release it left marked rolled back")
	}
	order := []string{"-check ", "stop db app", "-extract-", "deploy"}
	rest := docker + out
	for _, want := range order[:3] {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("missing %q in order:\n%s", want, docker)
		}
		rest = rest[i+len(want):]
	}
	if readString(filepath.Join(base, "shared", ".env")) != "DB_PASSWORD=new\n" || !strings.Contains(out, "differs from the environment's in DB_PASSWORD") {
		t.Errorf("shared/.env without --with-env: %q\n%s", readString(filepath.Join(base, "shared", ".env")), out)
	}
	// The way to bring it back, a command that works as copied: devopsy
	// alone, without the address, runs locally.
	if !regexp.MustCompile(`devopsy @\S+ --snapshots restore ` + id + ` --with-env`).MatchString(out) {
		t.Errorf("the --with-env hint names no address:\n%s", out)
	}
	if fileExists(filepath.Join(base, remote.SyncIncompleteFile)) || strings.Contains(readString(filepath.Join(base, ".devopsy-sync.services")), "stop ") {
		t.Errorf("a mark or stop lines left:\n%s", readString(filepath.Join(base, ".devopsy-sync.services")))
	}
	if out, code := s.run(nil, "@prod", "--snapshots", "restore", id, "--with-env", "--yes"); code != 0 || readString(filepath.Join(base, "shared", ".env")) != "DB_PASSWORD=old\n" {
		t.Errorf("--with-env (%d): %q\n%s", code, readString(filepath.Join(base, "shared", ".env")), out)
	}
	if out, code := s.run(nil, "@prod", "--snapshots", "restore", "1999", "--yes"); code == 0 || !strings.Contains(out, "has no snapshot 1999: nothing restored") {
		t.Errorf("a snapshot that is none (%d):\n%s", code, out)
	}
}

// A take killed outright leaves its snapshot lines: the next release's
// settle starts those services (nothing else would), unless the
// environment is marked, which keeps them down.
func TestSnapshotLeftoverLines(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	base := filepath.Join(s.root, "shop", "prod")
	record := filepath.Join(base, ".devopsy-sync.services")
	write(t, record, "snapshot 20261025000000 db\nsnapshot 20261025000000 app\n", 0o644)
	write(t, s.log+".stopped.shop-prod", "db\napp\n", 0o644)
	out, code := s.run(nil, "@prod", "--release")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(out, "starting what snapshot 20261025000000, cut off, left stopped: db app") || !strings.Contains(docker, "start db app") || fileExists(record) {
		t.Errorf("settled (%d), record %q:\n%s\n%s", code, readString(record), out, docker)
	}

	// Marked: the lines stay, the services down.
	write(t, record, "snapshot 20261025000000 db\n", 0o644)
	write(t, filepath.Join(base, remote.SyncIncompleteFile), "2026-10-25 00:00:00\t20261024000000\trestore\tthe restore of snapshot 20261024000000 did not complete\n", 0o644)
	out, code = s.run(nil, "@prod", "--export", "db")
	if !strings.Contains(readString(record), "snapshot 20261025000000 db") || strings.Contains(out, "left stopped") {
		t.Errorf("a marked environment's lines (%d), record %q:\n%s", code, readString(record), out)
	}
}

// A restore's mark refuses what a sync's or an import's does, with its
// own ways out (the restore again, another snapshot), and keeps the
// snapshot it names.
func TestRestoreMark(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	base := filepath.Join(s.root, "shop", "prod")
	write(t, filepath.Join(base, remote.SyncIncompleteFile), "2026-10-25 00:00:00\t"+m[1]+"\trestore\tit failed or was cut after its first write: its data may be partial\n", 0o644)
	s.logs()
	for args, want := range map[string]string{
		"--release":                             "the restore of snapshot " + m[1] + " into @devopsy@one:prod did not complete",
		"--import db --yes":                     "the restore of snapshot " + m[1] + " into @devopsy@one:prod did not complete, and only a restore completes it: nothing imported",
		"--import db --resolved --yes":          "--resolved is for imports",
		"--snapshot":                            "the restore of snapshot " + m[1] + " into @devopsy@one:prod did not complete",
		"--snapshots remove " + m[1] + " --yes": "the restore of snapshot " + m[1] + " did not complete: it stays",
	} {
		o, c := s.run(nil, append([]string{"@prod"}, strings.Fields(args)...)...)
		if c == 0 || !strings.Contains(o, want) {
			t.Errorf("%s (%d):\n%s", args, c, o)
		}
	}
	if !strings.Contains(readString(filepath.Join(base, remote.SyncIncompleteFile)), "restore") {
		t.Errorf("the mark went")
	}
	if out, code := s.run(nil, "@prod", "--status"); code != 0 || strings.Count(out, "the restore of snapshot "+m[1]+" did not complete") != 1 {
		t.Errorf("--status (%d), the mark said once:\n%s", code, out)
	}
}

// --destroy says how many snapshots go, frees their bookmarks with the
// releases', and removes them with everything else, the mirror (root's)
// emptied as root first.
func TestSnapshotDestroy(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	for range 2 {
		if out, code := s.run(nil, "@prod", "--snapshot"); code != 0 {
			t.Fatalf("take (%d):\n%s", code, out)
		}
	}
	s.logs()
	out, code := s.run(nil, "@prod", "--destroy")
	if code == 0 || !strings.Contains(out, "and its 2 snapshots") {
		t.Errorf("--destroy's prompt (%d):\n%s", code, out)
	}
	out, code = s.run(nil, "@prod", "--destroy", "--yes")
	docker, _ := s.logs()
	if code != 0 || fileExists(filepath.Join(s.root, "shop", "prod")) || !strings.Contains(docker, "-mirror ") {
		t.Errorf("--destroy (%d):\n%s\n%s", code, out, docker)
	}
}

// --no-code: the snapshot's data under the current release, which stays
// current, its run step run again; refused, naming each mismatch, when
// the current labels do not fit the snapshot's.
func TestSnapshotRestoreNoCode(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	id := m[1]
	current := s.current("prod")
	// What it asks before: no release is made.
	if out, code := s.run(nil, "@prod", "--snapshots", "restore", id, "--no-code"); code == 0 || !strings.Contains(out, "under its current release") || strings.Contains(out, "as a new release") {
		t.Errorf("--no-code's prompt (%d) says it restores as a new release:\n%s", code, out)
	}
	write(t, filepath.Join(s.mnt("prod"), "storage", "file.txt"), "changed\n", 0o644)
	s.logs()
	out, code = s.run(nil, "@prod", "--snapshots", "restore", id, "--no-code", "--yes")
	docker, _ := s.logs()
	if code != 0 || readString(filepath.Join(s.mnt("prod"), "storage", "file.txt")) != "from prod\n" || s.current("prod") != current || !strings.Contains(out, "deploy in shop-prod") {
		t.Fatalf("--no-code (%d), current %s (was %s):\n%s\n%s", code, s.current("prod"), current, out, docker)
	}
	if i, j := strings.Index(docker, "stop db app"), strings.Index(docker, "-extract-"); i < 0 || j < i {
		t.Errorf("stopped before the extraction:\n%s", docker)
	}

	// db without data now: the snapshot does not fit.
	s.labels("FAKE_DB=none")
	out, code = s.run(nil, "@prod", "--snapshots", "restore", id, "--no-code", "--yes")
	docker, _ = s.logs()
	if code == 0 || !strings.Contains(out, "snapshot "+id+" does not fit the current release's labels: nothing restored") || !strings.Contains(out, "the snapshot has data for db, which the current release has no service with data of") || strings.Contains(docker, " stop ") {
		t.Errorf("a mismatch (%d):\n%s\n%s", code, out, docker)
	}
}

// --snapshots export writes one tar, which --snapshots import stores as
// a snapshot of the environment under a new id, restorable as any; one
// that is no snapshot, or into an environment with no current release,
// is refused.
func TestSnapshotExportImport(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	exported, errs, code := s.runIO(nil, nil, "@prod", "--snapshots", "export", m[1])
	if code != 0 || !strings.Contains(strings.Join(tarNamesOf(t, exported), " "), m[1]+"/data/mnt.db.tar.zst") {
		t.Fatalf("export (%d):\n%s", code, errs)
	}
	write(t, filepath.Join(s.mnt("prod"), "storage", "file.txt"), "changed\n", 0o644)
	_, errs, code = s.runIO(nil, exported, "@prod", "--snapshots", "import")
	im := regexp.MustCompile(`imported snapshot ` + m[1] + ` as ([0-9]{14})`).FindStringSubmatch(errs)
	if code != 0 || im == nil || im[1] == m[1] {
		t.Fatalf("import (%d):\n%s", code, errs)
	}
	if !regexp.MustCompile(`devopsy @\S+ --snapshots restore ` + im[1]).MatchString(errs) {
		t.Errorf("the import's restore line names no address:\n%s", errs)
	}
	if out, code := s.run(nil, "@prod", "--snapshots", "restore", im[1], "--yes"); code != 0 || readString(filepath.Join(s.mnt("prod"), "storage", "file.txt")) != "from prod\n" {
		t.Errorf("a restore of the imported snapshot (%d):\n%s", code, out)
	}
	if _, errs, code := s.runIO(nil, []byte("not a tar"), "@prod", "--snapshots", "import"); code == 0 || !strings.Contains(errs, "nothing imported") {
		t.Errorf("no snapshot on stdin (%d):\n%s", code, errs)
	}
	if _, errs, code := s.runIO(nil, exported, "@solo", "--snapshots", "import"); code == 0 || !strings.Contains(errs, "has no current release: release it first") {
		t.Errorf("into an environment never released (%d):\n%s", code, errs)
	}
}

// tarNamesOf are the member names of the tar data.
func tarNamesOf(t *testing.T, data []byte) []string {
	t.Helper()
	var names []string
	for tr := tar.NewReader(bytes.NewReader(data)); ; {
		h, err := tr.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, strings.TrimSuffix(h.Name, "/"))
	}
}

// A child a restore left marked: a sync clears it only by copying every
// service of the snapshot, all of each (the child's list names them); a
// parent whose copy leaves one out is refused before anything stops.
func TestSyncRestoreMark(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	out, code := s.run(nil, "@dev", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	mark := filepath.Join(s.root, "shop", "dev", remote.SyncIncompleteFile)
	write(t, mark, "2026-10-25 00:00:00\t"+m[1]+"\trestore\tthe restore of snapshot "+m[1]+" did not complete\n", 0o644)
	s.logs()
	// prod copies app as nothing: app's storage stays half restored.
	write(t, s.log+".shop-prod", "FAKE_APP=keep\n", 0o644)
	out, code = s.run(nil, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code == 0 || !strings.Contains(out, "a restore into the child did not complete, and the parent does not copy all of app") || parentChanged(docker) {
		t.Errorf("a sync leaving app out (%d):\n%s\n%s", code, out, docker)
	}
	os.Remove(s.log + ".shop-prod")
	if out, code := s.run(nil, "@dev", "--sync", "--yes"); code != 0 || fileExists(mark) {
		t.Errorf("a sync copying it all (%d), marked %v:\n%s", code, fileExists(mark), out)
	}
}

// A restore cut after its first write keeps what it stopped down, the
// environment marked (a restore's mark, its snapshot named), the
// release not live; the same restore again empties and extracts it all,
// clears the mark, and leaves no stop lines in the record: the cut one's
// name services its run step starts.
func TestSnapshotRestoreCut(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	current := s.current("prod")
	base := filepath.Join(s.root, "shop", "prod")
	s.logs()
	out, code = s.run([]string{"FAIL_TAR=2"}, "@prod", "--snapshots", "restore", m[1], "--yes")
	docker, _ := s.logs()
	mark := readString(filepath.Join(base, remote.SyncIncompleteFile))
	if code == 0 || !strings.Contains(mark, "\t"+m[1]+"\trestore\t") || s.current("prod") != current || strings.Contains(docker[strings.Index(docker, "stop db app"):], " start ") {
		t.Fatalf("a cut restore (%d), mark %q, current %s (was %s):\n%s\n%s", code, mark, s.current("prod"), current, out, docker)
	}
	if !regexp.MustCompile(`restore it again:\n.*\$ devopsy @\S+ --snapshots restore ` + m[1]).MatchString(out) {
		t.Errorf("the cut restore's way out not said:\n%s", out)
	}
	out, code = s.run(nil, "@prod", "--snapshots", "restore", m[1], "--yes")
	if code != 0 || fileExists(filepath.Join(base, remote.SyncIncompleteFile)) || s.current("prod") == current {
		t.Errorf("the restore again (%d), marked %v:\n%s", code, fileExists(filepath.Join(base, remote.SyncIncompleteFile)), out)
	}
	if rec := readString(filepath.Join(base, ".devopsy-sync.services")); strings.Contains(rec, "stop ") {
		t.Errorf("the cut restore's stop lines left after the complete one:\n%s", rec)
	}
}

// A take that would not fit refuses before anything stops, naming the
// sizes.
func TestSnapshotSpace(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	// 2^40 kB a path: more than any test machine has.
	out, code := s.run([]string{"FAKE_DU=1099511627776"}, "@prod", "--snapshot")
	docker, _ := s.logs()
	if code == 0 || !strings.Contains(out, "not enough space for a snapshot at") || !strings.Contains(out, "nothing stopped") || strings.Contains(docker, " stop ") {
		t.Errorf("too big (%d):\n%s\n%s", code, out, docker)
	}
}

// Pruning after a take never removes the snapshot a restore's mark names:
// a restore that did not complete is run again from it (a take's tar
// runs under the snapshot lock alone, after the release lock went, so a
// restore may be writing meanwhile).
func TestPruneKeepsRestoreMarkedSnapshot(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	p := snapshotPaths{base: base, snaps: filepath.Join(base, "snapshots"), mirror: filepath.Join(base, "snapshots", remote.SnapshotMirror)}
	ids := []string{"20261025000001", "20261025000002", "20261025000003", "20261025000004"}
	for _, id := range ids {
		write(t, filepath.Join(p.snaps, id, remote.SnapshotManifest), "format\t1\nkind\tmanual\n", 0o600)
	}
	write(t, filepath.Join(base, remote.SyncIncompleteFile), remote.IncompleteLine("2026-10-25 00:00:00", ids[0], remote.IncompleteRestore, "the restore of snapshot "+ids[0]+" did not complete"), 0o644)
	pruneSnapshots(filepath.Join(base, "current"), p)
	if !fileExists(filepath.Join(p.snaps, ids[0])) {
		t.Errorf("the snapshot the restore's mark names (%s) was pruned; left: %v", ids[0], manualSnapshots(p))
	}
}

// A sync's holder killed before it decided leaves its files for the next
// lock-taker to settle (its writers ended, the environment marked, which
// every stop under the release lock does first). A take then refuses,
// nothing stopped: the data may be half a copy, and a snapshot of it
// would restore as complete. A restore settles them too and clears what
// it settled once its data is in, so the next release goes through
// instead of finding the files and marking a complete restore's data as
// a sync's that did not complete.
func TestSnapshotSettlesLeftoverSync(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	base := filepath.Join(s.root, "shop", "prod")
	taken := filepath.Join(base, remote.SyncTakenFile)
	write(t, taken, "devopsy@one:prod 1.2\n", 0o644)
	s.logs()
	out, code = s.run(nil, "@prod", "--snapshot")
	docker, _ := s.logs()
	if code == 0 || !strings.Contains(out, "did not complete") || strings.Contains(docker, " stop ") {
		t.Errorf("a take over a killed holder's files (%d):\n%s\n%s", code, out, docker)
	}
	write(t, taken, "devopsy@one:prod 1.2\n", 0o644)
	if out, code := s.run(nil, "@prod", "--snapshots", "restore", m[1], "--yes"); code != 0 || fileExists(taken) || fileExists(filepath.Join(base, remote.SyncIncompleteFile)) {
		t.Errorf("a restore over them (%d), its files left %v, marked %v:\n%s", code, fileExists(taken), fileExists(filepath.Join(base, remote.SyncIncompleteFile)), out)
	}
	if out, code := s.run(nil, "@prod", "--release"); code != 0 || strings.Contains(out, "did not complete") {
		t.Errorf("the release after a complete restore (%d):\n%s", code, out)
	}
}

// A --snapshots import killed outright (SIGKILL, a reboot) leaves its
// .import directory under the snapshots, gigabytes nothing lists: the
// next holder of the snapshot lock removes it, as it does a take's .tmp.
func TestSnapshotImportLeftover(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	left := filepath.Join(s.root, "shop", "prod", "snapshots", ".import.4242")
	write(t, filepath.Join(left, "20261025000000", remote.SnapshotManifest), "format\t1\nkind\tmanual\n", 0o600)
	if out, code := s.run(nil, "@prod", "--snapshot"); code != 0 {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	if fileExists(left) {
		t.Errorf("the killed import's directory left behind: %s", left)
	}
}

// A take killed outright after its stopped pass, before it bookmarked
// the release's images: the next take finishes its .tmp from the mirror
// when the manifest is there. What it finishes must restore (a snapshot
// without the release's images listing cannot), else it is no snapshot.
func TestSnapshotLeftoverWithoutListing(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	snaps := filepath.Join(s.root, "shop", "prod", "snapshots")
	left := remote.NextReleaseID(time.Now(), []string{m[1]})
	if out, err := exec.Command("cp", "-a", filepath.Join(snaps, m[1]), filepath.Join(snaps, left+".tmp")).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	os.Remove(filepath.Join(snaps, left+".tmp", images.ListFile))
	write(t, filepath.Join(snaps, remote.SnapshotMirror, remote.SnapshotComplete), left+"\n", 0o600)
	s.logs()
	if out, code := s.run(nil, "@prod", "--snapshot"); code != 0 {
		t.Fatalf("the take after it (%d):\n%s", code, out)
	}
	if !fileExists(filepath.Join(snaps, left)) {
		return
	}
	if out, code := s.run(nil, "@prod", "--snapshots", "restore", left, "--yes"); code != 0 {
		t.Errorf("the finished snapshot %s is listed but does not restore (%d):\n%s", left, code, out)
	}
}

// Without --with-env a restore names the keys of shared/.env that
// differ, never a value: a value over several lines (a key in PEM, its
// last base64 line padded with =) is one key, read as compose reads the
// file, and no line of it is printed.
func TestSnapshotRestoreEnvKeysOnly(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	env := filepath.Join(s.root, "shop", "prod", "shared", ".env")
	pem := func(secret string) string {
		return "TLS_KEY=\"-----BEGIN PRIVATE KEY-----\nMIIBVQIBADANBgkqhkiG9w0BAQEFAASCAT8wggE7AgEAAkEA" + secret + "==\n-----END PRIVATE KEY-----\"\n"
	}
	write(t, env, pem("oldSecretPart"), 0o600)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	write(t, env, pem("newSecretPart"), 0o600)
	s.logs()
	out, code = s.run(nil, "@prod", "--snapshots", "restore", m[1], "--yes")
	if code != 0 {
		t.Fatalf("restore (%d):\n%s", code, out)
	}
	if strings.Contains(out, "SecretPart") || !strings.Contains(out, "differs from the environment's in TLS_KEY:") {
		t.Errorf("the keys that differ, as said:\n%s", out)
	}
}

// A --no-code restore gives what it extracted into an owned entry to its
// owner by the current labels, as an import does: the tars carry the
// owners the files had when the snapshot was taken (another owner since,
// another server's), and the app runs as the label's.
func TestSnapshotRestoreNoCodeOwners(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	s.logs()
	// db owns its entry now (its UID changed since the snapshot).
	out, code = s.run([]string{"FAKE_DB_OWNER=10002:10002"}, "@prod", "--snapshots", "restore", m[1], "--no-code", "--yes")
	docker, _ := s.logs()
	owners := regexp.MustCompile(`run --rm --name devopsy-\S*owners\S* .*source=` + regexp.QuoteMeta(filepath.Join(s.mnt("prod"), "db")) + `,`)
	extract := strings.LastIndex(docker, "-extract-")
	if code != 0 || extract < 0 || !owners.MatchString(docker[extract:]) {
		t.Errorf("--no-code (%d): mnt/db not given to 10002:10002 after its extraction:\n%s\n%s", code, out, docker)
	}
}

// An imported snapshot is untrusted: its manifest's volume lines name the
// Docker volume a restore empties and extracts into as root, so one that
// is not this environment's own (another project's database) is refused,
// at the import or before anything stops, and that volume stays as it is.
func TestSnapshotImportForeignVolume(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	exported, errs, code := s.runIO(nil, nil, "@prod", "--snapshots", "export", m[1])
	if code != 0 {
		t.Fatalf("export (%d):\n%s", code, errs)
	}
	// The export, its manifest naming another project's volume for a key
	// no service mounts, with a tar of that key.
	var inner bytes.Buffer
	iw := tar.NewWriter(&inner)
	_ = iw.WriteHeader(&tar.Header{Name: "evil/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = iw.WriteHeader(&tar.Header{Name: "evil/x", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
	_, _ = iw.Write([]byte("x\n"))
	_ = iw.Close()
	var crafted bytes.Buffer
	tw := tar.NewWriter(&crafted)
	for tr := tar.NewReader(bytes.NewReader(exported)); ; {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		if h.Name == m[1]+"/manifest" {
			data = append(data, []byte("volume\tevil\tshop-other_db\n")...)
			h.Size = int64(len(data))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(data)
	}
	_ = tw.WriteHeader(&tar.Header{Name: m[1] + "/data/volume.evil.tar.zst", Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(inner.Len())})
	_, _ = tw.Write(inner.Bytes())
	_ = tw.Close()
	precious := filepath.Join(s.log+".vol", "shop-other_db", "precious")
	write(t, precious, "another project's rows\n", 0o644)

	_, errs, code = s.runIO(nil, crafted.Bytes(), "@prod", "--snapshots", "import")
	im := regexp.MustCompile(`imported snapshot ` + m[1] + ` as ([0-9]{14})`).FindStringSubmatch(errs)
	if code != 0 || im == nil {
		return // refused at the import
	}
	s.logs()
	out, code = s.run(nil, "@prod", "--snapshots", "restore", im[1], "--yes")
	docker, _ := s.logs()
	if !fileExists(precious) {
		t.Errorf("the restore (%d) emptied shop-other_db, another project's volume:\n%s\n%s", code, out, docker)
	}
}

// On a marked environment a --no-code restore clears the mark only by
// writing all of every service with data: one of its entries the
// snapshot does not hold (app's mnt/uploads, mounted since) keeps what
// the cut import half wrote, so the restore is refused, or the mark kept.
func TestSnapshotRestoreNoCodeMarkEntries(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	base := filepath.Join(s.root, "shop", "prod")
	write(t, filepath.Join(s.mnt("prod"), "uploads", "half.jpg"), "half\n", 0o644)
	mark := filepath.Join(base, remote.SyncIncompleteFile)
	write(t, mark, "2026-10-25 00:00:00\tapp\timport\tthe import of app did not complete\n", 0o644)
	write(t, s.log+".shop-prod", "FAKE_APP_MOUNT2=/uploads\n", 0o644)
	s.logs()
	out, code = s.run(nil, "@prod", "--snapshots", "restore", m[1], "--no-code", "--yes")
	if code == 0 && !fileExists(mark) {
		t.Errorf("the mark cleared (%d), app's mnt/uploads as the cut import left it:\n%s", code, out)
	}
}

// An entry that is another kind now than the snapshot holds it (a
// directory where its tar has a file) cannot be extracted over: the
// directory is the container's mount point. A restore refuses it before
// anything stops, as an import does, rather than after its mark, where
// every restore again fails the same way with the services down.
func TestSnapshotRestoreEntryKind(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	write(t, s.log+".shop-prod", "FAKE_APP_MOUNT=/app.ini\n", 0o644)
	write(t, filepath.Join(s.mnt("prod"), "app.ini"), "[prod]\n", 0o644)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	if err := os.Remove(filepath.Join(s.mnt("prod"), "app.ini")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(s.mnt("prod"), "app.ini", "conf.d"), "[now]\n", 0o644)
	s.logs()
	out, code = s.run(nil, "@prod", "--snapshots", "restore", m[1], "--yes")
	docker, _ := s.logs()
	if code == 0 || strings.Contains(docker, " stop ") || fileExists(filepath.Join(s.root, "shop", "prod", remote.SyncIncompleteFile)) {
		t.Errorf("mnt/app.ini a directory, a file in the snapshot: not refused before stopping (%d):\n%s\n%s", code, out, docker)
	}
}

// A manifest naming an entry or a volume key that is no single name (an
// imported one is anyone's) is refused, so nothing becomes a path out of
// mnt.
func TestSnapshotManifestNames(t *testing.T) {
	t.Parallel()
	for _, line := range []string{"entry\t../etc\tdir", "entry\t.\tdir", "entry\ta/b\tdir", "volume\t..\tx_y", "volume\tdb x\tx_db"} {
		dir := t.TempDir()
		write(t, filepath.Join(dir, remote.SnapshotManifest), "format\t1\nkind\tmanual\n"+line+"\n", 0o600)
		if _, err := readManifest(dir); err == nil {
			t.Errorf("%q passed", line)
		}
	}
	dir := t.TempDir()
	write(t, filepath.Join(dir, remote.SnapshotManifest), "format\t1\nentry\tstorage\tdir\nvolume\tdb\tx_db\n", 0o600)
	if _, err := readManifest(dir); err != nil {
		t.Error(err)
	}
}

// A restore completes only once its run step started what it stopped:
// one whose run step fails, full or --no-code, stays marked (a shell
// killed between its data and its run step leaves the same), and the
// same restore again completes it, its mark and record gone.
func TestSnapshotRestoreRunFails(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	base := filepath.Join(s.root, "shop", "prod")
	for _, args := range [][]string{{"--no-code"}, {}} {
		out, code = s.run([]string{"FAKE_FAIL_DEPLOY=1"}, append([]string{"@prod", "--snapshots", "restore", m[1], "--yes"}, args...)...)
		mark := readString(filepath.Join(base, remote.SyncIncompleteFile))
		// The fake's deploy starts nothing, so only the first restore finds
		// services running to stop and record.
		if code == 0 || !strings.Contains(mark, "\t"+remote.IncompleteRestore+"\t") || len(args) > 0 && !fileExists(filepath.Join(base, syncState)) {
			t.Errorf("%v: a failed run step (%d) left mark %q, record %v:\n%s", args, code, mark, fileExists(filepath.Join(base, syncState)), out)
		}
		out, code = s.run(nil, append([]string{"@prod", "--snapshots", "restore", m[1], "--yes"}, args...)...)
		if code != 0 || fileExists(filepath.Join(base, remote.SyncIncompleteFile)) || fileExists(filepath.Join(base, syncState)) {
			t.Errorf("%v: the restore again (%d) left the mark or the record:\n%s", args, code, out)
		}
	}
}

// On a marked environment a full restore clears the mark only when the
// snapshot holds all of every service with data of the restored code, as
// --no-code refuses otherwise: an entry mounted at the take but not yet
// written (app's mnt/uploads) is not in the snapshot, so a full restore
// leaves what the cut import half wrote there, and must not run it under
// a cleared mark: refused, the mark kept, or the entry emptied.
func TestSnapshotRestoreMarkMissingEntry(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	write(t, s.log+".shop-prod", "FAKE_APP_MOUNT2=/uploads\n", 0o644)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	base := filepath.Join(s.root, "shop", "prod")
	if manifest := readString(filepath.Join(base, "snapshots", m[1], "manifest")); strings.Contains(manifest, "uploads") {
		t.Fatalf("the snapshot holds uploads, which did not exist:\n%s", manifest)
	}
	half := filepath.Join(s.mnt("prod"), "uploads", "half.jpg")
	write(t, half, "half\n", 0o644)
	mark := filepath.Join(base, remote.SyncIncompleteFile)
	write(t, mark, "2026-10-25 00:00:00\tapp\timport\tthe import of app did not complete\n", 0o644)
	s.logs()
	out, code = s.run(nil, "@prod", "--snapshots", "restore", m[1], "--yes")
	if code == 0 && !fileExists(mark) && fileExists(half) {
		t.Errorf("the mark cleared (%d), app's mnt/uploads as the cut import left it:\n%s", code, out)
	}
}

// A full restore stops every service whose data it writes over: the
// restored code's services with data and the current release's. One that
// still mounts an entry the snapshot restores but carries no data label
// in the current release (db's went since the take) must stop too: else
// it runs on while its entry is emptied and extracted under it.
func TestSnapshotRestoreStopsCurrentWriter(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	// The current release's db has no data label now, and mounts mnt/db
	// as before; the snapshot's code has it with data.
	write(t, s.log+".current.shop-prod", "FAKE_DB=none\n", 0o644)
	s.logs()
	out, code = s.run(nil, "@prod", "--snapshots", "restore", m[1], "--yes")
	docker, _ := s.logs()
	extract := strings.Index(docker, "-extract-")
	stopped := regexp.MustCompile(`compose .* stop ([^\n]* )?db(\n| )`)
	if code == 0 && (extract < 0 || !stopped.MatchString(docker[:extract])) {
		t.Errorf("mnt/db emptied and extracted (%d) with db running:\n%s\n%s", code, out, docker)
	}
}

// A snapshot's tar whose members the archive names ./storage/x (one made
// by hand, imported): the check reads them as storage's, and the restore
// extracts storage by that name alone, which GNU tar does not match
// against ./storage/x. The entry is emptied first, so the restore fails
// after its first write, the data gone from the entry, and every restore
// again fails the same way. Refused before anything stops, or extracted
// by the names the archive gives: never emptied for nothing.
func TestSnapshotImportDotForms(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	exported, errs, code := s.runIO(nil, nil, "@prod", "--snapshots", "export", m[1])
	if code != 0 {
		t.Fatalf("export (%d):\n%s", code, errs)
	}
	// storage's tar, its members named ./storage/...: the same files.
	var inner bytes.Buffer
	iw := tar.NewWriter(&inner)
	_ = iw.WriteHeader(&tar.Header{Name: "./storage/", Typeflag: tar.TypeDir, Mode: 0o755})
	_ = iw.WriteHeader(&tar.Header{Name: "./storage/file.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 10})
	_, _ = iw.Write([]byte("from prod\n"))
	_ = iw.Close()
	var crafted bytes.Buffer
	tw := tar.NewWriter(&crafted)
	for tr := tar.NewReader(bytes.NewReader(exported)); ; {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		if h.Name == m[1]+"/data/mnt.storage.tar.zst" {
			data = inner.Bytes()
			h.Size = int64(len(data))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(data)
	}
	_ = tw.Close()
	_, errs, code = s.runIO(nil, crafted.Bytes(), "@prod", "--snapshots", "import")
	im := regexp.MustCompile(`imported snapshot ` + m[1] + ` as ([0-9]{14})`).FindStringSubmatch(errs)
	if code != 0 || im == nil {
		return // refused at the import
	}
	file := filepath.Join(s.mnt("prod"), "storage", "file.txt")
	write(t, file, "changed\n", 0o644)
	s.logs()
	out, code = s.run(nil, "@prod", "--snapshots", "restore", im[1], "--yes")
	docker, _ := s.logs()
	marked := fileExists(filepath.Join(s.root, "shop", "prod", remote.SyncIncompleteFile))
	switch {
	case code == 0 && readString(file) == "from prod\n":
	case code != 0 && !strings.Contains(docker, " stop ") && !marked:
	default:
		t.Errorf("restore (%d), storage/file.txt %q, marked %v:\n%s\n%s", code, readString(file), marked, out, docker)
	}
}

// A restore's release takes only its target lines and record from the
// snapshot: a mark the snapshot's release had, or one an imported
// snapshot carries, is never the new release's.
func TestSnapshotRestoreReleaseFiles(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	snap := filepath.Join(s.root, "shop", "prod", "snapshots", m[1])
	for _, f := range []string{remote.FailedFile, images.RolledBackFile, images.ForcedFile} {
		write(t, filepath.Join(snap, f), "", 0o600)
	}
	if out, code = s.run(nil, "@prod", "--snapshots", "restore", m[1], "--yes"); code != 0 {
		t.Fatalf("restore (%d):\n%s", code, out)
	}
	cur, _ := filepath.EvalSymlinks(filepath.Join(s.root, "shop", "prod", "current"))
	for _, f := range []string{remote.FailedFile, images.RolledBackFile, images.ForcedFile} {
		if fileExists(filepath.Join(filepath.Dir(cur), f)) {
			t.Errorf("the restored release has the snapshot's %s", f)
		}
	}
	if !fileExists(filepath.Join(filepath.Dir(cur), remote.TargetEnvFile)) || !fileExists(filepath.Join(filepath.Dir(cur), remote.RecordFile)) {
		t.Errorf("the restored release lacks its target lines or record")
	}
}

// A full restore passes its point of no return at the end of its fetch
// (the watcher disarmed), and writes the data after it, before its
// switch: a Ctrl-C while it stops the services and extracts (minutes for
// a large snapshot) cannot cancel it, and the operator must hear that,
// not that it is being cancelled (a second Ctrl-C then exits 130 while
// the restore goes live, and a connection lost there is said to be one
// the server cancels).
func TestSnapshotRestoreInterruptDuringData(t *testing.T) {
	t.Parallel()
	// The switch is known from the server's log marker, which needs GNU
	// tail, as on every server (macOS here has none).
	if exec.Command("/bin/sh", "-c", "PATH=/usr/bin:/bin tail --version").Run() != nil {
		t.Skip("no GNU tail: followOutput prints no marker")
	}
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	before := s.current("prod")
	s.logs()
	stopping := filepath.Join(t.TempDir(), "stopping")
	outFile, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "@prod", "--snapshots", "restore", m[1], "--yes")
	cmd.Dir = s.project
	cmd.Env = append(append([]string{}, s.env...), "FAKE_STOP_WAIT="+stopping)
	cmd.Stdin = strings.NewReader("")
	cmd.Stdout, cmd.Stderr = outFile, outFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); !fileExists(stopping); time.Sleep(50 * time.Millisecond) {
		if time.Since(start) > startWait {
			_ = cmd.Process.Kill()
			t.Fatalf("the restore's data step never stopped the services:\n%s", readString(outFile.Name()))
		}
	}
	_ = cmd.Process.Signal(syscall.SIGINT)
	for start := time.Now(); !strings.Contains(readString(outFile.Name()), "interrupted"); time.Sleep(50 * time.Millisecond) {
		if time.Since(start) > startWait {
			break
		}
	}
	said := readString(outFile.Name())
	write(t, stopping+".go", "", 0o644)
	_ = cmd.Wait()
	out = readString(outFile.Name())
	if s.current("prod") == before {
		t.Fatalf("the restore did not go live (%d):\n%s", cmd.ProcessState.ExitCode(), out)
	}
	if strings.Contains(said, "cancelling the release") {
		t.Errorf("a Ctrl-C past the restore's point of no return said it was cancelling it (it went live, exit %d):\n%s", cmd.ProcessState.ExitCode(), out)
	}
}

// A full restore whose data went in and whose run step then fails (it
// started the services, then a migration failed) switches back to the
// release it left, and skips that release's restart for the restore's
// mark, which says nothing runs on the data until a restore completes.
// What the failed run step started must not be left running on it then,
// under the restored release's config while current is the previous
// release: either the previous release comes back, as after any failed
// run step, or nothing runs, as the mark says. A --no-code restore whose
// run step fails stays marked too, and what that step started must not
// run on the marked data either.
func TestSnapshotRestoreRunFailsLeavesNothingHalf(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{}, {"--no-code"}} {
		s := newSyncSetup(t)
		write(t, filepath.Join(s.dot, "commands", "deploy"), "#!/bin/sh\necho \"deploy in $COMPOSE_PROJECT_NAME\"\n[ -z \"${FAKE_UP_THEN_FAIL:-}\" ] || { devopsy up -d --wait app db; exit 3; }\n", 0o755)
		s.releaseProd(nil)
		out, code := s.run(nil, "@prod", "--snapshot")
		m := snapshotID.FindStringSubmatch(out)
		if code != 0 || m == nil {
			t.Fatalf("take (%d):\n%s", code, out)
		}
		before := s.current("prod")
		s.logs()
		out, code = s.run([]string{"FAKE_UP_THEN_FAIL=1"}, append([]string{"@prod", "--snapshots", "restore", m[1], "--yes"}, args...)...)
		docker, _ := s.logs()
		mark := readString(filepath.Join(s.root, "shop", "prod", remote.SyncIncompleteFile))
		if code == 0 || s.current("prod") != before || !strings.Contains(mark, "\t"+remote.IncompleteRestore+"\t") {
			t.Fatalf("%v: a restore whose run step fails (%d), current %s (was %s), mark %q:\n%s", args, code, s.current("prod"), before, mark, out)
		}
		// Stopped or taken down (the fake's down records what it removed).
		stopped := append(strings.Fields(readString(s.log+".stopped.shop-prod")), strings.Fields(readString(s.log+".down.shop-prod"))...)
		restarted := strings.Contains(docker, "--release-steps restart run") || strings.Contains(out, "restarting "+strings.TrimPrefix(before, "releases/")+" with its own run step")
		if !restarted && (!slices.Contains(stopped, "db") || !slices.Contains(stopped, "app")) {
			t.Errorf("%v: the environment marked, and the failed run step's services left running on the restored data (stopped: %v), current %s:\n%s", args, stopped, s.current("prod"), out)
		}
	}
}

// --snapshots import reads a plain tar from stdin, and tar takes a stream
// cut at a member boundary for complete (no end-of-archive blocks): an
// operator's Ctrl-C, a dropped connection, or the pipe feeding it dying
// (aws s3 cp ... - |) at a 512-byte boundary between members. Such a
// stream must not be stored as a snapshot: here the cut drops the
// snapshot's images record (data/images), which a restore holds the data
// services on, and the import says it imported the snapshot.
func TestSnapshotImportCut(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	exported, errs, code := s.runIO(nil, nil, "@prod", "--snapshots", "export", m[1])
	if code != 0 {
		t.Fatalf("export (%d):\n%s", code, errs)
	}
	// The export as the server receives it when the stream ends right
	// before its last member: every member but that one, no end blocks.
	last := m[1] + "/data/images"
	var cut bytes.Buffer
	tw := tar.NewWriter(&cut)
	found := false
	for tr := tar.NewReader(bytes.NewReader(exported)); ; {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == last {
			found = true
			continue
		}
		data, _ := io.ReadAll(tr)
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(data)
	}
	_ = tw.Flush()
	if !found {
		t.Fatalf("the export has no %s: %v", last, tarNamesOf(t, exported))
	}
	_, errs, code = s.runIO(nil, cut.Bytes(), "@prod", "--snapshots", "import")
	if im := regexp.MustCompile(`imported snapshot ` + m[1] + ` as ([0-9]{14})`).FindStringSubmatch(errs); code == 0 || im != nil {
		t.Errorf("a stream cut before its last member stored as a snapshot (%d), without %s:\n%s", code, last, errs)
	}
}

// A snapshot's id is a UTC second picked per environment, so two
// environments on one server (prod and staging, taken by the same cron)
// get the same id when their stopped passes start in the same second.
// Every container a take or a restore names must then carry the
// environment's compose name, as the mirror's do: a tar container named
// by the id alone collides with the other take's, whose docker run fails
// on the name in use after its stopped pass.
func TestSnapshotContainerNames(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@prod", "--snapshots", "restore", m[1], "--yes"); code != 0 {
		t.Fatalf("restore (%d):\n%s", code, out)
	}
	docker, _ := s.logs()
	for _, name := range regexp.MustCompile(`run --rm --name (devopsy-snapshot-\S+)`).FindAllStringSubmatch(docker, -1) {
		if !strings.Contains(name[1], "shop-prod") {
			t.Errorf("a container named without the environment's compose name: %s", name[1])
		}
	}
}

// A release whose fetch was killed outright (its marker left) is recovered
// before a --no-code restore or a sync's holder runs anything of current's,
// as before every activation: marked failed, current's own names back.
func TestRecoverFetchBeforeData(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	cut := func(env string) string {
		cur, _ := filepath.EvalSymlinks(filepath.Join(s.root, "shop", env, "current"))
		rel := filepath.Join(filepath.Dir(filepath.Dir(cur)), "29991231235959")
		if out, err := exec.Command("cp", "-a", filepath.Dir(cur), rel).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		os.Remove(filepath.Join(rel, remote.FailedFile))
		write(t, filepath.Join(rel, images.FetchingFile), "", 0o644)
		return rel
	}
	rel := cut("prod")
	if out, code = s.run(nil, "@prod", "--snapshots", "restore", m[1], "--no-code", "--yes"); code != 0 {
		t.Fatalf("--no-code (%d):\n%s", code, out)
	}
	if fileExists(filepath.Join(rel, images.FetchingFile)) || !fileExists(filepath.Join(rel, remote.FailedFile)) {
		t.Errorf("--no-code ran over a cut fetch, not recovered:\n%s", out)
	}
	if out, code = s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("dev's release (%d):\n%s", code, out)
	}
	rel = cut("dev")
	if out, code = s.run(nil, "@dev", "--sync", "--yes"); code != 0 {
		t.Fatalf("sync (%d):\n%s", code, out)
	}
	if fileExists(filepath.Join(rel, images.FetchingFile)) || !fileExists(filepath.Join(rel, remote.FailedFile)) {
		t.Errorf("a sync's holder ran over a cut fetch, not recovered:\n%s", out)
	}
}

// A take's first pass holds no release lock, so a release may switch
// current meanwhile; its stopped pass, once it holds the lock, takes the
// release current is then (its code, its listing, the manifest's
// release), and must read that release's labels too: the snapshot's
// data is what the code it holds mounts, and its stop set what runs.
// The script went into current once, before the first pass, and the
// later steps run in the release it pointed to then.
func TestSnapshotReleaseDuringFirstPass(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	base := filepath.Join(s.root, "shop", "prod")
	old := s.current("prod")
	write(t, filepath.Join(s.mnt("prod"), "files", "upload.txt"), "new layout\n", 0o644)
	// The new release: app mounts mnt/files now, not mnt/storage (its
	// labels, read through current).
	write(t, s.log+".mirrorhook", `set -e
cp -a "`+filepath.Join(base, old)+`" "`+filepath.Join(base, "releases", "29990101000000")+`"
rm "`+filepath.Join(base, "current")+`"
ln -s releases/29990101000000/code "`+filepath.Join(base, "current")+`"
echo FAKE_APP_MOUNT=/files > "`+s.log+`.current.shop-prod"
`, 0o644)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil || s.current("prod") != "releases/29990101000000" {
		t.Fatalf("take (%d), current %s:\n%s", code, s.current("prod"), out)
	}
	manifest := readString(filepath.Join(base, "snapshots", m[1], "manifest"))
	if !strings.Contains(manifest, "release\t29990101000000\n") || !strings.Contains(manifest, "entry\tfiles\t") || strings.Contains(manifest, "entry\tstorage\t") {
		t.Errorf("the snapshot holds release 29990101000000's code, and the data of %s's labels:\n%s", old, manifest)
	}
}

// A take killed outright on the parent leaves its snapshot lines, its
// services down, for the next settle to start them. A sync from that
// parent stops and restores the parent's set by its record, and must not
// take those lines with it: the parent's services would stay down with
// nothing left to start them.
func TestSyncPushKeepsParentSnapshotLines(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	base := filepath.Join(s.root, "shop", "prod")
	record := filepath.Join(base, ".devopsy-sync.services")
	write(t, record, "snapshot 20261025000000 db\nsnapshot 20261025000000 app\n", 0o644)
	write(t, s.log+".stopped.shop-prod", "db\napp\n", 0o644)
	out, code := s.run(nil, "@dev", "--release")
	docker, _ := s.logs()
	if code != 0 {
		t.Fatalf("dev's first release (%d):\n%s\n%s", code, out, docker)
	}
	started := false
	for _, line := range strings.Split(docker, "\n") {
		if strings.HasPrefix(line, "shop-prod ") && strings.HasSuffix(line, " start db app") {
			started = true
		}
	}
	if !started && !strings.Contains(readString(record), "snapshot 20261025000000 db") {
		t.Errorf("the parent's snapshot lines went with the push's record, db and app left down with nothing to start them:\n%s\n%s", out, docker)
	}
}
