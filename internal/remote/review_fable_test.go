package remote

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// reviewFableLevel runs a project's --vars set over a project with one environment, prod,
// and whatever more makes, and returns what it printed.
func reviewFableLevel(t *testing.T, more func(base string)) string {
	t.Helper()
	root, bin := t.TempDir(), t.TempDir()
	base := filepath.Join(root, "app")
	write(t, filepath.Join(base, "prod", "releases", "1", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
	if err := os.Symlink(filepath.Join("releases", "1", CodeDir), filepath.Join(base, "prod", "current")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(base, "prod", ".lock"), "")
	write(t, filepath.Join(base, ".env"), "A='1'\n")
	more(base)
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n")
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\n"+sayCase()+`case "$1" in
  --stdin-guard) exec cat ;;
  --env-trace) echo traced; exit 0 ;;
  --prepare-release) exit 0 ;;
  --check-images) cat >/dev/null; exit 0 ;;
esac
printf 'root=%s\nimages=%s\nkeep=5\nmax_keep=5\n' "$FAKE_ROOT" "$FAKE_ROOT"
`)
	for _, f := range []string{"flock", "devopsy"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	level := &Level{Name: "project", Dir: "app", Link: "project.env"}
	cmd := exec.Command("sh", "-c", VarsSetScript(&Target{Name: "prod", Path: "app/prod"}, level, []string{"NEW"}))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}
	cmd.Stdin = strings.NewReader(GatedInput("NEW='1'\n"))
	var stdout strings.Builder
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v\n%s", err, stdout.String())
	}
	return stdout.String()
}

// BUG (review, fable): varsTrace's loop over a level's environments is varsApply's
// written a second time, and only varsApply's skips what a cut removal left
// (<environment>.rm.<pid>): the edit's levels lines still trace the half-removed
// environment.
func TestReviewFableTraceSkipsCutRemoval(t *testing.T) {
	t.Parallel()
	out := reviewFableLevel(t, func(base string) {
		write(t, filepath.Join(base, "prod.rm.12", "releases", "1", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
		if err := os.Symlink(filepath.Join("releases", "1", CodeDir), filepath.Join(base, "prod.rm.12", "current")); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(base, "prod.rm.12", ".lock"), "")
	})
	if _, applied := ParseVarsApplied(out); fmt.Sprint(applied) != fmt.Sprint([]Applied{{Dir: "prod", Word: "0"}}) {
		t.Errorf("applied %+v", applied)
	}
	if traces := ParseVarsTraces(out); len(traces) != 1 || traces["prod"] != "traced" {
		t.Errorf("traces of %v, want prod's only\n%s", traces, out)
	}
}

// BUG (review, fable): both loops take "$base"/*/*/current for an instance's
// environments, which without instances is a directory inside an environment: shared/ is
// the project's, and a current/ it keeps there (with a .devopsy in it) is traced as the
// environment "prod/shared", and with a .lock beside it checked as one.
func TestReviewFableLevelLoopsOnlyEnvironments(t *testing.T) {
	t.Parallel()
	out := reviewFableLevel(t, func(base string) {
		write(t, filepath.Join(base, "prod", "shared", "current", ".devopsy", "compose.yaml"), "services: {}\n")
		write(t, filepath.Join(base, "prod", "shared", ".lock"), "")
	})
	if _, applied := ParseVarsApplied(out); fmt.Sprint(applied) != fmt.Sprint([]Applied{{Dir: "prod", Word: "0"}}) {
		t.Errorf("applied %+v, want prod's only", applied)
	}
	if traces := ParseVarsTraces(out); len(traces) != 1 || traces["prod"] != "traced" {
		t.Errorf("traces of %v, want prod's only\n%s", traces, out)
	}
}
