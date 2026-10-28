package main

import (
	"fmt"
	"os"

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

install writes it to ` + skill.Dir + `/SKILL.md in the current directory,
or under $HOME with --global, and links ` + skill.ClaudeDir + ` to it
for Claude Code. Run it again after an upgrade: a copy devopsy wrote is
replaced, any other file is left alone.

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
	if global {
		dir, err = os.UserHomeDir()
	}
	if err != nil {
		report.Fail(err)
		return 1
	}
	path, notes, err := skill.Install(dir, version, stub)
	if err != nil {
		report.Fail(err)
		return 1
	}
	report.Info("wrote "+path, notes...)
	return 0
}
