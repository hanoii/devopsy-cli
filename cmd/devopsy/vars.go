package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"golang.org/x/term"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// runVars handles `devopsy @target --vars ...`: the target's variables file,
// shared/.env for releases or .devopsy/.env for a plain directory; with
// flag --vars-project or --vars-instance, the project's or the instance's
// .env on that server. Values
// never travel in arguments: set sends them on SSH's stdin, and they come
// from lookup (the caller's environment, plus the project's .env for project
// targets), a prompt (hidden unless --show), or stdin. --help and --show can
// go anywhere after --vars: names never start with "-".
func runVars(t *remote.Target, flag string, args []string, lookup func(string) (string, bool), color bool) int {
	fail := func(msg string) int {
		report.Error(msg)
		return 1
	}
	usage := "usage: devopsy @" + targetRef(t) + " " + flag + " [--show | get KEY | set [--show] KEY... | unset KEY...]"
	show := false
	var words []string
	for _, a := range args {
		switch a {
		case "--help", "-h":
			fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp[flag]))
			return 0
		case "--show":
			show = true
		default:
			words = append(words, a)
		}
	}
	var level *remote.Level
	switch flag {
	case "--vars-project":
		// On a server named alone too: only the project's name matters.
		if t.Project == nil {
			return fail("--vars-project edits a project's .env: run it in its checkout, or through an alias")
		}
		level = &remote.Level{Name: "project", Dir: t.Project.Name, Link: "project.env"}
	case "--vars-instance":
		for i := range t.Levels {
			if t.Levels[i].Name == "instance" {
				level = &t.Levels[i]
			}
		}
		if level == nil {
			return fail(fmt.Sprintf("--vars-instance: @%s has no instance (the project's instances: true)", targetRef(t)))
		}
	}
	args = words
	action := ""
	if len(args) > 0 {
		action, args = args[0], args[1:]
	}
	if show && action != "set" && action != "" && action != "list" {
		return fail("--show applies to the list and to set")
	}
	for _, k := range args {
		if !remote.VarNameRe.MatchString(k) {
			return fail(fmt.Sprintf("%q is not a variable name (values never go in arguments: set reads them from your environment, a prompt or stdin)", k))
		}
	}

	switch action {
	case "", "list", "get":
		if action == "get" && len(args) != 1 {
			return fail(usage)
		}
		if action != "get" && len(args) > 0 {
			return fail(usage)
		}
		var out bytes.Buffer
		code, err := remote.SSH(t, remote.VarsReadScript(t, level), bytes.NewReader(nil), &out, false)
		if err != nil {
			return fail(err.Error())
		}
		if code != 0 {
			return code
		}
		read, err := remote.ReadVarsOutput(out.String())
		if err != nil {
			return fail(err.Error())
		}
		file := read.File
		vars, err := dotenv.UnmarshalWithLookup(read.Content, func(string) (string, bool) { return "", false })
		if err != nil {
			return fail(fmt.Sprintf("%s:%s: %v", t.Host, file, err))
		}
		// The environment's trace, when its current release (or plain
		// directory) gave one: what get gives, and what the list counts.
		var trace *cli.EnvTraced
		if level == nil && read.Trace != "" {
			if tr, err := cli.ParseEnvTrace([]byte(read.Trace)); err == nil {
				trace = &tr
			}
		}
		if action == "get" {
			if level != nil {
				v, ok := vars[args[0]]
				if !ok {
					return fail(fmt.Sprintf("%s is not set in %s:%s", args[0], t.Host, file))
				}
				fmt.Println(v)
				return 0
			}
			v, where, ok := varsGet(args[0], vars, read, trace)
			if !ok {
				return fail(fmt.Sprintf("%s is not set in @%s", args[0], targetRef(t)))
			}
			report.Info(fmt.Sprintf("%s: the value %s", args[0], levelSays(where, ownFile(file), traceParent(trace))))
			fmt.Println(v)
			return 0
		}
		names := make([]string, 0, len(vars))
		for k := range vars {
			names = append(names, k)
		}
		sort.Strings(names)
		secrets, note := varsSecrets(t, read.Configs), " (secrets masked: --show)"
		if show {
			secrets, note = nil, ""
		}
		report.Info(fmt.Sprintf("%s:%s%s", t.Host, file, note))
		for _, k := range names {
			fmt.Println(cli.DotenvLine(k, secrets.Value(k, vars[k])))
		}
		if trace != nil {
			n := 0
			for _, v := range trace.Vars {
				// Not one the list printed, whatever overrides it there.
				if _, listed := vars[v.Name]; !listed && len(v.Defs) > 0 && v.Defs[0].Level != cli.LevelEnv {
					n++
				}
			}
			if n > 0 {
				what := fmt.Sprintf("%d more variables reach @%s from elsewhere (its other files, imports, the env capability, devopsy)", n, targetRef(t))
				if n == 1 {
					what = fmt.Sprintf("1 more variable reaches @%s from elsewhere (its other files, imports, the env capability, devopsy)", targetRef(t))
				}
				report.Info(what, "all of them, with their values:", report.CmdMark+"devopsy @"+targetRef(t)+" --env")
			}
		}
		return 0

	case "set":
		if len(args) == 0 {
			return fail(usage)
		}
		if t.Project != nil && t.Project.Dir != "" {
			if err := confirmUnnamedSecrets(args, t.Project, false); err != nil {
				report.Fail(err)
				return 1
			}
		}
		var lines strings.Builder
		for _, k := range args {
			v, err := varValue(k, len(args) == 1, show, lookup)
			if err != nil {
				return fail(err.Error())
			}
			lines.WriteString(cli.DotenvLine(k, v) + "\n")
		}
		return editVars(t, level, remote.VarsSetScript(t, level, args), lines.String(), "set", args, color)

	case "unset":
		if len(args) == 0 {
			return fail(usage)
		}
		return editVars(t, level, remote.VarsUnsetScript(t, level, args), strings.Join(args, "\n")+"\n", "unset", args, color)
	}
	return fail(usage)
}

