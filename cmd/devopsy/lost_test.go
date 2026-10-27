package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// A run step that exits 255 (drush does, on a PHP error) failed: the
// release went back, which devopsy must not report as a lost
// connection whose release goes on. Fails on v0.42.0: ssh passes the
// script's status on, and sshLost takes every 255 for ssh's own
// (cmd/devopsy/remote.go:380; sync.go:289 for a sync's run step).
func TestRunStepExit255(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "commands", "deploy"), "#!/bin/sh\n[ -z \"${FAKE_RUN_EXIT:-}\" ] || exit \"$FAKE_RUN_EXIT\"\necho deployed\n", 0o755)
	if out, code := s.run(nil, "@solo", "--release", "--yes"); code != 0 {
		t.Fatalf("first release (%d):\n%s", code, out)
	}
	first := s.current("solo")
	out, code := s.run([]string{"FAKE_RUN_EXIT=255"}, "@solo", "--release", "--yes")
	if code == 0 || s.current("solo") != first {
		t.Fatalf("a failed run step (%d), current %q, was %q:\n%s", code, s.current("solo"), first, out)
	}
	if strings.Contains(out, "connection to @") {
		t.Errorf("a run step exiting 255 reported as a lost connection:\n%s", out)
	}
}

// A session lost on the way (ssh's 255: a network gone, the server
// rebooted) is said by devopsy, as a release's, a sync's and an
// import's are: --export, --destroy, --snapshots export and --snapshots
// import left the operator with ssh's line alone and exit 255, which
// says neither that devopsy's command did not complete nor what may be
// left on the server (an export's services stopped until the server
// notices, a destroy half done, a truncated file on stdout). Found by
// hand (2026-10-27, iptables drops on an OrbStack machine). ssh exits
// 255 too when what it ran on the server was killed outright (SIGKILL,
// the OOM killer), so every one says both, in the same words
// (lostSays), and nothing that holds only for a lost connection (the
// server noticing: after a kill nothing does).
func TestLostSessionSays(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("dev's first release (%d):\n%s", code, out)
	}
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	for _, c := range []struct {
		lost  string
		stdin []byte
		args  []string
	}{
		{"'--export'", nil, []string{"@prod", "--export", "db"}},
		{"'--import'", tarOf(t, map[string]string{"db/ibdata": "rows\n"}), []string{"@prod", "--import", "db", "--yes"}},
		{"exec tar -C", nil, []string{"@prod", "--snapshots", "export", m[1]}},
		{"'receive'", []byte("not a tar"), []string{"@prod", "--snapshots", "import"}},
		{"taking snapshot", nil, []string{"@prod", "--snapshot"}},
		{"'--destroy-data'", nil, []string{"@prod", "--destroy", "--yes"}},
		{"--release-id", nil, []string{"@prod", "--release"}},
		{"--missing-vars", nil, []string{"@prod", "--release"}},
		{"ready for sync in", nil, []string{"@dev", "--sync", "--yes"}},
	} {
		_, errs, code := s.runIO([]string{"FAKE_SSH_LOST=" + c.lost}, c.stdin, c.args...)
		if code != 255 {
			t.Fatalf("%v: the fake lost no session (%d):\n%s", c.args, code, errs)
		}
		if !strings.Contains(errs, "devopsy: error: the connection to @devopsy@one:") || !strings.Contains(errs, ", or its devopsy on the server was killed: ") || strings.Contains(errs, "notices") {
			t.Errorf("%v: a session lost on the way (%d) does not say both a lost connection and a kill:\n%s", c.args, code, errs)
		}
	}
}

// A --destroy whose data step fails after it took the containers and
// volumes down (emptying mnt failed, or its devopsy was killed outright
// as the util container emptied it) said "<base> stays as it was": the
// containers and volumes are gone, mnt may be empty, and nothing on the
// server says so (--status shows the release current with no
// containers, a release goes live on what is left). Found by hand
// (2026-10-27): killed on the server while mnt was emptied, the
// container finished emptying it, and devopsy said it stayed as it was.
func TestDestroyDataFailedSays(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	s.logs()
	out, code := s.run([]string{"FAIL_DESTROY=1"}, "@prod", "--destroy", "--yes")
	docker, _ := s.logs()
	if code == 0 || !strings.Contains(docker, " down --volumes") {
		t.Fatalf("the fake failed no destroy after its down (%d):\n%s\n%s", code, out, docker)
	}
	if strings.Contains(out, "stays as it was") {
		t.Errorf("a destroy that took the containers and volumes down, then failed, says its environment stays as it was:\n%s", out)
	}
}

