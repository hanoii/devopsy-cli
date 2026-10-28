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

// versionKey is the frontmatter key that marks a copy devopsy wrote,
// with the version it came from (`stub` for the stub).
const versionKey = "devopsy:"

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
// other file. It returns the file written, the link when Claude Code
// reads the skill through it ("" when not), and notes for the operator:
// an entry already at Claude Code's location that is not that link is
// left alone, and said.
func Install(dir, version string, stub bool) (path, link string, notes []string, err error) {
	path = filepath.Join(dir, Dir, "SKILL.md")
	if old, err := os.ReadFile(path); err == nil && !Written(old) {
		return "", "", nil, fmt.Errorf("%s was not written by devopsy: remove or move it first", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", "", nil, err
	}
	if err := os.WriteFile(path, []byte(Content(version, stub)), 0o644); err != nil {
		return "", "", nil, err
	}
	link = filepath.Join(dir, ClaudeDir)
	note, err := linkClaude(link, filepath.Dir(path))
	if err != nil {
		return "", "", nil, err
	}
	if note != "" {
		return path, "", []string{note}, nil
	}
	return path, link, nil, nil
}

// linkClaude makes link a symlink to target (the skill's directory), or
// leaves what is there and says why. The link is relative, from where
// the link really is: with ~/.claude itself a link (a dotfiles
// checkout), "../.." from the name would climb out of the wrong place.
// A link already there that reaches target is kept; a dangling one
// (an install into a directory since moved) is replaced.
func linkClaude(link, target string) (note string, err error) {
	if _, err := os.Lstat(link); err == nil {
		if same, _ := filepath.EvalSymlinks(link); same != "" {
			if real, _ := filepath.EvalSymlinks(target); same == real {
				return "", nil
			}
			return link + " exists, left as it is: Claude Code reads another skill there", nil
		}
		if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
			return link + " exists, left as it is: Claude Code reads another skill there", nil
		}
		if err := os.Remove(link); err != nil {
			return "", err
		}
	}
	parent := filepath.Dir(link)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "no link for Claude Code at " + link + ": " + err.Error(), nil
	}
	from, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	to, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(from, to)
	if err != nil {
		rel = to
	}
	return "", os.Symlink(rel, link)
}

// Written reports whether a SKILL.md is one devopsy wrote: its
// frontmatter carries the version line. Line ends may be CRLF (a
// checkout with core.autocrlf).
func Written(data []byte) bool {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
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