// varValue finds the value to set for k: lookup, else a prompt at a terminal,
// hidden unless show, else stdin when it is the only key.
func varValue(k string, only, show bool, lookup func(string) (string, bool)) (string, error) {
	if v, ok := lookup(k); ok {
		return v, nil
	}
	if atTerminal() {
		report.Default().Prompt(k + ":")
		if show {
			line, err := stdinReader.ReadString('\n')
			if err == io.EOF && line != "" {
				err = nil
			}
			return strings.TrimSuffix(line, "\n"), err
		}
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		report.Default().EndPrompt()
		return string(b), err
	}
	if only {
		b, err := io.ReadAll(os.Stdin)
		return strings.TrimSuffix(string(b), "\n"), err
	}
	return "", fmt.Errorf("%s is not set in your environment or the project's .env, and stdin can only give one value", k)
}

func editVars(t *remote.Target, level *remote.Level, script, stdin, verb string, keys []string, color bool) int {
	var out bytes.Buffer
	// Gated: the values go once the server holds the release lock.
	code, err := remote.SSHContext(remote.WithGate(context.Background()), t, nil, script, strings.NewReader(remote.GatedInput(stdin)), &out, false, nil)
	if err != nil {
		report.Fail(err)
		return 1
	}
	if code != 0 {
		return code
	}
	file, applied, lines := remote.ParseVarsApplied(out.String())
	sayApplied(t, fmt.Sprintf("%s %s in %s:%s", verb, strings.Join(keys, ", "), t.Host, file), applied, lines)
	sayLevels(t, level, verb, keys, ownFile(file), remote.ParseVarsTraces(out.String()))
	return 0
}