// A --destroy that stopped halfway, its connection lost or its data step
// failed past its down, says how to finish it as a command to copy ($
// ...), but with words after the command ("devopsy @prod --destroy again
// finishes it", "devopsy @prod --destroy finishes it"): copied, they are
// usage errors (--destroy takes --yes and --purge alone). Every other fix
// after report.CmdMark is the command alone.
func TestDestroyHalfwayFixCopies(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	for name, env := range map[string]string{
		"its connection lost":            "FAKE_SSH_LOST=--destroy-data",
		"its data step failed past down": "FAIL_DESTROY=1",
	} {
		_, errs, code := s.runIO([]string{env}, nil, "@prod", "--destroy", "--yes")
		if code == 0 {
			t.Fatalf("%s: the destroy went through:\n%s", name, errs)
		}
		fixes := 0
		for _, line := range strings.Split(errs, "\n") {
			_, cmd, ok := strings.Cut(line, report.Bar+report.CmdMark)
			if !ok {
				continue
			}
			fixes++
			words := strings.Fields(cmd)
			if len(words) == 0 || words[0] != "devopsy" {
				t.Errorf("%s: the fix %q is no devopsy command", name, cmd)
				continue
			}
			for _, w := range words[1:] {
				if !strings.HasPrefix(w, "@") && !strings.HasPrefix(w, "-") {
					t.Errorf("%s: the fix %q has words after its command, which copied is a usage error:\n%s", name, cmd, errs)
					break
				}
			}
		}
		if fixes == 0 {
			t.Errorf("%s: no command to copy shown:\n%s", name, errs)
		}
	}
}

// --import <service> --resolved's second session lost on the way, or
// its devopsy on the server killed (ssh's 255 either way), left the
// operator with exit 255 and not a word: whether the import was
// resolved, what runs, that the mark may stay. Found by hand (OrbStack,
// 2026-10-27: the server's devopsy --import --resolved SIGKILLed during
// its owners' step; the mark and the record stayed, the services down).
func TestLostResolvedSays(t *testing.T) {
	t.Parallel()
	s := newImportSetup(t)
	if _, out, code := s.runIO(append(s.env, "FAKE_FAIL_IMPORT=1"), []byte("rows\n"), "@prod", "--import", "db", "--yes"); code == 0 || !s.mark().IsImport() {
		t.Fatalf("a failing import (%d):\n%s", code, out)
	}
	_, errs, code := s.runIO([]string{"FAKE_SSH_LOST='--resolved'"}, nil, "@prod", "--import", "db", "--resolved", "--yes")
	if code != 255 {
		t.Fatalf("the fake lost no session (%d):\n%s", code, errs)
	}
	if !strings.Contains(errs, "devopsy: error:") || !strings.Contains(errs, report.CmdMark+"devopsy @devopsy@one:prod --import db --resolved") {
		t.Errorf("a --resolved whose session was lost (%d) said nothing of devopsy's:\n%s", code, errs)
	}
	// Its devopsy killed as it starts the record: the mark stays, said.
	_, errs, code = s.runIO([]string{"FAKE_KILL_START=1"}, nil, "@prod", "--import", "db", "--resolved", "--yes")
	if code == 0 || !s.mark().IsImport() {
		t.Fatalf("the fake killed no --resolved (%d):\n%s", code, errs)
	}
	if !strings.Contains(errs, "was killed") || !strings.Contains(errs, report.CmdMark+"devopsy @devopsy@one:prod --import db --resolved") {
		t.Errorf("a --resolved whose devopsy was killed (%d) says neither so nor how to finish it:\n%s", code, errs)
	}
	if _, errs, code = s.runIO(nil, nil, "@prod", "--import", "db", "--resolved", "--yes"); code != 0 || s.mark() != nil {
		t.Errorf("the --resolved again did not finish it (%d):\n%s", code, errs)
	}
}

// A --destroy whose devopsy --destroy-data is killed outright on the
// server after its down (SIGKILL, the OOM killer, as the util container
// empties mnt) said only "<base> is not removed": the containers,
// volumes and built images gone, mnt maybe emptied, the release still
// current, and no word that it stopped halfway nor how to finish it, as
// a lost session's and a failed data step's say. Found by hand
// (OrbStack, 2026-10-27: devopsy --destroy-data SIGKILLed right after
// compose removed the network).
func TestDestroyDataKilledSays(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	s.logs()
	_, errs, code := s.runIO([]string{"FAKE_KILL_DESTROY=1"}, nil, "@prod", "--destroy", "--yes")
	docker, _ := s.logs()
	if code == 0 || !strings.Contains(docker, " down --volumes") {
		t.Fatalf("the fake killed no destroy after its down (%d):\n%s\n%s", code, errs, docker)
	}
	if !strings.Contains(errs, "halfway") || !strings.Contains(errs, report.Bar+report.CmdMark+"devopsy @devopsy@one:prod --destroy") {
		t.Errorf("a destroy whose data step was killed past its down says neither that it stopped halfway nor how to finish it (%d):\n%s", code, errs)
	}
}
