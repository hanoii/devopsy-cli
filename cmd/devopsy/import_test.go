package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// importHook is a data capability's import for the fake project: it
// records its pid and its parent's (the server's devopsy), which of the
// release's lock descriptors it got, waits for $FAKE_HOOK_WAIT.go when
// told, then takes stdin, failing with FAKE_FAIL_IMPORT.
const importHook = `#!/bin/sh
# A sync's import on the same server gets none of these: a clean
# environment.
echo $$ >"${FAKE_HOOK_PID:-/dev/null}"
echo $PPID >"${FAKE_HOOK_PPID:-/dev/null}"
for fd in 8 9; do [ ! -e /dev/fd/$fd ] || [ -z "${FAKE_HOOK_PID:-}" ] || echo "$fd" >>"$FAKE_HOOK_PID.fds"; done
if [ -n "${FAKE_HOOK_WAIT:-}" ]; then : >"$FAKE_HOOK_WAIT"; while [ ! -e "$FAKE_HOOK_WAIT.go" ]; do sleep 0.1; done; fi
{ echo "import $1 into $COMPOSE_PROJECT_NAME"; cat; } >"${FAKE_IMPORTED:-/dev/null}"
[ -z "${FAKE_FAIL_IMPORT:-}" ] || exit 4
`

// importSetup is a released prod (no parent) whose db is a stream (never
// stopped: its import needs it running), app labeled import and worker,
// depending on db, without a stop label: an import of db stops app and
// keeps worker running, named.
type importSetup struct {
	*syncSetup
	tmp, base, imported string
	env                 []string
}

func newImportSetup(t *testing.T) *importSetup {
	s := newSyncSetup(t)
	caps := filepath.Join(s.dot, "capabilities", "data")
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho dump of $1\n", 0o755)
	write(t, filepath.Join(caps, "import"), importHook, 0o755)
	s.releaseProd(nil)
	write(t, s.log+".shop-prod", "FAKE_DB=stream\nFAKE_APP=mnt+import\nFAKE_WORKER=none\n", 0o644)
	tmp := t.TempDir()
	is := &importSetup{syncSetup: s, tmp: tmp, base: filepath.Join(s.root, "shop", "prod"), imported: filepath.Join(tmp, "imported")}
	is.env = []string{"FAKE_IMPORTED=" + is.imported, "FAKE_HOOK_PID=" + filepath.Join(tmp, "hook.pid"), "FAKE_HOOK_PPID=" + filepath.Join(tmp, "hook.ppid")}
	return is
}

func (s *importSetup) mark() *remote.Incomplete { return readMark(s.base) }

// wait waits for cmd, up to 20 seconds, then kills it and fails.
func (s *importSetup) wait(cmd *exec.Cmd, errs *bytes.Buffer, what string) error {
	s.t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		s.t.Fatalf("%s never ended:\n%s", what, errs)
		return nil
	}
}

func (s *importSetup) record() string { return readString(filepath.Join(s.base, syncState)) }

// stopped is what the fake docker has stopped in prod.
func (s *importSetup) stopped() []string {
	return strings.Fields(readString(s.log + ".stopped.shop-prod"))
}

// start runs devopsy in the background with stdin, its own session (no
// terminal), stderr collected.
func (s *importSetup) start(extra []string, stdin []byte, args ...string) (*exec.Cmd, *bytes.Buffer) {
	s.t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir = s.project
	cmd.Env = append(append(append([]string{}, s.env...), s.syncSetup.env...), extra...)
	cmd.Stdin = bytes.NewReader(stdin)
	var errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &errs, &errs
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// What devopsy started may outlive it (killed, its server side goes
	// on) holding its output.
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		s.t.Fatal(err)
	}
	return cmd, &errs
}

// An import on a target, from its first write: the mark and the record
// exist while it loads; one that fails keeps what it stopped down, the
// imported service and what depends on it too, recorded, with the ways
// out and never --sync (prod has no parent); nothing runs on the data
// (a release refused before its upload, an export warns, another
// service's import refused before stdin is read) until an import of
// that service completes, which starts what the record holds and only
// then removes both.
func TestImportMarkOnTarget(t *testing.T) {
	s := newImportSetup(t)
	wait := filepath.Join(s.tmp, "hook")
	cmd, errs := s.start([]string{"FAKE_HOOK_WAIT=" + wait}, []byte("rows\n"), "@prod", "--import", "db", "--yes")
	waitFile(t, wait, "the import's hook", errs.String)
	if m := s.mark(); !m.IsImport() || m.From != "db" || !strings.Contains(s.record(), "stop app\n") || !strings.Contains(s.record(), "writer ") {
		t.Errorf("while it loads: mark %+v, record %q", m, s.record())
	}
	// The hook holds none of the release's locks.
	if fds := readString(filepath.Join(s.tmp, "hook.pid.fds")); fds != "" {
		t.Errorf("the hook got the release lock's descriptors: %q", fds)
	}
	write(t, wait+".go", "", 0o644)
	if err := s.wait(cmd, errs, "the import"); err != nil || readString(s.imported) != "import db into shop-prod\nrows\n" {
		t.Fatalf("a complete import (%v):\n%s", err, errs)
	}
	if s.mark() != nil || fileExists(filepath.Join(s.base, syncState)) || slices.Contains(s.stopped(), "app") {
		t.Errorf("after a complete import: mark %+v, record %q, stopped %v", s.mark(), s.record(), s.stopped())
	}
	if !strings.Contains(errs.String(), "devopsy: stopping app (import)\n") || !strings.Contains(errs.String(), "worker depends on db and keeps running while it is imported") || strings.Contains(errs.String(), remote.ImportMarker) {
		t.Errorf("a complete import said:\n%s", errs)
	}
	s.logs()

	// A failure from the first write: app (stopped for it), db (imported)
	// and worker (depending on it) stay down, recorded.
	_, out, code := s.runIO(append(s.env, "FAKE_FAIL_IMPORT=1"), []byte("rows\n"), "@prod", "--import", "db", "--yes")
	docker, _ := s.logs()
	if m := s.mark(); code == 0 || !m.IsImport() || m.From != "db" || !strings.Contains(out, "the import of db did not complete: its data may be partial, and app, db, worker stay down") {
		t.Fatalf("a failing import (%d), mark %+v:\n%s", code, m, out)
	}
	if rec := s.record(); rec != "stop app\nstop db\nstop worker\n" {
		t.Errorf("the record: %q", rec)
	}
	if !strings.Contains(docker, " stop db worker\n") || strings.Contains(docker, " start ") {
		t.Errorf("a failing import, docker:\n%s", docker)
	}
	if log := readString(filepath.Join(s.base, syncLog)); !strings.Contains(log, "from here the import of db goes on whatever happens to the session") {
		t.Errorf("the server's log of the import: %q", log)
	}

	// Nothing runs on it, said with the import's ways out.
	fixes := []string{"devopsy @devopsy@one:prod --import db < <file>", "devopsy @devopsy@one:prod --import db --resolved"}
	if out, code := s.run(nil, "@prod", "--status"); code != 0 || !strings.Contains(out, "the import of db did not complete") || strings.Contains(out, "--sync") {
		t.Errorf("--status (%d):\n%s", code, out)
	} else {
		for _, f := range fixes {
			if !strings.Contains(out, f) {
				t.Errorf("--status misses %q:\n%s", f, out)
			}
		}
	}
	releases, _ := os.ReadDir(filepath.Join(s.base, "releases"))
	if out, code := s.run(nil, "@prod", "--release"); code == 0 || !strings.Contains(out, "the import of db into @devopsy@one:prod did not complete") || strings.Contains(out, "--sync") || !strings.Contains(out, fixes[1]) {
		t.Errorf("a release (%d):\n%s", code, out)
	}
	if after, _ := os.ReadDir(filepath.Join(s.base, "releases")); len(after) != len(releases) {
		t.Errorf("a refused release left one: %d, %d before", len(after), len(releases))
	}
	if _, errs, code := s.runIO(s.env, nil, "@prod", "--export", "db"); code != 0 || !strings.Contains(errs, "the import of db into this environment did not complete: exporting the data as it is") {
		t.Errorf("--export (%d):\n%s", code, errs)
	}
	os.Remove(s.imported)
	if _, errs, code := s.runIO(s.env, []byte("files\n"), "@prod", "--import", "app", "--yes"); code == 0 || !strings.Contains(errs, "only db can be imported until one does: nothing imported") || fileExists(s.imported) {
		t.Errorf("another service's import (%d):\n%s", code, errs)
	}
	s.logs()

	// One whose restore fails keeps both.
	if _, errs, code := s.runIO(append(s.env, "FAIL_START=shop-prod"), []byte("rows\n"), "@prod", "--import", "db", "--yes"); code == 0 || s.mark() == nil || s.record() == "" {
		t.Errorf("a complete import whose restore fails (%d), mark %+v, record %q:\n%s", code, s.mark(), s.record(), errs)
	}
	// The retry, complete: what the record holds starts (db up alone for
	// its import, never stopped again after it: the earlier import
	// stopped it), then both go.
	_, out, code = s.runIO(s.env, []byte("rows\n"), "@prod", "--import", "db", "--yes")
	docker, _ = s.logs()
	if code != 0 || s.mark() != nil || fileExists(filepath.Join(s.base, syncState)) || readString(s.imported) != "import db into shop-prod\nrows\n" {
		t.Fatalf("the retry (%d), mark %+v, record %q:\n%s", code, s.mark(), s.record(), out)
	}
	if !strings.Contains(docker, "up -d --wait --no-deps db") || !strings.Contains(docker, " start app worker\n") || strings.Contains(out, "stopping db again") || len(s.stopped()) > 0 {
		t.Errorf("the retry, stopped %v:\n%s\n%s", s.stopped(), out, docker)
	}
}

