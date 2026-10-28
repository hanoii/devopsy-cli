package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// runEnvTrace is the hidden cli.EnvTraceFlag [KEY...], run by --vars'
// scripts in a release or a plain directory on a server: the variables
// as devopsy's commands there get them, each with its definitions by
// level (cli.EnvTrace), as one line of JSON on stdout.
func runEnvTrace(cwd string, args []string) int {
	for _, a := range args {
		if !remote.VarNameRe.MatchString(a) {
			report.Error("usage: devopsy " + cli.EnvTraceFlag + " [KEY...]")
			return 2
		}
	}
	out, err := cli.EnvTrace(cwd, os.Environ(), args)
	if err != nil {
		report.Fail(err)
		return 1
	}
	fmt.Println(string(out))
	return 0
}

// varsGet is --vars get's value of k and the level it comes from: the
// environment's trace when it has one (what its commands get), else its
// files nearest first (before its first release): its own, its inherited
// variables, the instance's and the project's.
func varsGet(k string, own map[string]string, read remote.VarsOutput, trace *cli.EnvTraced) (value, level string, ok bool) {
	if trace != nil {
		if defs := trace.Lookup(k); len(defs) > 0 {
			return string(defs[0].Value), defs[0].Level, true
		}
		return "", "", false
	}
	if v, ok := own[k]; ok {
		return v, cli.LevelEnv, true
	}
	for _, l := range []string{cli.LevelInherit, cli.LevelInstance, cli.LevelProject} {
		content, ok := read.Files[l]
		if !ok {
			continue
		}
		vars, err := dotenv.UnmarshalWithLookup(content, func(string) (string, bool) { return "", false })
		if err != nil {
			continue
		}
		if v, ok := vars[k]; ok {
			return v, l, true
		}
	}
	return "", "", false
}

// ownFile is how messages name the environment's own variables file:
// shared/.env, or a plain directory's .devopsy/.env.
func ownFile(path string) string {
	if strings.HasSuffix(path, "/"+cli.ProjectDirName+"/.env") {
		return cli.ProjectDirName + "/.env"
	}
	return "shared/.env"
}

// traceParent is the parent a trace names, "" without one.
func traceParent(trace *cli.EnvTraced) string {
	if trace == nil {
		return ""
	}
	return trace.Parent
}

// levelSays is where a value of level comes from, after "the value":
// own is the environment's own file (ownFile), parent its parent's name.
func levelSays(level, own, parent string) string {
	switch level {
	case cli.LevelEnv:
		return "from " + own
	case cli.LevelInherit:
		if parent == "" {
			return "inherited from its parent (shared/" + remote.InheritFile + ")"
		}
		return "inherited from " + parent + " (shared/" + remote.InheritFile + ")"
	case cli.LevelInstance:
		return "from the instance's .env"
	case cli.LevelProject:
		return "from the project's .env"
	case cli.LevelTarget:
		return "from the release's " + remote.TargetEnvFile + " (config.yaml)"
	case cli.LevelImported:
		return "imported by its release"
	case cli.LevelComputed:
		return "computed by the env capability"
	case cli.LevelDevopsy:
		return "set by devopsy"
	case cli.LevelCaller:
		return "from the session's environment"
	}
	return "from " + level
}

