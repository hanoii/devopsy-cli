package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/skill"
)

const skillUsage = "usage: devopsy --ai-skill [--stub] [install [--global]]"

// runAISkill implements `devopsy --ai-skill`: the agent skill for using
// devopsy printed, or installed into the project (`install`) or the home
// directory (`install --global`); `--stub` is the short form that sends
// the agent to this command.
func runAISkill(args []string, color bool) int {
	var stub, install, global bool
	for _, a := range args {
		switch a {
		case "--help", "-h":
			fmt.Println(skillUsage)
			fmt.Print(`
Prints the skill that teaches an agent (Claude Code, Codex, Cursor) to
work on a project that runs on devopsy: the layout of .devopsy/, the
labels, config.yaml, releases and what devopsy refuses. It is part of
this binary, so it matches this version.

install writes it to ` + skill.Dir + `/SKILL.md in the project (the
directory with .devopsy/, found from here as every command finds it;
outside a project, the current directory), or under $HOME with
--global, and links ` + skill.ClaudeDir + ` to it for Claude Code. Run
it again after an upgrade: a copy devopsy wrote is replaced; any other
file there is refused, so nothing of yours is overwritten.

--stub is the short form: a file that tells the agent to run
devopsy --ai-skill, for a project that would rather not carry the text
(a template, say) and still get the reference that matches the devopsy
installed.
`)
			return 0
		case "--stub":
			stub = true
		case "--global":
			global = true
		case "install":
			install = true
		default:
			report.Error(skillUsage)
			return 1
		}
	}
	if !install {
		if global {
			report.Error(skillUsage)
			return 1
		}
		fmt.Print(skill.Content(version, stub))
		return 0
	}
	dir, err := os.Getwd()
	switch {
	case global:
		dir, err = os.UserHomeDir()
	case err == nil:
		// The project's directory when in one, as every command finds
		// it: a skill in a subdirectory is one no agent reads.
		if project, perr := cli.FindProjectDir(dir); perr == nil {
			dir = filepath.Dir(project)
		}
	}
	if err != nil {
		report.Fail(err)
		return 1
	}
	path, link, notes, err := skill.Install(dir, version, stub)
	if err != nil {
		report.Fail(err)
		return 1
	}
	if link != "" {
		report.Info("wrote "+path+", linked from "+link, notes...)
	} else {
		report.Info("wrote "+path, notes...)
	}
	return 0
}
