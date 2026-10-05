package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	projectName := ""
	if v := os.Getenv("COMPOSE_PROJECT_NAME"); v != "" {
		projectName = v
	} else if named, err := cli.HasTopLevelName(filepath.Join(projectDir, "compose.yaml")); err != nil {
		return fail(err.Error())
	} else if !named {
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

	switch args[0] {
	case "releases":
		var out bytes.Buffer
		code, err := remote.SSH(t, remote.ReleasesScript(t), bytes.NewReader(nil), &out, false)
		if err != nil {
			return fail(err.Error())
		}
		fmt.Print(remote.FormatReleases(out.String()))
		return code

	case "rollback":
		cli.Fprint(os.Stderr, cyan, fmt.Sprintf("Rolling back %s (%s:%s)...", t.Name, t.Host, t.Path), color)
		return ssh(remote.ActivateScript(t, "", true, projectName, args[1:]), nil, tty)

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
			pw.CloseWithError(remote.Pack(pw, projectRoot, files, record.JSON()))
		}()
		if code := ssh(remote.UploadScript(t, record.ID), pr, false); code != 0 {
			return code
		}
		return ssh(remote.ActivateScript(t, record.ID, false, projectName, args[1:]), nil, tty)

	default:
		return ssh(remote.RunScript(t, projectName, args), nil, tty)
	}
}