// sayLevels says, after a --vars set or unset (flag's level, nil for the
// environment's own file), what the change means against the other
// places that define its keys, from the traces the script printed (by
// environment directory, remote.ParseVarsTraces). A set over another
// value: a warning naming the value it overrides now, with the unset
// that brings it back. A set that something nearer hides: a warning per
// environment naming everything that overrides it, with the unsets that
// let it through. An unset of the environment's own: the value it has
// now, if any. Never a value: names and levels only.
func sayLevels(t *remote.Target, level *remote.Level, verb string, keys []string, own string, traces map[string]string) {
	ours, flag, oursSays := cli.LevelEnv, "--vars", own
	if level != nil {
		ours, flag, oursSays = level.Name, "--vars-"+level.Name, "the "+level.Name+"'s .env"
	}
	addr := func(dir string) string {
		switch {
		case dir == "":
			return targetRef(t)
		case level != nil && level.Name == "instance":
			return t.Host + ":" + filepath.Base(level.Dir) + "/" + dir
		}
		return t.Host + ":" + dir
	}
	dirs := make([]string, 0, len(traces))
	parsed := map[string]cli.EnvTraced{}
	for dir, line := range traces {
		tr, err := cli.ParseEnvTrace([]byte(line))
		if err != nil {
			continue
		}
		dirs = append(dirs, dir)
		parsed[dir] = tr
	}
	sort.Strings(dirs)
	for _, k := range keys {
		if verb == "unset" {
			if level != nil {
				continue
			}
			for _, dir := range dirs {
				if defs := parsed[dir].Lookup(k); len(defs) > 0 {
					report.Info(fmt.Sprintf("%s now has the value %s", k, levelSays(defs[0].Level, own, parsed[dir].Parent)))
				}
			}
			continue
		}
		type overridden struct{ addr, says string }
		var over []overridden
		for _, dir := range dirs {
			tr := parsed[dir]
			defs := tr.Lookup(k)
			i := slices.IndexFunc(defs, func(d cli.EnvTracedDef) bool { return d.Level == ours })
			switch {
			case i > 0:
				sayHidden(k, oursSays, addr(dir), defs[:i], own, tr.Parent)
			case i == 0 && len(defs) > 1:
				over = append(over, overridden{addr(dir), levelSays(defs[1].Level, own, tr.Parent)})
			}
		}
		unset := report.CmdMark + "devopsy @" + targetRef(t) + " " + flag + " unset " + k
		switch {
		case len(over) == 0:
		case level == nil:
			report.Warn(fmt.Sprintf("%s overrides the value %s", k, over[0].says), "to use that value again:", unset)
		case len(over) == 1:
			report.Warn(fmt.Sprintf("%s in %s overrides, on @%s, the value %s", k, oursSays, over[0].addr, over[0].says), "to use that value again:", unset)
		default:
			details := []string{"the values it overrides:"}
			for _, o := range over {
				details = append(details, fmt.Sprintf("@%s: the value %s", o.addr, o.says))
			}
			report.Warn(fmt.Sprintf("%s in %s overrides another value on %d environments", k, oursSays, len(over)), append(details, "to use those values again:", unset)...)
		}
	}
}

// sayHidden warns that k, set in oursSays, has no effect on the
// environment at addr, where nearer definitions override it (by
// precedence), with what lets it through: the unset of each file of the
// environment's or the instance's, and how an inherited one goes.
func sayHidden(k, oursSays, addr string, nearer []cli.EnvTracedDef, own, parent string) {
	says := make([]string, 0, len(nearer))
	var fixes []string
	for _, d := range nearer {
		says = append(says, "the value "+levelSays(d.Level, own, parent))
		switch d.Level {
		case cli.LevelEnv:
			fixes = append(fixes, report.CmdMark+"devopsy @"+addr+" --vars unset "+k)
		case cli.LevelInstance:
			fixes = append(fixes, report.CmdMark+"devopsy @"+addr+" --vars-instance unset "+k)
		case cli.LevelInherit:
			from := "its parent"
			if parent != "" {
				from = parent
			}
			fixes = append(fixes, fmt.Sprintf("the inherited value goes once %s's inherit: no longer lists %s and @%s's next --inherit or --sync", from, k, addr))
		}
	}
	verb := "overrides"
	if len(says) > 1 {
		verb = "override"
	}
	details := []string{}
	if len(fixes) > 0 {
		details = append([]string{"to let it through:"}, fixes...)
	}
	report.Warn(fmt.Sprintf("%s in %s has no effect on @%s: %s %s it", k, oursSays, addr, joinAnd(says), verb), details...)
}
