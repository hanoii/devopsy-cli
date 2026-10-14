package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// CommandTargetEnv is the address a project command with `## Needs:` was
// given, for it to pass on: devopsy @"$DEVOPSY_COMMAND_TARGET" ...
const CommandTargetEnv = "DEVOPSY_COMMAND_TARGET"

// commandTargetInternal carries that address from the devopsy that read
// it to the one it execs locally, which runs the command.
const commandTargetInternal = "DEVOPSY_INTERNAL_COMMAND_TARGET"

// RunsLocallyNote follows, in help, a local command that takes an
// address, whose scope then looks like a remote one's.
const RunsLocallyNote = "(runs locally)"

// CommandRun is where a project command runs, from its `## Run:` and
// `## Needs:` lines.
type CommandRun struct {
	// Run is "" (where it is called: locally, or on the target with one),
	// "local" (this machine only) or "target" (in a release only).
	Run string
	// Needs is "", "target" or "server": the address a local command
	// takes, which it gets in DEVOPSY_COMMAND_TARGET as typed.
	Needs string
}

// Where is where devopsy is asked to run a project command.
type Where int

const (
	// Bare is local, without an address.
	Bare Where = iota
	// Addressed is local, with an address: a local command that needs one.
	Addressed
	// OnServer is in a release, on a server.
	OnServer
)

// ReadCommandRun reads a command's `## Run:` and `## Needs:` lines.
func ReadCommandRun(path string) (CommandRun, error) {
	c := CommandRun{
		Run:   strings.ToLower(commandHeader(path, "## Run:")),
		Needs: strings.ToLower(commandHeader(path, "## Needs:")),
	}
	if c.Run != "" && c.Run != "local" && c.Run != "target" {
		return CommandRun{}, fmt.Errorf("%s: ## Run: %s is not local or target", path, c.Run)
	}
	if c.Needs != "" && c.Needs != "target" && c.Needs != "server" {
		return CommandRun{}, fmt.Errorf("%s: ## Needs: %s is not target or server", path, c.Needs)
	}
	if c.Needs == "server" && c.Run != "local" {
		return CommandRun{}, fmt.Errorf("%s: ## Needs: server takes ## Run: local: on a server, a command comes from a target's release", path)
	}
	// Needs a target and runs where it is called: on the target.
	if c.Needs == "target" && c.Run == "" {
		c.Run = "target"
	}
	return c, nil
}

// RunsLocal reports whether the command runs locally when it is
// given an address.
func (c CommandRun) RunsLocal() bool { return c.Run == "local" && c.Needs != "" }

// Scope is the command's scope in help, like a flag's.
func (c CommandRun) Scope() Scope {
	switch {
	case c.Needs == "server":
		return Server
	case c.Needs == "target", c.Run == "target":
		return Target
	case c.Run == "local":
		return Local
	}
	return LocalOrTarget
}

// Check refuses the command where its lines say it does not run.
func (c CommandRun) Check(name, path string, where Where) error {
	switch where {
	case Bare:
		switch {
		case c.Needs != "":
			return fmt.Errorf("%s needs a %s (## Needs: %s in %s): devopsy @<%s> %s", name, c.Needs, c.Needs, path, c.Needs, name)
		case c.Run == "target":
			return fmt.Errorf("%s runs only on a target (## Run: target in %s): devopsy @<target> %s", name, path, name)
		}
	case Addressed:
		if c.Run == "local" && c.Needs == "" {
			return fmt.Errorf("%s runs only on your machine and takes no address (## Run: local in %s): devopsy %s", name, path, name)
		}
	case OnServer:
		if c.Run == "local" {
			return fmt.Errorf("%s runs only on your machine (## Run: local in %s), never on a server", name, path)
		}
	}
	return nil
}

// CheckCommand reads a project command's lines and checks them for where.
func CheckCommand(projectDir, name string, where Where) (CommandRun, error) {
	path := filepath.Join(projectDir, "commands", name)
	c, err := ReadCommandRun(path)
	if err != nil {
		return c, err
	}
	return c, c.Check(name, path, where)
}

// CommandOffered reports whether completion offers a command: locally, or
// after an address.
func CommandOffered(projectDir, name string, addressed bool) bool {
	c, err := ReadCommandRun(filepath.Join(projectDir, "commands", name))
	if err != nil {
		return false
	}
	if !addressed {
		return c.Check(name, "", Bare) == nil
	}
	return c.RunsLocal() || c.Check(name, "", OnServer) == nil
}

// InRelease reports whether projectDir is a release's .devopsy/, on a
// server: every release has target.env, a checkout never does.
func InRelease(projectDir string) bool {
	_, err := os.Stat(filepath.Join(projectDir, remote.TargetEnvFile))
	return err == nil
}

// CommandTargetEnviron is environ for the devopsy that runs a local
// command given address.
func CommandTargetEnviron(environ []string, address string) []string {
	return append(environ, commandTargetInternal+"="+address)
}
