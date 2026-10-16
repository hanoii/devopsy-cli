package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/term"

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
	root := filepath.Dir(projectDir)
	run := func(phase string, a []string) int { return runInPlace(root, phase, a) }
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

// runInPlace runs devopsy a in the project's root, as a server runs a
// step in the release, saying so for a phase's step.
func runInPlace(root, phase string, a []string) int {
	if phase != "" {
		sayErr(fmt.Sprintf("devopsy: running 'devopsy %s' (%s)", strings.Join(a, " "), phase))
	}
	self, err := os.Executable()
	if err != nil {
		self = "devopsy"
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

// runLocalDestroy is --destroy without a target, in place, after asking
// for the project's name (--yes skips it): defaults:' destroy step, when
// there is one, then what runDestroyData takes down. Nothing else: on a
// server devopsy then removes the environment's directory, which locally
// is the checkout.
func runLocalDestroy(cwd string, args []string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(os.Stderr, red, "devopsy: "+msg, color)
		return 1
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--destroy"]))
		return 0
	}
	yes := false
	for _, a := range args {
		switch a {
		case "--yes", "-y":
			yes = true
		default:
			return fail("usage: devopsy [@<target>] --destroy [--yes]")
		}
	}
	projectDir, err := cli.FindProjectDir(cwd)
	if err != nil {
		return fail(err.Error())
	}
	c, err := remote.LoadConfig(projectDir)
	if err != nil {
		return fail(err.Error())
	}
	var step []string
	if c.Defaults != nil {
		step = remote.StepArgs(c.Defaults.Destroy)
	}
	if len(step) > 0 {
		if _, err := cli.CheckCommand(projectDir, step[0], cli.Bare); err != nil {
			return fail("the destroy step: " + err.Error())
		}
	}
	root := filepath.Dir(projectDir)
	name := filepath.Base(root)
	if c.Project != nil && c.Project.Name != "" {
		name = c.Project.Name
	}
	if !yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fail("--destroy removes the local project's containers, volumes and data in .devopsy/mnt: add --yes when not at a terminal")
		}
		what := ""
		if len(step) > 0 {
			what = "runs 'devopsy " + strings.Join(step, " ") + "', then "
		}
		fmt.Fprintf(os.Stderr, "This %stakes %s's containers, volumes and built images down and empties %s. Type %s to go on: ", what, name, filepath.Join(projectDir, "mnt"), name)
		line, _ := stdinReader.ReadString('\n')
		if strings.TrimSpace(line) != name {
			cli.Fprint(os.Stderr, red, "Nothing removed.", color)
			return 1
		}
	}
	return runDestroyData(root, step, color)
}

// runDestroyData is --destroy where the project is (DestroyDataFlag, its
// arguments the destroy step, maybe none): the step, while everything
// runs (a last dump, an outside record); then docker compose down with
// the volumes and the images built without an image: name (--rmi local),
// every profile's; then mnt emptied as root in UtilImage, since container
// users own its files. A failed step changes nothing.
func runDestroyData(cwd string, step []string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(errOut, red, "devopsy: "+msg, color)
		return 1
	}
	projectDir, err := cli.FindProjectDir(cwd)
	if err != nil {
		return fail(err.Error())
	}
	root := filepath.Dir(projectDir)
	if len(step) > 0 {
		if code := runInPlace(root, "destroy", step); code != 0 {
			fail(fmt.Sprintf("the destroy step failed (%d): nothing removed", code))
			return code
		}
	}
	ctx, stop := interruptible()
	defer stop()
	sayErr("devopsy: taking the containers, volumes and built images down")
	if err := composeContext(ctx, root, errOut, "--profile", "*", "down", "--volumes", "--remove-orphans", "--rmi", "local"); err != nil {
		return fail("docker compose down: " + err.Error())
	}
	mnt := filepath.Join(projectDir, "mnt")
	if entries, err := os.ReadDir(mnt); err != nil || len(entries) == 0 {
		return 0
	}
	real, err := filepath.EvalSymlinks(mnt)
	if err != nil {
		return fail(err.Error())
	}
	if err := ensureUtilImage(ctx); err != nil {
		return fail(err.Error())
	}
	sayErr("devopsy: emptying " + mnt)
	if err := dockerRun(ctx, fmt.Sprintf("devopsy-destroy-%d", os.Getpid()), "-v", real+":/mnt", remote.UtilImage, "find", "/mnt", "-mindepth", "1", "-delete"); err != nil {
		return fail("emptying mnt: " + err.Error())
	}
	return 0
}
