// Command devopsy is a thin wrapper around `docker compose` for projects that
// keep their deployment in a .devopsy/ directory. See README.md.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/hanoii/devopsy-cli/internal/cli"
)

// Set at build time by GoReleaser.
var version = "dev"

const (
	red  = "\033[0;31m"
	cyan = "\033[0;36m"
)

func main() {
	os.Exit(run())
}

func run() int {
	color := term.IsTerminal(int(os.Stderr.Fd()))
	args := os.Args[1:]

	// `devopsy version` also prints compose's, which it passes through to
	// inside a project.
	isVersion := len(args) > 0 && args[0] == "version"
	if isVersion {
		fmt.Printf("devopsy %s\n", version)
	}

	cwd, err := os.Getwd()
	if err != nil {
		cli.Fprint(os.Stderr, red, err.Error(), color)
		return 1
	}

	if len(args) > 0 && strings.HasPrefix(args[0], "@") {
		return runRemote(cwd, args, color)
	}

	plan, err := cli.Build(cwd, args, os.Environ())
	var help *cli.Help
	var exitErr *cli.ExitError
	switch {
	case errors.As(err, &help):
		fmt.Fprint(os.Stderr, help.Text)
		return help.Code
	case errors.As(err, &exitErr):
		if isVersion && exitErr.Code == 100 {
			return 0
		}
		cli.Fprint(os.Stderr, red, exitErr.Msg, color)
		return exitErr.Code
	case err != nil:
		cli.Fprint(os.Stderr, red, err.Error(), color)
		return 1
	}

	if plan.Notice != "" {
		cli.Fprint(os.Stderr, cyan, plan.Notice, color)
	}
	path := plan.Path
	if path == "docker" {
		if path, err = exec.LookPath("docker"); err != nil {
			cli.Fprint(os.Stderr, red, "docker not found in PATH", color)
			return 127
		}
	}
	// Replace this process, so signals and the terminal go straight to the
	// command, as with the shell version's exec.
	err = syscall.Exec(path, plan.Args, plan.Env)
	// A script without a shebang: run it with sh, as a shell's exec would.
	if errors.Is(err, syscall.ENOEXEC) {
		err = syscall.Exec("/bin/sh", append([]string{"/bin/sh"}, plan.Args...), plan.Env)
	}
	cli.Fprint(os.Stderr, red, fmt.Sprintf("%s: %v", plan.Args[0], err), color)
	return 126
}
