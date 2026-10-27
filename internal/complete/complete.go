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
	// GroupTarget, GroupUserTarget, GroupServer, GroupProject,
	// GroupDevopsy, GroupCompose. Plain values,
	// like shell names, have none.
	Group string
}

// Groups, printed as a third field after the description.
const (
	GroupTarget = "target"
	// GroupUserTarget is a target from the user-level config.yaml.
	GroupUserTarget = "user-target"
	// GroupServer is a host from ~/.ssh/config, as @<host>:.
	GroupServer  = "server"
	GroupProject = "project"
	GroupDevopsy = "devopsy"
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

// builtins are the flags devopsy takes without a target, remoteCommands
// those after @<target>: cli.Flags, so help and completion agree.
var builtins, remoteCommands = candidates(cli.LocalFlags()), candidates(cli.RemoteFlags())

func candidates(flags []cli.Flag) []Candidate {
	var out []Candidate
	for _, f := range flags {
		out = append(out, Candidate{f.Name, f.Desc, GroupDevopsy})
	}
	return out
}

// Complete returns the candidates for words, the arguments after `devopsy`,
// the last being the word completed.
func Complete(cwd string, words []string, environ []string) Result {
	if len(words) == 0 {
		words = []string{""}
	}
	// -v or -vv goes before everything else.
	for len(words) > 1 && (words[0] == "-v" || words[0] == "-vv") {
		words = words[1:]
	}
	projectDir, _ := cli.FindProjectDir(cwd)
	cur := words[len(words)-1]

	if len(words) == 1 {
		// Targets first, then project commands, devopsy's flags and, from
		// the delegate, compose's.
		r := Result{Candidates: addresses(projectDir, cur, true)}
		r.Candidates = append(r.Candidates, projectCommands(projectDir, false)...)
		if strings.HasPrefix(cur, "-") {
			r.Candidates = append(r.Candidates, builtins...)
		}
		r = filter(r, cur)
		if !strings.HasPrefix(cur, "@") {
			r.Delegate = composeDelegate(cwd, projectDir, words, environ)
		}
		r.Directive = DirectiveNoFileComp | serversOnly(r)
		return r
	}

	switch first := words[0]; {
	case first == "--ssh-keys-generate":
		r := Result{Directive: DirectiveNoFileComp}
		if strings.HasPrefix(cur, "-") {
			r.Candidates = []Candidate{{"--out", "where the private key goes", GroupDevopsy}, {"--help", "how", GroupDevopsy}}
		}
		return filter(r, cur)
	case first == "--shell":
		if len(words) == 2 && !strings.HasPrefix(cur, "-") {
			return shellServices(projectDir, cur)
		}
		return Result{Directive: DirectiveNoFileComp}
	case first == "--debug":
		r := Result{Directive: DirectiveNoFileComp}
		switch {
		case len(words) == 2:
			for _, t := range []string{"environments", "env", "capabilities", "labels", "imports", "schema"} {
				r.Candidates = append(r.Candidates, Candidate{Value: t})
			}
		case len(words) == 3 && words[1] == "environments":
			for _, t := range remote.Targets(projectDir) {
				r.Candidates = append(r.Candidates, Candidate{Value: t.Name})
			}
		}
		return filter(r, cur)
	case first == "--env":
		return showFlag(words, cur)
	case first == "--export" || first == "--import":
		return dataFlags(projectDir, words, cur, false)
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

// dataFlags completes --export and --import: a service, which
// completion leaves to the user (its label may come from a variable only
// compose resolves), then --yes for --import, and on a target
// (onTarget) --resolved and --accept-image (a service), and --help
// first.
func dataFlags(projectDir string, words []string, cur string, onTarget bool) Result {
	given := words[1 : len(words)-1]
	if onTarget && words[0] == "--import" && len(given) > 0 && given[len(given)-1] == "--accept-image" {
		return shellServices(projectDir, cur)
	}
	list := []Candidate{{"--help", "what it moves, and how", GroupDevopsy}}
	if words[0] == "--import" {
		list = append(list, Candidate{"--yes", "on a target, do not ask for the environment's name", GroupDevopsy})
		if onTarget {
			list = append(list, Candidate{"--resolved", "accept the data an import that did not complete left, start what it kept down", GroupDevopsy},
				Candidate{"--accept-image", "run a database's own image on the imported data, its history over", GroupDevopsy})
		}
	}
	r := Result{Directive: DirectiveNoFileComp}
	for _, c := range list {
		if !slices.Contains(given, c.Value) && (c.Value != "--help" || len(given) == 0) {
			r.Candidates = append(r.Candidates, c)
		}
	}
	return filter(r, cur)
}

// showFlag completes --env's one option.
func showFlag(words []string, cur string) Result {
	r := Result{Directive: DirectiveNoFileComp}
	if len(words) == 2 {
		r.Candidates = []Candidate{{"--show", "secrets too", GroupDevopsy}}
	}
	return filter(r, cur)
}

// completeCommand completes `<command> [args...]`, run locally or on a
// server: a project command's arguments are file names, anything else is
// compose's.
func completeCommand(cwd, projectDir string, words []string, environ []string) Result {
	if len(words) == 1 {
		r := filter(Result{Candidates: projectCommands(projectDir, false)}, words[0])
		r.Delegate = composeDelegate(cwd, projectDir, words, environ)
		r.Directive = DirectiveNoFileComp
		return r
	}
	if projectDir != "" && isCommand(projectDir, words[0]) {
		return Result{}
	}
	return Result{Delegate: composeDelegate(cwd, projectDir, words, environ)}
}

// completeRemote completes the words after `@<address>`.
func completeRemote(cwd, projectDir, name string, words []string, environ []string) Result {
	var target *remote.Target
	// An address's environment is its last part; a bare name can be an alias.
	env := name[strings.LastIndexAny(name, ":/")+1:]
	for _, t := range remote.Targets(projectDir) {
		if t.Name == name || (!t.User && t.Name == env) {
			target = t
		}
	}
	// An alias is not this project: its commands and services are its
	// source's, when it names one, else unknown.
	user := target != nil && target.User
	if user {
		projectDir = ""
		if src := target.SourceDir(); src != "" {
			if dir, err := cli.FindProjectDir(src); err == nil && filepath.Dir(dir) == filepath.Clean(src) {
				projectDir, cwd = dir, src
			}
		}
	}
	cur := words[len(words)-1]
	none := Result{Directive: DirectiveNoFileComp}

	if len(words) == 1 {
		r := Result{Candidates: projectCommands(projectDir, true)}
		// --vars-instance only for a project with instances.
		var p *remote.Project
		if projectDir != "" {
			p, _ = remote.LoadProject(projectDir)
		}
		for _, c := range remoteCommands {
			if c.Value == "--vars-instance" && (p == nil || !p.Instances) {
				continue
			}
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

	// flags offers those of list not given yet, while every word so far is
	// one of them; --help only first.
	flags := func(list []Candidate) Result {
		given := words[1 : len(words)-1]
		var r Result
		for _, w := range given {
			if !slices.ContainsFunc(list, func(c Candidate) bool { return c.Value == w }) {
				return none
			}
		}
		for _, c := range list {
			if !slices.Contains(given, c.Value) && (c.Value != "--help" || len(given) == 0) {
				r.Candidates = append(r.Candidates, c)
			}
		}
		r = filter(r, cur)
		r.Directive = DirectiveNoFileComp
		return r
	}
	help := Candidate{"--help", "what it runs, and how", GroupDevopsy}
	switch words[0] {
	case "--release", "--release-sync":
		// It takes no command: its steps are in config.yaml.
		if len(words) > 2 && words[len(words)-2] == "--from" {
			return fromAddresses(projectDir, cur, user)
		}
		if len(words) > 2 && words[len(words)-2] == "--accept-image" {
			return shellServices(projectDir, cur)
		}
		given := words[1 : len(words)-1]
		for i := 0; i < len(given); i++ {
			switch given[i] {
			case "--from", "--accept-image":
				i++ // its value
			case "--yes", "-y":
			default:
				return none
			}
		}
		var r Result
		for _, c := range []Candidate{
			{"--from", "sync from another environment than its parent", GroupDevopsy},
			{"--accept-image", "run a database's image its data never ran, or moved past", GroupDevopsy},
			{"--yes", "create a new instance, or replace a child's data, without asking", GroupDevopsy},
			help,
		} {
			if !slices.Contains(given, c.Value) && (c.Value != "--help" || len(given) == 0) {
				r.Candidates = append(r.Candidates, c)
			}
		}
		r = filter(r, cur)
		r.Directive = DirectiveNoFileComp
		return r
	case "--release-no-sync", "--rollback", "--reload", "--inherit", "--status":
		return flags([]Candidate{help})
	case "--ssh-keys":
		switch {
		case len(words) == 2:
			r := Result{Candidates: []Candidate{{"list", "the authorized keys", GroupDevopsy}, {"add", "authorize a public key file", GroupDevopsy}, {"remove", "remove a key by fingerprint or comment", GroupDevopsy}}, Directive: DirectiveNoFileComp}
			return filter(r, cur)
		case words[1] == "add" && len(words) == 3 && !strings.HasPrefix(cur, "-"):
			return Result{} // a file
		case words[1] == "add" && strings.HasPrefix(cur, "-"):
			return filter(Result{Candidates: []Candidate{{"--restricted", "a machine's key: commands only, forwardings for syncs", GroupDevopsy}, help}, Directive: DirectiveNoFileComp}, cur)
		}
		return Result{Directive: DirectiveNoFileComp}
	case "--releases":
		// Ids are the server's: never offered.
		if len(words) == 2 {
			return filter(Result{Candidates: []Candidate{{"remove", "remove releases and the images only they hold", GroupDevopsy}, help}, Directive: DirectiveNoFileComp}, cur)
		}
		if words[1] == "remove" {
			return filter(Result{Candidates: []Candidate{{"failed", "every failed release", GroupDevopsy}, {"--yes", "do not ask first", GroupDevopsy}, help}, Directive: DirectiveNoFileComp}, cur)
		}
		return Result{Directive: DirectiveNoFileComp}
	case "--snapshots":
		// Ids are the server's: never offered.
		if len(words) == 2 {
			return filter(Result{Candidates: []Candidate{
				{"restore", "bring a snapshot back: data, code, images", GroupDevopsy},
				{"remove", "remove snapshots and the images only they hold", GroupDevopsy},
				{"export", "a snapshot to stdout, one tar", GroupDevopsy},
				{"import", "a snapshot from stdin", GroupDevopsy}, help}, Directive: DirectiveNoFileComp}, cur)
		}
		switch words[1] {
		case "restore":
			return filter(Result{Candidates: []Candidate{{"--no-code", "the data alone, under the current release", GroupDevopsy}, {"--with-env", "its shared/.env too", GroupDevopsy}, {"--yes", "do not ask first", GroupDevopsy}, help}, Directive: DirectiveNoFileComp}, cur)
		case "remove":
			return filter(Result{Candidates: []Candidate{{"--yes", "do not ask first", GroupDevopsy}, help}, Directive: DirectiveNoFileComp}, cur)
		}
		return Result{Directive: DirectiveNoFileComp}
	case "--sync", "--reset":
		if len(words) > 2 && words[len(words)-2] == "--from" {
			return fromAddresses(projectDir, cur, user)
		}
		if len(words) > 2 && words[len(words)-2] == "--accept-image" {
			return shellServices(projectDir, cur)
		}
		given := words[1 : len(words)-1]
		var r Result
		for _, c := range []Candidate{
			{"--from", "copy from another environment than its parent", GroupDevopsy},
			{"--accept-image", "run a database's own image on the copy, its history over", GroupDevopsy},
			{"--yes", "do not ask for the environment's name", GroupDevopsy},
			help,
		} {
			if !slices.Contains(given, c.Value) && (c.Value != "--help" || len(given) == 0) {
				r.Candidates = append(r.Candidates, c)
			}
		}
		r = filter(r, cur)
		r.Directive = DirectiveNoFileComp
		return r
	case "--vars", "--vars-instance", "--vars-project":
		r := Result{Directive: DirectiveNoFileComp}
		switch {
		case len(words) == 2 && strings.HasPrefix(cur, "-"):
			r.Candidates = []Candidate{{"--show", "secrets too", GroupDevopsy}}
		case len(words) == 2:
			r.Candidates = []Candidate{{"get", "one value", GroupDevopsy}, {"set", "set variables", GroupDevopsy}, {"unset", "remove variables", GroupDevopsy}}
		case words[1] == "set" && strings.HasPrefix(cur, "-"):
			r.Candidates = []Candidate{{"--show", "echo what you type at the prompt", GroupDevopsy}}
		case words[1] == "set" && projectDir != "" && !user:
			// A user-level target never takes values from a project's .env.
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
	case "--debug":
		// The topics about the release and the host; environments and
		// schema are about local config.
		if len(words) == 2 {
			r := Result{Directive: DirectiveNoFileComp}
			for _, t := range []string{"env", "capabilities", "labels", "imports"} {
				r.Candidates = append(r.Candidates, Candidate{Value: t})
			}
			return filter(r, cur)
		}
		return none
	case "--env":
		return showFlag(words, cur)
	case "--export", "--import":
		return dataFlags(projectDir, words, cur, true)
	case "--shell-host", "--ssh-config", "--environments", "--upgrade", "--log", "--help", "-h":
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
	if t.User {
		return t.Address + " (alias)"
	}
	if t.Host != "" {
		return "environment, on " + t.Host
	}
	return "environment"
}

// projectCommands are the commands that run locally, or on a target.
func projectCommands(projectDir string, addressed bool) []Candidate {
	if projectDir == "" {
		return nil
	}
	var out []Candidate
	for _, c := range cli.CustomCommands(projectDir) {
		if !cli.CommandOffered(projectDir, c, addressed) {
			continue
		}
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

// addresses offers addresses for cur, always with their @ (devopsy also
// takes one without it where an address is a flag's value): the project's
// environments, the user's aliases when aliases is set, and once an @ is
// typed the servers in ~/.ssh/config as @<host>:, which starts any
// address. Past a server or an instance, the environments after it.
func addresses(projectDir, cur string, aliases bool) []Candidate {
	var out []Candidate
	if i := strings.LastIndexAny(cur, ":/"); strings.HasPrefix(cur, "@") && i >= 0 {
		for _, t := range remote.Targets(projectDir) {
			if !t.User {
				out = append(out, Candidate{cur[:i+1] + t.Name, targetDescription(t), GroupTarget})
			}
		}
		return out
	}
	if cur != "" && !strings.HasPrefix(cur, "@") {
		return nil
	}
	// Environments before aliases (Targets' order).
	for _, t := range remote.Targets(projectDir) {
		group := GroupTarget
		if t.User {
			if !aliases {
				continue
			}
			group = GroupUserTarget
		}
		out = append(out, Candidate{"@" + t.Name, targetDescription(t), group})
	}
	if cur != "" {
		for _, h := range sshHosts() {
			out = append(out, Candidate{"@" + h + ":", "server, ~/.ssh/config", GroupServer})
		}
	}
	return out
}

// serversOnly is DirectiveNoSpace when every candidate left is a server
// (@<host>:), which an environment follows without a space.
func serversOnly(r Result) int {
	if len(r.Candidates) > 0 && !slices.ContainsFunc(r.Candidates, func(c Candidate) bool { return !strings.HasSuffix(c.Value, ":") }) {
		return DirectiveNoSpace
	}
	return 0
}

// fromAddresses completes --from's address: as the first word's, aliases
// aside, another environment of this project; none for a user-level
// target, another project.
func fromAddresses(projectDir, cur string, user bool) Result {
	r := Result{Directive: DirectiveNoFileComp}
	if !user {
		r.Candidates = addresses(projectDir, cur, false)
	}
	r = filter(r, cur)
	r.Directive |= serversOnly(r)
	return r
}
