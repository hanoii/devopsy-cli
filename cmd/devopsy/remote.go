package main

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// runRemote handles `devopsy @target ...`.
func runRemote(cwd string, args []string, color, verbose bool) int {
	fail := func(msg string) int {
		cli.Fprint(os.Stderr, red, msg, color)
		return 1
	}

	// Outside a project only user-level targets exist.
	projectDir, _ := cli.FindProjectDir(cwd)
	// The project's .env can set target hosts (DEVOPSY_TARGET_HOST...).
	var projectEnv func(string) (string, bool)
	env := cli.NewEnv(os.Environ())
	dotenvFile := ""
	if projectDir != "" {
		dotenvFile = filepath.Join(projectDir, ".env")
		if err := cli.LoadDotenv(env, dotenvFile); err != nil {
			return fail(err.Error())
		}
		projectEnv = env.Lookup
	}
	if verbose {
		// What runs over SSH, with the project's secrets masked. devopsy on
		// the server is verbose too.
		secrets := cli.NewSecrets(env, dotenvFile)
		remote.Verbose = true
		remote.Trace = func(msg string) { cli.Fprint(os.Stderr, "", secrets.Mask(msg), color) }
	}
	// @<instance>:<target>, or DEVOPSY_INSTANCE: one install of the project
	// among several on a server.
	name, instance := strings.TrimPrefix(args[0], "@"), ""
	if i, n, ok := strings.Cut(name, ":"); ok {
		instance, name = i, n
		if instance == "" {
			return fail("@:" + name + ": name the instance, or leave out the colon")
		}
	}
	if v := os.Getenv(remote.InstanceVar); v != "" {
		if instance != "" && instance != v {
			return fail(fmt.Sprintf("@%s:%s and %s=%s name different instances", instance, name, remote.InstanceVar, v))
		}
		instance = v
	}
	t, err := remote.LoadTarget(projectDir, name, instance, projectEnv)
	if err != nil {
		return fail(err.Error())
	}
	args = args[1:]
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(styleFor(os.Stdout).help(cli.RemoteHelp))
		return 0
	}

	// `devopsy @t --release --help` explains instead of releasing.
	if help, ok := cli.RemoteCommandHelp[args[0]]; ok && len(args) > 1 && (args[1] == "--help" || args[1] == "-h") {
		fmt.Print(styleFor(os.Stdout).help(help))
		return 0
	}

	// A user-level target belongs to no project, so nothing may be released
	// to it from wherever devopsy happens to run: only from its source,
	// where it then acts as the project's own target.
	if args[0] == "--release" || args[0] == "--rollback" {
		ok, why := t.ReleasesHere(projectDir)
		if !ok {
			return fail(why)
		}
		t.User = false
	}

	// Without a top-level name, compose would name the project after the
	// release directory, so it is <project>[-<instance>]-<target>. A
	// user-level target without source belongs to no local project: the
	// server's own files name it (target.env in releases, the directory
	// otherwise). A nested devopsy (DEVOPSY_PROJECT_DIR set: a project
	// command, or a release step, calling devopsy @target) inherits the local
	// project's COMPOSE_PROJECT_NAME, which says nothing about the target:
	// ignore it.
	projectName := ""
	if v := os.Getenv("COMPOSE_PROJECT_NAME"); v != "" && os.Getenv("DEVOPSY_PROJECT_DIR") == "" {
		projectName = v
	} else if t.Project != nil {
		raw, err := cli.TopLevelName(filepath.Join(t.Project.Dir, "compose.yaml"))
		if err != nil {
			return fail(err.Error())
		}
		if raw == "" {
			projectName = cli.NormalizeProjectName(t.ComposeName())
		}
	}

	tty := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	ssh := func(script string, stdin io.Reader, tty bool) int {
		code, err := remote.SSH(t, script, stdin, nil, tty)
		if err != nil {
			return fail(err.Error())
		}
		return code
	}

	switch args[0] {
	case "--shell-host":
		if len(args) > 1 {
			return fail("--shell-host takes no arguments: for one command, use 'devopsy @" + t.Name + " <command>'")
		}
		return ssh(remote.ShellScript(t), nil, tty)

	case "--vars":
		// A user-level target belongs to no project: its values never come
		// from the .env of whatever project devopsy runs in.
		lookup := os.LookupEnv
		if !t.User && projectEnv != nil {
			lookup = projectEnv
		}
		return runVars(t, args[1:], lookup, color)

	case "--releases":
		var out bytes.Buffer
		code, err := remote.SSH(t, remote.ReleasesScript(t), bytes.NewReader(nil), &out, false)
		if err != nil {
			return fail(err.Error())
		}
		if code != 0 {
			return code
		}
		fmt.Print(remote.FormatReleases(out.String()))
		return 0

	case "--destroy":
		return runDestroy(t, projectName, args[1:], color)

	case "--instances":
		if t.Project == nil {
			return fail("--instances: @" + t.Name + " has no project (a user-level target without source)")
		}
		var out bytes.Buffer
		code, err := remote.SSH(t, remote.InstancesScript(t), bytes.NewReader(nil), &out, false)
		if err != nil {
			return fail(err.Error())
		}
		if code != 0 {
			return code
		}
		if out.Len() == 0 {
			cli.Fprint(os.Stderr, cyan, fmt.Sprintf("No instances of %s on %s.", t.Project.Name, t.Host), color)
			return 0
		}
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			inst, targets, _ := strings.Cut(line, "\t")
			fmt.Printf("%-24s %s\n", inst, targets)
		}
		return 0

	case "--release", "--rollback":
		if len(args) > 1 {
			return fail(fmt.Sprintf("%s takes no command: what it runs is the target's %s: steps in %s", args[0], strings.TrimPrefix(args[0], "--"), t.File))
		}
		steps := t.Release
		if args[0] == "--rollback" {
			steps = t.Rollback
		}
		if steps == nil || len(remote.StepArgs(steps.Remote)) == 0 {
			return fail(missingSteps(t, strings.TrimPrefix(args[0], "--")))
		}
		local := func(phase string, list remote.StepList, commit string) int {
			for _, step := range list {
				cli.Fprint(os.Stderr, cyan, fmt.Sprintf("Running 'devopsy %s' locally (%s)...", step, phase), color)
				if code := runLocalStep(t, remote.StepArgs(step), commit); code != 0 {
					return code
				}
			}
			return 0
		}

		if args[0] == "--rollback" {
			if code := local("before", steps.Before, ""); code != 0 {
				return fail(fmt.Sprintf("a before step failed (%d): nothing changed on %s", code, t.Name))
			}
			cli.Fprint(os.Stderr, cyan, fmt.Sprintf("Rolling back %s (%s:%s)...", t.Name, t.Host, t.Path), color)
			if code := ssh(remote.ActivateScript(t, "", true, projectName, remote.StepArgs(steps.Remote)), nil, tty); code != 0 {
				return code
			}
			if code := local("after", steps.After, ""); code != 0 {
				return fail(fmt.Sprintf("an after step failed (%d): the rollback stays", code))
			}
			return 0
		}

		projectRoot := filepath.Dir(projectDir)
		files, err := remote.Files(projectRoot, t.Mode)
		if err != nil {
			return fail(err.Error())
		}
		record := remote.NewRecord(projectRoot, t.Mode, time.Now())
		commit := record.Commit
		if v := t.Env["DEVOPSY_RELEASE_COMMIT"]; v != "" {
			commit = v
		}
		if code := local("before", steps.Before, commit); code != 0 {
			return fail(fmt.Sprintf("a before step failed (%d): nothing changed on %s", code, t.Name))
		}
		src := record.Mode
		if record.Commit != "" {
			src += ", " + record.Branch + "@" + record.Commit[:min(10, len(record.Commit))]
			if record.Dirty {
				src += " with uncommitted changes"
			}
		}
		cli.Fprint(os.Stderr, cyan, fmt.Sprintf("Releasing %s to %s (%s:%s): %d files, %s...",
			record.ID, t.Name, t.Host, t.Path, len(files), src), color)
		if t.Mode == remote.ModeImage && record.Dirty && remote.DirtyOutsideDevopsy(projectRoot) {
			cli.Fprint(os.Stderr, yellow, fmt.Sprintf("Uncommitted changes outside .devopsy/ are not released in image mode: images come from the registry (DEVOPSY_RELEASE_COMMIT=%s).",
				record.Commit[:min(10, len(record.Commit))]), color)
		}

		pr, pw := io.Pipe()
		go func() {
			pw.CloseWithError(remote.Pack(pw, projectRoot, files, map[string][]byte{
				remote.RecordFile:                  record.JSON(),
				".devopsy/" + remote.TargetEnvFile: targetEnv(t, projectName, record.Commit),
			}))
		}()
		if code := ssh(remote.UploadScript(t, record.ID), pr, false); code != 0 {
			return code
		}
		if code := ssh(remote.ActivateScript(t, record.ID, false, projectName, remote.StepArgs(steps.Remote)), nil, tty); code != 0 {
			return code
		}
		if code := local("after", steps.After, commit); code != 0 {
			return fail(fmt.Sprintf("an after step failed (%d): the release stays", code))
		}
		return 0

	default:
		return ssh(remote.RunScript(t, projectName, args), nil, tty)
	}
}

