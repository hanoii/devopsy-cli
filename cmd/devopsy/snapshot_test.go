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
	"strconv"
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
	s.mirrored("prod")
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
	// With a mirror, during its stopped rsync; without one, during the
	// tars it writes while stopped.
	for _, slow := range []string{"FAKE_SLOW_STOPPED", "FAKE_SLOW_TAR"} {
		s := newSyncSetup(t)
		if slow == "FAKE_SLOW_STOPPED" {
			s.mirrored("prod")
		}
		s.releaseProd(nil)
		started := filepath.Join(t.TempDir(), "started")
		outFile, err := os.CreateTemp(t.TempDir(), "out")
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "@prod", "--snapshot")
		cmd.Dir = s.project
		cmd.Env = append(append([]string{}, s.env...), slow+"="+started)
		cmd.Stdin = strings.NewReader("")
		cmd.Stdout, cmd.Stderr = outFile, outFile
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		for start := time.Now(); !fileExists(started); time.Sleep(50 * time.Millisecond) {
			if time.Since(start) > startWait {
				_ = cmd.Process.Kill()
				t.Fatalf("%s: the stopped pass never started:\n%s", slow, readString(outFile.Name()))
			}
		}
		start := time.Now()
		_ = cmd.Process.Signal(syscall.SIGINT)
		_ = cmd.Wait()
		out := readString(outFile.Name())
		if cmd.ProcessState.ExitCode() != 130 || time.Since(start) > 15*time.Second || !strings.Contains(out, "cancelling the snapshot") {
			t.Fatalf("%s: interrupt (%d, %s):\n%s", slow, cmd.ProcessState.ExitCode(), time.Since(start), out)
		}
		base := filepath.Join(s.root, "shop", "prod")
		for !strings.Contains(readString(s.log), "start db app") || strings.Contains(readString(filepath.Join(base, ".devopsy-sync.services")), "snapshot ") {
			if time.Since(start) > 15*time.Second {
				t.Fatalf("%s: the services not started again, or their snapshot lines left:\n%s\n%s", slow, readString(s.log), readString(filepath.Join(base, ".devopsy-sync.services")))
			}
			time.Sleep(100 * time.Millisecond)
		}
		if left, _ := filepath.Glob(filepath.Join(base, "snapshots", "2*")); len(left) > 0 {
			t.Errorf("%s: snapshots left: %v", slow, left)
		}
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
	s.mirrored("prod")
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
	// Its count's session is bounded: a dead host never holds the prompt.
	if _, ssh := s.logs(); !strings.Contains(ssh, "ConnectTimeout=") {
		t.Errorf("--destroy's count, unbounded:\n%s", ssh)
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
	// By its start, as restore takes it.
	exported, errs, code := s.runIO(nil, nil, "@prod", "--snapshots", "export", m[1][:12])
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

// A release snapshot imported elsewhere (another environment, another
// server: a move) belongs to a release that is not there: it stays a
// snapshot to restore, never pruned at the next take as one whose release
// went.
func TestSnapshotImportReleaseKind(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.snapshotted("prod", "true", false)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@prod", "--release"); code != 0 {
		t.Fatalf("second release (%d):\n%s", code, out)
	}
	snaps := snapshotPaths{snaps: filepath.Join(s.root, "shop", "prod", "snapshots")}
	var rs []releaseSnapshot
	for start := time.Now(); time.Since(start) < startWait; time.Sleep(100 * time.Millisecond) {
		if rs = releaseSnapshots(snaps); len(rs) == 1 {
			break
		}
	}
	if len(rs) != 1 {
		t.Fatalf("release snapshots %+v, want one", rs)
	}
	exported, errs, code := s.runIO(nil, nil, "@prod", "--snapshots", "export", rs[0].id)
	if code != 0 {
		t.Fatalf("export (%d):\n%s", code, errs)
	}
	if out, code := s.run(nil, "@solo", "--release"); code != 0 {
		t.Fatalf("solo's release (%d):\n%s", code, out)
	}
	_, errs, code = s.runIO(nil, exported, "@solo", "--snapshots", "import")
	im := regexp.MustCompile(`imported snapshot ` + rs[0].id + ` as ([0-9]{14})`).FindStringSubmatch(errs)
	if code != 0 || im == nil {
		t.Fatalf("import (%d):\n%s", code, errs)
	}
	out, code := s.run(nil, "@solo", "--snapshot")
	if code != 0 {
		t.Fatalf("solo's take (%d):\n%s", code, out)
	}
	if !fileExists(filepath.Join(s.root, "shop", "solo", "snapshots", im[1], remote.SnapshotManifest)) {
		t.Errorf("the imported snapshot %s went at the next take:\n%s", im[1], out)
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
	pruneSnapshots(filepath.Join(base, "current"), p, 0)
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
	s.mirrored("prod")
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

// mirrored gives env snapshots: {mirror: true} in the setup's config.
func (s *syncSetup) mirrored(env string) {
	s.t.Helper()
	conf := filepath.Join(s.dot, "config.yaml")
	data := readString(conf)
	if !strings.Contains(data, "\n  "+env+": {}\n") {
		s.t.Fatalf("no %s: {} in the setup's config:\n%s", env, data)
	}
	write(s.t, conf, strings.Replace(data, "\n  "+env+": {}\n", "\n  "+env+": {snapshots: {mirror: true}}\n", 1), 0o644)
}

// Without a mirror (the default): no first pass, the tars written from
// the stopped data, nothing kept beside them; the snapshot restores as
// one taken with a mirror does. A take of it cut before its rename is
// finished by the next; an environment that stops keeping a mirror has
// its old one removed; the server's snapshots: {mirror: false} wins over
// an environment's mirror: true, said; --snapshots says which.
func TestSnapshotNoMirror(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	base := filepath.Join(s.root, "shop", "prod")
	snaps := filepath.Join(base, "snapshots")
	out, code := s.run(nil, "@prod", "--snapshot")
	docker, _ := s.logs()
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil || strings.Contains(docker, "-mirror-") || fileExists(filepath.Join(snaps, remote.SnapshotMirror)) {
		t.Fatalf("take (%d), a first pass or a mirror:\n%s\n%s", code, out, docker)
	}
	rest := docker
	for _, want := range []string{"stop db app", "-tar ", "start db app"} {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("missing %q in order, the tars while stopped:\n%s", want, docker)
		}
		rest = rest[i+len(want):]
	}
	if got := strings.Join(tarNames(t, filepath.Join(snaps, m[1], "data", "mnt.storage.tar.zst")), " "); !strings.Contains(got, "storage/file.txt") {
		t.Errorf("storage's tar: %s", got)
	}
	if out, code := s.run(nil, "@prod", "--snapshots"); code != 0 || !strings.Contains(out, "no mirror: a take is down while its tars are written") {
		t.Errorf("--snapshots (%d):\n%s", code, out)
	}
	write(t, filepath.Join(s.mnt("prod"), "storage", "file.txt"), "changed\n", 0o644)
	if out, code := s.run(nil, "@prod", "--snapshots", "restore", m[1], "--no-code", "--yes"); code != 0 || readString(filepath.Join(s.mnt("prod"), "storage", "file.txt")) != "from prod\n" {
		t.Fatalf("restore (%d):\n%s", code, out)
	}

	// Cut after its manifest, before its rename: complete, renamed.
	cut := remote.NextReleaseID(time.Now(), manualSnapshots(snapshotPaths{snaps: snaps}))
	if out, err := exec.Command("cp", "-a", filepath.Join(snaps, m[1]), filepath.Join(snaps, cut+".tmp")).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if out, code := s.run(nil, "@prod", "--snapshot"); code != 0 || !strings.Contains(out, "finishing snapshot "+cut+", cut off before its rename") || !fileExists(filepath.Join(snaps, cut, remote.SnapshotManifest)) {
		t.Errorf("the cut take not finished (%d):\n%s", code, out)
	}

	// With a mirror, then without: its mirror goes.
	s.mirrored("prod")
	if out, code := s.run(nil, "@prod", "--snapshot"); code != 0 || !fileExists(filepath.Join(snaps, remote.SnapshotMirror)) {
		t.Fatalf("a take with a mirror (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@prod", "--snapshots"); code != 0 || !strings.Contains(out, "a mirror of the data is kept beside them") {
		t.Errorf("--snapshots with a mirror (%d):\n%s", code, out)
	}
	server := filepath.Join(filepath.Dir(s.root), "home", "config.yaml")
	write(t, server, readString(server)+"snapshots:\n  mirror: false\n", 0o644)
	out, code = s.run(nil, "@prod", "--snapshot")
	if code != 0 || !strings.Contains(out, "this server keeps no snapshot mirrors") || !strings.Contains(out, "removing the snapshots' mirror") || fileExists(filepath.Join(snaps, remote.SnapshotMirror)) {
		t.Errorf("the server's mirror: false (%d):\n%s", code, out)
	}
}

// snapshotted gives env snapshots: {release: <how>} in the setup's config
// (how a YAML value: true, or {enabled: true, keep: 2}), and a mirror
// when mirror says so.
func (s *syncSetup) snapshotted(env, how string, mirror bool) {
	s.t.Helper()
	conf := filepath.Join(s.dot, "config.yaml")
	data := readString(conf)
	if !strings.Contains(data, "\n  "+env+": {}\n") {
		s.t.Fatalf("no %s: {} in the setup's config:\n%s", env, data)
	}
	write(s.t, conf, strings.Replace(data, "\n  "+env+": {}\n", "\n  "+env+": {snapshots: {release: "+how+", mirror: "+strconv.FormatBool(mirror)+"}}\n", 1), 0o644)
}

// A release with release snapshots on takes one of the release it
// replaces before its switch, with and without a mirror: every service
// with data and its dependents stopped (left stopped for the run step to
// start under the new code), the snapshot belonging to the release
// before, the record clean once the run step ended. A first release
// takes none (a rollback neither: TestReleaseSnapshotScripts).
func TestReleaseSnapshot(t *testing.T) {
	t.Parallel()
	for _, mirror := range []bool{false, true} {
		s := newSyncSetup(t)
		s.snapshotted("prod", "true", mirror)
		s.releaseProd(nil)
		snaps := filepath.Join(s.root, "shop", "prod", "snapshots")
		if left, _ := filepath.Glob(filepath.Join(snaps, "2*")); len(left) > 0 {
			t.Fatalf("mirror %v: a first release took a snapshot: %v", mirror, left)
		}
		a := s.current("prod")
		out, code := s.run(nil, "@prod", "--release")
		docker, _ := s.logs()
		if code != 0 {
			t.Fatalf("mirror %v: the second release (%d):\n%s", mirror, code, out)
		}
		rest := docker
		for _, want := range []string{"stop db app", "-tar"} {
			i := strings.Index(rest, want)
			if i < 0 && !(mirror && want == "-tar") {
				t.Fatalf("mirror %v: missing %q in order:\n%s", mirror, want, docker)
			}
			if i >= 0 {
				rest = rest[i+len(want):]
			}
		}
		if i, j := strings.Index(out, "stopped pass"), strings.Index(out, "current is now"); strings.Contains(out, "starting db app") || j < 0 {
			t.Errorf("mirror %v: the services started again before the switch, or no switch (%d, %d):\n%s", mirror, i, j, out)
		}
		var rs []releaseSnapshot
		for start := time.Now(); time.Since(start) < startWait; time.Sleep(100 * time.Millisecond) {
			if rs = releaseSnapshots(snapshotPaths{snaps: snaps}); len(rs) == 1 {
				break
			}
		}
		if len(rs) != 1 || "releases/"+rs[0].release != a {
			t.Fatalf("mirror %v: the release snapshots %+v, want one of %s:\n%s", mirror, rs, a, out)
		}
		if rec := readString(filepath.Join(s.root, "shop", "prod", ".devopsy-sync.services")); rec != "" {
			t.Errorf("mirror %v: the record after the run step:\n%s", mirror, rec)
		}
	}
}

// env sets env's YAML in the setup's config (prod: {} by default).
func (s *syncSetup) envConfig(env, yaml string) {
	s.t.Helper()
	conf := filepath.Join(s.dot, "config.yaml")
	data := readString(conf)
	if !strings.Contains(data, "\n  "+env+": {}\n") {
		s.t.Fatalf("no %s: {} in the setup's config:\n%s", env, data)
	}
	write(s.t, conf, strings.Replace(data, "\n  "+env+": {}\n", "\n  "+env+": "+yaml+"\n", 1), 0o644)
}

// A release snapshot skipped with nothing with data, said; refused, before
// anything stops, on a server keeping one release; a release interrupted
// during its stopped pass cancelled, its services started again and no
// half snapshot kept; and pruned once its release is gone.
func TestReleaseSnapshotCases(t *testing.T) {
	t.Parallel()
	t.Run("no data", func(t *testing.T) {
		t.Parallel()
		s := newSyncSetup(t)
		s.snapshotted("prod", "true", false)
		write(t, s.log+".shop-prod", "FAKE_APP=none\nFAKE_DB=none\n", 0o644)
		s.releaseProd(nil)
		out, code := s.run(nil, "@prod", "--release")
		docker, _ := s.logs()
		if code != 0 || !strings.Contains(out, "no release snapshot: no service has data") || strings.Contains(docker, " stop ") {
			t.Errorf("(%d):\n%s\n%s", code, out, docker)
		}
	})
	t.Run("keep 1", func(t *testing.T) {
		t.Parallel()
		s := newSyncSetup(t)
		s.snapshotted("prod", "true", false)
		s.releaseProd(nil)
		server := filepath.Join(filepath.Dir(s.root), "home", "config.yaml")
		write(t, server, readString(server)+"  keep: 1\n", 0o644)
		before := s.current("prod")
		out, code := s.run(nil, "@prod", "--release")
		docker, _ := s.logs()
		if code == 0 || !strings.Contains(out, "this server keeps 1 release") || strings.Contains(docker, " stop ") || s.current("prod") != before {
			t.Errorf("(%d):\n%s\n%s", code, out, docker)
		}
	})
	t.Run("interrupted", func(t *testing.T) {
		t.Parallel()
		s := newSyncSetup(t)
		s.snapshotted("prod", "true", false)
		s.releaseProd(nil)
		before := s.current("prod")
		started := filepath.Join(t.TempDir(), "started")
		outFile, err := os.CreateTemp(t.TempDir(), "out")
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "@prod", "--release")
		cmd.Dir = s.project
		cmd.Env = append(append([]string{}, s.env...), "FAKE_SLOW_TAR="+started)
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
		base := filepath.Join(s.root, "shop", "prod")
		for !strings.Contains(readString(s.log), "start db app") || strings.Contains(readString(filepath.Join(base, ".devopsy-sync.services")), "snapshot ") {
			if time.Since(start) > 15*time.Second {
				t.Fatalf("the services not started again, or their snapshot lines left:\n%s\n%s", out, readString(filepath.Join(base, ".devopsy-sync.services")))
			}
			time.Sleep(100 * time.Millisecond)
		}
		if cmd.ProcessState.ExitCode() == 0 || s.current("prod") != before {
			t.Errorf("not cancelled (%d), current %s (was %s):\n%s", cmd.ProcessState.ExitCode(), s.current("prod"), before, out)
		}
		// The signal reaches the script alone here (no sshd leads its
		// group), so the stopped pass may finish first: a complete
		// snapshot of the release before is right then, a half one never.
		if left, _ := filepath.Glob(filepath.Join(base, "snapshots", "*.tmp")); len(left) > 0 {
			t.Errorf("a half snapshot left: %v", left)
		}
	})
	t.Run("pruned", func(t *testing.T) {
		t.Parallel()
		s := newSyncSetup(t)
		s.envConfig("prod", "{releases: {keep: 2}, snapshots: {release: true}}")
		s.releaseProd(nil)
		snaps := snapshotPaths{snaps: filepath.Join(s.root, "shop", "prod", "snapshots")}
		var second string
		for i := range 2 {
			if out, code := s.run(nil, "@prod", "--release"); code != 0 {
				t.Fatalf("release %d (%d):\n%s", i+2, code, out)
			}
			if i == 0 {
				second = s.current("prod")
			}
		}
		// The third release prunes the first: its snapshot goes with it,
		// once the detached tar's pruning ran after the activation.
		var rs []releaseSnapshot
		for start := time.Now(); time.Since(start) < startWait; time.Sleep(100 * time.Millisecond) {
			if rs = releaseSnapshots(snaps); len(rs) == 1 && "releases/"+rs[0].release == second {
				return
			}
		}
		t.Errorf("release snapshots %+v, want one, of %s", rs, second)
	})
	t.Run("pruned once off", func(t *testing.T) {
		t.Parallel()
		if _, err := os.Stat("/proc/self/cwd"); err != nil {
			t.Skip("waiting for the detached tar reads /proc")
		}
		// A release snapshot whose release is gone goes at the next
		// pruning, by a holder of the snapshot lock (a take, a removal),
		// whatever removed the release: here releases once release
		// snapshots are off, then a manual take.
		s := newSyncSetup(t)
		s.envConfig("prod", "{releases: {keep: 2}, snapshots: {release: true}}")
		s.releaseProd(nil)
		snaps := snapshotPaths{snaps: filepath.Join(s.root, "shop", "prod", "snapshots")}
		if out, code := s.run(nil, "@prod", "--release"); code != 0 {
			t.Fatalf("second release (%d):\n%s", code, out)
		}
		var rs []releaseSnapshot
		for start := time.Now(); time.Since(start) < startWait; time.Sleep(100 * time.Millisecond) {
			if rs = releaseSnapshots(snaps); len(rs) == 1 {
				break
			}
		}
		if len(rs) != 1 {
			t.Fatalf("release snapshots %+v, want one", rs)
		}
		// Its detached tar's pruning done: it would otherwise prune for
		// the releases below.
		s.waitReleaseTars()
		conf := filepath.Join(s.dot, "config.yaml")
		write(t, conf, strings.Replace(readString(conf), "snapshots: {release: true}", "snapshots: {release: false}", 1), 0o644)
		for i := range 2 {
			if out, code := s.run(nil, "@prod", "--release"); code != 0 {
				t.Fatalf("release %d (%d):\n%s", i+3, code, out)
			}
		}
		if fileExists(filepath.Join(s.root, "shop", "prod", "releases", rs[0].release)) {
			t.Fatalf("release %s not pruned", rs[0].release)
		}
		if out, code := s.run(nil, "@prod", "--snapshot"); code != 0 || !strings.Contains(out, "its release "+rs[0].release+" is gone") {
			t.Fatalf("a take (%d):\n%s", code, out)
		}
		if left := releaseSnapshots(snaps); len(left) > 0 {
			t.Errorf("release snapshots %+v left, their release gone", left)
		}
	})
}

// waitReleaseTars waits until no detached release-tar of the setup's
// environments runs (Linux: /proc).
func (s *syncSetup) waitReleaseTars() {
	s.t.Helper()
	for start := time.Now(); ; time.Sleep(100 * time.Millisecond) {
		busy := false
		procs, _ := filepath.Glob("/proc/[0-9]*")
		for _, p := range procs {
			cmd, _ := os.ReadFile(filepath.Join(p, "cmdline"))
			cwd, _ := os.Readlink(filepath.Join(p, "cwd"))
			if strings.Contains(string(cmd), "release-tar") && strings.HasPrefix(cwd, s.root) {
				busy = true
			}
		}
		if !busy {
			return
		}
		if time.Since(start) > startWait {
			s.t.Fatal("a release's detached tar still runs")
		}
	}
}

// childSnapshotted gives dev (prod's child) release snapshots in the
// setup's config.
func (s *syncSetup) childSnapshotted() {
	s.t.Helper()
	conf := filepath.Join(s.dot, "config.yaml")
	data := readString(conf)
	if !strings.Contains(data, "\n  dev: {parent: prod}\n") {
		s.t.Fatalf("no dev: {parent: prod} in the setup's config:\n%s", data)
	}
	write(s.t, conf, strings.Replace(data, "\n  dev: {parent: prod}\n", "\n  dev: {parent: prod, snapshots: {release: true}}\n", 1), 0o644)
}

// A release that syncs with release snapshots on: what the snapshot's
// stopped pass left stopped becomes the release's stop lines at the
// switch, which the sync's finish restores or keeps down. A copy cut
// leaves the child partial, its services down until a sync completes,
// and the record must keep saying so: the next sync's finish go starts
// what the record names before its hook, and nothing started them here
// (the restart of the release before skips a marked environment).
func TestReleaseSnapshotSyncPartialKeepsStops(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.childSnapshotted()
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("dev's first release (%d):\n%s", code, out)
	}
	s.logs()
	dev := filepath.Join(s.root, "shop", "dev")
	out, code := s.run([]string{"FAIL_RSYNC=23"}, "@dev", "--release-sync", "--yes")
	docker, _ := s.logs()
	down := readString(s.log + ".stopped.shop-dev")
	if code == 0 || !strings.Contains(docker, "shop-dev compose -f "+filepath.Join(dev, "current", ".devopsy", "compose.yaml")+" stop db app") || !strings.Contains(out, "db app stay down: the data is partial until a sync completes") || !fileExists(filepath.Join(dev, remote.SyncIncompleteFile)) || !strings.Contains(down, "db\n") || !strings.Contains(down, "app\n") {
		t.Fatalf("a release that syncs, its copy cut after the release snapshot (%d), down: %q:\n%s\n%s", code, down, out, docker)
	}
	if rec := readString(filepath.Join(dev, ".devopsy-sync.services")); !strings.Contains(rec, "stop db\n") || !strings.Contains(rec, "stop app\n") {
		t.Errorf("db and app stay down on the partial copy, and the record no longer says so (nothing would start them before the next sync's finish hook):\n%q", rec)
	}
}