// Before its first write an import changes nothing for good: a bad tar
// or a stop that fails restores the set, the session closed before the
// byte the first write waits for imports nothing, and none of them leaves
// a mark. A devopsy killed between the stop and that byte leaves the
// record, so the next import restores what it stopped.
func TestImportBeforeFirstWrite(t *testing.T) {
	s := newImportSetup(t)
	write(t, s.log+".shop-prod", "FAKE_DB=mnt+own\n", 0o644)
	// app depends on db: both down for db's import, both back.
	if _, out, code := s.runIO(s.env, []byte("not a tar"), "@prod", "--import", "db", "--yes"); code == 0 || !strings.Contains(out, "stdin is not a complete tar") {
		t.Errorf("a bad tar (%d):\n%s", code, out)
	}
	if _, out, code := s.runIO(append(s.env, "FAIL_STOP=shop-prod"), tarOf(t, map[string]string{"db/x": "x"}), "@prod", "--import", "db", "--yes"); code == 0 || !strings.Contains(out, "devopsy: stopping db (own), app (depends on db)") || !strings.Contains(out, "devopsy: starting db app") {
		t.Errorf("a stop that fails (%d):\n%s", code, out)
	}
	if s.mark() != nil || fileExists(filepath.Join(s.base, syncState)) || len(s.stopped()) > 0 {
		t.Errorf("after: mark %+v, record %q, stopped %v", s.mark(), s.record(), s.stopped())
	}

	// A session closed before the byte: the set back, nothing imported.
	write(t, s.log+".shop-prod", "FAKE_DB=stream\nFAKE_APP=mnt+import\n", 0o644)
	prod := filepath.Join(s.base, "current")
	server := exec.Command(binary, "--import", "db", "--yes")
	server.Dir = prod
	server.Env = append(append(append([]string{}, s.syncSetup.env...), s.env...), "PWD="+prod, "COMPOSE_PROJECT_NAME=shop-prod", remote.ImportEndEnv+"=END")
	server.Stdin = strings.NewReader("rows\nEND")
	out, err := server.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "the session ended before the import started: nothing imported") || fileExists(s.imported) || s.mark() != nil || len(s.stopped()) > 0 {
		t.Errorf("no byte (%v), stopped %v:\n%s", err, s.stopped(), out)
	}
	s.logs()

	// Killed between the stop and the byte (here, its stream's up): the
	// record names what it stopped, no mark; the next import restores it.
	up := filepath.Join(s.tmp, "up")
	cmd, errs := s.start([]string{"FAKE_UP_WAIT=" + up}, []byte("rows\n"), "@prod", "--import", "db", "--yes")
	waitFile(t, up, "the import's up", errs.String)
	pid, err := strconv.Atoi(strings.TrimSpace(readString(up)))
	if err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	write(t, up+".go", "", 0o644)
	_ = cmd.Wait()
	if s.mark() != nil || s.record() != "stop app\n" || !slices.Contains(s.stopped(), "app") {
		t.Fatalf("killed before the byte: mark %+v, record %q, stopped %v:\n%s", s.mark(), s.record(), s.stopped(), errs)
	}
	if _, out, code := s.runIO(s.env, []byte("rows\n"), "@prod", "--import", "db", "--yes"); code != 0 || fileExists(filepath.Join(s.base, syncState)) || slices.Contains(s.stopped(), "app") {
		t.Errorf("the next import (%d), record %q, stopped %v:\n%s", code, s.record(), s.stopped(), out)
	}
}

