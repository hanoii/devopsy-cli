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
	// Env is what every action gets in its environment, when it says.
	Env string
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
		Name:     "status",
		What:     "lines of the project's own under devopsy --status, after the containers: its URL, a maintenance mode, a version",
		Who:      "any project with something to say about itself",
		CalledBy: "devopsy [@<target>] --status, locally and on servers",
		Actions: []CapabilityAction{
			{
				Name:  "show",
				Usage: "show",
				Does:  "prints what to add, one fact per line, with the project's environment loaded; stdout goes through as it is",
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
		Name:     DataCapability,
		What:     "moves the data of the services labeled devopsy.data=stream (export, import), in syncs and --export and --import, and hooks into syncs (a child environment's data from its parent's: devopsy @<child> --sync or --reset and a child's first release) with prepare and finish; each runs only when its file is there, besides devopsy's handling of the labels (mnt: its entries of mnt rsynced, or a tar; devopsy.data.stop: what is stopped meanwhile)",
		Who:      "projects whose data a copy of .devopsy/mnt cannot move, like a database to dump, or that ready or sanitize an environment around a sync",
		CalledBy: "devopsy [@<target>] --export <service> (export), devopsy [@<target>] --import <service> (import), and devopsy @<child> --sync or --reset, a child's first release or --release-sync (all four); --import --resolved runs none",
		Actions: []CapabilityAction{
			{
				Name:  "prepare",
				Usage: "prepare parent|child",
				Does:  "in syncs only, on each side, in its current release, before anything is stopped there: the child's under its release lock before the parent's push starts, the parent's under a shared hold of its lock. A failure aborts the sync before anything stops on that side (finish abort runs)",
			},
			{
				Name:  "export",
				Usage: "export <service>",
				Does:  "writes a stream service's data to stdout: on the parent's server in a sync, once what the sync stops there is stopped and the rsync is done, once per stream service in depends_on order; and devopsy --export. Required when a service is labeled stream (checked before anything stops). devopsy spools the output and only imports it once this exited 0. stdin is empty; when the service is in DEVOPSY_DATA_STOP it is stopped: use devopsy run --rm --no-deps -T <service> rather than exec, or refuse",
			},
			{
				Name:  "import",
				Usage: "import <service>",
				Does:  "reads a stream service's data from stdin, what export wrote, complete: on the child's server in a sync, and devopsy --import; the service is up (devopsy starts it with --no-deps first, and stops it again after if it was down) unless it is in DEVOPSY_DATA_STOP. Required on the child when a service is labeled stream there (checked before anything stops anywhere)",
			},
			{
				Name:  "finish",
				Usage: "finish parent|child go|abort|partial",
				Does:  "in syncs only, on each side once the sync ended. The parent's: go or abort, its stopped services back. The child's says what the copy did to its data: go (complete), abort (nothing of the parent's written), both with its stopped services back, or partial (written in part: they stay down, and the hook must not start them). On the child after go it sanitizes what a copy of production should not keep (scrubbed emails and passwords, a login the child did not have) before the release's run step runs",
			},
		},
		Env: "every action gets DEVOPSY_DATA_MNT and DEVOPSY_DATA_STREAM, its side's services labeled devopsy.data=mnt and devopsy.data=stream (depends_on order), and DEVOPSY_DATA_STOP, what this operation stops on this side (devopsy.data.stop and the services depending on them; at finish ... go|abort they are back up, at finish child partial what stays down), space separated (a stopped stream service is in both); in a sync also DEVOPSY_SYNC_FROM_TARGET and DEVOPSY_SYNC_TO_TARGET (the parent's and child's addresses) and DEVOPSY_SYNC_ARGS (--sync's arguments after --)",
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
const EnvGuard = "DEVOPSY_INTERNAL_ENV_COMPUTE"

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
	p.verbose = append(p.verbose, "computed "+file)
	return nil
}