// A --no-code restore with release snapshots on, cancelled during the
// snapshot's stopped pass (Ctrl-C, the session gone): the services it
// stopped start again, their snapshot lines go, as a release's do
// (not_live's hook) and a take's. The watcher signals the script alone
// here, as OrbStack's SSH would; under sshd the step sees the signal
// too, but ignores it once its tars are complete, and the shell is what
// starts the services again either way.
func TestRestoreNoCodeInterruptDuringReleaseSnapshot(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.snapshotted("prod", "true", false)
	// One entry, db's: the stopped pass says nothing to the session once
	// its tar started, so it completes whatever became of the session.
	write(t, s.log+".shop-prod", "FAKE_APP=none\n", 0o644)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	s.logs()
	started := filepath.Join(t.TempDir(), "started")
	outFile, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "@prod", "--snapshots", "restore", m[1], "--no-code", "--yes")
	cmd.Dir = s.project
	cmd.Env = append(append([]string{}, s.env...), "FAKE_SLOW_TAR="+started)
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
	_ = cmd.Process.Signal(syscall.SIGINT)
	_ = cmd.Wait()
	out = readString(outFile.Name())
	// The tar goes on: the stopped pass ends with db and app down.
	write(t, started+".go", "", 0o644)
	start := time.Now()
	base := filepath.Join(s.root, "shop", "prod")
	for !strings.Contains(readString(s.log), "start db app") || strings.Contains(readString(filepath.Join(base, ".devopsy-sync.services")), "snapshot ") {
		if time.Since(start) > 15*time.Second {
			t.Fatalf("the services not started again, or their snapshot lines left (down: %q):\n%s\n%s\n%s", readString(s.log+".stopped.shop-prod"), out, readString(filepath.Join(base, ".devopsy-sync.services")), readString(s.log))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if cmd.ProcessState.ExitCode() == 0 || strings.Contains(readString(s.log), "-extract-") {
		t.Errorf("not cancelled (%d), or the data restored:\n%s", cmd.ProcessState.ExitCode(), out)
	}
}

// A release snapshot's detached tar killed outright (a reboot) leaves
// its <id>.tmp, bookmarked, which only a holder of the snapshot lock
// settles: finished from the mirror when the marker names it, else
// removed with its bookmarks. The next release holds that lock for its
// own snapshot, and an environment with release snapshots alone may
// never see a manual take: it must settle the leftover too, or the .tmp
// and the images its bookmarks keep stay for good, listed INCOMPLETE.
func TestReleaseSnapshotSettlesLeftoverTmp(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.envConfig("prod", "{releases: {keep: 3}, snapshots: {release: true, mirror: true}}")
	s.releaseProd(nil)
	snaps := snapshotPaths{snaps: filepath.Join(s.root, "shop", "prod", "snapshots")}
	if out, code := s.run(nil, "@prod", "--release"); code != 0 {
		t.Fatalf("the second release (%d):\n%s", code, out)
	}
	var rs []releaseSnapshot
	for start := time.Now(); time.Since(start) < startWait && len(rs) != 1; time.Sleep(100 * time.Millisecond) {
		rs = releaseSnapshots(snaps)
	}
	if len(rs) != 1 {
		t.Fatalf("release snapshots after the second release: %+v", rs)
	}
	// A release snapshot's tar killed before its rename, the mirror's
	// marker naming a later one since: nobody's. An id of its own, older,
	// so the next take's id never coincides with it.
	left := "20200101000000"
	if out, err := exec.Command("cp", "-a", filepath.Join(snaps.snaps, rs[0].id), filepath.Join(snaps.snaps, left+".tmp")).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if out, code := s.run(nil, "@prod", "--release"); code != 0 {
		t.Fatalf("the third release (%d):\n%s", code, out)
	}
	// Settled by the activation or by its detached tar right after it.
	for start := time.Now(); fileExists(filepath.Join(snaps.snaps, left+".tmp")); time.Sleep(100 * time.Millisecond) {
		if time.Since(start) > 10*time.Second {
			out, _ := s.run(nil, "@prod", "--snapshots")
			t.Fatalf("the cut take's %s.tmp left by the release after it:\n%s", left, out)
		}
	}
}

// A release snapshot's detached tar prunes once the activation ended, and
// removing a snapshot frees images: only under the server's image lock,
// exclusive, as every removal of images is (another environment's fetch
// may be bookmarking the same image meanwhile). Held by someone else, the
// pruning waits for it and the snapshot stays; freed, it goes.
func TestReleaseSnapshotPruneHoldsImageLock(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.envConfig("prod", "{releases: {keep: 2}, snapshots: {release: true, mirror: true}}")
	s.releaseProd(nil)
	first := strings.TrimPrefix(s.current("prod"), "releases/")
	snaps := snapshotPaths{snaps: filepath.Join(s.root, "shop", "prod", "snapshots")}
	if out, code := s.run(nil, "@prod", "--release"); code != 0 {
		t.Fatalf("the second release (%d):\n%s", code, out)
	}
	var rs []releaseSnapshot
	for start := time.Now(); time.Since(start) < startWait && len(rs) != 1; time.Sleep(100 * time.Millisecond) {
		rs = releaseSnapshots(snaps)
	}
	if len(rs) != 1 || rs[0].release != first {
		t.Fatalf("release snapshots after the second release: %+v, want one of %s", rs, first)
	}
	orphan := filepath.Join(snaps.snaps, rs[0].id)
	// The third release prunes the first, whose snapshot is then nobody's:
	// its detached tar, held in its tars until told to go on, prunes it
	// once the activation ended.
	started := filepath.Join(t.TempDir(), "started")
	if out, code := s.run([]string{"FAKE_SLOW_TAR=" + started}, "@prod", "--release"); code != 0 {
		t.Fatalf("the third release (%d):\n%s", code, out)
	}
	if !fileExists(started) || fileExists(filepath.Join(s.root, "shop", "prod", "releases", first)) {
		t.Fatalf("the detached tar not started (%v), or the first release kept (%v)", fileExists(started), fileExists(filepath.Join(s.root, "shop", "prod", "releases", first)))
	}
	lock, err := os.OpenFile(filepath.Join(s.root, remote.ImagesLock), os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	write(t, started+".go", "", 0o644)
	time.Sleep(3 * time.Second)
	if !fileExists(orphan) {
		t.Errorf("the first release's snapshot %s was removed, its images freed, while the server's image lock was held by someone else", rs[0].id)
	}
	lock.Close()
	for start := time.Now(); fileExists(orphan); time.Sleep(100 * time.Millisecond) {
		if time.Since(start) > startWait {
			t.Fatalf("the first release's snapshot %s not pruned once the image lock was free", rs[0].id)
		}
	}
}

// A mirror take's tar step that fails on its last tar (Docker answering
// an error, a disk full) leaves its .tmp with the manifest, the listing
// and every tar file, the last one cut, named by the mirror's marker; a
// take queued behind it (another operator's, a cron's) then rewrites the
// mirror and the marker in its own stopped pass. The next take finds that
// .tmp complete but for the marker, and must not take it for a take
// without a mirror cut before its rename: renamed into place, it is a
// snapshot that lists as taken, counts towards keep (pruning a good one
// for it) and restores as nothing. Whatever ends up renamed must restore.
func TestSnapshotLeftoverCutTar(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.mirrored("prod")
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
	// Its last tar cut inside its second block (tar pads a whole archive
	// to its record size, so a cut must land inside a member); the marker
	// names the later take.
	cut := filepath.Join(snaps, left+".tmp", remote.SnapshotData, "mnt.storage.tar.zst")
	if err := os.Truncate(cut, 700); err != nil {
		t.Fatal(err)
	}
	if got := readString(filepath.Join(snaps, remote.SnapshotMirror, remote.SnapshotComplete)); strings.TrimSpace(got) != m[1] {
		t.Fatalf("the marker names %q, not the take %s", got, m[1])
	}
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

// With release snapshots on, a restore, full or --no-code, first takes a
// snapshot of the data it replaces, of the release current then: the
// restore undone by restoring that one. Its data comes back, its record
// clean, the restored snapshot kept.
func TestRestoreTakesReleaseSnapshot(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{}, {"--no-code"}} {
		s := newSyncSetup(t)
		s.envConfig("prod", "{releases: {keep: 3}, snapshots: {release: {enabled: true, keep: all}}}")
		s.releaseProd(nil)
		out, code := s.run(nil, "@prod", "--snapshot")
		m := snapshotID.FindStringSubmatch(out)
		if code != 0 || m == nil {
			t.Fatalf("%v: take (%d):\n%s", args, code, out)
		}
		write(t, filepath.Join(s.mnt("prod"), "storage", "file.txt"), "changed\n", 0o644)
		before := s.current("prod")
		snaps := snapshotPaths{snaps: filepath.Join(s.root, "shop", "prod", "snapshots")}
		s.logs()
		out, code = s.run(nil, append([]string{"@prod", "--snapshots", "restore", m[1], "--yes"}, args...)...)
		if code != 0 || readString(filepath.Join(s.mnt("prod"), "storage", "file.txt")) != "from prod\n" {
			t.Fatalf("%v: restore (%d):\n%s", args, code, out)
		}
		// What the snapshot of the replaced data left stopped stays down
		// for the restore's data step: never started on the data about to
		// be replaced, to be stopped again.
		if docker, _ := s.logs(); strings.Contains(out, "left stopped") || strings.Count(docker, "stop db app") != 1 {
			t.Errorf("%v: the services started between the snapshot and the restore's data:\n%s\n%s", args, out, docker)
		}
		var rs []releaseSnapshot
		for start := time.Now(); time.Since(start) < startWait; time.Sleep(100 * time.Millisecond) {
			if rs = releaseSnapshots(snaps); len(rs) == 1 {
				break
			}
		}
		if len(rs) != 1 || "releases/"+rs[0].release != before {
			t.Fatalf("%v: release snapshots %+v, want one of %s:\n%s", args, rs, before, out)
		}
		if got := strings.Join(tarNames(t, filepath.Join(snaps.snaps, rs[0].id, "data", "mnt.storage.tar.zst")), " "); !strings.Contains(got, "storage/file.txt") {
			t.Errorf("%v: the replaced data's tar: %s", args, got)
		}
		if rec := readString(filepath.Join(s.root, "shop", "prod", ".devopsy-sync.services")); rec != "" || fileExists(filepath.Join(s.root, "shop", "prod", remote.SyncIncompleteFile)) {
			t.Errorf("%v: the record or the mark left:\n%s", args, rec)
		}
		if !fileExists(filepath.Join(snaps.snaps, m[1], remote.SnapshotManifest)) {
			t.Errorf("%v: the restored snapshot went", args)
		}
	}
}

// The reflink check tries one file of each entry and volume; where it
// passes and the clone then fails on another (a filesystem mounted inside
// an entry, a btrfs nodatacow file), the take must still be possible: the
// services are stopped already, and its tars can come from the stopped
// data, as a take without a mirror's. Otherwise every take fails the same
// way, with nothing to turn reflinks off, and with release snapshots on
// every release fails before its switch.
func TestSnapshotReflinkCloneFails(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.snapshotted("prod", "true", false)
	s.releaseProd(nil)
	env := []string{"FAKE_REFLINK=1", "FAIL_CLONE=1"}
	if out, code := s.run(env, "@prod", "--snapshot"); code != 0 {
		t.Errorf("a take whose clone fails after the check passed (%d):\n%s", code, out)
	}
	if out, code := s.run(env, "@prod", "--release"); code != 0 {
		t.Errorf("a release whose release snapshot's clone fails after the check passed (%d):\n%s", code, out)
	}
}

// A snapshot devopsy says it took restores. Data holding a FIFO (a mail
// queue's public/pickup, say): a take with a mirror leaves it out (the
// mirror's rsync --no-D), one without a mirror or by reflinks tars it, and
// a restore's check refuses any member but files, directories and links.
func TestSnapshotFIFORestores(t *testing.T) {
	t.Parallel()
	for _, env := range [][]string{nil, {"FAKE_REFLINK=1"}} {
		s := newSyncSetup(t)
		s.releaseProd(nil)
		if out, err := exec.Command("mkfifo", filepath.Join(s.mnt("prod"), "storage", "pickup")).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		out, code := s.run(env, "@prod", "--snapshot")
		m := snapshotID.FindStringSubmatch(out)
		if code != 0 || m == nil {
			continue
		}
		if out, code := s.run(env, "@prod", "--snapshots", "restore", m[1], "--no-code", "--yes"); code != 0 {
			t.Errorf("%v: snapshot %s taken, its restore refused (%d):\n%s", env, m[1], code, out)
		}
	}
}

// A restore with release snapshots on whose data step refuses, nothing
// restored (here mnt/storage a file now, a directory in the snapshot),
// leaves the services running: what its snapshot of the data it replaces
// stopped starts again, as before any stop a refusal does.
func TestRestoreRefusedStartsReleaseSnapshotSet(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{}, {"--no-code"}} {
		s := newSyncSetup(t)
		s.envConfig("prod", "{releases: {keep: 3}, snapshots: {release: {enabled: true, keep: all}}}")
		s.releaseProd(nil)
		out, code := s.run(nil, "@prod", "--snapshot")
		m := snapshotID.FindStringSubmatch(out)
		if code != 0 || m == nil {
			t.Fatalf("%v: take (%d):\n%s", args, code, out)
		}
		if err := os.RemoveAll(filepath.Join(s.mnt("prod"), "storage")); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(s.mnt("prod"), "storage"), "a file now\n", 0o644)
		s.logs()
		out, code = s.run(nil, append([]string{"@prod", "--snapshots", "restore", m[1], "--yes"}, args...)...)
		if code == 0 || !strings.Contains(out, "nothing restored") {
			t.Fatalf("%v: a restore over an entry of another kind (%d):\n%s", args, code, out)
		}
		if down := strings.Fields(readString(s.log + ".stopped.shop-prod")); len(down) > 0 {
			t.Errorf("%v: nothing restored, and %v left stopped (record: %q):\n%s", args, down, readString(filepath.Join(s.root, "shop", "prod", ".devopsy-sync.services")), out)
		}
	}
}