// The operator's side of an import on a target: the server's marker never
// shows; a Ctrl-C before it imports nothing, the set comes back; one
// after it says the import goes on, and it does; a session gone after it
// (devopsy killed) leaves the import to complete on its own.
func TestImportSession(t *testing.T) {
	s := newImportSetup(t)
	// Interrupted while the server stops app, before the first write.
	stop := filepath.Join(s.tmp, "stop")
	cmd, errs := s.start([]string{"FAKE_STOP_WAIT=" + stop}, []byte("rows\n"), "@prod", "--import", "db", "--yes")
	waitFile(t, stop, "the import's stop", errs.String)
	_ = cmd.Process.Signal(syscall.SIGINT)
	time.Sleep(300 * time.Millisecond)
	write(t, stop+".go", "", 0o644)
	_ = s.wait(cmd, errs, "a Ctrl-C before the first write")
	if code := cmd.ProcessState.ExitCode(); code != 130 || !strings.Contains(errs.String(), "interrupted: nothing imported into @devopsy@one:prod") || fileExists(s.imported) || s.mark() != nil || slices.Contains(s.stopped(), "app") {
		t.Errorf("a Ctrl-C before the first write (%d), stopped %v:\n%s", code, s.stopped(), errs)
	}
	os.Remove(stop)
	os.Remove(stop + ".go")

	// Interrupted during the load: it goes on, and completes.
	wait := filepath.Join(s.tmp, "hook")
	cmd, errs = s.start([]string{"FAKE_HOOK_WAIT=" + wait}, []byte("rows\n"), "@prod", "--import", "db", "--yes")
	waitFile(t, wait, "the import's hook", errs.String)
	_ = cmd.Process.Signal(syscall.SIGINT)
	time.Sleep(300 * time.Millisecond)
	write(t, wait+".go", "", 0o644)
	if err := s.wait(cmd, errs, "a Ctrl-C during the load"); err != nil || !strings.Contains(errs.String(), "interrupted: the import of db goes on on @devopsy@one:prod") || readString(s.imported) != "import db into shop-prod\nrows\n" || s.mark() != nil {
		t.Errorf("a Ctrl-C during the load (%v), mark %+v:\n%s", err, s.mark(), errs)
	}
	if strings.Contains(errs.String(), remote.ImportMarker) {
		t.Errorf("the marker shows:\n%s", errs)
	}
	os.Remove(wait)
	os.Remove(wait + ".go")
	os.Remove(s.imported)

	// devopsy killed during the load: the session's end interrupts
	// nothing on the server.
	cmd, errs = s.start([]string{"FAKE_HOOK_WAIT=" + wait}, []byte("rows\n"), "@prod", "--import", "db", "--yes")
	waitFile(t, wait, "the import's hook", errs.String)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	write(t, wait+".go", "", 0o644)
	for deadline := time.Now().Add(10 * time.Second); s.mark() != nil || fileExists(filepath.Join(s.base, remote.SyncImportingFile)); time.Sleep(100 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the import did not complete once its session went: mark %+v, record %q", s.mark(), s.record())
		}
	}
	if readString(s.imported) != "import db into shop-prod\nrows\n" || slices.Contains(s.stopped(), "app") {
		t.Errorf("after its session went: %q, stopped %v", readString(s.imported), s.stopped())
	}
}

// A devopsy killed outright mid-load leaves the mark and the record, its
// hook loading on; the next import of that service ends that hook (on
// Linux, by its process group, recorded) and the project's import
// containers before anything else, and goes on.
func TestImportLeftoverWriter(t *testing.T) {
	s := newImportSetup(t)
	wait := filepath.Join(s.tmp, "hook")
	cmd, errs := s.start([]string{"FAKE_HOOK_WAIT=" + wait}, []byte("rows\n"), "@prod", "--import", "db", "--yes")
	waitFile(t, wait, "the import's hook", errs.String)
	hook, _ := strconv.Atoi(strings.TrimSpace(readString(filepath.Join(s.tmp, "hook.pid"))))
	server, _ := strconv.Atoi(strings.TrimSpace(readString(filepath.Join(s.tmp, "hook.ppid"))))
	defer func() { _ = syscall.Kill(-hook, syscall.SIGKILL) }()
	_ = syscall.Kill(server, syscall.SIGKILL)
	_ = cmd.Wait()
	if !s.mark().IsImport() || !strings.Contains(s.record(), "writer "+strconv.Itoa(hook)+"\n") {
		t.Fatalf("after a kill: mark %+v, record %q:\n%s", s.mark(), s.record(), errs)
	}
	// A recorded group that is another environment's import now (its
	// pid reused after a reboot): left alone.
	other := exec.Command("sh", "-c", "sleep 30; :", "/srv/shop/staging/current/.devopsy/capabilities/data/import")
	other.Env = []string{"COMPOSE_PROJECT_NAME=shop-staging", "PATH=" + os.Getenv("PATH")}
	other.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	otherEnded := make(chan error, 1)
	go func() { otherEnded <- other.Wait() }()
	defer func() { _ = syscall.Kill(-other.Process.Pid, syscall.SIGKILL) }()
	write(t, filepath.Join(s.base, syncState), s.record()+"writer "+strconv.Itoa(other.Process.Pid)+"\n", 0o644)
	write(t, s.log+".shop-prod", "FAKE_DB=stream\nFAKE_APP=mnt+import\nFAKE_WORKER=none\nFAKE_IMPORT_CONTAINERS='devopsy-import-shop-prod-123-extract-0 devopsy-import-shop-prod-123-owners-0 devopsy-import-shop-prod-eu-9-extract-0 devopsy-import-shop-prod-1-9-owners-0'\n", 0o644)
	s.logs()
	os.Remove(wait)
	_, out, code := s.runIO(s.env, []byte("rows\n"), "@prod", "--import", "db", "--yes")
	docker, _ := s.logs()
	if code != 0 || s.mark() != nil || !strings.Contains(docker, "rm -f devopsy-import-shop-prod-123-extract-0 devopsy-import-shop-prod-123-owners-0\n") || strings.Contains(docker, "shop-prod-eu-9") || strings.Contains(docker, "shop-prod-1-9") {
		t.Errorf("the next import (%d), mark %+v:\n%s\n%s", code, s.mark(), out, docker)
	}
	// The killed import's spool (a whole dump, in the environment's
	// directory) goes with the next import of its service: only a sync's
	// finish removed spools, and nothing else ever would.
	if spools, _ := filepath.Glob(filepath.Join(s.base, remote.StreamSpoolPrefix+"*")); len(spools) > 0 {
		t.Errorf("the killed import's spool is left after the next import: %v", spools)
	}
	// Gone, or a zombie nothing reaps (a container's init may not).
	if runtime.GOOS == "linux" {
		if stat, err := os.ReadFile("/proc/" + strconv.Itoa(hook) + "/stat"); err == nil {
			if _, rest, _ := strings.Cut(string(stat), ") "); !strings.HasPrefix(rest, "Z") {
				t.Errorf("the leftover hook still runs:\n%s", out)
			}
		}
	}
	select {
	case <-otherEnded:
		t.Errorf("another environment's import, its group recorded, was ended:\n%s", out)
	default:
	}

	// db down before: brought up for the import, killed with it, it is
	// stopped again and never started by what resolves it.
	write(t, s.log+".shop-prod", "FAKE_DB=stream\nFAKE_APP=mnt+import\nFAKE_WORKER=none\nFAKE_DB_DOWN=1\n", 0o644)
	cmd, errs = s.start([]string{"FAKE_HOOK_WAIT=" + wait}, []byte("rows\n"), "@prod", "--import", "db", "--yes")
	waitFile(t, wait, "the second import's hook", errs.String)
	hook, _ = strconv.Atoi(strings.TrimSpace(readString(filepath.Join(s.tmp, "hook.pid"))))
	server, _ = strconv.Atoi(strings.TrimSpace(readString(filepath.Join(s.tmp, "hook.ppid"))))
	defer func() { _ = syscall.Kill(-hook, syscall.SIGKILL) }()
	_ = syscall.Kill(server, syscall.SIGKILL)
	_ = cmd.Wait()
	if !strings.Contains(s.record(), "writer "+strconv.Itoa(hook)+" down\n") {
		t.Fatalf("a killed import of a db down before: record %q:\n%s", s.record(), errs)
	}
	os.Remove(wait)
	s.logs()
	_, out, code = s.runIO(nil, nil, "@prod", "--import", "db", "--resolved", "--yes")
	docker, _ = s.logs()
	if code != 0 || strings.Contains(docker, " start db") || !slices.Contains(s.stopped(), "db") {
		t.Errorf("resolved after a killed import of a db down before (%d), stopped %v:\n%s\n%s", code, s.stopped(), out, docker)
	}
}

