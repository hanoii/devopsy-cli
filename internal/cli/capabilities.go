package cli

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"
)

// Capability describes an interface devopsy defines and a project may
// implement in .devopsy/capabilities/<Name>/<action>. This is what
// `devopsy --debug capabilities` prints, so it lives with the code that
// calls them; README's Capabilities section has the full contracts.
type Capability struct {
	Name string
	// What it lets devopsy do, in a sentence.
	What string
	// Who implements it, and what in devopsy calls it.
	Who, CalledBy string
	Actions       []CapabilityAction
}

// CapabilityAction is one executable of a capability.
type CapabilityAction struct {
	Name, Usage, Does string
	// Output is an example of what it prints, when devopsy reads it.
	Output string
}

// Capabilities are the capabilities this devopsy calls.
var Capabilities = []Capability{
	{
		Name:     "env",
		What:     "values a project derives from its environment, like its hosts from COMPOSE_PROJECT_NAME and an imported wildcard domain, computed by the project so devopsy knows none of them",
		Who:      "projects whose compose files or commands need derived values",
		CalledBy: "every devopsy command in the project (not help or completion), locally and on servers",
		Actions: []CapabilityAction{
			{
				Name:   "compute",
				Usage:  "compute",
				Does:   "gets the project's environment and prints .env lines; devopsy sets those not set yet (the lowest precedence) and devopsy --env shows them. Runs before every command, so keep it fast; devopsy run inside it skips it",
				Output: "SITE_URL='https://shop-prod.vm1.example.com'",
			},
		},
	},
	{
		Name:     "shell",
		What:     "replaces devopsy's own --shell, for a project needing more than its labels say (devopsy.shell picks the service, devopsy.shell.user the user)",
		Who:      "any project; most need only the labels",
		CalledBy: "devopsy [@<target>] --shell [service] [exec options...] [-- command...]",
		Actions: []CapabilityAction{
			{
				Name:  "open",
				Usage: "open [service] [exec options...] [-- command...]",
				Does:  "gets --shell's arguments and opens an interactive shell, or runs the command after --; devopsy execs it with the terminal attached",
			},
		},
	},
	{
		Name:     "sync",
		What:     "hooks into a sync (a child environment's data from its parent's, devopsy @<child> --sync and a child's first release); each action runs only when its file is there, besides devopsy's handling of the devopsy.sync labels (skip by default; live, pause and stop rsynced; custom left to push)",
		Who:      "projects whose data a plain copy of .devopsy/mnt cannot sync, like a database to dump, or that flush caches before a copy",
		CalledBy: "devopsy @<child> --sync, and a child's first release or --release-sync",
		Actions: []CapabilityAction{
			{
				Name:  "flush",
				Usage: "flush <service>",
				Does:  "on each side, once per running service labeled devopsy.sync=live or pause, before it is paused or copied (the child's before its services are paused, the parent's before its own are paused and copied); never for stop, skip or custom. A failure aborts the sync",
			},
			{
				Name:  "prepare",
				Usage: "prepare",
				Does:  "on the child's server, in its current release under its release lock, before the copy: readies it to be written, before devopsy flushes, pauses and stops its labeled services. Gets DEVOPSY_SYNC_FROM_TARGET (the parent's address) and DEVOPSY_SYNC_ARGS (--sync's arguments after --). A failure aborts the sync; finish abort runs",
			},
			{
				Name:  "push",
				Usage: "push",
				Does:  "on the parent's server, in its current release under a shared hold of its lock, after devopsy has copied the services labeled live, pause and stop: copies those labeled custom (required when any is, not run when none is). Gets DEVOPSY_SYNC_CUSTOM (those services, space separated), DEVOPSY_SYNC_TO (user@host of the child's server, empty on the same one), DEVOPSY_SYNC_SSH_PORT, DEVOPSY_SYNC_TO_BASE and DEVOPSY_SYNC_TO_DIR (the child's directory and its current .devopsy/), DEVOPSY_SYNC_TO_PROJECT, DEVOPSY_SYNC_TO_TARGET, DEVOPSY_SYNC_FROM_TARGET and DEVOPSY_SYNC_ARGS, its own to use; stdin is empty. Helpers: devopsy --sync-rsync <entry of mnt>... [-- rsync args] copies those entries as the generic path does; devopsy --sync-exec <devopsy args> runs devopsy in the child's current release as its project, with a clean environment, stdin and stdout through: devopsy exec -T db mariadb-dump app | devopsy --sync-exec exec -T db mariadb app. Exit 0 lets the child go on",
			},
			{
				Name:  "finish",
				Usage: "finish go|abort",
				Does:  "on the child's server, after the copy (go) or a failure (abort), once devopsy has started the stopped labeled services, waited for them to be healthy and unpaused the paused: restores what prepare changed, and after go sanitizes what a copy of production should not keep (scrubbed emails and passwords, a login the child did not have) before the release's run step runs. Same variables as prepare",
			},
		},
	},
}

// ImplementedActions lists the actions of c the project at projectDir (its
// .devopsy/) has.
func ImplementedActions(projectDir string, c Capability) []string {
	var have []string
	if projectDir == "" {
		return nil
	}
	for _, a := range c.Actions {
		if isExecutableFile(filepath.Join(projectDir, "capabilities", c.Name, a.Name)) {
			have = append(have, a.Name)
		}
	}
	return have
}

// EnvGuard is set while the env capability runs, so a devopsy it calls
// does not run it again.
const EnvGuard = "DEVOPSY_ENV_COMPUTE"

// computeEnv runs the project's env capability, if it has one, and sets
// the variables it prints that are not set yet.
func (p *loadedProject) computeEnv() error {
	if _, ok := p.env.Lookup(EnvGuard); ok {
		return nil
	}
	file := filepath.Join(p.dir, "capabilities", "env", "compute")
	if !isExecutableFile(file) {
		return nil
	}
	cmd := exec.Command(file)
	cmd.Env = append(p.env.Environ(), EnvGuard+"=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := fmt.Sprintf("%s: %v", file, err)
		if s := strings.TrimSpace(stderr.String()); s != "" {
			msg += "\n" + s
		}
		return &ExitError{Code: 1, Msg: msg}
	}
	vars, err := dotenv.UnmarshalBytesWithLookup(out, p.env.Lookup)
	if err != nil {
		return &ExitError{Code: 1, Msg: fmt.Sprintf("%s: its output: %v", file, err)}
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := p.env.Lookup(k); !ok {
			p.env.Set(k, vars[k])
			p.env.origin[k] = file
		}
		p.env.Mark(k)
		p.env.defs[k] = append(p.env.defs[k], EnvDef{Kind: EnvComputed, Source: file, Value: vars[k]})
	}
	p.verbose = append(p.verbose, "devopsy: computed "+file)
	return nil
}
