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
	if len(args) > 1 {
		return fail("usage: devopsy --init [project]")
	}
	dot := filepath.Join(cwd, cli.ProjectDirName)
	if found, err := cli.FindProjectDir(cwd); err == nil {
		dot = found
	}
	file := filepath.Join(dot, remote.ConfigFile)
	if _, err := os.Stat(file); err == nil {
		report.Say(report.InfoLevel, file+" exists: nothing to do", nil, report.Hint("every key:", "devopsy --debug schema"))
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
		return failErr(&report.Failure{Msg: "--init: no terminal to ask the project's name on", Remedies: []report.Remedy{report.Fix("name it:", "devopsy --init "+suggest)}})
	}
	if !remote.ValidName(name) {
		return fail(fmt.Sprintf("project %q: lowercase letters, digits and -", name))
	}

	content := fmt.Sprintf(`# devopsy config. devopsy --debug schema documents every key.
project: %s
defaults:
  release: up -d --wait --remove-orphans
  rollback: up -d --wait --remove-orphans
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
	report.Say(report.InfoLevel, "wrote "+file, nil, report.Hint(then, "devopsy @<server>:prod --release"))
	return 0
}
