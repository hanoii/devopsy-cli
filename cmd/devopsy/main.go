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
	red    = "\033[0;31m"
	cyan   = "\033[0;36m"
	yellow = "\033[0;33m"
)

func main() {
	os.Exit(run())
}

func run() int {
	color := term.IsTerminal(int(os.Stderr.Fd()))
	args := os.Args[1:]

	cwd, err := os.Getwd()
	if err != nil {
		cli.Fprint(os.Stderr, red, err.Error(), color)
		return 1
	}

	// Built-ins are flags, so they never clash with project commands or
	// docker compose commands, which are words.
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		projectDir, _ := cli.FindProjectDir(cwd)
		fmt.Print(cli.Usage(projectDir))
		return 0
	}
	if args[0] == "--version" {
		fmt.Printf("devopsy %s\n", version)
		if out, err := exec.Command("docker", "compose", "version", "--short").Output(); err == nil {
			fmt.Printf("docker compose %s\n", strings.TrimSpace(string(out)))
		} else {
			fmt.Println("docker compose: not available")
		}
		return 0
	}
	if args[0] == "--upgrade" {
		return runUpgrade(args[1:], color)
	}
	if args[0] == upgradeCheck {
		return runUpgradeCheck()
	}
	updateNotice(color)
	if strings.HasPrefix(args[0], "@") {
		return runRemote(cwd, args, color)
	}

	plan, err := cli.Build(cwd, args, os.Environ())
	var help *cli.Help
	var exitErr *cli.ExitError
	var output *cli.Output
	switch {
	case errors.As(err, &output):
		fmt.Print(output.Text)
		return 0
	case errors.As(err, &help):
		fmt.Fprint(os.Stderr, help.Text)
		return help.Code
	case errors.As(err, &exitErr):
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