// --import --resolved: the data as an import left it accepted, on a
// target only: refused without a mark, with another service's or a
// sync's, and without --yes away from a terminal (its text naming what
// starts); with it, what the record holds starts, the imported service
// included, and only then do the mark and the record go. One whose
// start fails keeps both, and a second one works.
func TestImportResolved(t *testing.T) {
	s := newImportSetup(t)
	if _, out, code := s.runIO(nil, nil, "@prod", "--import", "db", "--resolved", "--yes"); code == 0 || !strings.Contains(out, "no import of db to resolve on @devopsy@one:prod") {
		t.Errorf("without a mark (%d):\n%s", code, out)
	}
	if _, out, code := s.runIO(nil, nil, "--import", "db", "--resolved"); code == 0 || !strings.Contains(out, "--resolved works on a target only") {
		t.Errorf("locally (%d):\n%s", code, out)
	}
	if _, out, code := s.runIO(append(s.env, "FAKE_FAIL_IMPORT=1"), []byte("rows\n"), "@prod", "--import", "db", "--yes"); code == 0 || !s.mark().IsImport() {
		t.Fatalf("a failing import (%d):\n%s", code, out)
	}
	if _, out, code := s.runIO(nil, nil, "@prod", "--import", "app", "--resolved", "--yes"); code == 0 || !strings.Contains(out, "is db's, not app's: nothing resolved") || !strings.Contains(out, "--import db --resolved") {
		t.Errorf("another service (%d):\n%s", code, out)
	}
	if _, out, code := s.runIO(nil, nil, "@prod", "--import", "db", "--resolved"); code == 0 || !strings.Contains(out, "nothing resolved: no terminal to confirm it on") || !strings.Contains(out, "the import of db into shop/prod did not complete; db was left stopped with app and worker: this starts them on the data the import left") {
		t.Errorf("without --yes (%d):\n%s", code, out)
	}
	s.logs()
	if _, out, code := s.runIO([]string{"FAIL_START=shop-prod"}, nil, "@prod", "--import", "db", "--resolved", "--yes"); code == 0 || !s.mark().IsImport() || s.record() == "" || !strings.Contains(out, "the mark stays") {
		t.Errorf("a start that fails (%d):\n%s", code, out)
	}
	_, out, code := s.runIO(nil, nil, "@prod", "--import", "db", "--resolved", "--yes")
	docker, _ := s.logs()
	if code != 0 || s.mark() != nil || fileExists(filepath.Join(s.base, syncState)) || !strings.Contains(docker, " start app db worker\n") || len(s.stopped()) > 0 {
		t.Errorf("resolved (%d), mark %+v, stopped %v:\n%s\n%s", code, s.mark(), s.stopped(), out, docker)
	}
	// A sync's mark: only a sync.
	write(t, filepath.Join(s.base, remote.SyncIncompleteFile), remote.IncompleteLine("2026-10-22 10:00:00", "vm1:live", remote.IncompleteCopy, "the copy was cut"), 0o644)
	if _, out, code := s.runIO(nil, nil, "@prod", "--import", "db", "--resolved", "--yes"); code == 0 || !strings.Contains(out, "only a sync completes it: nothing resolved") || !strings.Contains(out, "--sync") {
		t.Errorf("a sync's mark (%d):\n%s", code, out)
	}
	if _, out, code := s.runIO(s.env, []byte("rows\n"), "@prod", "--import", "db", "--yes"); code == 0 || !strings.Contains(out, "only a sync completes it: nothing imported") || fileExists(filepath.Join(s.base, syncState)) {
		t.Errorf("an import over a sync's mark (%d):\n%s", code, out)
	}
	if m := s.mark(); m == nil || m.Code != remote.IncompleteCopy {
		t.Errorf("an import removed a sync's mark: %+v", m)
	}
	// An import's mark with the files of a sync's holder killed before it
	// decided (a sync into the marked environment, cut): the data is partly
	// a sync's, which only a sync completes. An import settles them first
	// and refuses; --resolved must not clear the import's mark and start
	// everything over them, with --status then saying a sync was cut and
	// nothing runs on it.
	write(t, filepath.Join(s.base, remote.SyncIncompleteFile), remote.IncompleteLine("2026-10-22 10:00:00", "db", remote.IncompleteImport, "it failed or was cut after its first write: its data may be partial"), 0o644)
	write(t, filepath.Join(s.base, syncState), "stop app\n", 0o644)
	write(t, filepath.Join(s.base, remote.SyncStartedFile), "devopsy@one:live 1.2\nwriter import db 999 running\n", 0o644)
	s.logs()
	_, out, code = s.runIO(nil, nil, "@prod", "--import", "db", "--resolved", "--yes")
	docker, _ = s.logs()
	if code == 0 || s.mark() == nil || !strings.Contains(out, "--sync") || strings.Contains(docker, " start ") {
		t.Errorf("--resolved over a killed holder's sync files (%d), mark %+v:\n%s\n%s", code, s.mark(), out, docker)
	}
}

// --resolved's prompt names what the record holds, which is what starts:
// a service down before the import (brought up for it, stopped again by
// the failure, never recorded) is not among them, and the prompt must
// not say it was left stopped and starts now.
func TestImportResolvedPromptRecord(t *testing.T) {
	s := newImportSetup(t)
	write(t, s.log+".shop-prod", "FAKE_DB=stream\nFAKE_APP=mnt+import\nFAKE_WORKER=none\nFAKE_DB_DOWN=1\n", 0o644)
	if _, out, code := s.runIO(append(s.env, "FAKE_FAIL_IMPORT=1"), []byte("rows\n"), "@prod", "--import", "db", "--yes"); code == 0 || !s.mark().IsImport() || s.record() != "stop app\nstop worker\n" {
		t.Fatalf("a failing import of a db down before (%d), record %q:\n%s", code, s.record(), out)
	}
	_, out, code := s.runIO(nil, nil, "@prod", "--import", "db", "--resolved")
	if code == 0 || !strings.Contains(out, "nothing resolved: no terminal to confirm it on") {
		t.Fatalf("without --yes (%d):\n%s", code, out)
	}
	if strings.Contains(out, "db was left stopped") || !strings.Contains(out, "app and worker") {
		t.Errorf("the prompt claims db was left stopped and starts, but the record holds app and worker only:\n%s", out)
	}
}

