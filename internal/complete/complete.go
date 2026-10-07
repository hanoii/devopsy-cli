// Package complete computes shell completion candidates for devopsy, and
// holds the shell scripts that ask for them.
//
// The scripts run `devopsy --complete <words...>`, with every word after
// `devopsy` up to the one being completed (empty when it is new). devopsy
// prints one candidate per line, `value<TAB>description`, then a last line
// `:<directive>`. This is cobra's protocol, so docker compose's own
// completion (`docker __complete compose ...`, docker's CLI is cobra) passes
// through unchanged: it knows compose's commands, flags and the project's
// services. devopsy adds a third field, the group, to its own candidates
// and compose's, so shells that can show it set them apart.
package complete

import (
	"embed"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Flag is the hidden flag the scripts call.
const Flag = "--complete"

// Directives, as in cobra.
const (
	DirectiveNoSpace    = 2 // do not add a space after the completion
	DirectiveNoFileComp = 4 // do not fall back to file names
)

// Candidate is one completion.
type Candidate struct {
	Value, Description string
	// Group tells candidates apart, for shells that can show it:
	// GroupTarget, GroupUserTarget, GroupProject, GroupDevopsy,
	// GroupCompose. Plain values,
	// like shell names, have none.
	Group string
}

// Groups, printed as a third field after the description.
const (
	GroupTarget = "target"
	// GroupUserTarget is a target from the user-level targets.yaml.
	GroupUserTarget = "user-target"
	GroupProject    = "project"
	GroupDevopsy    = "devopsy"
	// GroupCompose marks the delegate's candidates.
	GroupCompose = "compose"
)

// Result is what to offer. When Delegate is set, the caller also runs it and
// adds its candidates; its directive wins.
type Result struct {
	Candidates []Candidate
	Directive  int
	// Delegate is a `docker __complete compose ...` command, with the
	// project's environment.
	Delegate *cli.Plan
}

//go:embed scripts
var scripts embed.FS

// Shells lists the shells Script supports.
var Shells = []string{"bash", "fish", "zsh"}

// Script returns the completion script for shell, or "" if not supported.
func Script(shell string) string {
	data, err := scripts.ReadFile("scripts/devopsy." + shell)
	if err != nil {
		return ""
	}
	return string(data)
}

// builtins are the flags devopsy itself takes first.
var builtins = []Candidate{
	{"--help", "devopsy's help", GroupDevopsy},
	{"--version", "devopsy's and docker compose's versions", GroupDevopsy},
	{"--env", "the variables devopsy loads and computes", GroupDevopsy},
	{"--shell", "a shell in a container", GroupDevopsy},
	{"--context-hash", "a hash of what an image is built from", GroupDevopsy},
	{"--upgrade", "replace devopsy with the latest release", GroupDevopsy},
	{"--verbose", "also print what devopsy found and runs", GroupDevopsy},
	{"--completion", "a shell completion script", GroupDevopsy},
}

// remoteCommands follow @<target>.
var remoteCommands = []Candidate{
	{"--release", "upload a new release and run its steps", GroupDevopsy},
	{"--rollback", "back to the previous release, and run its steps", GroupDevopsy},
	{"--releases", "list the releases on the server", GroupDevopsy},
	{"--domains", "DNS and certificates per host", GroupDevopsy},
	{"--shell", "a shell in a container", GroupDevopsy},
	{"--shell-host", "a shell on the server itself", GroupDevopsy},
	{"--vars", "the server's variables (shared/.env)", GroupDevopsy},
	{"--help", "help on server commands", GroupDevopsy},
}

// Complete returns the candidates for words, the arguments after `devopsy`,
// the last being the word completed.
func Complete(cwd string, words []string, environ []string) Result {
	if len(words) == 0 {
		words = []string{""}
	}
	// --verbose goes before everything else.
	for len(words) > 1 && (words[0] == "--verbose" || words[0] == "-v") {
		words = words[1:]
	}
	projectDir, _ := cli.FindProjectDir(cwd)
	cur := words[len(words)-1]

	if len(words) == 1 {
		// Targets first, then project commands, devopsy's flags and, from
		// the delegate, compose's.
		var r Result
		if cur == "" || strings.HasPrefix(cur, "@") {
			// The project's targets before user-level ones.
			targets := remote.Targets(projectDir)
			sort.SliceStable(targets, func(i, j int) bool { return !targets[i].User && targets[j].User })
			for _, t := range targets {
				group := GroupTarget
				if t.User {
					group = GroupUserTarget
				}
				r.Candidates = append(r.Candidates, Candidate{"@" + t.Name, targetDescription(t), group})
			}
		}
		r.Candidates = append(r.Candidates, projectCommands(projectDir)...)
		if strings.HasPrefix(cur, "-") {
			r.Candidates = append(r.Candidates, builtins...)
		}
		r = filter(r, cur)
		if !strings.HasPrefix(cur, "@") {
			r.Delegate = composeDelegate(cwd, projectDir, words, environ)
		}
		r.Directive = DirectiveNoFileComp
		return r
	}

	switch first := words[0]; {
	case first == "--shell":
		if len(words) == 2 && !strings.HasPrefix(cur, "-") {
			return shellServices(projectDir, cur)
		}
		return Result{Directive: DirectiveNoFileComp}
	case first == "--completion":
		if len(words) == 2 {
			var r Result
			for _, s := range Shells {
				r.Candidates = append(r.Candidates, Candidate{Value: s})
			}
			r.Directive = DirectiveNoFileComp
			return filter(r, cur)
		}
		return Result{Directive: DirectiveNoFileComp}
	case strings.HasPrefix(first, "@"):
		return completeRemote(cwd, projectDir, strings.TrimPrefix(first, "@"), words[1:], environ)
	case first == "--":
		// Compose's, whatever the project's commands are.
		return Result{Delegate: composeDelegate(cwd, projectDir, words[1:], environ)}
	case strings.HasPrefix(first, "-"):
		return Result{Directive: DirectiveNoFileComp}
	}
	return completeCommand(cwd, projectDir, words, environ)
}

// completeCommand completes `<command> [args...]`, run locally or on a
// server: a project command's arguments are file names, anything else is
// compose's.
func completeCommand(cwd, projectDir string, words []string, environ []string) Result {
	if len(words) == 1 {
		r := filter(Result{Candidates: projectCommands(projectDir)}, words[0])
		r.Delegate = composeDelegate(cwd, projectDir, words, environ)
		r.Directive = DirectiveNoFileComp
		return r
	}
	if projectDir != "" && isCommand(projectDir, words[0]) {
		return Result{}
	}
	return Result{Delegate: composeDelegate(cwd, projectDir, words, environ)}
}

// completeRemote completes the words after `@<name>`.
func completeRemote(cwd, projectDir, name string, words []string, environ []string) Result {
	var target *remote.Target
	for _, t := range remote.Targets(projectDir) {
		if t.Name == name {
			target = t
		}
	}
	// A user-level target is not this project: its commands and services
	// are unknown here.
	if target != nil && target.User {
		projectDir = ""
	}
	cur := words[len(words)-1]
	none := Result{Directive: DirectiveNoFileComp}

	if len(words) == 1 {
		r := Result{Candidates: projectCommands(projectDir)}
		for _, c := range remoteCommands {
			if strings.HasPrefix(c.Value, "-") == strings.HasPrefix(cur, "-") {
				r.Candidates = append(r.Candidates, c)
			}
		}
		r = filter(r, cur)
		if !strings.HasPrefix(cur, "-") {
			r.Delegate = composeDelegate(cwd, projectDir, words, environ)
		}
		r.Directive = DirectiveNoFileComp
		return r
	}

	switch words[0] {
	case "--release", "--rollback":
		// They take no command: their steps are in targets.yaml.
		if len(words) == 2 {
			r := filter(Result{Candidates: []Candidate{{"--help", "what it runs, and how", GroupDevopsy}}}, cur)
			r.Directive = DirectiveNoFileComp
			return r
		}
		return none
	case "--domains":
		if len(words) == 2 {
			r := filter(Result{Candidates: []Candidate{{"--retry", "ask the proxy for missing certificates", GroupDevopsy}}}, cur)
			r.Directive = DirectiveNoFileComp
			return r
		}
		return none
	case "--vars":
		r := Result{Directive: DirectiveNoFileComp}
		switch {
		case len(words) == 2:
			r.Candidates = []Candidate{{"get", "one value", GroupDevopsy}, {"set", "set variables", GroupDevopsy}, {"unset", "remove variables", GroupDevopsy}}
		case words[1] == "set" && strings.HasPrefix(cur, "-"):
			r.Candidates = []Candidate{{"--show", "echo what you type at the prompt", GroupDevopsy}}
		case words[1] == "set" && projectDir != "":
			// The keys of the project's .env: what set copies from.
			for _, k := range dotenvKeys(filepath.Join(projectDir, ".env"), words[2:len(words)-1]) {
				r.Candidates = append(r.Candidates, Candidate{Value: k})
			}
		}
		return filter(r, cur)
	case "--shell":
		if len(words) == 2 && !strings.HasPrefix(cur, "-") {
			return shellServices(projectDir, cur)
		}
		return none
	case "--shell-host", "--releases", "--help", "-h":
		return none
	case "--":
		return Result{Delegate: composeDelegate(cwd, projectDir, words[1:], environ)}
	}
	return completeCommand(cwd, projectDir, words, environ)
}

// shellServices completes --shell's service from the local compose files.
func shellServices(projectDir, cur string) Result {
	r := Result{Directive: DirectiveNoFileComp}
	for _, s := range cli.Services(projectDir) {
		r.Candidates = append(r.Candidates, Candidate{Value: s})
	}
	return filter(r, cur)
}

// composeDelegate asks docker compose to complete words, with the project's
// files and environment, or bare outside a project.
func composeDelegate(cwd, projectDir string, words []string, environ []string) *cli.Plan {
	if projectDir != "" {
		if plan, err := cli.Compose(cwd, words, environ); err == nil {
			plan.Args = append([]string{"docker", "__complete"}, plan.Args[1:]...)
			return plan
		}
	}
	return &cli.Plan{Path: "docker", Args: append([]string{"docker", "__complete", "compose"}, words...), Env: environ}
}

func targetDescription(t *remote.Target) string {
	d := t.Path
	if t.Host != "" {
		d = t.Host + ":" + t.Path
	}
	if t.User {
		d += " (user-level)"
	}
	return d
}

func projectCommands(projectDir string) []Candidate {
	if projectDir == "" {
		return nil
	}
	var out []Candidate
	for _, c := range cli.CustomCommands(projectDir) {
		out = append(out, Candidate{c, cli.CommandDescription(filepath.Join(projectDir, "commands", c)), GroupProject})
	}
	return out
}

func isCommand(projectDir, name string) bool {
	return slices.Contains(cli.CustomCommands(projectDir), name)
}

// dotenvKeys lists the keys of a .env file, without parsing values, except
// those in skip.
func dotenvKeys(file string, skip []string) []string {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil
	}
	var keys []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		k, _, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" || strings.HasPrefix(k, "#") || strings.ContainsAny(k, " \t\"'") {
			continue
		}
		if !slices.Contains(skip, k) && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

// filter keeps the candidates starting with prefix.
func filter(r Result, prefix string) Result {
	var out []Candidate
	for _, c := range r.Candidates {
		if strings.HasPrefix(c.Value, prefix) {
			out = append(out, c)
		}
	}
	r.Candidates = out
	return r
}
