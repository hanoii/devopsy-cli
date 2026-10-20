package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// runInit implements `devopsy --init [project]`: a new .devopsy/config.yaml,
// in the project devopsy finds from the current directory, else in a new
// .devopsy/ there. An
// existing config is never touched: --debug schema documents every key.
func runInit(cwd string, args []string, color bool) int {
	fail := func(msg string) int {
		report.Error(msg)
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
		report.Info(file+" exists: nothing to do", "devopsy --debug schema documents every key")
		return 0
	}

	suggest := strings.Trim(regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(strings.ToLower(filepath.Base(filepath.Dir(dot))), "-"), "-")
	name := ""
	switch {
	case len(args) == 1:
		name = args[0]
	case atTerminal():
		name = report.Default().Input(stdinReader, "the project's name on servers (its directory and compose names)", suggest)
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
	then := "then release with:"
	if _, err := os.Stat(filepath.Join(dot, "compose.yaml")); err != nil {
		then = "add " + filepath.Join(dot, "compose.yaml") + ", then release with:"
	}
	report.Info("wrote "+file, then, report.CmdMark+"devopsy @<server>:prod --release")
	return 0
}