// sayApplied says said, and what applies a change of t's variables, from
// what the server's check found (remote.VarsApplied, ParseVarsApplied).
func sayApplied(t *remote.Target, said, applied string, lines []string) {
	var what []string
	for _, l := range lines {
		if k, v, ok := strings.Cut(l, " "); ok && (k == "missing" || k == "image") {
			what = append(what, v)
		}
	}
	switch applied {
	case "plain":
		report.Info(said, "running containers keep their old values until they are recreated")
	case "first":
		report.Info(said, "nothing is released there yet: its first release applies it")
	case "marked":
		report.Info(said, markedSays, report.CmdMark+"devopsy @"+targetRef(t)+" --status")
	case "cut":
		report.Info(said, "its release was cut off past its switch: a reload applies it once that is finished, as --status shows:", report.CmdMark+"devopsy @"+targetRef(t)+" --status")
	case "0":
		report.Info(said, "running containers keep their old values until they are recreated: to apply it now,", report.CmdMark+"devopsy @"+targetRef(t)+" --reload")
	case "3":
		report.Info(said, append(append([]string{"it needs another image than the release runs, which only a release fetches:"}, what...), "to apply it:", report.CmdMark+"devopsy @"+targetRef(t)+" --release")...)
	case "4":
		report.Warn(said+": compose now requires variables that are not set, and the next reload or release refuses until they are: "+strings.Join(what, ", "),
			"to set them:", report.CmdMark+"devopsy @"+targetRef(t)+" --vars set <KEY>...")
	case "":
		// The instance's or the project's file, several environments'.
		report.Info(said, "running containers keep their old values until they are recreated: each environment's --reload applies it")
	default:
		// The check could not tell (compose's configuration unreadable):
		// a reload says why.
		report.Info(said, "running containers keep their old values until they are recreated: a reload applies it once its checks pass", report.CmdMark+"devopsy @"+targetRef(t)+" --reload")
	}
}

// varsSecrets masks a --vars listing: the secrets: of the server's
// configs, which name what the releases there treat as secret, and the
// local project's, which may be newer. A config devopsy cannot read is
// skipped.
func varsSecrets(t *remote.Target, configs []string) *cli.Secrets {
	dir := ""
	if t.Project != nil {
		dir = t.Project.Dir
	}
	names := localSecrets(dir)
	for _, c := range configs {
		n, _ := remote.ConfigSecrets([]byte(c))
		names = append(names, n...)
	}
	return cli.NewSecrets(cli.NewEnv(nil), names)
}

// confirmUnnamedSecrets asks before going on with variables named like
// secrets that the project's secrets: does not list, whose values devopsy
// would print. Without a terminal, or with yes, it fails: an answer
// nobody gives is no.
func confirmUnnamedSecrets(names []string, p *remote.Project, yes bool) error {
	unnamed := cli.UnnamedSecrets(names, p.Secrets, p.Public)
	if len(unnamed) == 0 {
		return nil
	}
	file := filepath.Join(p.Dir, remote.ConfigFile)
	what, its, them := unnamed[0]+" looks like a secret", "its value", "it"
	if len(unnamed) > 1 {
		what, its, them = strings.Join(unnamed, ", ")+" look like secrets", "their values", "them"
	}
	warning := fmt.Sprintf("%s, but secrets: does not list %s", what, them)
	why := fmt.Sprintf("devopsy would print %s in --env, --vars and its messages", its)
	fix := report.Fix(fmt.Sprintf("add %s to secrets: in %s to mask %s, or to public: if not secret", them, file, its))
	if yes || !atTerminal() {
		return &report.Failure{Msg: warning, Details: []string{why}, Fixes: []report.Remedy{fix}}
	}
	report.Default().Say(report.WarnLevel, warning, []string{why}, []report.Remedy{fix})
	if !report.Default().Confirm(stdinReader, "go on anyway?") {
		return errors.New("nothing changed")
	}
	return nil
}

// localSecrets is the local project's secrets: list, nil outside a project
// or without a config devopsy can read.
func localSecrets(projectDir string) []string {
	if projectDir == "" {
		return nil
	}
	p, err := remote.LoadProject(projectDir)
	if err != nil || p == nil {
		return nil
	}
	return p.Secrets
}

// markedSays is what a change of variables says on an environment whose
// data a sync, an import or a restore did not complete: a reload and a
// release refuse there until that is settled, as --status shows.
const markedSays = "its data is from a sync, an import or a restore that did not complete: a reload or a release applies it once that is settled, as --status shows:"
