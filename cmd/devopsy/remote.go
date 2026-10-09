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

	// Outside a project only aliases exist.
	projectDir, _ := cli.FindProjectDir(cwd)
	// The project's .env can name the server and instance (DEVOPSY_SERVER,
	// DEVOPSY_INSTANCE).
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
	// @[<server>:][<instance>/]<environment>, or an alias. Help needs no
	// server: it only explains.
	addr := strings.TrimPrefix(args[0], "@")
	load := remote.LoadTarget
	if len(args) == 1 || args[1] == "--help" || args[1] == "-h" || (len(args) > 2 && (args[2] == "--help" || args[2] == "-h")) {
		load = remote.DescribeTarget
	}
	t, err := load(projectDir, addr, projectEnv)
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

	// environments and schema explain local config: on a server they would
	// describe the release's copy and the server's own user config.
	if args[0] == "--debug" && len(args) > 1 && (args[1] == "environments" || args[1] == "schema") {
		return fail(fmt.Sprintf("--debug %s is about this machine's config: run devopsy --debug %s, without a target", args[1], strings.Join(args[1:], " ")))
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

	// The compose project: <project>[-<instance>]-<environment>, whatever
	// compose.yaml's name: says. A user-level target without source belongs
	// to no local project: the server's own files name it (target.env in
	// releases). A nested devopsy (DEVOPSY_PROJECT_DIR set: a project
	// command, or a release step, calling devopsy @target) inherits the local
	// project's COMPOSE_PROJECT_NAME, which says nothing about the target:
	// ignore it.
	projectName := ""
	if v := os.Getenv("COMPOSE_PROJECT_NAME"); v != "" && os.Getenv("DEVOPSY_PROJECT_DIR") == "" {
		projectName = v
	} else if t.Project != nil {
		projectName = t.ComposeName()
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

	case "--ssh-config":
		if len(args) > 1 {
			return fail("--ssh-config takes no arguments")
		}
		// ssh -G only reads config; without it the block has no "Now" lines.
		effective, _ := remote.SSHEffective(t.Host)
		fmt.Print(remote.SSHConfig(t, effective))
		return 0

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

	case "--log":
		if len(args) > 2 || (len(args) == 2 && strings.HasPrefix(args[1], "-")) {
			return fail("usage: devopsy @" + t.Name + " --log [<release id>]")
		}
		prefix := ""
		if len(args) == 2 {
			prefix = args[1]
		}
		return ssh(remote.LogReadScript(t, prefix), bytes.NewReader(nil), false)

	case "--release", "--rollback":
		yes := len(args) == 2 && (args[1] == "--yes" || args[1] == "-y") && args[0] == "--release"
		if len(args) > 1 && !yes {
			return fail(fmt.Sprintf("%s takes no command: what it runs is the environment's %s: steps in %s", args[0], strings.TrimPrefix(args[0], "--"), t.File))
		}
		steps := t.Release
		if args[0] == "--rollback" {
			steps = t.Rollback
		}
		if steps == nil || len(remote.StepArgs(steps.Run)) == 0 {
			return fail(missingSteps(t, strings.TrimPrefix(args[0], "--")))
		}
		// When run fails, current goes back to the previous release, and its
		// own run step brings its containers back: the rollback's, else the
		// release's.
		restart := t.Rollback
		if restart == nil || len(remote.StepArgs(restart.Run)) == 0 {
			restart = t.Release
		}
		phases := remote.RemotePhases(steps, restart)

		// The whole run is recorded and saved on the server once it has
		// been touched: devopsy @<target> --log.
		what := strings.TrimPrefix(args[0], "--")
		rlog := newReleaseLog(what, t)
		say := func(c, msg string) {
			cli.Fprint(os.Stderr, c, msg, color)
			rlog.Line(msg)
		}
		stop := func(msg string) int {
			rlog.Line(msg)
			return fail(msg)
		}
		session := func(script string) int {
			code, err := remote.SSHLog(t, script, nil, nil, tty, rlog)
			if err != nil {
				return stop(err.Error())
			}
			return code
		}
		// save ends the log with code and saves it in release id's
		// directory on the server: a rollback's is appended to the log of
		// the release it restores, which its script names.
		save := func(id string, code int) int {
			rlog.Line(fmt.Sprintf("devopsy: %s exited %d", what, code))
			text := rlog.Text()
			rollback := id == ""
			if rollback {
				m := rollingBackTo.FindSubmatch(text)
				if m == nil {
					return code
				}
				id = string(m[1])
				text = append([]byte("\n"), text...)
			}
			if c, err := remote.SSH(t, remote.LogSaveScript(t, id, rollback), bytes.NewReader(text), io.Discard, false); err != nil || c != 0 {
				cli.Fprint(os.Stderr, yellow, fmt.Sprintf("devopsy: could not save the %s log on the server (%v, %d)", what, err, c), color)
			}
			return code
		}
		local := func(phase string, list []string, commit string) int {
			for _, step := range list {
				say(cyan, fmt.Sprintf("Running 'devopsy %s' locally (%s)...", step, phase))
				if code := runLocalStep(t, remote.StepArgs(step), commit); code != 0 {
					rlog.Line(fmt.Sprintf("devopsy: 'devopsy %s' exited %d", step, code))
					return code
				}
			}
			return 0
		}

		if args[0] == "--rollback" {
			if code := local("before", remote.Commands(steps.Before, false), ""); code != 0 {
				return fail(fmt.Sprintf("a before step failed (%d): nothing changed on %s", code, t.Name))
			}
			say(cyan, fmt.Sprintf("Rolling back %s (%s:%s)...", t.Address, t.Host, t.Path))
			if code := session(remote.ActivateScript(t, "", true, projectName, phases)); code != 0 {
				return save("", code)
			}
			if code := local("after", remote.Commands(steps.After, false), ""); code != 0 {
				return save("", stop(fmt.Sprintf("an after step failed (%d): the rollback stays", code)))
			}
			return save("", 0)
		}

		// A role taken by another compose project on the server fails before
		// anything is uploaded (--prepare-release checks it again, under the
		// release lock).
		if role, _, err := cli.ProjectRoles(projectDir); err != nil {
			return fail(err.Error())
		} else if role != "" {
			var out bytes.Buffer
			if code, err := remote.SSH(t, remote.RoleHoldersScript(t, role), bytes.NewReader(nil), &out, false); err != nil {
				return fail(err.Error())
			} else if code != 0 {
				return code
			}
			for _, holder := range strings.Fields(out.String()) {
				if holder != projectName {
					return fail(fmt.Sprintf("role %s is held by %s on %s: one compose project per role (%s=%s). Nothing changed.", role, holder, t.Host, cli.RoleLabel, role))
				}
			}
		}

		// A new instance is a new site: confirm it, unless --yes.
		if t.Instance != "" && !yes {
			code, err := remote.SSH(t, remote.InstanceExistsScript(t), bytes.NewReader(nil), io.Discard, false)
			if err != nil {
				return fail(err.Error())
			}
			if code != 0 {
				q := fmt.Sprintf("New instance: %s has no %q instance of %s yet.\nThis release creates it: %s, compose project %s, its own data and URL.",
					t.Host, t.Instance, t.Project.Name, t.Path, t.ComposeName())
				if !term.IsTerminal(int(os.Stdin.Fd())) {
					return fail(q + "\nAdd --yes when not at a terminal: devopsy @" + t.Address + " --release --yes")
				}
				fmt.Fprintf(os.Stderr, "%s\nCreate it? [y/N] ", q)
				line, _ := stdinReader.ReadString('\n')
				if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
					return fail("Nothing released.")
				}
			}
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
		if code := local("before", remote.Commands(steps.Before, false), commit); code != 0 {
			return fail(fmt.Sprintf("a before step failed (%d): nothing changed on %s", code, t.Address))
		}
		src := record.Mode
		if record.Commit != "" {
			src += ", " + record.Branch + "@" + record.Commit[:min(10, len(record.Commit))]
			if record.Dirty {
				src += " with uncommitted changes"
			}
		}
		say(cyan, fmt.Sprintf("Releasing %s to %s (%s:%s): %d files, %s...",
			record.ID, t.Address, t.Host, t.Path, len(files), src))
		if t.Mode == remote.ModeImage && record.Dirty && remote.DirtyOutsideDevopsy(projectRoot) {
			say(yellow, fmt.Sprintf("Uncommitted changes outside .devopsy/ are not released in image mode: images come from the registry (DEVOPSY_RELEASE_COMMIT=%s).",
				record.Commit[:min(10, len(record.Commit))]))
		}

		pr, pw := io.Pipe()
		go func() {
			pw.CloseWithError(remote.Pack(pw, projectRoot, files, map[string][]byte{
				remote.RecordFile:                  record.JSON(),
				".devopsy/" + remote.TargetEnvFile: targetEnv(t, projectName, record.Commit),
			}))
		}()
		if code, err := remote.SSHLog(t, remote.UploadScript(t, record.ID), pr, nil, false, rlog); err != nil || code != 0 {
			if err != nil {
				return stop(err.Error())
			}
			return code
		}
		if code := session(remote.ActivateScript(t, record.ID, false, projectName, phases)); code != 0 {
			return save(record.ID, code)
		}
		if code := local("after", remote.Commands(steps.After, false), commit); code != 0 {
			return save(record.ID, stop(fmt.Sprintf("an after step failed (%d): the release stays", code)))
		}
		return save(record.ID, 0)

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
	cmd.Env = append(cmd.Env, "DEVOPSY_TARGET="+t.Address, "DEVOPSY_ENVIRONMENT="+t.Name, "DEVOPSY_INSTANCE="+t.Instance)
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

// missingSteps explains how to define an environment's release or rollback steps,
// with a starting point for its mode.
func missingSteps(t *remote.Target, which string) string {
	remoteCmd := "up -d --wait --remove-orphans --pull always"
	if t.Mode == remote.ModeBuild {
		remoteCmd = "up -d --wait --remove-orphans --build"
	}
	return fmt.Sprintf(`environment %s has no %s steps: define them in %s, for example:

  environments:
    %s:
      %s: %s

That is the run step alone: one devopsy command on the server, once the
release is current; usually a project command (deploy). before, prepare
and after steps can go around it. See 'devopsy @%s --%s --help'.`,
		t.Name, which, t.File, cmp.Or(t.Pattern, t.Name), which, remoteCmd, t.Name, which)
}

func missingDestroy(t *remote.Target) string {
	return fmt.Sprintf(`environment %s has no destroy step: define it in %s, for example:

  environments:
    %s:
      destroy: destroy

One devopsy command on the server, in the current release, before devopsy
removes the directory: usually a project command that runs down --volumes
and removes what its containers own in shared/mnt. See 'devopsy @%s
--destroy --help'.`, t.Name, cmp.Or(t.File, "the project's config"), cmp.Or(t.Pattern, t.Name), t.Name)
}

// targetEnv renders a target's env as the release's .devopsy/target.env.
// commit is the release's git commit, if any.
func targetEnv(t *remote.Target, projectName, commit string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by devopsy from the %q environment in config.yaml. Do not edit:\n", t.Name)
	fmt.Fprintf(&b, "# change config.yaml and release again, or override in .env.\n")
	// The project's name, when devopsy derives it: so it is the same however
	// devopsy runs on the server, not "current".
	if projectName != "" && t.Env["COMPOSE_PROJECT_NAME"] == "" {
		b.WriteString(cli.DotenvLine("COMPOSE_PROJECT_NAME", projectName) + "\n")
	}
	// Where the environment sits: project, instance and target.
	if t.Project != nil {
		for _, kv := range [][2]string{{"DEVOPSY_PROJECT", t.Project.Name}, {"DEVOPSY_INSTANCE", t.Instance}, {"DEVOPSY_ENVIRONMENT", t.Name}, {"DEVOPSY_TARGET", t.Address}} {
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
// environment's destroy step, then its directory on the server. It asks for
// the target's name unless --yes.
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
	steps := remote.StepArgs(t.Destroy)
	if len(steps) == 0 {
		cli.Fprint(os.Stderr, red, missingDestroy(t), color)
		return 1
	}
	where := t.Host + ":" + t.Path
	if !yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			cli.Fprint(os.Stderr, red, "--destroy removes "+where+" with its data: add --yes when not at a terminal", color)
			return 1
		}
		fmt.Fprintf(os.Stderr, "This runs 'devopsy %s' in %s, then removes it: its releases and shared/ (data, .env). Type %s to go on: ", strings.Join(steps, " "), where, t.Name)
		line, _ := stdinReader.ReadString('\n')
		if strings.TrimSpace(line) != t.Name {
			cli.Fprint(os.Stderr, red, "Nothing removed.", color)
			return 1
		}
	}
	code, err := remote.SSH(t, remote.DestroyScript(t, projectName, steps), bytes.NewReader(nil), nil, false)
	if err != nil {
		cli.Fprint(os.Stderr, red, err.Error(), color)
		return 1
	}
	return code
}
