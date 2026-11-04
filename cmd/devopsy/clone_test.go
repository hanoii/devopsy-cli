package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// cloneData's script copies each entry and volume by its exact name,
// whatever the shell makes of it, and runs a cp that failed once more (a
// file a running service removed meanwhile fails it, the rest copied):
// run here against a cp that fails its first try of each source, and
// one that always fails, which fails the script.
func TestCloneScript(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	bin, log := filepath.Join(tmp, "bin"), filepath.Join(tmp, "cp.log")
	// cp logs its source and destination, one call a line, and fails the
	// first call for each source (FAIL_ALWAYS: every call).
	write(t, filepath.Join(bin, "cp"), `#!/bin/sh
for a; do src=$dst; dst=$a; done
printf '%s|%s\n' "$src" "$dst" >>"$CP_LOG"
[ -z "${FAIL_ALWAYS:-}" ] || exit 1
n=$(grep -cxF -- "$src|$dst" "$CP_LOG")
[ "$n" -gt 1 ]
`, 0o755)
	write(t, filepath.Join(bin, "mkdir"), "#!/bin/sh\nexit 0\n", 0o755)
	name := `a"b$c'd`
	script := cloneScript(dataSet{entries: []string{"files", name}, volumes: []cli.Volume{{Key: "dbdata"}}})
	run := func(extra ...string) error {
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "CP_LOG=" + log}, extra...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("the script: %v\n%s", err, out)
		}
		return err
	}
	if err := run(); err != nil {
		t.Fatalf("a cp that fails once failed the script: %v", err)
	}
	data, _ := os.ReadFile(log)
	want := []string{"/m/mnt/files|/c/mnt/", "/m/mnt/files|/c/mnt/", "/m/mnt/" + name + "|/c/mnt/", "/m/mnt/" + name + "|/c/mnt/", "/m/volumes/dbdata/.|/c/volumes/dbdata/", "/m/volumes/dbdata/.|/c/volumes/dbdata/"}
	if got := strings.Split(strings.TrimSpace(string(data)), "\n"); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("cp's calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	os.Remove(log)
	if err := run("FAIL_ALWAYS=1"); err == nil {
		t.Errorf("a cp failing twice did not fail the script")
	}
}

