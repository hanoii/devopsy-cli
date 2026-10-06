package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

const remoteUsage = `Usage: devopsy @<target> <command> [args...]

  release [cmd...]    upload the project as a new release and make it current;
                      with cmd, run 'devopsy cmd' there and go back to the
                      previous release if it fails
  rollback [cmd...]   make the release before the current one current again,
                      same cmd handling
  releases            list the releases on the server
  domains [--retry]   check the environment's public host and DEVOPSY_DOMAINS:
                      DNS, acme-dns challenge, certificate, and what to do
                      next; --retry asks Traefik for missing certificates
  <anything else>     run 'devopsy <anything else>' in the current release

Targets are defined in .devopsy/targets.yaml.
`

// runRemote handles `devopsy @target ...`.
func runRemote(cwd string, args []string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(os.Stderr, red, msg, color)
		return 1
	}

	projectDir, err := cli.FindProjectDir(cwd)
	if err != nil {
		return fail(err.Error())
	}
	t, err := remote.LoadTarget(projectDir, strings.TrimPrefix(args[0], "@"))
	if err != nil {
		return fail(err.Error())
	}
	args = args[1:]
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprint(os.Stderr, remoteUsage)
		if len(args) == 0 {
			return 1
		}
		return 0
	}

	// Without a top-level name, compose would name the project after the
	// release directory, so fix it to the target directory's name.
	// urlName is the name the public URL uses, when it is known here: not when
	// compose.yaml's name depends on the server's environment.
	projectName, urlName := "", ""
	if v := os.Getenv("COMPOSE_PROJECT_NAME"); v != "" {
		projectName = v
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

	switch args[0] {
	case "releases":
		var out bytes.Buffer
		code, err := remote.SSH(t, remote.ReleasesScript(t), bytes.NewReader(nil), &out, false)
		if err != nil {
			return fail(err.Error())
		}
		fmt.Print(remote.FormatReleases(out.String()))
		return code

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

		pr, pw := io.Pipe()
		go func() {
			pw.CloseWithError(remote.Pack(pw, projectRoot, files, map[string][]byte{
				remote.RecordFile:                  record.JSON(),
				".devopsy/" + remote.TargetEnvFile: targetEnv(t),
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
func targetEnv(t *remote.Target) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# Written by devopsy from the %q target in targets.yaml. Do not edit:\n", t.Name)
	fmt.Fprintf(&b, "# change targets.yaml and release again, or override in .env.\n")
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
		checker := remote.PublicChecker()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
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
		return remote.Check(ctx, facts, checker, serverIPs), facts, 0
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
