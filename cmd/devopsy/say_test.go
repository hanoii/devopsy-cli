package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// --say formats as everything devopsy says, at the levels scripts use,
// and fails when its write does (a session gone), as echo would, which
// the scripts rely on before a switch; never by SIGPIPE.
func TestSay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if out, code := runDevopsy(t, dir, nil, "--say", "warning", "careful", "why"); code != 0 || out != "devopsy: warning: careful\ndevopsy: | why\n" {
		t.Errorf("warning (%d): %q", code, out)
	}
	// --since says the time since a script's clock, and a clock that is
	// no number (a date that failed) leaves the line as it is.
	if out, code := runDevopsy(t, dir, nil, "--say", "info", "--since", "1", "done", "why"); code != 0 || !regexp.MustCompile(`^devopsy: done \([0-9]+h[0-9]{2}m\)\ndevopsy: \| why\n$`).MatchString(out) {
		t.Errorf("--since: %d %q", code, out)
	}
	if out, code := runDevopsy(t, dir, nil, "--say", "info", "--since", "", "done"); code != 0 || out != "devopsy: done\n" {
		t.Errorf("--since without a clock: %d %q", code, out)
	}
	if out, code := runDevopsy(t, dir, nil, "--say", "info", "--since", "1"); code != 2 || !strings.Contains(out, "usage: devopsy --say") {
		t.Errorf("--since without a message: %d %q", code, out)
	}
	if out, code := runDevopsy(t, dir, nil, "--say", "info", "current is now 1"); code != 0 || out != "devopsy: current is now 1\n" {
		t.Errorf("info (%d): %q", code, out)
	}
	// Fixes and hints come as report lays them out: the first --fix or
	// --hint ends the details, each then its text and its commands.
	if out, code := runDevopsy(t, dir, nil, "--say", "error", "refused", "why", "--hint", "see why:", "devopsy @x --log", "--fix", "sync it again:", "devopsy @x --sync", "--fix", "", "devopsy @x --reset", "devopsy @x --status"); code != 0 ||
		out != "devopsy: error: refused\ndevopsy: | why\ndevopsy: | see why:\ndevopsy: | $ devopsy @x --log\ndevopsy: | to fix it, do one of these:\ndevopsy: | 1. sync it again:\ndevopsy: |    $ devopsy @x --sync\ndevopsy: | 2. $ devopsy @x --reset\ndevopsy: |    $ devopsy @x --status\n" {
		t.Errorf("fixes (%d): %q", code, out)
	}
	if out, code := runDevopsy(t, dir, nil, "--say", "warning", "careful", "--fix"); code != 0 || out != "devopsy: warning: careful\n" {
		t.Errorf("a bare --fix (%d): %q", code, out)
	}
	for _, lv := range []string{"loud", "debug", "trace"} {
		if out, code := runDevopsy(t, dir, nil, "--say", lv, "x"); code != 2 || !strings.Contains(out, "devopsy: error: usage: devopsy --say info|warning|error ") {
			t.Errorf("level %s (%d): %q", lv, code, out)
		}
	}

	// Its stderr gone: exit 1, not a signal.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	cmd := exec.Command(binary, "--say", "info", "nobody reads this")
	cmd.Stderr = w
	err = cmd.Run()
	w.Close()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Errorf("stderr gone: %v", err)
	}
}

// What is likely to take long says its time when it ends, only then, and
// on a line of its own: the fetch, the run step, a sync, a take, a
// restore, a restart, a rollback, an export, an import and the release as
// a whole. A step that fails says its failure and no time.
func TestLongStretchesSayTheirTime(t *testing.T) {
	t.Parallel()
	took := func(t *testing.T, what, out string, lines ...string) {
		t.Helper()
		for _, line := range lines {
			if !regexp.MustCompile(`(?m)^devopsy: ` + line + ` \((<1s|[0-9]+s)\)$`).MatchString(out) {
				t.Errorf("%s: no %q with its time:\n%s", what, line, out)
			}
		}
	}
	s := newSyncSetup(t)
	s.envConfig("prod", "{snapshots: {mirror: true}, rollback: deploy}")
	out, code := s.run(nil, "@prod", "--release")
	if code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	id := strings.TrimPrefix(s.current("prod"), "releases/")
	took(t, "a release", out, "images fetched", "'devopsy deploy' done", "released "+id)

	out, code = s.run(nil, "@dev", "--release-no-sync")
	if code != 0 {
		t.Fatalf("dev's first release (%d):\n%s", code, out)
	}
	if out, code = s.run(nil, "@dev", "--sync", "--yes"); code != 0 {
		t.Fatalf("--sync (%d):\n%s", code, out)
	}
	took(t, "a sync", out, "data synced", "'devopsy deploy' done")

	out, code = s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("--snapshot (%d):\n%s", code, out)
	}
	took(t, "a take", out, "snapshot "+m[1]+" taken")
	if out, code = s.run(nil, "@prod", "--snapshots", "restore", m[1], "--no-code", "--yes"); code != 0 {
		t.Fatalf("--no-code restore (%d):\n%s", code, out)
	}
	took(t, "a --no-code restore", out, "snapshot "+m[1]+"'s data restored", "'devopsy deploy' done")
	if out, code = s.run(nil, "@prod", "--snapshots", "restore", m[1], "--yes"); code != 0 {
		t.Fatalf("restore (%d):\n%s", code, out)
	}
	took(t, "a restore", out, "snapshot "+m[1]+"'s data restored", "'devopsy deploy' done")

	if out, code = s.run(nil, "@prod", "--release"); code != 0 {
		t.Fatalf("second release (%d):\n%s", code, out)
	}
	if out, code = s.run(nil, "@prod", "--rollback", "--yes"); code != 0 {
		t.Fatalf("--rollback (%d):\n%s", code, out)
	}
	took(t, "a rollback", out, "'devopsy deploy' done", "rolled back devopsy@one:prod")

	before := strings.TrimPrefix(s.current("prod"), "releases/")
	out, code = s.run([]string{"FAKE_FAIL_ONCE=" + filepath.Join(s.root, "failed-once")}, "@prod", "--release")
	if code == 0 {
		t.Fatalf("a release with a failing run step went through:\n%s", out)
	}
	// The failed step says no time; the restart's, its own.
	if n := strings.Count(out, "'devopsy deploy' done ("); n != 1 || !strings.Contains(out, "restarting "+before+" with its own run step") || strings.Contains(out, "\ndevopsy: released ") {
		t.Errorf("a failing run step (%d lines with a time):\n%s", n, out)
	}
}

// An export and an import say their time on their lines' stream, never
// in the data on stdout.
func TestExportImportSayTheirTime(t *testing.T) {
	t.Parallel()
	s := newImportSetup(t)
	data, errs, code := s.runIO(s.env, nil, "@prod", "--export", "db")
	if code != 0 || !regexp.MustCompile(`(?m)^devopsy: exported db \((<1s|[0-9]+s)\)$`).MatchString(errs) || strings.Contains(string(data), "exported db") {
		t.Errorf("--export (%d): %q\n%s", code, data, errs)
	}
	_, errs, code = s.runIO(s.env, []byte("rows\n"), "@prod", "--import", "db", "--yes")
	if code != 0 || !regexp.MustCompile(`(?m)^devopsy: imported db \((<1s|[0-9]+s)\)$`).MatchString(errs) {
		t.Errorf("--import (%d):\n%s", code, errs)
	}
}
