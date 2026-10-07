package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
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
	t, err := remote.LoadTarget(projectDir, strings.TrimPrefix(args[0], "@"), projectEnv)
	if err != nil {
		return fail(err.Error())
	}
	args = args[1:]
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(cli.RemoteHelp)
		return 0
	}

	// Without a top-level name, compose would name the project after the
	// release directory, so fix it to the target directory's name.
	// A user-level target belongs to no local project: the server's own
	// files name it (target.env in releases, the directory otherwise).
	// A nested devopsy (DEVOPSY_PROJECT_DIR set: a project command, or a
	// release step, calling devopsy @target) inherits the local project's
	// COMPOSE_PROJECT_NAME, which says nothing about the target: ignore it.
	projectName := ""
	if v := os.Getenv("COMPOSE_PROJECT_NAME"); v != "" && os.Getenv("DEVOPSY_PROJECT_DIR") == "" {
		projectName = v
	} else if t.User {
	} else if raw, err := cli.TopLevelName(filepath.Join(projectDir, "compose.yaml")); err != nil {
		return fail(err.Error())
	} else if raw == "" {
		projectName = cli.NormalizeProjectName(filepath.Base(t.Path))
	}

	tty := term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd()))
	ssh := func(script string, stdin io.Reader, tty bool) int {
		code, err := remote.SSH(t, script, stdin, nil, tty)
		if err != nil {
			return fail(err.Error())
		}
		return code
	}

	// `devopsy @t release --help` explains instead of releasing.
	if help, ok := cli.RemoteCommandHelp[args[0]]; ok && len(args) > 1 && (args[1] == "--help" || args[1] == "-h") {
		fmt.Print(help)
		return 0
	}

	// A user-level target belongs to no project, so nothing may be released
	// to it from wherever devopsy happens to run.
	if t.User && (args[0] == "release" || args[0] == "rollback") {
		return fail(fmt.Sprintf("@%s is a user-level target (%s): %s needs a target defined by the project, in .devopsy/targets.yaml", t.Name, t.File, args[0]))
	}

	switch args[0] {
	case "--shell":
		if len(args) > 1 {
			return fail("--shell takes no arguments: for one command, use 'devopsy @" + t.Name + " <command>'")
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

	case "releases":
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

	case "domains":
		retry := len(args) > 1 && args[1] == "--retry"
		return runDomains(t, projectName, retry, color)

	case "release", "rollback":
		if len(args) > 1 {
			return fail(fmt.Sprintf("%s takes no command: what it runs is the target's %s: steps in %s", args[0], args[0], t.File))
		}
		steps := t.Release
		if args[0] == "rollback" {
			steps = t.Rollback
		}
		if steps == nil || len(remote.StepArgs(steps.Remote)) == 0 {
			return fail(missingSteps(t, args[0]))
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

		if args[0] == "rollback" {
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

  %s:
    %s:
      before: []          # local devopsy commands, before the upload
      remote: %s
      after: []           # local devopsy commands, once it is live

remote is one devopsy command, run on the server after the switch; usually a
project command (deploy) doing that and more. See 'devopsy @%s %s --help'.`,
		t.Name, which, t.File, t.Name, which, remoteCmd, t.Name, which)
}

// targetEnv renders a target's env as the release's .devopsy/target.env.
// commit is the release's git commit, if any.
func targetEnv(t *remote.Target, projectName, commit string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by devopsy from the %q target in targets.yaml. Do not edit:\n", t.Name)
	fmt.Fprintf(&b, "# change targets.yaml and release again, or override in .env.\n")
	// The project's name, when devopsy derives it from the target path: so it is
	// the same however devopsy runs on the server, not "current".
	if projectName != "" && t.Env["COMPOSE_PROJECT_NAME"] == "" {
		b.WriteString(cli.DotenvLine("COMPOSE_PROJECT_NAME", projectName) + "\n")
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

// runDomains implements `devopsy @target domains [--retry]`. The server's
// proxy reports what it knows through its domains capability; the checks
// run here, from outside, as visitors see the hosts.
func runDomains(t *remote.Target, projectName string, retry bool, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(os.Stderr, red, msg, color)
		return 1
	}
	var out bytes.Buffer
	code, err := remote.SSH(t, remote.EnvScript(t, projectName), bytes.NewReader(nil), &out, false)
	if err != nil {
		return fail(err.Error())
	}
	if code != 0 {
		return code
	}
	env, err := remote.ParseEnv(out.String())
	if err != nil {
		return fail(err.Error())
	}
	proxyDir := env["DEVOPSY_PROXY_DIR"]
	if proxyDir == "" {
		proxyDir = remote.DefaultProxyDir
	}
	// On the proxy's own target, every host it routes; on a project, its own.
	server := path.Clean(t.Path) == path.Clean(proxyDir)
	hosts, name := remote.ProjectHosts(env), cli.NormalizeProjectName(env["DEVOPSY_PROJECT_NAME"])
	factsArgs := hosts
	if server {
		factsArgs, name = []string{"--all"}, "server"
	} else if len(hosts) == 0 {
		return fail("no hosts: set DEVOPSY_PUBLIC_DOMAIN or DEVOPSY_DOMAINS for this target")
	}
	capability := func(action string, args []string, stdout io.Writer) int {
		code, err := remote.SSH(t, remote.CapabilityScript(proxyDir, "domains", action, args), bytes.NewReader(nil), stdout, false)
		if err != nil {
			return fail(err.Error())
		}
		return code
	}

	check := func() ([]remote.DomainReport, *remote.ProxyFacts, int) {
		var out bytes.Buffer
		if code := capability("facts", factsArgs, &out); code != 0 {
			return nil, nil, code
		}
		facts, err := remote.ParseProxyFacts(out.Bytes())
		if err != nil {
			return nil, nil, fail(err.Error())
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		checker := remote.PublicChecker(ctx)
		// The server's addresses: as its public host resolves (right behind
		// NAT too), then as the proxy reports it.
		var serverIPs []string
		if env["DEVOPSY_PUBLIC_DOMAIN"] != "" && env["DEVOPSY_PUBLIC_HOST"] != "" {
			if ips, err := checker.LookupIP(ctx, env["DEVOPSY_PUBLIC_HOST"]); err == nil {
				serverIPs = append(serverIPs, ips...)
			}
		}
		if facts.IP != "" {
			serverIPs = append(serverIPs, facts.IP)
		}
		checked := hosts
		if server {
			checked = facts.AllHosts()
		}
		return remote.Check(ctx, facts, checker, serverIPs, checked), facts, 0
	}

	reports, facts, code := check()
	if code != 0 {
		return code
	}
	fmt.Print(remote.FormatReports(reports))

	// A previous --retry is only clutter once every certificate exists.
	cleanup := func(reports []remote.DomainReport, facts *remote.ProxyFacts) {
		if !remote.AllCertified(reports) || !facts.HasRetry(name) {
			return
		}
		if capability("retry", []string{name, "--done"}, io.Discard) == 0 {
			cli.Fprint(os.Stderr, cyan, "All certificates exist: removed the retry request from the proxy.", color)
		}
	}
	if !retry {
		cleanup(reports, facts)
		return 0
	}

	var pending []string
	for _, r := range reports {
		if r.Routed && !r.Cert.Valid {
			pending = append(pending, r.Host)
		}
	}
	if len(pending) == 0 {
		cli.Fprint(os.Stderr, cyan, "Nothing to retry.", color)
		return 0
	}
	if code := capability("retry", append([]string{name}, pending...), io.Discard); code != 0 {
		return code
	}
	cli.Fprint(os.Stderr, cyan, "Asked the proxy to request the missing certificates. Checking again in 45 seconds...", color)
	time.Sleep(45 * time.Second)
	reports, facts, code = check()
	if code != 0 {
		return code
	}
	fmt.Print(remote.FormatReports(reports))
	cleanup(reports, facts)
	return 0
}
