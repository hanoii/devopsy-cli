package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Bug (review round 2): a release hint and a restore's and a sync's fixes
// name the target as targetRef, the alias as typed, where a hint to
// release, roll back, sync or restore names remote.HintAddress, the
// alias only with source: (AGENTS.md's Output): --vars set's "to apply
// it:" (sayApplied, applied 3) and --resolved's ways out of a restore's
// or a sync's mark (runRemoteResolved: RestoreFixes, "sync it:",
// ResetFix). Through a user-level alias without source:, the command
// shown refuses to run when copied.
func TestReleaseAndRestoreHintsThroughAliasNameHintAddress(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(filepath.Dir(s.project), "home", "config.yaml"), "releases:\n  root: "+s.root+"\naliases:\n  a-shop: {project: shop, to: \"devopsy@one:prod\"}\n", 0o644)
	s.releaseProd(nil)
	out, code := s.run([]string{"FAKE_DB_IMAGE=mariadb:12"}, "@a-shop", "--vars", "set", "FAKE_DB_IMAGE")
	if code != 0 || !strings.Contains(out, "$ devopsy @devopsy@one:prod --release") || strings.Contains(out, "$ devopsy @a-shop --release") {
		t.Errorf("--vars set's release hint through an alias without source (%d):\n%s", code, out)
	}
	base := filepath.Join(s.root, "shop", "prod")
	write(t, filepath.Join(base, remote.SyncIncompleteFile), remote.IncompleteLine("2026-10-22 10:00:00", "20261020100000", remote.IncompleteRestore, "it was cut"), 0o644)
	out, code = s.run(nil, "@a-shop", "--import", "db", "--resolved", "--yes")
	if code == 0 || !strings.Contains(out, "$ devopsy @devopsy@one:prod --snapshots restore 20261020100000") || strings.Contains(out, "$ devopsy @a-shop --snapshots") {
		t.Errorf("--resolved on a restore's mark through an alias without source (%d):\n%s", code, out)
	}
	write(t, filepath.Join(base, remote.SyncIncompleteFile), remote.IncompleteLine("2026-10-22 10:00:00", "vm1:live", remote.IncompleteCopy, "the copy was cut"), 0o644)
	out, code = s.run(nil, "@a-shop", "--import", "db", "--resolved", "--yes")
	if code == 0 || !strings.Contains(out, "$ devopsy @devopsy@one:prod --sync") || !strings.Contains(out, "$ devopsy @devopsy@one:prod --reset") || strings.Contains(out, "$ devopsy @a-shop --") {
		t.Errorf("--resolved on a sync's mark through an alias without source (%d):\n%s", code, out)
	}
}

// Bug (review round 2): a restore's question names the snapshot that
// would keep the data in its words ("devopsy @<ref> --snapshot first
// keeps it"), a command outside a remedy: unbolded, uncopyable and, with
// no terminal, a detail of the failure where every other command is a
// hint after "$ ".
func TestRestoreQuestionOffersSnapshotAsAHint(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	out, code = s.run(nil, "@prod", "--snapshots", "restore", m[1])
	if code == 0 || !strings.Contains(out, "nothing restored: no terminal to confirm it on") {
		t.Fatalf("restore without a terminal (%d):\n%s", code, out)
	}
	if strings.Contains(out, "--snapshot first keeps it") || !strings.Contains(out, "devopsy: | $ devopsy @devopsy@one:prod --snapshot\n") {
		t.Errorf("the question names --snapshot in its words, not as a hint:\n%s", out)
	}
}

// Bug (review round 2): a sync whose parent has no current release says
// "release it first" with no command, where the scripts' same refusal
// (releaseFirst) and a child's first release offer the parent's release
// as a fix, after "$ ": one refusal, two layouts.
func TestSyncParentNotReleasedOffersItsRelease(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	out, code := s.run(nil, "@dev", "--sync", "--yes")
	if code == 0 || !strings.Contains(out, "the parent @devopsy@one:prod (devopsy@one:shop/prod) has no current release") {
		t.Fatalf("parent not released (%d):\n%s", code, out)
	}
	if !strings.Contains(out, "devopsy: | $ devopsy @devopsy@one:prod --release\n") {
		t.Errorf("the parent's release is not offered as a fix:\n%s", out)
	}
}

// Bug (review round 2): refusals that name the right command in their
// words, outside a remedy, locally: --release and --reload given
// arguments ("devopsy --release (a target's: devopsy @<target>
// --release)"), --environments with no server ("devopsy @<server>
// --environments, or DEVOPSY_SERVER ..."). Each command is a fix, after
// "$ ", as the round-1 fix made --shell-host's.
func TestLocalRefusalsOfferTheirCommandsAsFixes(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	dot := filepath.Join(project, ".devopsy")
	write(t, filepath.Join(dot, "compose.yaml"), "services: {}\n", 0o644)
	write(t, filepath.Join(dot, "config.yaml"), "project: shop\nenvironments:\n  prod:\n    release: deploy\n", 0o644)
	for _, c := range []struct {
		args  []string
		cmds  []string
		words string
	}{
		{[]string{"--release", "x"}, []string{"$ devopsy --release", "$ devopsy @<target> --release"}, "locally: devopsy --release ("},
		{[]string{"--reload", "x"}, []string{"$ devopsy --reload", "$ devopsy @<target> --reload"}, "locally: devopsy --reload ("},
		{[]string{"--environments"}, []string{"$ devopsy @<server> --environments"}, "no server: devopsy @<server>"},
	} {
		out, code := runDevopsy(t, project, nil, c.args...)
		if code == 0 || strings.Contains(out, c.words) {
			t.Errorf("%v: the command in the words (%d):\n%s", c.args, code, out)
			continue
		}
		for _, cmd := range c.cmds {
			if !strings.Contains(out, "devopsy: | "+cmd+"\n") && !strings.Contains(out, ". "+cmd+"\n") {
				t.Errorf("%v: %q is not a fix:\n%s", c.args, cmd, out)
			}
		}
	}
}