// Where the data clones by reflinks, a take clones it while stopped (no
// first pass, whatever its mirror setting), makes the tars from the clone
// once the services are back, and removes the clone; a mirror left from
// before goes. A take cut after its clone is finished from it by the
// next; a release's snapshot clones the same way.
func TestSnapshotReflink(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.mirrored("prod")
	s.releaseProd(nil)
	snaps := filepath.Join(s.root, "shop", "prod", "snapshots")
	// A mirror from a take before the data moved onto reflinks.
	if out, code := s.run(nil, "@prod", "--snapshot"); code != 0 || !fileExists(filepath.Join(snaps, remote.SnapshotMirror)) {
		t.Fatalf("a take with a mirror (%d):\n%s", code, out)
	}
	s.logs()
	reflink := []string{"FAKE_REFLINK=1"}
	out, code := s.run(reflink, "@prod", "--snapshot")
	docker, _ := s.logs()
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil || strings.Contains(docker, "-mirror-0") || !strings.Contains(out, "removing the snapshots' mirror") || fileExists(filepath.Join(snaps, remote.SnapshotMirror)) {
		t.Fatalf("take (%d), a first pass or the mirror kept:\n%s\n%s", code, out, docker)
	}
	rest := docker
	for _, want := range []string{"-reflink ", "stop db app", "-clone ", "start db app", "-tar ", "-unclone "} {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("missing %q in order:\n%s", want, docker)
		}
		rest = rest[i+len(want):]
	}
	if got := strings.Join(tarNames(t, filepath.Join(snaps, m[1], "data", "mnt.storage.tar.zst")), " "); !strings.Contains(got, "storage/file.txt") || !strings.Contains(readString(filepath.Join(snaps, m[1], remote.SnapshotManifest)), "copy\treflink\n") || fileExists(filepath.Join(s.root, "shop", "prod", "snapshots", ".clone", m[1])) {
		t.Errorf("the snapshot from its clone: %s\n%s", got, readString(filepath.Join(snaps, m[1], remote.SnapshotManifest)))
	}
	if out, code := s.run(reflink, "@prod", "--snapshots"); code != 0 || !strings.Contains(out, "the last take cloned the data by reflinks") {
		t.Errorf("--snapshots (%d):\n%s", code, out)
	}

	// Cut after its clone: finished from it.
	cut := remote.NextReleaseID(time.Now(), manualSnapshots(snapshotPaths{snaps: snaps}))
	if out, err := exec.Command("cp", "-a", filepath.Join(snaps, m[1]), filepath.Join(snaps, cut+".tmp")).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	tars, _ := filepath.Glob(filepath.Join(snaps, cut+".tmp", "data", "*.tar.zst"))
	for _, f := range tars {
		os.Remove(f)
	}
	clone := filepath.Join(snaps, ".clone", cut)
	write(t, filepath.Join(clone, "mnt", "storage", "file.txt"), "cloned\n", 0o644)
	write(t, filepath.Join(clone, "mnt", "db", "ibdata"), "rows\n", 0o644)
	write(t, filepath.Join(clone, remote.SnapshotComplete), cut+"\n", 0o600)
	if out, code := s.run(reflink, "@prod", "--snapshot"); code != 0 || !strings.Contains(out, "finishing snapshot "+cut) || !fileExists(filepath.Join(snaps, cut, "data", "mnt.storage.tar.zst")) || fileExists(clone) {
		t.Errorf("the cut take not finished from its clone (%d):\n%s", code, out)
	}

	// A clone without its .tmp (a take killed outright during its clone,
	// or after its rename, before its clone went): nobody's, removed by
	// the next take, there no .tmp at all.
	left := filepath.Join(snaps, ".clone", "20200101000000")
	write(t, filepath.Join(left, "mnt", "db", "ibdata"), "old rows\n", 0o644)
	if out, code := s.run(reflink, "@prod", "--snapshot"); code != 0 || fileExists(left) {
		t.Errorf("a leftover clone kept (%d, there: %v):\n%s", code, fileExists(left), out)
	}

	// A release's snapshot, cloned.
	s2 := newSyncSetup(t)
	s2.snapshotted("prod", "true", false)
	s2.releaseProd(nil)
	s2.logs()
	if out, code := s2.run(reflink, "@prod", "--release"); code != 0 {
		t.Fatalf("a release (%d):\n%s", code, out)
	}
	docker, _ = s2.logs()
	snaps2 := snapshotPaths{snaps: filepath.Join(s2.root, "shop", "prod", "snapshots")}
	for start := time.Now(); len(releaseSnapshots(snaps2)) == 0 && time.Since(start) < startWait; time.Sleep(100 * time.Millisecond) {
	}
	if rs := releaseSnapshots(snaps2); len(rs) != 1 || !strings.Contains(docker, "-clone ") {
		t.Errorf("a release's snapshot, cloned: %+v\n%s", rs, docker)
	}
}
// A release that syncs runs no partialGuard: the sync's prepare settles a
// killed holder's files, after the switch. With release snapshots on, the
// snapshot of what the release replaces is taken before that, and must
// not be one of the half-synced data a settle is about to mark (a
// restore of it clears marks: it would restore as complete), as a take
// and a plain release settle first; the mark then skips it, said.
func TestReleaseSyncSettlesBeforeReleaseSnapshot(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.childSnapshotted()
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("dev's first release (%d):\n%s", code, out)
	}
	s.logs()
	dev := filepath.Join(s.root, "shop", "dev")
	write(t, filepath.Join(dev, remote.SyncTakenFile), "devopsy@one:prod 1.2\n", 0o644)
	out, code := s.run(nil, "@dev", "--release-sync", "--yes")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(out, "left its files") || fileExists(filepath.Join(dev, remote.SyncIncompleteFile)) {
		t.Fatalf("a release that syncs over a killed holder's files (%d):\n%s\n%s", code, out, docker)
	}
	rs := releaseSnapshots(snapshotPaths{snaps: filepath.Join(dev, "snapshots")})
	if len(rs) > 0 || !strings.Contains(out, "no release snapshot: the environment is marked") {
		t.Errorf("a release snapshot %+v of the data a killed sync holder left, taken before the settle marked it:\n%s", rs, out)
	}
}

