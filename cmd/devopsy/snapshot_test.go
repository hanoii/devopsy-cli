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