// cloneCheck makes the clone's directory, $base/.devopsy-sync.clone.<id>,
// before the stop, and a stop that fails removes it, for a sync and for an
// export (here db's stop fails on the parent, FAIL_STOP): left there, it
// would read as what a killed copy left, and the next push, export or
// --destroy-data would remove the parent's containers by label and run an
// unclone container as root for an empty directory.
func TestCloneDirGoneAfterStopFailure(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	s.logs()
	clones := func() []string {
		left, _ := filepath.Glob(filepath.Join(s.root, "shop", "prod", remote.SyncClonePrefix+"*"))
		return left
	}

	// A sync whose stop on the parent fails after the reflink check.
	out, code := s.run([]string{"FAKE_REFLINK=1", "FAIL_STOP=shop-prod"}, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code == 0 || !strings.Contains(docker, "-reflink ") || strings.Contains(docker, "-clone ") {
		t.Fatalf("the sync (%d) should fail at the stop, after the check and before any clone:\n%s\n%s", code, out, docker)
	}
	if left := clones(); len(left) > 0 {
		t.Errorf("a sync whose stop failed left the clone's directory, which nothing used: %v", left)
	}
	out, code = s.run([]string{"FAKE_REFLINK=1"}, "@dev", "--sync", "--yes")
	if code != 0 {
		t.Fatalf("the sync after (%d):\n%s", code, out)
	}
	if strings.Contains(out, "a clone an earlier copy left") {
		t.Errorf("the next sync took the directory for a killed copy's clone:\n%s", out)
	}
	s.logs()

	// An export on a target whose stop fails likewise.
	cmd := exec.Command(binary, "@prod", "--export", "db")
	cmd.Dir, cmd.Env = s.project, append(append([]string{}, s.env...), "FAKE_REFLINK=1", "FAIL_STOP=shop-prod")
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &stderr
	err := cmd.Run()
	docker, _ = s.logs()
	if err == nil || !strings.Contains(docker, "-reflink ") || strings.Contains(docker, "-clone ") {
		t.Fatalf("the export (%v) should fail at the stop, after the check and before any clone:\n%s\n%s", err, stderr.String(), docker)
	}
	if left := clones(); len(left) > 0 {
		t.Errorf("an export whose stop failed left the clone's directory: %v", left)
	}
}

// Where the parent's data clones, a custom service its labels do not stop
// is exported after the set started again, so its hook's DEVOPSY_DATA_STOP
// names nothing that runs: docs/capabilities.md says a service named there
// is stopped, to reach with devopsy run --rm --no-deps -T, and a script
// following it would run a second container on the parent's data, or dump
// without a transaction under the writer.
func TestCloneStreamExportToldWhatIsStopped(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	caps := filepath.Join(s.dot, "capabilities", "data")
	exports := s.log + ".exports"
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho \"$1|${DEVOPSY_DATA_STOP:-}|$(devopsy ps --services --status running | tr '\\n' ' ')\" >> \""+exports+"\"\necho \"dump of $1\"\n", 0o755)
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\ncat >/dev/null\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	s.logs()
	// app copied as volumes, stopped own; db a dump, never stopped.
	s.labels("FAKE_APP=mnt+own\nFAKE_DB=stream")
	out, code := s.run([]string{"FAKE_REFLINK=1"}, "@dev", "--sync", "--yes")
	if code != 0 || !strings.Contains(out, "the data clones by reflinks") {
		t.Fatalf("sync (%d):\n%s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(readString(exports)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("the export hook never ran:\n%s", out)
	}
	for _, l := range lines {
		f := strings.SplitN(l, "|", 3)
		if len(f) != 3 {
			t.Fatalf("export line %q", l)
		}
		running := strings.Fields(f[2])
		for _, stop := range strings.Fields(f[1]) {
			if slices.Contains(running, stop) {
				t.Errorf("export %s told DEVOPSY_DATA_STOP=%q while %s runs (running: %v):\n%s", f[0], f[1], stop, running, out)
			}
		}
	}
}

// A push cancelled during the reflink check (cloneCheck: a docker run,
// then du over the data; Ctrl-C or the session gone kills its container)
// finds reflinks false, and stops nothing: the parent's db stays up once
// the push's context is done.
func TestCloneCheckCancelStopsNothing(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	caps := filepath.Join(s.dot, "capabilities", "data")
	finished := s.log + ".finish"
	write(t, filepath.Join(caps, "finish"), "#!/bin/sh\necho \"$1 $2\" >> \""+finished+"\"\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	s.logs()
	os.Remove(finished)

	wait := filepath.Join(t.TempDir(), "check")
	cut := exec.Command(binary, "@dev", "--sync", "--yes")
	cut.Dir, cut.Stdin = s.project, strings.NewReader("")
	cut.Env = append(append([]string{}, s.env...), "FAKE_REFLINK=1", "FAKE_RUN_WAIT="+wait, "FAKE_RUN_WAIT_ON=-reflink ", "FAKE_SSH_DETACH=1")
	if err := cut.Start(); err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); !fileExists(wait); time.Sleep(50 * time.Millisecond) {
		if time.Since(start) > startWait {
			_ = cut.Process.Kill()
			t.Fatalf("the reflink check never ran:\n%s", readString(s.log))
		}
	}
	_ = cut.Process.Signal(syscall.SIGTERM)
	_ = cut.Wait()
	// The push ends with its finish parent hook, whatever happened.
	for start := time.Now(); !strings.Contains(readString(finished), "parent"); time.Sleep(100 * time.Millisecond) {
		if time.Since(start) > startWait {
			t.Fatalf("the push never finished:\n%s", readString(s.log))
		}
	}
	docker, _ := s.logs()
	for _, line := range strings.Split(docker, "\n") {
		if strings.HasPrefix(line, "shop-prod compose") && strings.Contains(line, " stop db") {
			t.Errorf("prod's db stopped by a push cancelled during its reflink check, before anything changed:\n%s", docker)
			break
		}
	}
}

// logLineAt is the first line of log starting with prefix and holding
// sub, with its index, or -1.
func logLineAt(log, prefix, sub string) (string, int) {
	at := 0
	for _, l := range strings.Split(log, "\n") {
		if strings.HasPrefix(l, prefix) && strings.Contains(l, sub) {
			return l, at
		}
		at += len(l) + 1
	}
	return "", -1
}

// A service labeled devopsy.data.stop=always stays down while a custom
// service is exported after the clone, as docs/data.md's table says: db is
// custom with stop=never (a hot dump), app is volumes with stop=always
// (its files must match the dump), so app starts only after db's export.
func TestCloneStreamExportKeepsAlwaysDown(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	caps := filepath.Join(s.dot, "capabilities", "data")
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho \"dump of $1\"\n", 0o755)
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\ncat >/dev/null\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	s.logs()
	s.labels("FAKE_APP=mnt+always\nFAKE_DB=stream")
	out, code := s.run([]string{"FAKE_REFLINK=1"}, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(out, "the data clones by reflinks") {
		t.Fatalf("the sync (%d):\n%s", code, out)
	}
	// app (always) is stopped, cloned... and must still be down when db
	// is exported: the export before the start.
	if _, at := logLineAt(docker, "shop-prod ", " stop app"); at < 0 {
		t.Fatalf("app (devopsy.data.stop=always) not stopped on the parent:\n%s", docker)
	}
	export, start := strings.Index(out, "exporting db"), strings.Index(out, "starting app")
	if export < 0 || start < 0 {
		t.Fatalf("no export or no start:\n%s", out)
	}
	if start < export {
		t.Errorf("app (devopsy.data.stop=always) was started before db was exported: the dump does not match the files the child gets:\n%s", out)
	}
}

// The reflink check and the space check run before the parent's services
// stop, as a snapshot's take does (snapshotCheck, takeCopy): only the
// clone belongs between the stop and the start, whose downtime must stay
// seconds whatever the size, and a du over the data set can take minutes
// for a warning.
func TestCloneChecksBeforeTheStop(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run([]string{"FAKE_REFLINK=1"}, "@dev", "--release")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(out, "the data clones by reflinks") {
		t.Fatalf("first release (%d):\n%s", code, out)
	}
	_, stop := logLineAt(docker, "shop-prod ", " stop db")
	_, start := logLineAt(docker, "shop-prod ", " start db")
	if stop < 0 || start < stop {
		t.Fatalf("no stop and start of db on the parent:\n%s", docker)
	}
	down := docker[stop:start]
	if !strings.Contains(down, "-clone ") {
		t.Fatalf("the clone not made while db was stopped:\n%s", down)
	}
	if strings.Contains(down, " du -sk ") {
		t.Errorf("du over the whole data set ran while the parent's services were stopped (downtime for a warning):\n%s", down)
	}
	if strings.Contains(down, "-reflink ") {
		t.Errorf("the reflink check ran while the parent's services were stopped, not before:\n%s", down)
	}
}

// The copy into a child by reflinks (shareInto) is cp -a as root on the
// host, where bind mounts are not nodev, so it leaves out FIFOs, sockets
// and device nodes, as a sync's rsync does (--no-D) and an import and a
// snapshot's tars refuse. Run here as devopsy runs it (shareScript, the
// parent's and the child's data this test's directories). A FIFO stands
// for every special file: it needs no root and no reflink.
func TestShareLeavesSpecialFiles(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("GNU cp (--reflink) runs the script")
	}
	if out, err := exec.Command("cp", "--version").Output(); err != nil || !strings.Contains(string(out), "GNU") {
		t.Skip("GNU cp runs the script")
	}
	tmp := t.TempDir()
	src, dst := filepath.Join(tmp, "src"), filepath.Join(tmp, "dst")
	for _, d := range []string{src, dst} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := syscall.Mkfifo(filepath.Join(src, "pipe"), 0o666); err != nil {
		t.Fatal(err)
	}
	// Refusing (a failure, the entry then rsynced) or leaving none out
	// alike keep it out of the child's entry.
	_ = exec.Command("sh", "-c", shareScript, "sh", src, dst).Run()
	if info, err := os.Lstat(filepath.Join(dst, "pipe")); err == nil && info.Mode()&os.ModeNamedPipe != 0 {
		t.Errorf("the share made the parent's FIFO in the child's entry (an rsync's --no-D never does)")
	}
}

