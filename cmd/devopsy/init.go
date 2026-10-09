package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/term"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// runInit implements `devopsy --init [project]`: a new .devopsy/config.yaml,
// in the project devopsy finds from here, else in a new .devopsy/ here. An
// existing config is never touched: --debug schema documents every key.
func runInit(cwd string, args []string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(os.Stderr, red, msg, color)
		return 1
	}
	if len(args) > 1 {
		return fail("usage: devopsy --init [project]")
	}
	dot := filepath.Join(cwd, cli.ProjectDirName)
	if found, err := cli.FindProjectDir(cwd); err == nil {
		dot = found
	}
	file := filepath.Join(dot, remote.ConfigFile)
	if _, err := os.Stat(file); err == nil {
		cli.Fprint(os.Stderr, cyan, file+" exists: nothing to do. devopsy --debug schema documents every key.", color)
		return 0
	}

	suggest := strings.Trim(regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(strings.ToLower(filepath.Base(filepath.Dir(dot))), "-"), "-")
	name := ""
	switch {
	case len(args) == 1:
		name = args[0]
	case term.IsTerminal(int(os.Stdin.Fd())):
		fmt.Fprintf(os.Stderr, "The project's name on servers (its directory and compose names) [%s]: ", suggest)
		line, _ := stdinReader.ReadString('\n')
		name = strings.TrimSpace(line)
		if name == "" {
			name = suggest
		}
	default:
		return fail("devopsy --init <project>: name the project when not at a terminal (" + suggest + "?)")
	}
	if !remote.ValidName(name) {
		return fail(fmt.Sprintf("project %q: lowercase letters, digits and -", name))
	}

	content := fmt.Sprintf(`# devopsy config. devopsy --debug schema documents every key.
project: %s
defaults:
  release: up -d --wait --remove-orphans --build
  rollback: up -d --wait --remove-orphans --build
environments:
  prod: {}
`, name)
	if err := os.MkdirAll(dot, 0o755); err != nil {
		return fail(err.Error())
	}
	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		return fail(err.Error())
	}
	cli.Fprint(os.Stderr, cyan, "Wrote "+file+".", color)
	if _, err := os.Stat(filepath.Join(dot, "compose.yaml")); err != nil {
		fmt.Fprintf(os.Stderr, "Add %s, then ", filepath.Join(dot, "compose.yaml"))
	} else {
		fmt.Fprint(os.Stderr, "Then ")
	}
	fmt.Fprint(os.Stderr, "release with: devopsy @<server>:prod --release\n")
	return 0
}