// An import whose session ends while its script waits for the release
// lock (Ctrl-C, a drop) changes nothing once the lock comes: like the
// upload's, what the session sent is not acted on (nothing stopped, no
// bounce of the import set), after devopsy said nothing was imported.
func TestImportCutWhileWaitingForLock(t *testing.T) {
	s := newImportSetup(t)
	// A release lock held by another session: flock -n 9 fails and the
	// wait lasts until the busy file goes.
	busy := filepath.Join(s.tmp, "busy")
	write(t, busy, "", 0o644)
	write(t, filepath.Join(filepath.Dir(s.root), "bin", "flock"), "#!/bin/sh\necho \"flock $*\" >> \"$DOCKER_LOG\"\nif [ -e \"$FAKE_LOCK_BUSY\" ]; then\n  case \"$*\" in\n    '-n 9') exit 1 ;;\n    '-w 600 9') while [ -e \"$FAKE_LOCK_BUSY\" ]; do sleep 0.1; done ;;\n  esac\nfi\nexit 0\n", 0o755)
	cmd, errs := s.start([]string{"FAKE_LOCK_BUSY=" + busy}, []byte("rows\n"), "@prod", "--import", "db", "--yes")
	for start := time.Now(); !strings.Contains(errs.String(), "waiting for the release running on"); time.Sleep(50 * time.Millisecond) {
		if time.Since(start) > 10*time.Second {
			t.Fatalf("the import never waited for the lock:\n%s", errs)
		}
	}
	_ = cmd.Process.Signal(syscall.SIGINT)
	for start := time.Now(); !strings.Contains(errs.String(), "interrupted: nothing imported into @devopsy@one:prod"); time.Sleep(50 * time.Millisecond) {
		if time.Since(start) > 10*time.Second {
			t.Fatalf("the interrupt was not taken:\n%s", errs)
		}
	}
	s.logs()
	os.Remove(busy)
	_ = s.wait(cmd, errs, "the interrupted import")
	docker, _ := s.logs()
	if fileExists(s.imported) || s.mark() != nil || strings.Contains(docker, " stop ") || strings.Contains(docker, " up -d ") || strings.Contains(docker, " start ") {
		t.Errorf("after the lock came, the cut import acted on what the session had sent (imported %v, mark %+v):\n%s\n%s", fileExists(s.imported), s.mark(), errs, docker)
	}
}

// A service kept down that Docker would start again on its own (restart:
// always) is said: a reboot would run it on the partial data.
func TestImportRestartAlways(t *testing.T) {
	s := newImportSetup(t)
	write(t, s.log+".shop-prod", "FAKE_DB=stream\nFAKE_APP=mnt+import\nFAKE_DB_RESTART=always\n", 0o644)
	if _, out, code := s.runIO(append(s.env, "FAKE_FAIL_IMPORT=1"), []byte("rows\n"), "@prod", "--import", "db", "--yes"); code == 0 || !strings.Contains(out, "devopsy: warning: db has restart: always: Docker starts it again when its daemon starts (a reboot), on the data as it is") || strings.Contains(out, "app has restart") {
		t.Errorf("restart: always (%d):\n%s", code, out)
	}
}

// What a prompt that replaces or removes an environment's data asks to
// type: its full name, with the instance in a project that has them.
func TestConfirmWord(t *testing.T) {
	for _, c := range []struct{ config, address, want string }{
		{"project: whoami\nenvironments:\n  prod: {}\n", "vm1:prod", "whoami/prod"},
		{"project: shop\ninstances: true\nenvironments:\n  prod: {}\n", "vm1:eu/prod", "shop/eu/prod"},
	} {
		dir := t.TempDir()
		write(t, filepath.Join(dir, ".devopsy", "config.yaml"), c.config, 0o644)
		write(t, filepath.Join(dir, ".devopsy", "compose.yaml"), "services: {}\n", 0o644)
		tg, err := remote.LoadTarget(filepath.Join(dir, ".devopsy"), c.address, func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatal(err)
		}
		if got := confirmWord(tg); got != c.want {
			t.Errorf("%s: %q, want %q", c.address, got, c.want)
		}
	}
}

