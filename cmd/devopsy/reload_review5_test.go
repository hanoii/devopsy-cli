package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Bug (review 5): --vars set and --inherit on an environment whose
// activation was cut off past its switch (ActivatingFile, nothing holding
// the lock) name --reload as what applies the change, and the reload
// refuses there now (cutRefusal), as reviews 1, 3 and 4 found for a plain
// directory, an unreleased and a marked environment: varsApply tells
// plain, first, marked and the images check's exit, never a cut
// activation, and runInherit says --reload whatever the child is. The
// follow-up should be what finishes the cut activation (--release here),
// as the reload's refusal and --status say.
func TestReview5VarsHintCut(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "config.yaml"), strings.Replace(readString(filepath.Join(s.dot, "config.yaml")), "  prod: {}\n", "  prod: {inherit: [SITE_NAME]}\n", 1), 0o644)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("dev's release (%d):\n%s", code, out)
	}
	s.logs()
	for _, env := range []string{"prod", "dev"} {
		base := filepath.Join(s.root, "shop", env)
		id := strings.TrimPrefix(s.current(env), "releases/")
		write(t, filepath.Join(base, remote.ActivatingFile), id+"\trelease\t\t\n", 0o644)
	}
	reload, rcode := s.run(nil, "@prod", "--reload")
	if rcode == 0 || !strings.Contains(reload, "cut off past its switch") {
		t.Fatalf("a cut release's reload went through (%d):\n%s", rcode, reload)
	}
	out, code := s.run([]string{"SITE_NAME=shop"}, "@prod", "--vars", "set", "SITE_NAME")
	if code != 0 {
		t.Fatalf("--vars set (%d):\n%s", code, out)
	}
	if strings.Contains(out, "--reload") {
		t.Errorf("--vars set on a cut release names --reload, which refuses there:\n%s", out)
	}
	write(t, filepath.Join(s.root, "shop", "prod", "shared", ".env"), "SITE_NAME=shop\n", 0o600)
	out, code = s.run(nil, "@dev", "--inherit")
	if code != 0 {
		t.Fatalf("--inherit (%d):\n%s", code, out)
	}
	if strings.Contains(out, "--reload") {
		t.Errorf("--inherit into a cut child names --reload, which refuses there:\n%s", out)
	}
}

// Bug (review 5): devopsy --reload typed in a release on a server checks
// its reload: step's command as one run locally without an address
// (cli.Bare in runLocalReload), so a step whose command runs only on a
// target (## Run: target) is refused there ("runs only on a target:
// devopsy @<target> apply"), though that is where it is: the release's
// own --reload (--release-steps reload run) runs it, a release checks it
// as a remote step (checkStepScopes, cli.OnServer), and where a command
// runs is decided by the directory (cli.InRelease).
func TestReview5LocalReloadInReleaseRunTarget(t *testing.T) {
	t.Parallel()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(tmp, "releases", "20261027000000")
	dir := filepath.Join(rel, remote.CodeDir, ".devopsy")
	write(t, filepath.Join(dir, "compose.yaml"), "services: {app: {image: x}}\n", 0o644)
	write(t, filepath.Join(dir, "config.yaml"), "project: app\ndefaults: {release: deploy}\nenvironments:\n  prod: {reload: apply}\n", 0o644)
	write(t, filepath.Join(dir, "commands", "apply"), "#!/bin/sh\n## Description: a\n## Run: target\necho APPLIED\n", 0o755)
	write(t, filepath.Join(rel, remote.TargetEnvFile), "COMPOSE_PROJECT_NAME='app-prod'\nDEVOPSY_ENVIRONMENT='prod'\n", 0o644)
	if out, code := runDevopsy(t, filepath.Join(rel, remote.CodeDir), nil, "--reload"); code != 0 || !strings.Contains(out, "APPLIED") {
		t.Errorf("--reload in a release, its step a target's command (%d):\n%s", code, out)
	}
}
