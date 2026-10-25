package main

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

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
	write(t, filepath.Join(base, remote.SyncIncompleteFile), "2026-10-25 00:00:00\t"+m[1]+"\trestore\tthe restore of snapshot "+m[1]+" did not complete\n", 0o644)
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
	if out, code := s.run(nil, "@prod", "--status"); code != 0 || !strings.Contains(out, "the restore of snapshot "+m[1]+" did not complete") {
		t.Errorf("--status (%d):\n%s", code, out)
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
