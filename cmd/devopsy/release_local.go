package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// runLocalRelease is --release without a target: a release in place, in
// the local project. defaults:' release steps, the remote phases only
// (before and after steps are the operator's side of a deploy): prepare,
// --missing-vars, the images as build mode would get them on a server
// (a local image-mode project has no image of its own in a registry), then
// run. Nothing to switch or go back to: a failure stops there.
func runLocalRelease(cwd string, args []string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(os.Stderr, red, "devopsy: "+msg, color)
		return 1
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--release"]))
		return 0
	}
	if len(args) > 0 {
		return fail("--release takes nothing locally: devopsy --release (a target's: devopsy @<target> --release)")
	}
	projectDir, err := cli.FindProjectDir(cwd)
	if err != nil {
		return fail(err.Error())
	}
	c, err := remote.LoadConfig(projectDir)
	if err != nil {
		return fail(err.Error())
	}
	if c.Defaults == nil || c.Defaults.Release == nil {
		report.Fail(os.Stderr, color, report.Errorf([]string{
			"Without a target, --release runs the release steps under defaults: in .devopsy/config.yaml, in place.",
		}, "the project has no release steps under defaults:"),
			"Add them to .devopsy/config.yaml, like:\ndefaults:\n  release: {prepare: secrets, run: deploy}",
			"Release to a target instead:\ndevopsy @<target> --release")
		return 1
	}
	steps := c.Defaults.Release
	// Every step runs here: a command that only runs on a target says so
	// before anything runs.
	for _, step := range append(append([]string{}, steps.Prepare...), steps.Run) {
		a := remote.StepArgs(step)
		if len(a) == 0 {
			continue
		}
		if _, err := cli.CheckCommand(projectDir, a[0], cli.Bare); err != nil {
			return fail("a release step: " + err.Error())
		}
	}
	self, err := os.Executable()
	if err != nil {
		self = "devopsy"
	}
	root := filepath.Dir(projectDir)
	// run runs devopsy a in the project's root, as a server runs a step in
	// the release, saying so for a phase's step.
	run := func(phase string, a []string) int {
		if phase != "" {
			sayErr(fmt.Sprintf("devopsy: running 'devopsy %s' (%s)", strings.Join(a, " "), phase))
		}
		cmd := exec.Command(self, a...)
		cmd.Dir = root
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				return exitErr.ExitCode()
			}
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	for _, step := range steps.Prepare {
		if code := run("prepare", remote.StepArgs(step)); code != 0 {
			return code
		}
	}
	if code := run("", []string{"--missing-vars"}); code != 0 {
		return code
	}
	sayErr("devopsy: pulling and building images")
	if code := run("", []string{"--", "pull", "--ignore-buildable"}); code != 0 {
		return code
	}
	if code := run("", []string{"--", "build", "--pull"}); code != 0 {
		return code
	}
	if a := remote.StepArgs(steps.Run); len(a) > 0 {
		return run("run", a)
	}
	return 0
}
