package skill_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/skill"
)

// Review (Opus), bug: Install's link for Claude Code is relative
// ("../../.agents/skills/devopsy"), and the kernel resolves it from where
// the link really is. When .claude, or .claude/skills, is itself a link
// (a home directory whose ~/.claude lives in a dotfiles checkout), the
// link lands there and "../.." climbs out of that directory: Claude Code
// gets a dangling link, Install says nothing, and a second Install sees
// the link it expects and stays silent too.
func TestReviewOpusInstallThroughLinkedClaudeDir(t *testing.T) {
	t.Parallel()
	for _, linked := range []string{".claude", ".claude/skills"} {
		t.Run(linked, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			real := filepath.Join(t.TempDir(), "dotfiles", "claude")
			if err := os.MkdirAll(real, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, linked)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, filepath.Join(dir, linked)); err != nil {
				t.Fatal(err)
			}
			for i := range 2 {
				path, notes, err := skill.Install(dir, "v1", false)
				if err != nil {
					t.Fatalf("install %d: %v", i, err)
				}
				want, _ := os.ReadFile(path)
				got, err := os.ReadFile(filepath.Join(dir, skill.ClaudeDir, "SKILL.md"))
				if err != nil || string(got) != string(want) {
					t.Errorf("install %d: Claude Code's location does not reach the skill (%v), notes %q", i, err, notes)
				}
			}
		})
	}
}

// Review (Opus), bug: Written reads the frontmatter with "\n" line ends
// only, so a copy devopsy wrote, checked out with CRLF line ends
// (core.autocrlf=true, an editor), is taken for the user's own file:
// Install refuses to refresh it, "not written by devopsy".
func TestReviewOpusWrittenWithCRLF(t *testing.T) {
	t.Parallel()
	crlf := strings.ReplaceAll(skill.Content("v1", false), "\n", "\r\n")
	if !skill.Written([]byte(crlf)) {
		t.Error("devopsy's own copy, with CRLF line ends, is not recognized")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, skill.Dir, "SKILL.md")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, []byte(crlf), 0o644)
	if _, _, err := skill.Install(dir, "v2", false); err != nil {
		t.Errorf("refreshing devopsy's own copy with CRLF line ends: %v", err)
	}
}
