package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"golang.org/x/term"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/uids"
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
		if err := uids.Reserved(k); err != nil && action == "set" {
			return fail(err.Error())
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
				report.Say(report.InfoLevel, what, nil, report.Hint("all of them, with their values:", "devopsy @"+targetRef(t)+" --env"))
			}
		}
		return 0

	case "set":
		if len(args) == 0 {
			return fail(usage)
		}
		if t.Project != nil && t.Project.Dir != "" {
			if err := confirmUnnamedSecrets(args, t.Project, false); err != nil {
				return failErr(err)
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
		return failErr(err)
	}
	if code != 0 {
		return code
	}
	file, applied := remote.ParseVarsApplied(out.String())
	report.Default().Debug("the file: " + t.Host + ":" + file)
	var unset []string
	if verb == "unset" {
		unset = keys
	}
	sayApplied(t, level, fmt.Sprintf("%s %s in %s", verb, strings.Join(keys, ", "), varsScope(t, level)), unset, applied)
	sayLevels(t, level, verb, keys, ownFile(file), remote.ParseVarsTraces(out.String()))
	return 0
}

// varsScope is where a --vars edit was made, as its first line names it: the
// environment by its address, the instance or the project by name on its server, never
// the file.
func varsScope(t *remote.Target, level *remote.Level) string {
	switch {
	case level == nil:
		return "environment @" + targetRef(t)
	case level.Name == "instance":
		return "instance " + filepath.Base(level.Dir) + " on " + t.Host
	}
	return "project " + level.Dir + " on " + t.Host
}

// sayApplied says said, and what applies a change of t's variables, from what the
// server's check found in each environment (remote.ParseVarsApplied): the environment's
// own, or with level each one under it. unset are the keys the change unset.
func sayApplied(t *remote.Target, level *remote.Level, said string, unset []string, applied []remote.Applied) {
	if level == nil {
		if len(applied) == 0 {
			applied = []remote.Applied{{}}
		}
		sayAppliedHere(t, said, unset, applied[0])
		return
	}
	if len(applied) == 0 {
		report.Info(said, "no environment there is released yet: each one's first release applies it")
		return
	}
	// Environments with the same outcome on one detail, in order.
	type group struct {
		envs    []string
		details []string
	}
	var groups []*group
	add := func(env string, details ...string) {
		for _, g := range groups {
			if slices.Equal(g.details, details) {
				g.envs = append(g.envs, env)
				return
			}
		}
		groups = append(groups, &group{envs: []string{env}, details: details})
	}
	var refusing []string
	fix, asBefore := cli.Explained{}, true
	for _, a := range applied {
		switch a.Word {
		case "busy":
			add(a.Dir, "not checked, its lock is held now")
		case "marked":
			add(a.Dir, "its data is from a sync, an import or a restore that did not complete: a reload or a release applies it once that is settled")
		case "cut":
			add(a.Dir, "its release was cut off past its switch: a reload applies it once that is finished")
		case "0":
			add(a.Dir, "a reload applies it")
		case "3":
			add(a.Dir, append([]string{"it needs another image than the release runs, which only a release fetches:"}, imageLines(a.Lines)...)...)
		case "4":
			m := cli.ParseMissing(a.Lines)
			if m == nil {
				add(a.Dir, "devopsy could not tell what applies it: its reload checks it")
				continue
			}
			e := m.Explain(unset)
			refusing, asBefore = append(refusing, a.Dir), asBefore && e.AsBefore
			add(a.Dir, e.Details...)
			fix.SetAgain = appendNew(fix.SetAgain, e.SetAgain...)
			fix.UnsetAgain = appendNew(fix.UnsetAgain, e.UnsetAgain...)
			fix.SetThem = appendNew(fix.SetThem, e.SetThem...)
		default:
			add(a.Dir, "devopsy could not tell what applies it: its reload checks it")
		}
	}
	var details []string
	for _, g := range groups {
		details = append(details, report.JoinAnd(g.envs)+": "+g.details[0])
		details = append(details, g.details[1:]...)
	}
	if len(refusing) == 0 {
		report.Info(said, details...)
		return
	}
	head := said + ", and the next reload or release of " + report.JoinAnd(refusing) + " refuses"
	if asBefore {
		head += " as before"
	}
	report.Say(report.WarnLevel, head, details, varsFix(t, level, fix))
}

