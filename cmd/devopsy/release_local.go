package main

import (
	"context"
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
		report.Error(msg)
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
		report.Fail(report.Errorf([]string{
			"without a target, --release runs the release steps under defaults: in .devopsy/config.yaml, in place",
		}, "the project has no release steps under defaults:"),
			report.Fix("add them to .devopsy/config.yaml, like:\ndefaults:\n  release: {prepare: secrets, run: deploy}"),
			report.Fix("release to a target instead:", "devopsy @<target> --release"))
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
	report.Info("pulling and building images")
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
		report.Info(fmt.Sprintf("running 'devopsy %s' (%s)", strings.Join(a, " "), phase))
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
		report.Fail(err)
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
		report.Error(msg)
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
	step, err := destroyStep(projectDir)
	if err != nil {
		return fail(err.Error())
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
		what := ""
		if len(step) > 0 {
			what = "runs 'devopsy " + strings.Join(step, " ") + "', then "
		}
		if err := typeToGoOn(fmt.Sprintf("this %stakes %s's containers, volumes and built images down and empties %s", what, name, filepath.Join(projectDir, "mnt")),
			name, "nothing removed", false); err != nil {
			report.Fail(err)
			return 1
		}
	}
	return destroyData(root, step, color)
}

// destroyStep is what --destroy runs first in projectDir: in a release
// (a server's), the destroy: step of the environment it was made for, as
// its own config says; locally defaults:' destroy:. Maybe none.
func destroyStep(projectDir string) ([]string, error) {
	if cli.InRelease(projectDir) {
		t, err := remote.ReleaseEnvironment(projectDir)
		if err != nil {
			return nil, err
		}
		return remote.StepArgs(t.Destroy), nil
	}
	c, err := remote.LoadConfig(projectDir)
	if err != nil {
		return nil, err
	}
	if c.Defaults == nil {
		return nil, nil
	}
	return remote.StepArgs(c.Defaults.Destroy), nil
}

// runDestroyData is the hidden DestroyDataFlag, run by DestroyScript in a
// server's release: destroyData with the release's own destroy step.
func runDestroyData(cwd string, args []string, color bool) int {
	if len(args) > 0 {
		report.Error(cli.DestroyDataFlag + " takes nothing")
		return 2
	}
	projectDir, err := cli.FindProjectDir(cwd)
	if err != nil {
		report.Fail(err)
		return 1
	}
	step, err := destroyStep(projectDir)
	if err != nil {
		report.Error("the release's destroy step: " + err.Error())
		return 1
	}
	return destroyData(filepath.Dir(projectDir), step, color)
}

// runReleaseSteps is the hidden ReleaseStepsFlag:
// <rollback|restart|release> <phase> [<release>], run by ActivateScript
// where the phase runs, and by a sync's holder (release run). It runs
// the phase's steps from the config of the release given (default the
// one here), for the environment it was made for: a rollback's are the
// restored release's rollback:, a restart's the previous release's
// rollback: run step, else its release: one, a sync's its release:
// one. Before and after are the remote ones. The first failure ends it, with its code.
func runReleaseSteps(cwd string, args []string, color bool) int {
	fail := func(msg string) int {
		report.Error(msg)
		return 1
	}
	if len(args) < 2 || len(args) > 3 {
		return fail("usage: " + cli.ReleaseStepsFlag + " rollback|restart|release before|prepare|run|after [<release>]")
	}
	kind, phase := args[0], args[1]
	projectDir, err := cli.FindProjectDir(cwd)
	if err != nil {
		return fail(err.Error())
	}
	from := projectDir
	if len(args) == 3 {
		from = filepath.Join(args[2], ".devopsy")
	}
	t, err := remote.ReleaseEnvironment(from)
	if err != nil {
		return fail("the release's steps: " + err.Error())
	}
	var steps *remote.Steps
	switch kind {
	case "rollback":
		steps = t.Rollback
	case "restart":
		steps = t.Rollback
		if steps == nil || len(remote.StepArgs(steps.Run)) == 0 {
			steps = t.Release
		}
	case "release":
		steps = t.Release
	default:
		return fail("usage: " + cli.ReleaseStepsFlag + " rollback|restart|release ...")
	}
	if steps == nil || len(remote.StepArgs(steps.Run)) == 0 {
		return fail(fmt.Sprintf("the release's config gives environment %s no %s run step", t.Name, kind))
	}
	var list []string
	switch phase {
	case "before":
		list = remote.Commands(steps.Before, true)
	case "prepare":
		list = steps.Prepare
	case "run":
		list = []string{steps.Run}
	case "after":
		list = remote.Commands(steps.After, true)
	default:
		return fail("usage: " + cli.ReleaseStepsFlag + " rollback|restart|release before|prepare|run|after [<release>]")
	}
	root := filepath.Dir(projectDir)
	for _, step := range list {
		if code := runInPlace(root, phase, remote.StepArgs(step)); code != 0 {
			return code
		}
	}
	return 0
}

