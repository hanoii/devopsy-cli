package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
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
	// urlName is the name the public URL uses, when it is known here: not when
	// compose.yaml's name depends on the server's environment.
	// A user-level target belongs to no local project: the server's own
	// files name it (target.env in releases, the directory otherwise).
	projectName, urlName := "", ""
	if v := os.Getenv("COMPOSE_PROJECT_NAME"); v != "" {
		projectName = v
	} else if t.User {
	} else if raw, err := cli.TopLevelName(filepath.Join(projectDir, "compose.yaml")); err != nil {
		return fail(err.Error())
	} else if raw == "" {
		projectName = cli.NormalizeProjectName(filepath.Base(t.Path))
	} else if !strings.Contains(raw, "$") {
		urlName = raw
	}
	if urlName == "" {
		urlName = projectName
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

	case "rollback":
		cli.Fprint(os.Stderr, cyan, fmt.Sprintf("Rolling back %s (%s:%s)...", t.Name, t.Host, t.Path), color)
		return ssh(remote.ActivateScript(t, "", true, projectName, urlName, args[1:]), nil, tty)

	case "release":
		projectRoot := filepath.Dir(projectDir)
		files, err := remote.Files(projectRoot, t.Mode)
		if err != nil {
			return fail(err.Error())
		}
		record := remote.NewRecord(projectRoot, t.Mode, time.Now())
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
		return ssh(remote.ActivateScript(t, record.ID, false, projectName, urlName, args[1:]), nil, tty)

	default:
		return ssh(remote.RunScript(t, projectName, args), nil, tty)
	}
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

// runDomains implements `devopsy @target domains [--retry]`.
func runDomains(t *remote.Target, projectName string, retry bool, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(os.Stderr, red, msg, color)
		return 1
	}
	check := func() ([]remote.DomainReport, *remote.Facts, int) {
		var out bytes.Buffer
		code, err := remote.SSH(t, remote.DomainsScript(t, projectName), bytes.NewReader(nil), &out, false)
		if err != nil {
			return nil, nil, fail(err.Error())
		}
		if code != 0 {
			return nil, nil, code
		}
		facts, err := remote.ParseFacts(out.String())
		if err != nil {
			return nil, nil, fail(err.Error())
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		checker := remote.PublicChecker(ctx)
		// The server's addresses: as its public host resolves (right behind
		// NAT too), then as it reports itself.
		var serverIPs []string
		if facts.Env["DEVOPSY_PUBLIC_DOMAIN"] != "" {
			if ips, err := checker.LookupIP(ctx, facts.Env["DEVOPSY_PUBLIC_HOST"]); err == nil {
				serverIPs = append(serverIPs, ips...)
			}
		}
		if facts.ServerIP != "" {
			serverIPs = append(serverIPs, facts.ServerIP)
		}
		// On the server's own Traefik, every domain on the server; on a
		// project, its own.
		hosts := facts.Hosts()
		if path.Clean(t.Path) == path.Clean(facts.TraefikDir) {
			hosts = facts.ServerHosts()
		}
		return remote.Check(ctx, facts, checker, serverIPs, hosts), facts, 0
	}

	var project string

	reports, facts, code := check()
	if code != 0 {
		return code
	}
	fmt.Print(remote.FormatReports(reports))
	project = cli.NormalizeProjectName(facts.Env["DEVOPSY_PROJECT_NAME"])

	// A previous --retry's file is only clutter once every certificate exists.
	cleanup := func(reports []remote.DomainReport) {
		if !remote.AllCertified(reports) {
			return
		}
		var out bytes.Buffer
		if _, err := remote.SSH(t, remote.CleanupRetryScript(facts.TraefikDir, project), bytes.NewReader(nil), &out, false); err == nil && strings.TrimSpace(out.String()) == "removed" {
			cli.Fprint(os.Stderr, cyan, "All certificates exist: removed the retry file from Traefik's configuration.", color)
		}
	}
	if !retry {
		cleanup(reports)
		return 0
	}

	pending := map[string][]string{}
	for _, r := range reports {
		if r.Routed && !r.Cert.Valid && r.Resolver != "" {
			pending[r.Resolver] = append(pending[r.Resolver], r.Host)
		}
	}
	if len(pending) == 0 {
		cli.Fprint(os.Stderr, cyan, "Nothing to retry.", color)
		return 0
	}
	if code, err := remote.SSH(t, remote.RetryScript(facts.TraefikDir, project, pending, time.Now()), bytes.NewReader(nil), nil, false); err != nil {
		return fail(err.Error())
	} else if code != 0 {
		return code
	}
	cli.Fprint(os.Stderr, cyan, "Asked Traefik to request the missing certificates. Checking again in 45 seconds...", color)
	time.Sleep(45 * time.Second)
	reports, _, code = check()
	if code != 0 {
		return code
	}
	fmt.Print(remote.FormatReports(reports))
	cleanup(reports)
	return 0
}
