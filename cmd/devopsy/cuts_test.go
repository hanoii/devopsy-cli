package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// cutRun runs devopsy bounded: a killed script's children may hold its output.
func cutRun(s *syncSetup, extra []string, args ...string) (string, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = s.project
	cmd.Env = append(append([]string{}, s.env...), extra...)
	cmd.Stdin = strings.NewReader("")
	cmd.WaitDelay = 2 * time.Second
	out, _ := cmd.CombinedOutput()
	return string(out), cmd.ProcessState.ExitCode()
}

// What a release snapshot or a restore stopped and could not start again past
// its run step stays in the record, so the next lock-taker starts it: the
// warning alone left it down for good, the mark and the record gone.
func TestLeftDownStartFailsStaysRecorded(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, config string
		args         func(s *syncSetup) []string
	}{
		{"a release snapshot", "{snapshots: {release: true, mirror: true}}", func(*syncSetup) []string { return []string{"--release"} }},
		{"a restore", "", func(s *syncSetup) []string {
			out, code := s.run(nil, "@prod", "--snapshot")
			m := snapshotID.FindStringSubmatch(out)
			if code != 0 || m == nil {
				s.t.Fatalf("take (%d):\n%s", code, out)
			}
			return []string{"--snapshots", "restore", m[1], "--yes"}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newSyncSetup(t)
			if c.config != "" {
				s.envConfig("prod", c.config)
			}
			s.releaseProd(nil)
			s.releaseProd(nil)
			base := filepath.Join(s.root, "shop", "prod")
			args := append([]string{"@prod"}, c.args(s)...)
			out, code := s.run([]string{"FAIL_START=shop-prod"}, args...)
			s.waitReleaseTars()
			if code != 0 || !strings.Contains(out, "they stay stopped") {
				t.Fatalf("the start past the run step failing (%d):\n%s", code, out)
			}
			rec := readString(filepath.Join(base, syncState))
			for _, d := range strings.Fields(readString(s.log + ".stopped.shop-prod")) {
				if !strings.Contains(rec, "stop "+d+"\n") {
					t.Errorf("%s is down, and not in the record %q:\n%s", d, rec, out)
				}
			}
			if out, code := s.run(nil, "@prod", "--release"); code != 0 {
				t.Fatalf("the next release (%d):\n%s", code, out)
			}
			s.waitReleaseTars()
			if left := strings.Fields(readString(s.log + ".stopped.shop-prod")); len(left) != 0 {
				t.Errorf("the next release left %v down", left)
			}
		})
	}
}

