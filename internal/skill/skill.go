// Package skill is the agent skill for using devopsy: a SKILL.md embedded
// in the binary, so it describes the devopsy that prints it, and its
// install into a project or a home directory.
package skill

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

//go:embed SKILL.md
var content string

//go:embed STUB.md
var stub_ string

// Dir is where Install writes the skill, under a project or a home
// directory: the location agents share (Codex, Cursor, the skills
// installer).
const Dir = ".agents/skills/devopsy"

// ClaudeDir is Claude Code's own location, which Install links to Dir.
const ClaudeDir = ".claude/skills/devopsy"

// versionKey is the frontmatter line that marks a copy devopsy wrote,
// with the version it came from (`stub` for the stub).
const versionKey = "devopsy: "

// Content is the skill, with version in its frontmatter. The stub is
// the short form that sends the agent to `devopsy --ai-skill`, for a
// project that would rather not carry the text.
func Content(version string, stub bool) string {
	if stub {
		return stub_
	}
	return strings.ReplaceAll(content, "{{VERSION}}", version)
}

// Install writes the skill under dir (a project, or the home directory
// with --global) and links Claude Code's location to it. It overwrites
// a copy devopsy wrote (marked by its version line) and refuses any
// other file; an entry already at Claude Code's location that is not
// that link is left alone, and noted. It returns the file written and
// notes for the operator.
func Install(dir, version string, stub bool) (path string, notes []string, err error) {
	path = filepath.Join(dir, Dir, "SKILL.md")
	if old, err := os.ReadFile(path); err == nil && !Written(old) {
		return "", nil, fmt.Errorf("%s was not written by devopsy: remove or move it first", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(path, []byte(Content(version, stub)), 0o644); err != nil {
		return "", nil, err
	}
	link := filepath.Join(dir, ClaudeDir)
	want := filepath.Join("..", "..", Dir)
	switch got, err := os.Readlink(link); {
	case err == nil && got == want:
	case err == nil:
		notes = append(notes, link+" links elsewhere ("+got+"), left as it is")
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			return "", nil, err
		}
		if err := os.Symlink(want, link); err != nil {
			return "", nil, err
		}
	default:
		notes = append(notes, link+" exists and is not a link, left as it is")
	}
	return path, notes, nil
}

// Written reports whether a SKILL.md is one devopsy wrote: its
// frontmatter carries the version line.
func Written(data []byte) bool {
	rest, ok := bytes.CutPrefix(data, []byte("---\n"))
	if !ok {
		return false
	}
	front, _, ok := bytes.Cut(rest, []byte("\n---\n"))
	if !ok {
		return false
	}
	for _, line := range bytes.Split(front, []byte("\n")) {
		if bytes.HasPrefix(line, []byte(versionKey)) {
			return true
		}
	}
	return false
}