// sayAppliedHere says said and what applies the change in t, a, its own check.
func sayAppliedHere(t *remote.Target, said string, unset []string, a remote.Applied) {
	switch a.Word {
	case "plain":
		report.Info(said, "running containers keep their old values until they are recreated")
	case "first":
		report.Info(said, "nothing is released there yet: its first release applies it")
	case "marked":
		report.Say(report.InfoLevel, said, nil, report.Hint(markedSays, "devopsy @"+targetRef(t)+" --status"))
	case "cut":
		report.Say(report.InfoLevel, said, nil, report.Hint("its release was cut off past its switch: a reload applies it once that is finished, as --status shows:", "devopsy @"+targetRef(t)+" --status"))
	case "0":
		report.Say(report.InfoLevel, said, nil, report.Hint("running containers keep their old values until they are recreated: to apply it now,", "devopsy @"+targetRef(t)+" --reload"))
	case "3":
		report.Say(report.InfoLevel, said, append([]string{"it needs another image than the release runs, which only a release fetches:"}, imageLines(a.Lines)...), report.Hint("to apply it:", "devopsy @"+remote.HintAddress(t)+" --release"))
	case "4":
		if m := cli.ParseMissing(a.Lines); m != nil {
			e := m.Explain(unset)
			head := said + ", and its next reload or release refuses"
			if e.AsBefore {
				head += " as before"
			}
			report.Say(report.WarnLevel, head, e.Details, varsFix(t, nil, e))
			return
		}
		fallthrough
	default:
		// The check could not tell (compose's configuration unreadable):
		// a reload says why.
		report.Say(report.InfoLevel, said, nil, report.Hint("running containers keep their old values until they are recreated: a reload applies it once its checks pass", "devopsy @"+targetRef(t)+" --reload"))
	}
}

// imageLines are a check's "image <what>" lines' whats.
func imageLines(lines []string) []string {
	var what []string
	for _, l := range lines {
		if v, ok := strings.CutPrefix(l, "image "); ok {
			what = append(what, v)
		}
	}
	return what
}

// varsFix is the one fix of what compose misses after a change (cli.Explained), at the
// level it was made: the keys to set again, to unset again and to set, every command
// needed.
func varsFix(t *remote.Target, level *remote.Level, e cli.Explained) report.Remedy {
	flag := "--vars"
	if level != nil {
		flag = "--vars-" + level.Name
	}
	var parts, cmds []string
	add := func(what, word string, keys []string) {
		if len(keys) > 0 {
			parts = append(parts, what)
			cmds = append(cmds, "devopsy @"+targetRef(t)+" "+flag+" "+word+" "+strings.Join(keys, " "))
		}
	}
	add("set "+report.JoinAnd(e.SetAgain)+" again", "set", e.SetAgain)
	add("unset "+report.JoinAnd(e.UnsetAgain)+" again", "unset", e.UnsetAgain)
	them := "set them"
	if len(parts) > 0 {
		them = "set " + report.JoinAnd(e.SetThem)
	}
	add(them, "set", e.SetThem)
	return report.Fix(strings.Join(parts, ", then ")+":", cmds...)
}

// appendNew appends what list lacks of vs.
func appendNew(list []string, vs ...string) []string {
	for _, v := range vs {
		if !slices.Contains(list, v) {
			list = append(list, v)
		}
	}
	return list
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
		return &report.Failure{Msg: warning, Details: []string{why}, Remedies: []report.Remedy{fix}}
	}
	report.Default().Say(report.WarnLevel, warning, []string{why}, fix)
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
