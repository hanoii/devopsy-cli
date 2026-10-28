package skill_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
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
	// A copy checked out with CRLF line ends (core.autocrlf) is still
	// devopsy's (review: it was refused as the user's own).
	if !skill.Written([]byte(strings.ReplaceAll(c, "\n", "\r\n"))) {
		t.Error("devopsy's own copy, with CRLF line ends, is not recognized")
	}
	s := skill.Content("v1.2.3", true)
	if !strings.Contains(s, "devopsy: stub\n") || !strings.Contains(s, "devopsy --ai-skill install") || !skill.Written([]byte(s)) {
		t.Errorf("stub:\n%s", s)
	}
}

// publicLabels are the labels the README documents for projects: what
// the skill may name. Each must be one the code reads, so the list
// follows a rename, and the skill may name no other (an internal or an
// old one).
var publicLabels = []string{
	"devopsy.data", "devopsy.data.copy", "devopsy.data.stop", "devopsy.data.exclude",
	"devopsy.mnt.owner", "devopsy.role", "devopsy.export", "devopsy.import",
	"devopsy.shell", "devopsy.shell.user",
}

// Every flag the skill names is one of devopsy's (or a word one of them
// takes), every devopsy.* label a public one the code reads, and its
// config.yaml example loads: a rename or a schema change that leaves the
// skill behind fails here.
// Not parallel: t.Setenv for DEVOPSY_HOME, so the example loads without
// the user's own config.
func TestContentNamesWhatExists(t *testing.T) {
	c := skill.Content("v0", false)
	var known []string
	for _, f := range append(cli.LocalFlags(), cli.RemoteFlags()...) {
		known = append(known, f.Name)
		// The flags a flag takes after it (--from, --yes, --accept-image).
		known = append(known, regexp.MustCompile(`--[a-z][a-z0-9-]*`).FindAllString(f.Args, -1)...)
	}
	// --ssh-keys add's own word, not in the table's Args.
	known = append(known, "--restricted")
	for _, m := range regexp.MustCompile(`(?:^|[^A-Za-z0-9-])(--[a-z][a-z0-9-]*)`).FindAllStringSubmatch(c, -1) {
		if !slices.Contains(known, m[1]) {
			t.Errorf("the skill names %s, not a flag of devopsy's", m[1])
		}
	}

	var src strings.Builder
	for _, dir := range []string{"../cli", "../../cmd/devopsy", "../remote", "../images"} {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			src.Write(b)
		}
	}
	for _, l := range publicLabels {
		if !strings.Contains(src.String(), `"`+l) {
			t.Errorf("publicLabels names %s, which the code never reads", l)
		}
	}
	for _, m := range regexp.MustCompile(`(devopsy\.[A-Za-z0-9_.]+)`).FindAllStringSubmatch(c, -1) {
		name := strings.TrimRight(m[1], ".")
		for _, prefix := range []string{"devopsy.export.", "devopsy.import."} {
			if strings.HasPrefix(name, prefix) {
				name = strings.TrimSuffix(prefix, ".")
			}
		}
		if !slices.Contains(publicLabels, name) {
			t.Errorf("the skill names label %s, not a public one", m[1])
		}
	}

	// The config.yaml example, as devopsy loads it.
	m := regexp.MustCompile("(?s)```yaml\n(project:.*?)```").FindStringSubmatch(c)
	if m == nil {
		t.Fatal("no config.yaml example")
	}
	dir := t.TempDir()
	t.Setenv("DEVOPSY_HOME", dir)
	project := filepath.Join(dir, "app", ".devopsy")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, "config.yaml"), []byte(m[1]), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.LoadConfig(project); err != nil {
		t.Errorf("the config.yaml example does not load: %v", err)
	}
}