// Where the child's mnt does not clone the parent's (ext4, or another
// filesystem), a service labeled devopsy.data.copy.reflink=true is
// rsynced, said once, after one check against the child's mnt: nothing
// empties the child's entry first, which would make every resync a full
// copy and show a running child an empty entry.
func TestShareChecksBeforeEmptying(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	s.logs()
	write(t, s.log+".shop-dev", "FAKE_APP_REFLINK=true\nFAKE_APP_VOLUME=uploads\n", 0o644)
	write(t, s.log+".shop-prod", "FAKE_APP_VOLUME=uploads\n", 0o644)
	if err := os.MkdirAll(filepath.Join(s.log+".vol", "shop-prod_uploads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.log+".vol", "shop-dev_uploads"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No FAKE_REFLINK: nothing clones on this server.
	out, code := s.run(nil, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code != 0 {
		t.Fatalf("sync (%d):\n%s\n%s", code, out, docker)
	}
	// The first that will not clone (the fake's exit 4) stops the rest:
	// one try, said once.
	if n := strings.Count(docker, "-share-"); n != 1 {
		t.Errorf("%d clones tried where the first found no reflinks:\n%s", n, docker)
	}
	if n := strings.Count(out, "no reflinks between the parent's data and the child's"); n != 1 {
		t.Errorf("said %d times, not once:\n%s", n, out)
	}
	// And the script itself empties nothing where its file does not
	// clone: run here, on a filesystem without reflinks.
	if runtime.GOOS != "linux" {
		return
	}
	tmp := t.TempDir()
	src, dst := filepath.Join(tmp, "src"), filepath.Join(tmp, "dst")
	write(t, filepath.Join(src, "file.txt"), "parent's\n", 0o644)
	write(t, filepath.Join(dst, "keep.txt"), "child's\n", 0o644)
	if exec.Command("cp", "--reflink=always", filepath.Join(src, "file.txt"), filepath.Join(tmp, "probe")).Run() == nil {
		t.Skip("this filesystem clones by reflinks")
	}
	err := exec.Command("sh", "-c", shareScript, "sh", src, dst).Run()
	if exitCode(err) != 4 || !fileExists(filepath.Join(dst, "keep.txt")) {
		t.Errorf("the script without reflinks: %v, the child's file kept %v", err, fileExists(filepath.Join(dst, "keep.txt")))
	}
}

// A child's entry that is a symbolic link (an entry nobody owns may be
// one: the data on another disk, or uploads shared with another
// environment) is left to the rsync, which mounts the child's mnt whole
// and never leaves it: shareInto would mount it by its path, which Docker
// resolves on the host, and empty its target as root. An import, a restore
// and the owners' step (makeEntry) refuse such an entry too.
func TestShareLeavesLinkedEntry(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	s.logs()
	write(t, s.log+".shop-dev", "FAKE_APP_REFLINK=true\n", 0o644)
	elsewhere := filepath.Join(t.TempDir(), "uploads")
	write(t, filepath.Join(elsewhere, "keep.txt"), "not dev's\n", 0o644)
	entry := filepath.Join(s.mnt("dev"), "storage")
	if err := os.RemoveAll(entry); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, entry); err != nil {
		t.Fatal(err)
	}
	out, _ := s.run([]string{"FAKE_REFLINK=1"}, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	for _, line := range strings.Split(docker, "\n") {
		if strings.Contains(line, "-share-") && strings.Contains(line, "source="+entry+",target=/d") {
			t.Errorf("a root container emptied and wrote the child's entry, a link to %s, by its path (Docker follows it on the host):\n%s\n%s", elsewhere, line, out)
		}
	}
	if !strings.Contains(out, "rsynced, not shared (devopsy.data.copy.reflink)") {
		t.Errorf("the linked entry not left to the rsync, said:\n%s", out)
	}
}