// A sync into a child whose import did not complete: refused before the
// parent stops anything when the parent does not copy that service (only
// a sync that writes it may clear the mark); one that copies it clears
// the mark and starts what the import kept down, the child's prepare
// ending what the import left first (its containers). A sync cut over it
// leaves the sync's mark, which only a sync clears: an import and
// --resolved refuse, naming --sync.
func TestSyncOverImportMark(t *testing.T) {
	s := newImportSetup(t)
	write(t, s.log+".shop-prod", "FAKE_DB=stream\nFAKE_APP=mnt+import\n", 0o644)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	dev := filepath.Join(s.root, "shop", "dev")
	devMark := func() *remote.Incomplete { return readMark(dev) }
	write(t, s.log+".shop-dev", "FAKE_DB=stream\nFAKE_APP=mnt+import\n", 0o644)
	if _, out, code := s.runIO(append(s.env, "FAKE_FAIL_IMPORT=1"), []byte("rows\n"), "@dev", "--import", "db", "--yes"); code == 0 || !devMark().IsImport() {
		t.Fatalf("a failing import into dev (%d):\n%s", code, out)
	}
	// On a child, a sync is a way out too.
	if out, _ := s.run(nil, "@dev", "--status"); !strings.Contains(out, "devopsy @devopsy@one:dev --sync") {
		t.Errorf("--status of an import-marked child:\n%s", out)
	}
	s.logs()
	write(t, s.log+".shop-prod", "FAKE_DB=none\nFAKE_APP=mnt+import\n", 0o644)
	out, code := s.run(s.env, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code == 0 || !strings.Contains(out, "the import of db into the child did not complete, and the parent does not copy db: a sync completes it only by copying it: nothing changed") || strings.Contains(docker, "shop-prod compose -f "+filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml")+" stop") || !devMark().IsImport() {
		t.Errorf("a parent not copying db (%d):\n%s\n%s", code, out, docker)
	}
	write(t, s.log+".shop-prod", "FAKE_DB=stream\nFAKE_APP=mnt+import\n", 0o644)
	write(t, s.log+".shop-dev", "FAKE_DB=stream\nFAKE_APP=mnt+import\nFAKE_IMPORT_CONTAINERS=devopsy-import-shop-dev-77\n", 0o644)
	out, code = s.run(s.env, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	stopped := strings.Fields(readString(s.log + ".stopped.shop-dev"))
	if code != 0 || devMark() != nil || fileExists(filepath.Join(dev, syncState)) || slices.Contains(stopped, "app") || slices.Contains(stopped, "db") || !strings.Contains(docker, "rm -f devopsy-import-shop-dev-77") {
		t.Errorf("a sync copying db (%d), mark %+v, stopped %v:\n%s\n%s", code, devMark(), stopped, out, docker)
	}

	// Marked again, then a sync cut after it wrote: the sync's mark.
	if _, out, code := s.runIO(append(s.env, "FAKE_FAIL_IMPORT=1"), []byte("rows\n"), "@dev", "--import", "db", "--yes"); code == 0 || !devMark().IsImport() {
		t.Fatalf("a failing import into dev (%d):\n%s", code, out)
	}
	if out, code := s.run(append(s.env, "FAIL_RSYNC=23"), "@dev", "--sync", "--yes"); code == 0 || devMark() == nil || devMark().Code != remote.IncompleteCopy {
		t.Fatalf("a cut sync over an import's mark (%d), mark %+v:\n%s", code, devMark(), out)
	}
	if _, out, code := s.runIO(s.env, []byte("rows\n"), "@dev", "--import", "db", "--yes"); code == 0 || !strings.Contains(out, "only a sync completes it: nothing imported") {
		t.Errorf("an import over the sync's mark (%d):\n%s", code, out)
	}
	if _, out, code := s.runIO(nil, nil, "@dev", "--import", "db", "--resolved", "--yes"); code == 0 || !strings.Contains(out, "--sync") {
		t.Errorf("--resolved over the sync's mark (%d):\n%s", code, out)
	}
}

// A sync over an import's mark that writes nothing (refused: the parent
// does not copy the service) leaves what the import stopped down, and
// only that: the push says nothing changed, so a service the sync's own
// prepare stopped, whose data the import never touched (db, own), runs
// again, as it did before the sync.
func TestSyncRefusedOverImportMark(t *testing.T) {
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	dev := filepath.Join(s.root, "shop", "dev")
	write(t, s.log+".shop-dev", "FAKE_APP=mnt+import\n", 0o644)
	if _, errs, code := s.runIO([]string{"FAIL_TAR=1"}, tarOf(t, map[string]string{"storage/f": "x\n"}), "@dev", "--import", "app", "--yes"); code == 0 || readMark(dev) == nil || readMark(dev).From != "app" {
		t.Fatalf("a failing import of app into dev (%d), mark %+v:\n%s", code, readMark(dev), errs)
	}
	if stopped := strings.Fields(readString(s.log + ".stopped.shop-dev")); slices.Contains(stopped, "db") || !slices.Contains(stopped, "app") {
		t.Fatalf("after the import: stopped %v", stopped)
	}
	write(t, s.log+".shop-prod", "FAKE_APP=none\n", 0o644)
	out, code := s.run(nil, "@dev", "--sync", "--yes")
	stopped := strings.Fields(readString(s.log + ".stopped.shop-dev"))
	if code == 0 || !strings.Contains(out, "the parent does not copy app") || !readMark(dev).IsImport() || slices.Contains(stopped, "db") || !slices.Contains(stopped, "app") {
		t.Errorf("a refused sync over app's import mark (%d), stopped %v:\n%s", code, stopped, out)
	}

	// A sync whose child prepare fails (its hook refuses, before it stops
	// anything) writes nothing either: what the import stopped stays
	// down, as the mark says.
	hook := filepath.Join(dev, "current", ".devopsy", "capabilities", "data", "prepare")
	write(t, hook, "#!/bin/sh\n[ \"$1\" != child ] || { echo 'prepare child refuses'; exit 3; }\n", 0o755)
	out, code = s.run(nil, "@dev", "--sync", "--yes")
	stopped = strings.Fields(readString(s.log + ".stopped.shop-dev"))
	if code == 0 || !strings.Contains(out, "prepare child refuses") || !readMark(dev).IsImport() || !slices.Contains(stopped, "app") {
		t.Errorf("a sync whose prepare failed over app's import mark (%d), stopped %v:\n%s", code, stopped, out)
	}
	os.Remove(hook)

	// The parent copies app, but not all of it: the child's app mounts
	// cache too, which the parent lacks, so what the import half wrote
	// there would stay. Refused, naming it; with it, the sync clears the
	// mark.
	prodMnt := filepath.Join(s.root, "shop", "prod", "shared", "mnt")
	write(t, filepath.Join(prodMnt, "storage", "f"), "prod\n", 0o644)
	write(t, s.log+".shop-prod", "FAKE_APP=mnt+import\n", 0o644)
	write(t, s.log+".shop-dev", "FAKE_APP=mnt+import\nFAKE_APP_MOUNT2=/cache\n", 0o644)
	out, code = s.run(nil, "@dev", "--sync", "--yes")
	if code == 0 || !strings.Contains(out, "the parent does not copy ./mnt/cache, which the child's app mounts: a sync completes it only by copying all of it: nothing changed") || !readMark(dev).IsImport() {
		t.Errorf("a sync copying part of app (%d):\n%s", code, out)
	}
	// The case --reset is the way out of (a half-written entry the
	// parent lacks): the refusal on the marked child says so, as every
	// refusal on a marked child does (remote.ResetFix).
	if !strings.Contains(out, "$ devopsy @devopsy@one:dev --reset") {
		t.Errorf("the refusal of a sync copying part of app does not offer --reset:\n%s", out)
	}
	write(t, filepath.Join(prodMnt, "cache", "c"), "prod\n", 0o644)
	write(t, s.log+".shop-prod", "FAKE_APP=mnt+import\nFAKE_APP_MOUNT2=/cache\n", 0o644)
	if out, code := s.run(nil, "@dev", "--sync", "--yes"); code != 0 || readMark(dev) != nil {
		t.Errorf("a sync copying all of app (%d), mark %+v:\n%s", code, readMark(dev), out)
	}
}

// The data labels are a feature: a server's devopsy without it reads them
// as no label, so what reads them refuses before anything changes, with
// the upgrade line (an export, a sync: the parent's check, before the
// child stops anything; a child's release that syncs, before its
// upload), and nothing else does (a release that does not sync).
func TestDataLabelsOlderServer(t *testing.T) {
	s := newImportSetup(t)
	write(t, filepath.Join(s.dot, "config.yaml"), "project: shop\ndefaults:\n  mode: image\n  release: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n  fresh: {parent: prod}\n", 0o644)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	old := filepath.Join(s.tmp, "old")
	write(t, filepath.Join(old, "devopsy"), "#!/bin/sh\nif [ \"$1\" = --release-settings ]; then "+binary+" --release-settings | sed 's/ "+remote.DataLabelsFeature+" / /'; exit; fi\nexec "+binary+" \"$@\"\n", 0o755)
	var env []string
	for _, kv := range s.syncSetup.env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			kv = "PATH=" + old + ":" + v
		}
		env = append(env, kv)
	}
	run := func(args ...string) (string, int) {
		cmd := exec.Command(binary, args...)
		cmd.Dir = s.project
		cmd.Env = append(env, s.env...)
		cmd.Stdin = strings.NewReader("")
		out, _ := cmd.CombinedOutput()
		return string(out), cmd.ProcessState.ExitCode()
	}
	lacks := "the server's devopsy lacks what this needs (" + remote.DataLabelsFeature
	s.logs()
	for _, args := range [][]string{{"@prod", "--export", "db"}, {"@dev", "--sync", "--yes"}, {"@fresh", "--release"}} {
		out, code := run(args...)
		docker, _ := s.logs()
		if code == 0 || !strings.Contains(out, lacks) || !strings.Contains(out, "--upgrade") || strings.Contains(docker, " stop ") {
			t.Errorf("%v (%d):\n%s\n%s", args, code, out, docker)
		}
	}
	if fileExists(filepath.Join(s.root, "shop", "fresh", "releases")) {
		t.Errorf("a child's first release uploaded")
	}
	if out, code := run("@dev", "--release-no-sync"); code != 0 || strings.Contains(out, "lacks") {
		t.Errorf("a release that does not sync (%d):\n%s", code, out)
	}
	// One lacking only --reset's feature: a reset refuses before anything
	// stops (an older parent would sync without wiping), a sync goes on.
	write(t, filepath.Join(old, "devopsy"), "#!/bin/sh\nif [ \"$1\" = --release-settings ]; then "+binary+" --release-settings | sed 's/ "+remote.ResetFeature+" / /'; exit; fi\nexec "+binary+" \"$@\"\n", 0o755)
	s.logs()
	out, code := run("@dev", "--reset", "--yes")
	docker, _ := s.logs()
	if code == 0 || !strings.Contains(out, "the server's devopsy lacks what this needs ("+remote.ResetFeature) || strings.Contains(docker, " stop ") || strings.Contains(docker, " down ") {
		t.Errorf("a reset on a server without it (%d):\n%s\n%s", code, out, docker)
	}
	if out, _ := run("@dev", "--sync", "--yes"); strings.Contains(out, "lacks") || !strings.Contains(out, "copying @devopsy@one:prod") {
		t.Errorf("a sync on a server without reset:\n%s", out)
	}
}