// An activation cut at every docker call it makes, by that call failing or by
// its script killed outright, keeps the promises a release makes whatever
// happens: current is a whole release, and the next release settles what is
// left and goes through with nothing left down, marked or cut. Hundreds of
// runs, so only when DEVOPSY_TEST_CUTS is set (e2e.yml).
func TestActivationCuts(t *testing.T) {
	t.Parallel()
	if os.Getenv("DEVOPSY_TEST_CUTS") == "" {
		t.Skip("DEVOPSY_TEST_CUTS is not set")
	}
	how := []string{"fail"}
	if runtime.GOOS == "linux" {
		how = append(how, "kill")
	}
	modes := map[string]struct {
		config string
		env    string
		args   func(s *syncSetup) []string
	}{
		"release":  {"", "prod", func(*syncSetup) []string { return []string{"--release"} }},
		"snapshot": {"{snapshots: {release: true, mirror: true}}", "prod", func(*syncSetup) []string { return []string{"--release"} }},
		"rollback": {"{rollback: deploy}", "prod", func(*syncSetup) []string { return []string{"--rollback", "--yes"} }},
		"sync": {"", "dev", func(s *syncSetup) []string {
			if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
				s.t.Fatalf("dev's release (%d):\n%s", code, out)
			}
			s.logs()
			return []string{"--sync", "--yes"}
		}},
		"release-sync": {"", "dev", func(s *syncSetup) []string {
			if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
				s.t.Fatalf("dev's release (%d):\n%s", code, out)
			}
			s.logs()
			return []string{"--release-sync", "--yes"}
		}},
		// A run step that ups what it runs, as a project's does: a restore's
		// failure past its switch takes the containers down, which only an
		// up brings back.
		"restore": {"{release: up -d}", "prod", func(s *syncSetup) []string {
			out, code := s.run(nil, "@prod", "--snapshot")
			m := snapshotID.FindStringSubmatch(out)
			if code != 0 || m == nil {
				s.t.Fatalf("take (%d):\n%s", code, out)
			}
			return []string{"--snapshots", "restore", m[1], "--yes"}
		}},
	}
	setup := func(t *testing.T, config string) *syncSetup {
		s := newSyncSetup(t)
		if config != "" {
			s.envConfig("prod", config)
		}
		s.releaseProd(nil)
		s.releaseProd(nil)
		return s
	}
	for name, m := range modes {
		// How many calls this mode makes.
		s := setup(t, m.config)
		count := filepath.Join(t.TempDir(), "count")
		if out, code := cutRun(s, []string{"FAKE_CUT_COUNT=" + count}, append([]string{"@" + m.env}, m.args(s)...)...); code != 0 {
			t.Fatalf("%s (%d):\n%s", name, code, out)
		}
		s.waitReleaseTars()
		calls := strings.Count(readString(count), "\n")
		t.Logf("%s: %d calls", name, calls)
		for _, h := range how {
			for at := 1; at <= calls; at++ {
				t.Run(fmt.Sprintf("%s/%s/%d", name, h, at), func(t *testing.T) {
					t.Parallel()
					s := setup(t, m.config)
					args := m.args(s)
					base := filepath.Join(s.root, "shop", m.env)
					was := s.current(m.env)
					count := filepath.Join(t.TempDir(), "count")
					out, code := cutRun(s, []string{"FAKE_CUT_COUNT=" + count, fmt.Sprintf("FAKE_CUT_AT=%d", at), "FAKE_CUT_HOW=" + h}, append([]string{"@" + m.env}, args...)...)
					cutAt := readString(count)
					if n := strings.Split(strings.TrimSpace(cutAt), "\n"); len(n) >= at {
						cutAt = n[at-1]
					}
					s.waitReleaseTars()
					said := func() string {
						docker, _ := s.logs()
						return fmt.Sprintf("the cut (%d) at %q:\n%s\n--- docker since:\n%s", code, cutAt, out, docker)
					}
					// current is a whole release.
					cur := s.current(m.env)
					if cur == "" || !fileExists(filepath.Join(base, cur, "code", ".devopsy")) {
						t.Errorf("current %q (was %q) is no whole release: %s", cur, was, said())
					}
					if st, c := cutRun(s, nil, "@"+m.env, "--status"); c != 0 && !strings.Contains(st, "did not complete") {
						t.Errorf("--status after it (%d):\n%s\n%s", c, st, said())
					}
					// A mark has its own way out, which a release refuses by
					// design, and so has a killed holder's sync, whose files the
					// next lock-taker reads as one that did not complete.
					wayOut := func() {
						t.Helper()
						way := []string{"--sync", "--yes"}
						if mark := readMark(base); mark != nil && mark.IsRestore() {
							way = []string{"--snapshots", "restore", mark.From, "--yes"}
						}
						if again, c := cutRun(s, nil, append([]string{"@" + m.env}, way...)...); c != 0 {
							t.Errorf("the mark's way out %v (%d):\n%s\n%s", way, c, again, said())
						}
					}
					if readMark(base) != nil {
						wayOut()
					}
					nextArgs := []string{"@" + m.env, "--release"}
					if m.env == "dev" {
						nextArgs = []string{"@dev", "--release-no-sync"}
					}
					next, c := cutRun(s, nil, nextArgs...)
					if c != 0 && strings.Contains(next, "did not complete") {
						wayOut()
						next, c = cutRun(s, nil, nextArgs...)
					}
					s.waitReleaseTars()
					if c != 0 {
						t.Errorf("the next release (%d):\n%s\n%s", c, next, said())
						return
					}
					// A sync's parent settles at its own next lock.
					if m.env == "dev" {
						if st, c := cutRun(s, nil, "@prod", "--release"); c != 0 {
							t.Errorf("the parent's next release (%d):\n%s\n%s", c, st, said())
						}
						s.waitReleaseTars()
					}
					for _, p := range []string{"shop-prod", "shop-dev"} {
						if left := strings.Fields(readString(s.log + ".stopped." + p)); len(left) != 0 {
							t.Errorf("after the next release, %s has %v left down:\n%s\n%s", p, left, next, said())
						}
					}
					for _, f := range []string{remote.ActivatingFile, remote.SyncIncompleteFile} {
						if fileExists(filepath.Join(base, f)) {
							t.Errorf("after the next release, %s left: %q\n%s\n%s", f, readString(filepath.Join(base, f)), next, said())
						}
					}
					if rec := readString(filepath.Join(base, syncState)); strings.TrimSpace(rec) != "" {
						t.Errorf("after the next release, the record holds %q\n%s\n%s", rec, next, said())
					}
					if fileExists(filepath.Join(base, s.current(m.env), remote.PendingFile)) {
						t.Errorf("after the next release, current is pending:\n%s\n%s", next, said())
					}
				})
			}
		}
	}
}
