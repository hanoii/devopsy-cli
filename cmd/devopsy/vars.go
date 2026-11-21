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
		args = unique(args)
		if t.Project != nil && t.Project.Dir != "" {
			if err := confirmUnnamedSecrets(args, t.Project, false); err != nil {
				return failErr(err)
			}
		}
		var lines strings.Builder
		var empty []string
		for _, k := range args {
			v, err := varValue(k, len(args) == 1, show, lookup)
			if err != nil {
				return fail(err.Error())
			}
			if v == "" {
				empty = append(empty, k)
			}
			lines.WriteString(cli.DotenvLine(k, v) + "\n")
		}
		return editVars(t, level, remote.VarsSetScript(t, level, args), lines.String(), "set", args, empty, color)

	case "unset":
		if len(args) == 0 {
			return fail(usage)
		}
		args = unique(args)
		return editVars(t, level, remote.VarsUnsetScript(t, level, args), strings.Join(args, "\n")+"\n", "unset", args, nil, color)
	}
	return fail(usage)
}

// unique is keys each once, in order: a key typed twice is one edit, said once.
func unique(keys []string) []string {
	seen := map[string]bool{}
	return slices.DeleteFunc(slices.Clone(keys), func(k string) bool {
		twice := seen[k]
		seen[k] = true
		return twice
	})
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

// editVars runs a --vars edit of keys and says what it did and what applies it; empty are
// the keys a set gives an empty value.
func editVars(t *remote.Target, level *remote.Level, script, stdin, verb string, keys, empty []string, color bool) int {
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
	outcomes := remote.ParseVarsKeys(out.String())
	said, unchanged := editSaid(t, level, verb, keys, outcomes)
	// What the edit took, from what its script did: a key removed, or set empty where it
	// was not already.
	edit := cli.Edit{File: ".env", Known: len(outcomes) > 0, Gone: map[string]bool{}, Unchanged: unchanged}
	if level != nil {
		edit.File = level.Name + ".env"
	}
	for _, k := range keys {
		if o := outcomes[k]; o == "removed" || (slices.Contains(empty, k) && o != "same" && o != "") {
			edit.Gone[k] = true
		}
	}
	sayApplied(t, level, said, edit, applied)
	// What overrides a value stays said when it was the value already; what an unset
	// leaves, only where it removed something.
	sayLevels(t, level, verb, slices.DeleteFunc(slices.Clone(keys), func(k string) bool { return outcomes[k] == "absent" }), ownFile(file), remote.ParseVarsTraces(out.String()))
	return 0
}

// editSaid is a --vars edit's first line, from what the server's edit did to each key
// (remote.ParseVarsKeys): one clause per outcome, in the keys' order, the first naming
// where (varsScope), and whether the edit changed nothing. Its words say what was done,
// never a verb a reader takes for one to do ("unset X"). A key the server did not say is
// the verb's (removed, saved).
func editSaid(t *remote.Target, level *remote.Level, verb string, keys []string, outcomes map[string]string) (string, bool) {
	on := " on "
	if level != nil {
		on = " in "
	}
	type clause struct {
		one, several, prep string
		keys               []string
	}
	clauses := map[string]*clause{
		"removed": {one: "removed", several: "removed", prep: " from "},
		"absent":  {one: "was not set", several: "were not set", prep: on},
		"saved":   {one: "saved", several: "saved", prep: on},
		"same":    {one: "already had that value", several: "already had those values", prep: on},
	}
	var order []string
	unchanged := true
	for _, k := range keys {
		o := outcomes[k]
		switch {
		case o == "added" || o == "changed" || (o != "absent" && o != "same" && o != "removed" && verb == "set"):
			o = "saved"
		case o != "absent" && o != "same":
			o = "removed"
		}
		if o != "absent" && o != "same" {
			unchanged = false
		}
		if len(clauses[o].keys) == 0 {
			order = append(order, o)
		}
		clauses[o].keys = append(clauses[o].keys, k)
	}
	parts := make([]string, len(order))
	for i, o := range order {
		c := clauses[o]
		words := c.one
		if len(c.keys) > 1 {
			words = c.several
		}
		parts[i] = strings.Join(c.keys, ", ") + " " + words
		if i == 0 {
			parts[i] += c.prep + varsScope(t, level)
		}
	}
	said := strings.Join(parts, ", ")
	if unchanged {
		said += ", so nothing changed"
	}
	return said, unchanged
}

// varsScope is where a --vars edit was made, as its first line names it: the
// environment by its address, the instance or the project by name on its server, never
// the file.
func varsScope(t *remote.Target, level *remote.Level) string {
	switch {
	case level == nil:
		return "@" + targetRef(t)
	case level.Name == "instance":
		return "instance " + filepath.Base(level.Dir) + " on " + t.Host
	}
	return "project " + level.Dir + " on " + t.Host
}

// sayApplied says said, and what applies a change of t's variables, from what the
// server's check found in each environment (remote.ParseVarsApplied): the environment's
// own, or with level each one under it, environments with the same outcome on one
// detail. edit is what the change did (cli.Edit): one that changed nothing needs nothing
// to apply it, so only what compose misses is said then.
func sayApplied(t *remote.Target, level *remote.Level, said string, edit cli.Edit, applied []remote.Applied) {
	if level == nil {
		a := remote.Applied{}
		if len(applied) > 0 {
			a = applied[0]
		}
		o := outcomeOf(t, a, edit)
		if o.missing == nil {
			report.Say(report.InfoLevel, said, o.details, o.hints...)
			return
		}
		report.Say(report.WarnLevel, said+willFail("its next reload or release", *o.missing), o.details, varsFix(t, nil, *o.missing)...)
		return
	}
	if len(applied) == 0 {
		o := outcomeOf(t, remote.Applied{Word: "none"}, edit)
		report.Info(said, o.details...)
		return
	}
	type group struct {
		envs    []string
		details []string
	}
	var groups []*group
	var refusing []string
	// As before is the file's, the same in every environment.
	fix := cli.Explained{AsBefore: edit.Known && edit.Unchanged}
	for _, a := range applied {
		o := outcomeOf(t, a, edit)
		if o.missing != nil {
			refusing, fix = append(refusing, a.Dir), fix.Merge(*o.missing)
		}
		if len(o.details) == 0 {
			continue
		}
		if i := slices.IndexFunc(groups, func(g *group) bool { return slices.Equal(g.details, o.details) }); i >= 0 {
			groups[i].envs = append(groups[i].envs, a.Dir)
		} else {
			groups = append(groups, &group{envs: []string{a.Dir}, details: o.details})
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
	whose := refusing[0] + "'s next reload or release"
	if len(refusing) > 1 {
		whose = "the next reload or release of " + report.JoinAnd(refusing)
	}
	report.Say(report.WarnLevel, said+willFail(whose, fix), details, varsFix(t, level, fix)...)
}

// willFail ends a change's first line with what it leaves failing: whose next reload or
// release, "so" when the change took what is missing, "as before" when it changed
// nothing, else "and": devopsy cannot tell whether it failed before.
func willFail(whose string, e cli.Explained) string {
	switch {
	case e.AsBefore:
		return ", but " + whose + " will still fail, as before"
	case e.Caused:
		return ", so " + whose + " will fail"
	}
	return ", and " + whose + " will fail"
}

// outcome is what the check found in one environment, worded: what applies the change,
// with the commands that do when the environment's own edit says it (hints), or what
// compose misses (missing), its details then the explanation's.
type outcome struct {
	details []string
	hints   []report.Remedy
	missing *cli.Explained
}

// outcomeOf words a, the check in one environment of t after edit (none: a level with
// no environment released): after an edit that changed nothing, only what compose misses
// and what was not checked, since nothing is to apply.
func outcomeOf(t *remote.Target, a remote.Applied, edit cli.Edit) outcome {
	status := report.Hint("as --status shows:", "devopsy @"+targetRef(t)+" --status")
	reload := report.Hint("to apply it now:", "devopsy @"+targetRef(t)+" --reload")
	if edit.Known && edit.Unchanged && a.Word != "4" && a.Word != "busy" {
		return outcome{}
	}
	switch a.Word {
	case "none":
		return outcome{details: []string{"no environment there is released yet: each one's first release applies it"}}
	case "plain":
		return outcome{details: []string{"running containers keep their old values until they are recreated"}}
	case "first":
		return outcome{details: []string{"nothing is released there yet: its first release applies it"}}
	case "busy":
		return outcome{details: []string{"not checked, its lock is held now"}}
	case "marked":
		return outcome{details: []string{"its data is from a sync, an import or a restore that did not complete: a reload or a release applies it once that is settled"}, hints: []report.Remedy{status}}
	case "cut":
		return outcome{details: []string{"its release was cut off past its switch: a reload applies it once that is finished"}, hints: []report.Remedy{status}}
	case "0":
		return outcome{details: []string{"running containers keep their old values until a reload recreates them"}, hints: []report.Remedy{reload}}
	case "3":
		var what []string
		for _, l := range a.Lines {
			if v, ok := strings.CutPrefix(l, "image "); ok {
				what = append(what, v)
			}
		}
		return outcome{details: append([]string{"it needs another image than the release runs, which only a release fetches:"}, what...), hints: []report.Remedy{report.Hint("to apply it:", "devopsy @"+remote.HintAddress(t)+" --release")}}
	case "4":
		if m := cli.ParseMissing(a.Lines); m != nil {
			e := m.Explain(edit)
			return outcome{details: e.Details, missing: &e}
		}
	}
	// The check could not tell (compose's configuration unreadable): a reload says why.
	return outcome{details: []string{"devopsy could not tell what applies it: a reload applies it once its checks pass"}, hints: []report.Remedy{reload}}
}

// varsFix is the one fix of what compose misses after a change (cli.Explained), at the
// level it was made: the keys to set again, to unset again and to set, every command
// needed; none when no key brings it back, which the details say.
func varsFix(t *remote.Target, level *remote.Level, e cli.Explained) []report.Remedy {
	flag := "--vars"
	if level != nil {
		flag = "--vars-" + level.Name
	}
	words, args := e.Fix(flag)
	if len(args) == 0 {
		return nil
	}
	cmds := make([]string, len(args))
	for i, a := range args {
		cmds[i] = "devopsy @" + targetRef(t) + " " + a
	}
	return []report.Remedy{report.Fix(words+":", cmds...)}
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