// A child's first release creates, before its finish hook, the services
// with data and the child side's set by labels: an import app without
// data, and a worker without labels that depends on an own database (up
// creates dependencies, never dependents).
func TestSyncFirstReleaseCreatesSet(t *testing.T) {
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "config.yaml"), "project: shop\ndefaults:\n  mode: image\n  release: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n", 0o644)
	write(t, filepath.Join(s.dot, "capabilities", "data", "finish"), "#!/bin/sh\necho \"finish $1 $2\"\n", 0o755)
	s.releaseProd(nil)
	s.labels("FAKE_APP=-+import\nFAKE_DB=mnt+own\nFAKE_WORKER=none")
	defer s.labels("")
	write(t, s.log+".shop-dev", "FAKE_APP=-+import\nFAKE_DB=mnt+own\nFAKE_WORKER=none\nFAKE_ABSENT='app db worker'\n", 0o644)
	out, code := s.run(nil, "@dev", "--release")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(out, "devopsy: creating app db worker") || strings.Index(out, "devopsy: creating app db worker") > strings.Index(out, "finish child go") || !strings.Contains(docker, " up -d --wait app db worker\n") {
		t.Errorf("first release (%d):\n%s\n%s", code, out, docker)
	}

	// A worker in a profile that is not up (COMPOSE_PROFILES unset): an
	// up naming it starts it all the same, so it is not created, as the
	// run step's up would not: stops leave such a service as it is too.
	write(t, filepath.Join(s.dot, "config.yaml"), "project: shop\ndefaults:\n  mode: image\n  release: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n  fresh: {parent: prod}\n", 0o644)
	write(t, s.log+".shop-fresh", "FAKE_APP=-+import\nFAKE_DB=mnt+own\nFAKE_WORKER=none\nFAKE_WORKER_PROFILE=tools\nFAKE_ABSENT='app db worker'\n", 0o644)
	out, code = s.run(nil, "@fresh", "--release")
	docker, _ = s.logs()
	if code != 0 || !strings.Contains(out, "devopsy: creating app db\n") || !strings.Contains(docker, " up -d --wait app db\n") {
		t.Errorf("first release, worker in an inactive profile (%d):\n%s\n%s", code, out, docker)
	}

	// With its profile up it is created; streamed in an inactive one, it
	// is up for its import only.
	write(t, filepath.Join(s.dot, "config.yaml"), "project: shop\ndefaults:\n  mode: image\n  release: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n  on: {parent: prod}\n  streamed: {parent: prod}\n", 0o644)
	write(t, s.log+".shop-on", "FAKE_APP=-+import\nFAKE_DB=mnt+own\nFAKE_WORKER=none\nFAKE_WORKER_PROFILE=tools\nFAKE_ABSENT='app db worker'\n", 0o644)
	out, code = s.run([]string{"COMPOSE_PROFILES=web,tools"}, "@on", "--release")
	if code != 0 || !strings.Contains(out, "devopsy: creating app db worker\n") {
		t.Errorf("first release, worker in an active profile (%d):\n%s", code, out)
	}
	s.labels("FAKE_APP=-+import\nFAKE_DB=mnt\nFAKE_WORKER=stream")
	write(t, filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "capabilities", "data", "export"), "#!/bin/sh\necho dump\n", 0o755)
	write(t, filepath.Join(s.dot, "capabilities", "data", "import"), "#!/bin/sh\ncat >/dev/null\n", 0o755)
	write(t, s.log+".shop-streamed", "FAKE_APP=-+import\nFAKE_DB=mnt\nFAKE_WORKER=stream\nFAKE_WORKER_PROFILE=tools\nFAKE_ABSENT='app db worker'\n", 0o644)
	out, code = s.run(nil, "@streamed", "--release")
	if code != 0 || !strings.Contains(out, "devopsy: creating app db\n") || !strings.Contains(out, "stopping worker again") {
		t.Errorf("first release, a stream worker in an inactive profile (%d):\n%s", code, out)
	}
}

// The hooks' DEVOPSY_DATA_STOP is what the operation stops on their side
// (by labels, at prepare and finish go), and at finish child partial what
// the record says is down; settle with labels it cannot read (a release
// from before them) warns and goes on, with what the record says.
func TestDataStopEnv(t *testing.T) {
	s := newSyncSetup(t)
	caps := filepath.Join(s.dot, "capabilities", "data")
	write(t, filepath.Join(caps, "prepare"), "#!/bin/sh\necho \"prepare $1 stop=$DEVOPSY_DATA_STOP\"\n", 0o755)
	write(t, filepath.Join(caps, "finish"), "#!/bin/sh\necho \"finish $1 $2 stop=$DEVOPSY_DATA_STOP\"\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.labels("FAKE_APP=mnt+import\nFAKE_DB=mnt")
	defer s.labels("")
	out, code := s.run(nil, "@dev", "--sync", "--yes")
	for _, want := range []string{"prepare child stop=app\n", "prepare parent stop=\n", "finish parent go stop=\n", "finish child go stop=app\n"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("missing %q (%d):\n%s", want, code, out)
		}
	}
	out, code = s.run([]string{"FAIL_RSYNC=23"}, "@dev", "--sync", "--yes")
	if code == 0 || !strings.Contains(out, "finish child partial stop=app\n") {
		t.Errorf("a cut sync (%d):\n%s", code, out)
	}
	// A killed holder's record, labels from before devopsy.data: settled
	// all the same.
	dev := filepath.Join(s.root, "shop", "dev")
	write(t, filepath.Join(dev, remote.SyncStartedFile), "devopsy@one:prod 1.2\n", 0o644)
	write(t, s.log+".shop-dev", "FAKE_APP=old\n", 0o644)
	cmd := exec.Command(binary, "--sync-services", "settle")
	cmd.Dir = filepath.Join(dev, "current")
	cmd.Env = append(append([]string{}, s.env...), "COMPOSE_PROJECT_NAME=shop-dev", "PWD="+cmd.Dir)
	settled, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(settled), "devopsy: warning: settling with the record alone: service app: devopsy.sync=rsync") || !strings.Contains(string(settled), "finish child partial stop=app") || fileExists(filepath.Join(dev, remote.SyncStartedFile)) {
		t.Errorf("settle with old labels (%v):\n%s", err, settled)
	}
}