// destroyData is --destroy where the project is, root its directory, with
// step, maybe none: the step, while everything
// runs (a last dump, an outside record); then docker compose down with
// the volumes and the images built without an image: name (--rmi local),
// every profile's; then mnt emptied as root in UtilImage, since container
// users own its files. A failed step changes nothing.
func destroyData(cwd string, step []string, color bool) int {
	fail := func(msg string) int {
		report.Error(msg)
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
	// A sync's record left (its holder killed outright): its writers end
	// first, so nothing writes into mnt while it is emptied.
	if cli.InRelease(projectDir) {
		// In current, or a release's code (no current: a first release
		// that failed).
		base := filepath.Dir(root)
		if filepath.Base(root) == remote.CodeDir {
			base = filepath.Dir(filepath.Dir(base))
		}
		started, taken := filepath.Join(base, remote.SyncStartedFile), filepath.Join(base, remote.SyncTakenFile)
		if fileExists(started) {
			_ = os.Rename(started, taken)
		}
		// Destroy takes everything down anyway: no closure.
		if err := endWriters(root, base, []cli.SyncService{}); err != nil {
			return fail(err.Error() + ": nothing removed")
		}
		// An import a killed devopsy left may still extract into mnt
		// (not Compose's, so down --remove-orphans leaves it), and its
		// files as root would fail the removal of $base.
		if err := removeContainers(importContainers(root)); err != nil {
			return fail(err.Error() + ": nothing removed")
		}
		// And an owners' step's, which would chown in it as root.
		if err := removeContainers(ownersContainers(root)); err != nil {
			return fail(err.Error() + ": nothing removed")
		}
	}
	ctx, stop := interruptible()
	defer stop()
	report.Info("taking the containers, volumes and built images down")
	if err := composeContext(ctx, root, errOut, "--profile", "*", "down", "--volumes", "--remove-orphans", "--rmi", "local"); err != nil {
		return fail("docker compose down: " + err.Error())
	}
	if err := emptyMnt(ctx, projectDir, fmt.Sprintf("devopsy-destroy-%d", os.Getpid())); err != nil {
		return fail(err.Error())
	}
	return 0
}

// emptyMnt empties the project's mnt as root in remote.UtilImage, in a
// container named name (files containers wrote as other users
// included), never removing it: on servers it is a link to shared/mnt.
func emptyMnt(ctx context.Context, projectDir, name string) error {
	mnt := filepath.Join(projectDir, "mnt")
	if entries, err := os.ReadDir(mnt); err != nil || len(entries) == 0 {
		return nil
	}
	real, err := filepath.EvalSymlinks(mnt)
	if err != nil {
		return err
	}
	if err := ensureUtilImage(ctx); err != nil {
		return err
	}
	report.Info("emptying " + mnt)
	if err := dockerRun(ctx, name, "-v", real+":/mnt", remote.UtilImage, "find", "/mnt", "-mindepth", "1", "-delete"); err != nil {
		return fmt.Errorf("emptying mnt: %w", err)
	}
	return nil
}