func TestInstall(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path, link, notes, err := skill.Install(dir, "v1", false)
	if err != nil || len(notes) != 0 || path != filepath.Join(dir, skill.Dir, "SKILL.md") || link != filepath.Join(dir, skill.ClaudeDir) {
		t.Fatalf("install: %q %q %v %v", path, link, notes, err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != skill.Content("v1", false) {
		t.Error("the file is not the content")
	}
	if got, err := os.Readlink(link); err != nil || got != "../../"+skill.Dir {
		t.Errorf("link: %q %v", got, err)
	}
	// Through the link, the file is readable.
	if through, _ := os.ReadFile(filepath.Join(link, "SKILL.md")); string(through) != string(data) {
		t.Error("the link does not reach the file")
	}

	// Again, another version, the stub: replaced, the link kept.
	if _, link2, notes, err := skill.Install(dir, "v2", true); err != nil || len(notes) != 0 || link2 != link {
		t.Fatalf("reinstall: %q %v %v", link2, notes, err)
	}
	if data, _ := os.ReadFile(path); string(data) != skill.Content("v2", true) {
		t.Error("the second install did not replace the file")
	}

	// A copy with CRLF line ends is still devopsy's: refreshed.
	crlf := t.TempDir()
	crlfPath := filepath.Join(crlf, skill.Dir, "SKILL.md")
	os.MkdirAll(filepath.Dir(crlfPath), 0o755)
	os.WriteFile(crlfPath, []byte(strings.ReplaceAll(skill.Content("v1", false), "\n", "\r\n")), 0o644)
	if _, _, _, err := skill.Install(crlf, "v2", false); err != nil {
		t.Errorf("refreshing devopsy's own copy with CRLF line ends: %v", err)
	}

	// A file devopsy did not write is refused, and nothing changes.
	own := t.TempDir()
	ownPath := filepath.Join(own, skill.Dir, "SKILL.md")
	os.MkdirAll(filepath.Dir(ownPath), 0o755)
	os.WriteFile(ownPath, []byte("---\nname: devopsy\n---\nmine\n"), 0o644)
	if _, _, _, err := skill.Install(own, "v1", false); err == nil || !strings.Contains(err.Error(), "not written by devopsy") {
		t.Errorf("own file: %v", err)
	}
	if data, _ := os.ReadFile(ownPath); string(data) != "---\nname: devopsy\n---\nmine\n" {
		t.Error("the own file changed")
	}
	if _, err := os.Lstat(filepath.Join(own, skill.ClaudeDir)); err == nil {
		t.Error("a link was made for a refused install")
	}
}

// What is already at Claude Code's location: a directory or a link to
// another skill is left alone and said, with no link reported; a
// dangling link is replaced; a .claude that is a file (review: its
// ENOTDIR was taken for "something is there") is said as what it is.
func TestInstallClaudeLocationTaken(t *testing.T) {
	t.Parallel()
	taken := t.TempDir()
	os.MkdirAll(filepath.Join(taken, skill.ClaudeDir), 0o755)
	if _, link, notes, err := skill.Install(taken, "v1", false); err != nil || link != "" || len(notes) != 1 || !strings.Contains(notes[0], "another skill") {
		t.Errorf("taken: %q %v %v", link, notes, err)
	}

	elsewhere := t.TempDir()
	other := filepath.Join(elsewhere, "other")
	os.MkdirAll(other, 0o755)
	os.MkdirAll(filepath.Join(elsewhere, ".claude", "skills"), 0o755)
	os.Symlink(other, filepath.Join(elsewhere, skill.ClaudeDir))
	if _, link, notes, err := skill.Install(elsewhere, "v1", false); err != nil || link != "" || len(notes) != 1 || !strings.Contains(notes[0], "another skill") {
		t.Errorf("elsewhere: %q %v %v", link, notes, err)
	}

	dangling := t.TempDir()
	os.MkdirAll(filepath.Join(dangling, ".claude", "skills"), 0o755)
	os.Symlink("../../gone", filepath.Join(dangling, skill.ClaudeDir))
	if path, link, notes, err := skill.Install(dangling, "v1", false); err != nil || link == "" || len(notes) != 0 {
		t.Errorf("dangling: %q %v %v", link, notes, err)
	} else if got, _ := os.ReadFile(filepath.Join(link, "SKILL.md")); len(got) == 0 {
		t.Errorf("the dangling link was not replaced (%s)", path)
	}

	file := t.TempDir()
	os.WriteFile(filepath.Join(file, ".claude"), []byte("not a directory\n"), 0o644)
	path, link, notes, err := skill.Install(file, "v1", false)
	if err != nil || path == "" || link != "" || len(notes) != 1 || !strings.Contains(notes[0], "no link for Claude Code") || !strings.Contains(notes[0], "not a directory") {
		t.Errorf(".claude a file: %q %q %v %v", path, link, notes, err)
	}
	if _, err := os.Lstat(filepath.Join(file, skill.ClaudeDir)); err == nil {
		t.Error("a link under a file")
	}
}

// The link for Claude Code is relative from where the link really is:
// with .claude, or .claude/skills, itself a link (a home directory whose
// ~/.claude lives in a dotfiles checkout), "../.." from the name would
// climb out of the wrong place and dangle (review).
func TestInstallThroughLinkedClaudeDir(t *testing.T) {
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
				path, link, notes, err := skill.Install(dir, "v1", false)
				if err != nil || link == "" {
					t.Fatalf("install %d: %q %v %v", i, link, notes, err)
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