// runLocalStep runs `devopsy args...` here, in the local project, for a
// release or rollback of t: with the target's env, DEVOPSY_TARGET and, for
// a release, DEVOPSY_RELEASE_COMMIT, over the caller's environment. Never
// the server's shared/.env: its secrets stay there.
func runLocalStep(t *remote.Target, args []string, commit string) int {
	self, err := os.Executable()
	if err != nil {
		self = "devopsy"
	}
	cmd := exec.Command(self, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	for k, v := range t.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Env = append(cmd.Env, "DEVOPSY_TARGET="+t.Name)
	if commit != "" {
		cmd.Env = append(cmd.Env, "DEVOPSY_RELEASE_COMMIT="+commit)
	}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// missingSteps explains how to define a target's release or rollback steps,
// with a starting point for its mode.
func missingSteps(t *remote.Target, which string) string {
	remoteCmd := "up -d --wait --remove-orphans --pull always"
	if t.Mode == remote.ModeBuild {
		remoteCmd = "up -d --wait --remove-orphans --build"
	}
	return fmt.Sprintf(`@%s has no %s steps: define them in %s, for example:

  targets:
    %s:
      %s:
        before: []        # local devopsy commands, before the upload
        remote: %s
        after: []         # local devopsy commands, once it is live

remote is one devopsy command, run on the server after the switch; usually a
project command (deploy) doing that and more. See 'devopsy @%s --%s --help'.`,
		t.Name, which, t.File, cmp.Or(t.Pattern, t.Name), which, remoteCmd, t.Name, which)
}

// targetEnv renders a target's env as the release's .devopsy/target.env.
// commit is the release's git commit, if any.
func targetEnv(t *remote.Target, projectName, commit string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by devopsy from the %q target in config.yaml. Do not edit:\n", t.Name)
	fmt.Fprintf(&b, "# change config.yaml and release again, or override in .env.\n")
	// The project's name, when devopsy derives it: so it is the same however
	// devopsy runs on the server, not "current".
	if projectName != "" && t.Env["COMPOSE_PROJECT_NAME"] == "" {
		b.WriteString(cli.DotenvLine("COMPOSE_PROJECT_NAME", projectName) + "\n")
	}
	// Where the environment sits: project, instance and target.
	if t.Project != nil {
		for _, kv := range [][2]string{{"DEVOPSY_PROJECT", t.Project.Name}, {"DEVOPSY_INSTANCE", t.Instance}, {"DEVOPSY_ENVIRONMENT", t.Name}} {
			if _, ok := t.Env[kv[0]]; !ok {
				b.WriteString(cli.DotenvLine(kv[0], kv[1]) + "\n")
			}
		}
	}
	// The commit released, for image tags: in image mode, compose.yaml can
	// use the image CI built from that commit, so each release, and each
	// rollback, runs its own image.
	if commit != "" && t.Env["DEVOPSY_RELEASE_COMMIT"] == "" {
		b.WriteString(cli.DotenvLine("DEVOPSY_RELEASE_COMMIT", commit) + "\n")
	}
	keys := make([]string, 0, len(t.Env))
	for k := range t.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(cli.DotenvLine(k, t.Env[k]) + "\n")
	}
	return []byte(b.String())
}

// runDestroy implements `devopsy @target --destroy [--yes]`: the
// environment's containers, volumes and directory on the server. It asks
// for the target's name unless --yes.
func runDestroy(t *remote.Target, projectName string, args []string, color bool) int {
	yes := false
	for _, a := range args {
		switch a {
		case "--yes", "-y":
			yes = true
		default:
			cli.Fprint(os.Stderr, red, "usage: devopsy @"+t.Name+" --destroy [--yes]", color)
			return 1
		}
	}
	where := t.Host + ":" + t.Path
	if !yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			cli.Fprint(os.Stderr, red, "--destroy removes "+where+" with its data: add --yes when not at a terminal", color)
			return 1
		}
		fmt.Fprintf(os.Stderr, "This removes %s: its containers, volumes, releases and shared/ (data, .env). Type %s to go on: ", where, t.Name)
		line, _ := stdinReader.ReadString('\n')
		if strings.TrimSpace(line) != t.Name {
			cli.Fprint(os.Stderr, red, "Nothing removed.", color)
			return 1
		}
	}
	code, err := remote.SSH(t, remote.DestroyScript(t, projectName), bytes.NewReader(nil), nil, false)
	if err != nil {
		cli.Fprint(os.Stderr, red, err.Error(), color)
		return 1
	}
	return code
}