// A local Ctrl-C while what was stopped starts again: the terminal's
// signal reaches devopsy's group, never the restore's compose, and
// everything comes back.
func TestRestoreSurvivesCtrlC(t *testing.T) {
	s := newImportSetup(t)
	start := filepath.Join(s.tmp, "start")
	write(t, filepath.Join(s.dot, "mnt", "db", "x"), "x\n", 0o644)
	cmd := exec.Command(binary, "--export", "db")
	cmd.Dir = s.project
	cmd.Env = append(append([]string{}, s.syncSetup.env...), "FAKE_DB=mnt+own", "FAKE_START_WAIT="+start)
	var errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &errs
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFile(t, start, "the restore's start", errs.String)
	// As a terminal does: the whole foreground group.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
	time.Sleep(300 * time.Millisecond)
	write(t, start+".go", "", 0o644)
	_ = cmd.Wait()
	if stopped := strings.Fields(readString(s.log + ".stopped.shop")); len(stopped) > 0 {
		t.Errorf("left down after a Ctrl-C during the restore: %v\n%s", stopped, errs.String())
	}
}

// --reset starts a child over from its parent: everything a sync checks
// first, then at the copy's first write every container and volume goes
// and mnt is emptied (entries the labels do not copy included), the copy
// goes in as on a first release, the services with data are created and
// the run step runs; any mark goes once it completes. A parent that
// refuses, or has nothing to copy, wipes nothing; an environment without
// a parent, and no terminal without --yes, refuse.
func TestSyncReset(t *testing.T) {
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	dev := filepath.Join(s.root, "shop", "dev")
	write(t, filepath.Join(s.mnt("dev"), "storage", "old.txt"), "dev\n", 0o644)
	write(t, filepath.Join(s.mnt("dev"), "logs", "access.log"), "dev\n", 0o644)
	kept := func() bool { return fileExists(filepath.Join(s.mnt("dev"), "logs", "access.log")) }
	if out, code := s.run(nil, "@prod", "--reset", "--yes"); code == 0 || !strings.Contains(out, "is not a child") {
		t.Errorf("a reset of an environment without a parent (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--reset"); code == 0 || !strings.Contains(out, "nothing reset") || !strings.Contains(out, "throws away all of @devopsy@one:dev's data") || !kept() {
		t.Errorf("a reset without --yes away from a terminal (%d):\n%s", code, out)
	}
	// The child's db a stream the parent copies raw: refused by the push
	// before its first write, nothing wiped.
	write(t, s.log+".shop-dev", "FAKE_DB=stream\n", 0o644)
	s.logs()
	out, code := s.run(nil, "@dev", "--reset", "--yes")
	docker, _ := s.logs()
	if code == 0 || strings.Contains(docker, " down ") || !kept() {
		t.Errorf("a reset the parent refuses (%d):\n%s\n%s", code, out, docker)
	}
	write(t, s.log+".shop-prod", "FAKE_APP=none\nFAKE_DB=none\n", 0o644)
	os.Remove(s.log + ".shop-dev")
	out, code = s.run(nil, "@dev", "--reset", "--yes")
	docker, _ = s.logs()
	if code == 0 || !strings.Contains(out, "the parent has no data to start the child over from") || strings.Contains(docker, " down ") || !kept() {
		t.Errorf("a reset from a parent without data (%d):\n%s\n%s", code, out, docker)
	}
	os.Remove(s.log + ".shop-prod")

	// Over an import's mark, which it clears.
	if _, errs, code := s.runIO([]string{"FAIL_TAR=1"}, tarOf(t, map[string]string{"storage/f": "x\n"}), "@dev", "--import", "app", "--yes"); code == 0 || !readMark(dev).IsImport() {
		t.Fatalf("a failing import into dev (%d):\n%s", code, errs)
	}
	s.logs()
	out, code = s.run(nil, "@dev", "--reset", "--yes")
	docker, _ = s.logs()
	if code != 0 || readMark(dev) != nil || fileExists(filepath.Join(dev, syncState)) || !strings.Contains(docker, "shop-dev compose -f "+filepath.Join(dev, "current", ".devopsy", "compose.yaml")+" --profile * down --volumes --remove-orphans\n") || !strings.Contains(docker, " up -d --wait app db\n") || !strings.Contains(out, "deploy in shop-dev") || !strings.Contains(out, "devopsy: resetting @devopsy@one:dev: every container and volume goes and mnt is emptied; its shared/.env stays\n") {
		t.Errorf("a reset (%d), mark %+v:\n%s\n%s", code, readMark(dev), out, docker)
	}
	if kept() || fileExists(filepath.Join(s.mnt("dev"), "storage", "old.txt")) || readString(filepath.Join(s.mnt("dev"), "storage", "file.txt")) != "from prod\n" || readString(filepath.Join(s.mnt("dev"), "db", "ibdata")) != "rows\n" {
		t.Errorf("after the reset, dev's mnt is not prod's:\n%s", out)
	}
	// The wipe's emptying of mnt is the sync's own write: its container
	// is named by the sync's id, as its rsync's are, so a holder that
	// decides while it runs (the session cut mid-wipe) ends it
	// (endWriters), and it never deletes what a later sync copies.
	for _, l := range strings.Split(docker, "\n") {
		if strings.Contains(l, " find /mnt -mindepth 1 -delete") && !strings.Contains(l, "--name "+remote.SyncContainerPrefix) {
			t.Errorf("the wipe empties mnt in a container no holder ends: %s", l)
		}
	}
}

// A reset whose parent has none of the entries its labels name (a prod
// that never wrote its storage) copies nothing, so no write ever claims
// the child: it must not report a reset done with the child's data
// untouched (the help: all of it goes).
func TestSyncResetNothingPresent(t *testing.T) {
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	write(t, filepath.Join(s.mnt("dev"), "storage", "old.txt"), "dev\n", 0o644)
	os.RemoveAll(filepath.Join(s.mnt("prod"), "storage"))
	os.RemoveAll(filepath.Join(s.mnt("prod"), "db"))
	s.logs()
	out, code := s.run(nil, "@dev", "--reset", "--yes")
	docker, _ := s.logs()
	if code == 0 && fileExists(filepath.Join(s.mnt("dev"), "storage", "old.txt")) {
		t.Errorf("a reset that wiped nothing succeeded (%d), down: %v:\n%s\n%s", code, strings.Contains(docker, " down "), out, docker)
	}
}
