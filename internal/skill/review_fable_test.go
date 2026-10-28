package skill_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/skill"
)

// Bug: when .claude is a plain file, Readlink of .claude/skills/devopsy
// fails with ENOTDIR, which Install takes for "something is there": its
// note says the entry exists and is not a link, while nothing exists at
// that path, and the link is silently never made. The note should say
// what is in the way (.claude is not a directory), or the install fail.
func TestInstallClaudeDirIsAFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".claude"), []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, notes, err := skill.Install(dir, "v1", false)
	if err == nil && path == "" {
		t.Fatal("neither a path nor an error")
	}
	link := filepath.Join(dir, skill.ClaudeDir)
	if _, lerr := os.Lstat(link); lerr == nil {
		t.Fatalf("%s exists under a file", link)
	}
	for _, n := range notes {
		if strings.Contains(n, "exists") {
			t.Errorf("the note says the entry exists, which it does not: %q", n)
		}
	}
	if err == nil && len(notes) == 0 {
		t.Error("the link was not made, and nothing said so")
	}
}
