package skill_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/skill"
)

func TestContent(t *testing.T) {
	t.Parallel()
	c := skill.Content("v1.2.3", false)
	for _, want := range []string{"---\nname: devopsy\n", "\ndevopsy: v1.2.3\n", "devopsy `v1.2.3`"} {
		if !strings.Contains(c, want) {
			t.Errorf("content lacks %q", want)
		}
	}
	if strings.Contains(c, "{{") {
		t.Error("a placeholder left")
	}
	if !skill.Written([]byte(c)) {
		t.Error("the content is not recognized as devopsy's")
	}
	s := skill.Content("v1.2.3", true)
	if !strings.Contains(s, "devopsy: stub\n") || !strings.Contains(s, "devopsy --ai-skill install") || !skill.Written([]byte(s)) {
		t.Errorf("stub:\n%s", s)
	}
}

// Every flag the skill names is one of devopsy's, and every devopsy.*
// label one the code reads: a rename that leaves the skill behind fails
// here.
func TestContentNamesWhatExists(t *testing.T) {
	t.Parallel()
	c := skill.Content("v0", false)
	var flags []string
	for _, f := range append(cli.LocalFlags(), cli.RemoteFlags()...) {
		flags = append(flags, f.Name)
	}
	// Words a project command or compose takes, named with `--` in the
	// text: not devopsy's flags.
	notFlags := []string{"--restricted", "--pull", "--rm", "--from", "--yes", "--accept-image", "--resolved", "--show", "--ip", "--out", "--purge", "--global", "--stub"}
	for _, m := range regexp.MustCompile("`(--[a-z-]+)").FindAllStringSubmatch(c, -1) {
		if !slices.Contains(flags, m[1]) && !slices.Contains(notFlags, m[1]) {
			t.Errorf("the skill names %s, not a flag of devopsy's", m[1])
		}
	}

	var labels []string
	for _, dir := range []string{"../cli", "../../cmd/devopsy", "../remote", "../images"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range regexp.MustCompile(`"(devopsy\.[a-z.]+)`).FindAllStringSubmatch(string(src), -1) {
				labels = append(labels, strings.TrimSuffix(m[1], "."))
			}
		}
	}
	for _, m := range regexp.MustCompile(`(devopsy\.[a-z]+(?:\.[a-z]+)*)`).FindAllStringSubmatch(c, -1) {
		if !slices.Contains(labels, m[1]) {
			t.Errorf("the skill names label %s, which the code never reads", m[1])
		}
	}
}

func TestInstall(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path, notes, err := skill.Install(dir, "v1", false)
	if err != nil || len(notes) != 0 || path != filepath.Join(dir, skill.Dir, "SKILL.md") {
		t.Fatalf("install: %q %v %v", path, notes, err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != skill.Content("v1", false) {
		t.Error("the file is not the content")
	}
	link := filepath.Join(dir, skill.ClaudeDir)
	if got, err := os.Readlink(link); err != nil || got != "../../"+skill.Dir {
		t.Errorf("link: %q %v", got, err)
	}
	// Through the link, the file is readable.
	if through, _ := os.ReadFile(filepath.Join(link, "SKILL.md")); string(through) != string(data) {
		t.Error("the link does not reach the file")
	}

	// Again, another version: replaced, the link kept.
	if _, notes, err := skill.Install(dir, "v2", true); err != nil || len(notes) != 0 {
		t.Fatalf("reinstall: %v %v", notes, err)
	}
	if data, _ := os.ReadFile(path); string(data) != skill.Content("v2", true) {
		t.Error("the second install did not replace the file")
	}

	// A file devopsy did not write is refused, and nothing changes.
	own := t.TempDir()
	ownPath := filepath.Join(own, skill.Dir, "SKILL.md")
	os.MkdirAll(filepath.Dir(ownPath), 0o755)
	os.WriteFile(ownPath, []byte("---\nname: devopsy\n---\nmine\n"), 0o644)
	if _, _, err := skill.Install(own, "v1", false); err == nil || !strings.Contains(err.Error(), "not written by devopsy") {
		t.Errorf("own file: %v", err)
	}
	if data, _ := os.ReadFile(ownPath); string(data) != "---\nname: devopsy\n---\nmine\n" {
		t.Error("the own file changed")
	}
	if _, err := os.Lstat(filepath.Join(own, skill.ClaudeDir)); err == nil {
		t.Error("a link was made for a refused install")
	}

	// Claude Code's location already taken: left alone, said.
	taken := t.TempDir()
	os.MkdirAll(filepath.Join(taken, skill.ClaudeDir), 0o755)
	if _, notes, err := skill.Install(taken, "v1", false); err != nil || len(notes) != 1 || !strings.Contains(notes[0], "not a link") {
		t.Errorf("taken: %v %v", notes, err)
	}
	elsewhere := t.TempDir()
	os.MkdirAll(filepath.Join(elsewhere, ".claude", "skills"), 0o755)
	os.Symlink("/elsewhere", filepath.Join(elsewhere, skill.ClaudeDir))
	if _, notes, err := skill.Install(elsewhere, "v1", false); err != nil || len(notes) != 1 || !strings.Contains(notes[0], "links elsewhere") {
		t.Errorf("elsewhere: %v %v", notes, err)
	}
}
