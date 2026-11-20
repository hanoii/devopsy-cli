package remote

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A level's listing reads the configs (their secrets:) of the environments each_env
// lists, no other directory's. BUG (review 2, fable): VarsReadScript keeps a loop of its
// own over "$base"/*/current and "$base"/*/*/current, the pattern each_env replaced in
// the trace and the check, so it still takes what a cut removal left (.rm.) and a
// directory inside an environment (its shared/current) for environments: their
// secrets: mask the listing.
func TestVarsReadLevelListsEachEnv(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	base := filepath.Join(root, "app")
	config := func(dir, name string) {
		write(t, filepath.Join(dir, "releases", "1", CodeDir, ".devopsy", "config.yaml"), "project: app\nsecrets: ["+name+"]\n")
		if err := os.Symlink(filepath.Join("releases", "1", CodeDir), filepath.Join(dir, "current")); err != nil {
			t.Fatal(err)
		}
	}
	config(filepath.Join(base, "prod"), "OF_PROD")
	config(filepath.Join(base, "qa.rm.12"), "OF_A_CUT_REMOVAL")
	write(t, filepath.Join(base, "prod", "shared", "current", ".devopsy", "config.yaml"), "project: app\nsecrets: [OF_SHARED]\n")
	write(t, filepath.Join(base, ".env"), "A='1'\n")
	level := &Level{Name: "project", Dir: "app", Link: "project.env"}
	out, err := runVarsRaw(t, root, VarsReadScript(&Target{Name: "prod", Host: "h", Path: "app/prod"}, level), "")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "OF_PROD") {
		t.Fatalf("prod's config not read:\n%s", out)
	}
	for _, name := range []string{"OF_A_CUT_REMOVAL", "OF_SHARED"} {
		if strings.Contains(out, name) {
			t.Errorf("the config of what is no environment read (%s):\n%s", name, out)
		}
	}
}
