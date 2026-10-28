package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/cli"
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
