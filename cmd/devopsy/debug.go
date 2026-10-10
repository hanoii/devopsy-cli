package main

import (
	"cmp"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// debugTopics are what `devopsy --debug <topic>` explains.
var debugTopics = []string{"environments", "env", "capabilities", "labels", "imports", "schema"}

// runDebug implements `devopsy --debug [environments [name] [--yaml] |
// env [--show] [VAR...] | capabilities | labels | imports]`: what devopsy sees and computes, to explain its
// behavior. On a server (`devopsy @<target> --debug`), the server's view.
func runDebug(cwd string, args []string, color bool) int {
	projectDir, _ := cli.FindProjectDir(cwd)
	st := styleFor(os.Stdout)
	topic := ""
	if len(args) > 0 {
		topic = args[0]
	}
	switch topic {
	case "--help", "-h":
		fmt.Print(st.help(cli.DebugHelp))
	case "":
		debugSummary(st, projectDir)
	case "environments":
		asYAML := slices.Contains(args[1:], "--yaml")
		name := ""
		for _, a := range args[1:] {
			if a != "--yaml" {
				name = a
			}
		}
		return debugTargets(st, projectDir, name, asYAML, color)
	case "env":
		return debugEnv(st, cwd, args[1:], color)
	case "capabilities":
		debugCapabilities(st, projectDir)
	case "labels":
		return debugLabels(st, projectDir, color)
	case "imports":
		return debugImports(st, color)
	case "schema":
		if slices.Contains(args[1:], "--user") {
			fmt.Print(remote.UserSchema)
		} else {
			fmt.Print(remote.ProjectSchema)
		}
	default:
		cli.Fprint(os.Stderr, red, fmt.Sprintf("--debug: no topic %q: %s", topic, strings.Join(debugTopics, ", ")), color)
		return 1
	}
	return 0
}

func debugSummary(st style, projectDir string) {
	field := func(k, v string) { fmt.Printf("%s %s\n", st.head(fmt.Sprintf("%-22s", k)), v) }
	self, _ := os.Executable()
	field("devopsy", version+" "+st.dim("("+tilde(self)+")"))
	compose := "not available"
	if out, err := exec.Command("docker", "compose", "version", "--short").Output(); err == nil {
		compose = strings.TrimSpace(string(out))
	}
	field("docker", strings.TrimPrefix(dockerVersion(), "docker "))
	field("docker compose", compose)
	field("user config", tilde(remote.UserConfigFile()))
	if r, err := remote.ReleaseSettings(); err == nil {
		field("releases here", fmt.Sprintf("root %s, keep %d, max_keep %d %s", tilde(r.Root), r.Keep, r.MaxKeep, st.dim("(for releases landing on this machine)")))
	} else {
		field("releases here", st.warn(err.Error()))
	}
	if projectDir == "" {
		field("project", st.warn("none (no .devopsy/ here or above)"))
	} else {
		files := []string{"compose.yaml"}
		for _, f := range []string{"compose.override.yaml", "compose.override.yml", ".env", "instance.env", "project.env", "target.env", remote.ConfigFile, remote.LocalConfigFile} {
			if _, err := os.Stat(filepath.Join(projectDir, f)); err == nil {
				files = append(files, f)
			}
		}
		field("project", tilde(projectDir)+" "+st.dim("("+strings.Join(files, ", ")+")"))
		field("commands", orNone(cli.CustomCommands(projectDir)))
		switch p, err := remote.LoadProject(projectDir); {
		case err != nil:
			field("name on servers", st.warn(err.Error()))
		case p == nil || p.Name == "":
			field("name on servers", st.warn("none: set project: in "+remote.ConfigFile))
		default:
			v := p.Name
			switch p.Instances {
			case remote.InstancesRequired:
				v += " (instances required: @[<server>:]<instance>/<environment>)"
			case remote.InstancesNone:
				v += " (no instances)"
			}
			field("name on servers", v)
		}
	}
	var project, user []string
	for _, t := range remote.Targets(projectDir) {
		if t.User {
			user = append(user, t.Name)
		} else {
			project = append(project, t.Name)
		}
	}
	field("environments", orNone(project))
	if pats := remote.Patterns(projectDir); len(pats) > 0 {
		field("env patterns", strings.Join(pats, ", "))
	}
	for _, k := range []string{remote.ServerVar, remote.InstanceVar} {
		if v := os.Getenv(k); v != "" {
			field(k, v)
		}
	}
	field("aliases", orNone(user))
	var caps []string
	for _, c := range cli.Capabilities {
		if have := cli.ImplementedActions(projectDir, c); len(have) > 0 {
			caps = append(caps, c.Name+" ("+strings.Join(have, ", ")+")")
		}
	}
	field("capabilities", orNone(caps))
	if projectDir != "" {
		if labels, err := cli.DevopsyLabels(projectDir); err == nil {
			field("devopsy labels", orNone(labelList(labels)))
		}
	}
	fmt.Printf("\n%s devopsy --debug environments [name or address] [--yaml] | env [--show] [VAR...] | capabilities | labels | imports | schema [--user]\n", st.dim("More:"))
}

// yamlTarget is an environment as devopsy uses it, for --debug environments --yaml.
type yamlTarget struct {
	Address  string            `yaml:"target,omitempty"`
	Host     string            `yaml:"server,omitempty"`
	Path     string            `yaml:"path"`
	Mode     string            `yaml:"mode"`
	Source   string            `yaml:"source,omitempty"`
	Release  *yamlSteps        `yaml:"release,omitempty"`
	Rollback *yamlSteps        `yaml:"rollback,omitempty"`
	Destroy  string            `yaml:"destroy,omitempty"`
	Keep     int               `yaml:"keep,omitempty"`
	Env      map[string]string `yaml:"env,omitempty"`
}

type yamlSteps struct {
	Before  []map[string]string `yaml:"before,omitempty"`
	Prepare []string            `yaml:"prepare,omitempty"`
	Run     string              `yaml:"run"`
	After   []map[string]string `yaml:"after,omitempty"`
}

func toYAMLSteps(s *remote.Steps) *yamlSteps {
	if s == nil {
		return nil
	}
	phase := func(steps []remote.Step) []map[string]string {
		var out []map[string]string
		for _, st := range steps {
			where := "local"
			if st.Remote {
				where = "remote"
			}
			out = append(out, map[string]string{where: st.Cmd})
		}
		return out
	}
	return &yamlSteps{Before: phase(s.Before), Prepare: s.Prepare, Run: s.Run, After: phase(s.After)}
}

// debugTargets shows targets as devopsy computes them: with where each value
// came from, or as plain YAML (--yaml).
func debugTargets(st style, projectDir, name string, asYAML, color bool) int {
	var projectEnv func(string) (string, bool)
	if projectDir != "" {
		env := cli.NewEnv(os.Environ())
		if err := cli.LoadDotenv(env, filepath.Join(projectDir, ".env")); err == nil {
			projectEnv = env.Lookup
		}
	}
	// Every environment and alias, or one: a name, or any address.
	var names []string
	if name != "" {
		names = []string{strings.TrimPrefix(name, "@")}
	}
	for _, t := range remote.Targets(projectDir) {
		if name == "" {
			names = append(names, t.Name)
		}
	}
	if len(names) == 0 {
		cli.Fprint(os.Stderr, red, "no environments or aliases", color)
		return 1
	}

	if asYAML {
		// A mapping node, so the order is Targets' (a map would sort by name).
		out := &yaml.Node{Kind: yaml.MappingNode}
		for _, n := range names {
			t, err := remote.DescribeTarget(projectDir, n, projectEnv)
			if err != nil {
				fmt.Fprintf(os.Stderr, "# %s: %v\n", n, err)
				continue
			}
			var value yaml.Node
			if err := value.Encode(&yamlTarget{Address: t.Address, Host: t.Host, Path: t.Path, Mode: t.Mode, Source: t.Source,
				Release: toYAMLSteps(t.Release), Rollback: toYAMLSteps(t.Rollback), Destroy: t.Destroy, Keep: t.Keep, Env: t.Env}); err != nil {
				cli.Fprint(os.Stderr, red, err.Error(), color)
				return 1
			}
			out.Content = append(out.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: n}, &value)
		}
		enc := yaml.NewEncoder(os.Stdout)
		enc.SetIndent(2)
		if err := enc.Encode(out); err != nil {
			cli.Fprint(os.Stderr, red, err.Error(), color)
			return 1
		}
		return 0
	}

	for i, n := range names {
		if i > 0 {
			fmt.Println()
		}
		t, err := remote.DescribeTarget(projectDir, n, projectEnv)
		if err != nil {
			fmt.Printf("%s\n  %s %v\n", st.name(n), st.warn("error:"), err)
			continue
		}
		kind := ""
		if t.User {
			kind = ", alias " + t.Alias
		}
		fmt.Printf("%s  %s\n", st.name(st.head(n)), st.dim("("+tilde(t.File)+kind+")"))
		row := func(field, value, from string) {
			if value == "" {
				return
			}
			if from != "" {
				from = "  " + st.dim("("+tilde(from)+")")
			}
			fmt.Printf("  %s %s%s\n", st.head(fmt.Sprintf("%-9s", field)), value, from)
		}
		if t.Host != "" {
			row("target", t.Address, "")
		}
		row("server", cmp.Or(t.Host, st.warn("none yet: @<server>:"+t.Name+", or "+remote.ServerVar)), t.From["server"])
		row("instance", t.Instance, t.From["instance"])
		if t.Project != nil {
			row("project", t.Project.Name, filepath.Join(t.Project.Dir, remote.ConfigFile))
		}
		row("compose", t.ComposeName(), "")
		row("path", t.Path, t.From["path"])
		row("mode", t.Mode, t.From["mode"])
		row("source", t.Source, "")
		row("release", steps(t.Release), t.From["release"])
		row("rollback", steps(t.Rollback), t.From["rollback"])
		row("destroy", t.Destroy, t.From["destroy"])
		if t.Keep > 0 {
			row("keep", fmt.Sprint(t.Keep), t.From["keep"])
		}
		keys := make([]string, 0, len(t.Env))
		for k := range t.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			row("env", cli.DotenvLine(k, t.Env[k]), t.From["env."+k])
		}
	}
	return 0
}

func steps(s *remote.Steps) string {
	if s == nil {
		return ""
	}
	phase := func(steps []remote.Step) string {
		var out []string
		for _, st := range steps {
			out = append(out, st.String())
		}
		return strings.Join(out, ", ")
	}
	parts := []string{}
	if len(s.Before) > 0 {
		parts = append(parts, "before: "+phase(s.Before))
	}
	if len(s.Prepare) > 0 {
		parts = append(parts, "prepare: "+strings.Join(s.Prepare, ", "))
	}
	parts = append(parts, "run: "+s.Run)
	if len(s.After) > 0 {
		parts = append(parts, "after: "+phase(s.After))
	}
	return strings.Join(parts, "; ")
}

// debugEnv traces the project's variables: each with the place that sets it
// and the ones it overrides, by precedence.
func debugEnv(st style, cwd string, args []string, color bool) int {
	show := false
	var names []string
	for _, a := range args {
		switch {
		case a == "--show":
			show = true
		case strings.HasPrefix(a, "-"):
			cli.Fprint(os.Stderr, red, "usage: devopsy --debug env [--show] [VAR...]", color)
			return 1
		default:
			names = append(names, a)
		}
	}
	projectDir, vars, secrets, err := cli.TraceEnv(cwd, os.Environ(), names)
	if err != nil {
		cli.Fprint(os.Stderr, red, err.Error(), color)
		return 1
	}
	if show {
		secrets = nil
	}
	// Files as the project names them; on servers most are links.
	file := func(path string) string {
		name := path
		if rel, err := filepath.Rel(projectDir, path); err == nil && !strings.HasPrefix(rel, "..") {
			name = rel
		}
		if real, err := filepath.EvalSymlinks(path); err == nil && real != path {
			name += " " + st.dim("(-> "+tilde(real)+")")
		}
		return name
	}
	source := func(d cli.EnvDef) string {
		switch d.Kind {
		case cli.EnvCaller:
			return "the caller's environment"
		case cli.EnvImported:
			return file(d.Source) + ", imported at release"
		case cli.EnvDevopsy:
			return "devopsy, from " + tilde(d.Source)
		case cli.EnvComputed:
			return file(d.Source) + ", computed"
		}
		return file(d.Source)
	}
	fmt.Println(st.dim("First wins: the caller's environment, .env, instance.env, project.env, target.env, the env capability."))
	for _, v := range vars {
		if len(v.Defs) == 0 {
			fmt.Printf("%s %s\n", st.name(v.Name), st.warn("not set"))
			continue
		}
		// Masked before quoting: quoting can escape a secret out of sight.
		quoted := func(value string) string {
			return strings.TrimPrefix(cli.DotenvLine("", secrets.Mask(value)), "=")
		}
		fmt.Printf("%s=%s\n", st.name(v.Name), quoted(v.Defs[0].Value))
		for i, d := range v.Defs {
			if i == 0 {
				fmt.Printf("  %s  %s\n", st.ok("set by    "), source(d))
				continue
			}
			fmt.Printf("  %s  %s: %s\n", st.warn("overridden"), source(d), quoted(d.Value))
		}
	}
	return 0
}

func debugCapabilities(st style, projectDir string) {
	fmt.Println("A capability is an interface devopsy defines and a project may implement:")
	fmt.Println("executables in .devopsy/capabilities/<capability>/<action>, which devopsy runs")
	fmt.Println("(people never do), in the project's directory with its environment.")
	fmt.Println(st.dim("Full contracts: devopsy-cli's README, Capabilities."))
	for _, c := range cli.Capabilities {
		fmt.Printf("\n%s\n", st.name(st.head(c.Name)))
		fmt.Printf("  %s\n", wrap(c.What, 76, "  "))
		fmt.Printf("  %s %s\n", st.head("implemented by:"), c.Who)
		fmt.Printf("  %s %s\n", st.head("called by:     "), c.CalledBy)
		if projectDir != "" {
			have := cli.ImplementedActions(projectDir, c)
			if len(have) == 0 {
				fmt.Printf("  %s %s\n", st.head("this project:  "), st.warn("does not implement it"))
			} else {
				fmt.Printf("  %s %s\n", st.head("this project:  "), st.ok("implements "+strings.Join(have, ", ")))
			}
		}
		fmt.Printf("  %s %s\n", st.head("actions:"), st.dim(".devopsy/capabilities/"+c.Name+"/<action>"))
		for _, a := range c.Actions {
			fmt.Printf("    %s\n", st.name(a.Usage))
			fmt.Printf("      %s\n", wrap(a.Does, 72, "      "))
			if a.Output != "" {
				for _, l := range strings.Split(a.Output, "\n") {
					fmt.Printf("        %s\n", st.dim(l))
				}
			}
		}
	}
}

// wrap breaks text into lines of at most width, the next ones indented.
func wrap(text string, width int, indent string) string {
	var lines []string
	line := ""
	for _, w := range strings.Fields(text) {
		if line != "" && len(line)+1+len(w) > width {
			lines = append(lines, line)
			line = w
			continue
		}
		if line != "" {
			line += " "
		}
		line += w
	}
	lines = append(lines, line)
	return strings.Join(lines, "\n"+indent)
}

func debugLabels(st style, projectDir string, color bool) int {
	if projectDir == "" {
		cli.Fprint(os.Stderr, red, "--debug labels: not in a devopsy project", color)
		return 1
	}
	labels, err := cli.DevopsyLabels(projectDir)
	if err != nil {
		cli.Fprint(os.Stderr, red, err.Error(), color)
		return 1
	}
	fmt.Println(st.head("Labels devopsy reads, on compose services:"))
	for _, l := range [][2]string{
		{cli.ShellLabel + "=true", "the service --shell opens by default"},
		{cli.ShellUserLabel + "=<user>", "the user of its shell and --shell commands, unless --user"},
		{cli.RoleLabel + "=<name>", "a role only one compose project per host holds, checked at --release"},
		{cli.ExportPrefix + "<KEY>=<value>", "a fact for other projects, read from running containers"},
		{cli.ImportPrefix + "<VAR>=<role or project>/<KEY>[?]", "at --release, into target.env unless the target sets VAR; ? if optional"},
	} {
		fmt.Printf("  %s\n      %s\n", st.name(l[0]), l[1])
	}
	fmt.Println()
	if len(labels) == 0 {
		fmt.Println("This project sets none.")
	} else {
		fmt.Println(st.head("This project's:"))
		for _, l := range labelList(labels) {
			fmt.Println("  " + l)
		}
	}
	debugHost(st, projectDir)
	return 0
}

// debugHost shows the roles held on this host, through the local docker, and
// what this project's imports resolve to now.
func debugHost(st style, projectDir string) {
	fmt.Println()
	fmt.Println(st.head("On this host (running containers):"))
	host, err := cli.Running()
	if err != nil {
		fmt.Println("  " + st.warn(err.Error()))
		return
	}
	roles := map[string][]string{}
	for _, c := range host {
		r, ok := c.Labels[cli.RoleLabel]
		if ok && !slices.Contains(roles[r], c.Name()) {
			roles[r] = append(roles[r], c.Name())
		}
	}
	if len(roles) == 0 {
		fmt.Println("  no roles")
	}
	for _, r := range slices.Sorted(maps.Keys(roles)) {
		fmt.Printf("  %s  held by %s\n", st.name("role "+r), strings.Join(roles[r], ", "))
		exports := map[string]bool{}
		for _, c := range host {
			if !slices.Contains(roles[r], c.Name()) {
				continue
			}
			for k, v := range c.Labels {
				if strings.HasPrefix(k, cli.ExportPrefix) {
					exports[strings.TrimPrefix(k, cli.ExportPrefix)+"="+v] = true
				}
			}
		}
		for _, e := range slices.Sorted(maps.Keys(exports)) {
			fmt.Printf("    %s %s\n", st.dim("exports"), e)
		}
	}
	_, imports, err := cli.ProjectRoles(projectDir)
	if err != nil {
		fmt.Println("  " + st.warn(err.Error()))
		return
	}
	if len(imports) == 0 {
		return
	}
	fmt.Println()
	fmt.Println(st.head("This project's imports:"))
	_, imported, released := cli.ReleaseEnv(projectDir)
	for _, imp := range imports {
		now := ""
		switch e, err := host.Resolve(imp); {
		case err != nil:
			now = st.warn(err.Error())
		case !e.Found:
			now = st.warn(e.Why(imp))
		default:
			now = fmt.Sprintf("%q from %s", e.Value, e.Project)
		}
		fmt.Printf("  %s\n      now: %s\n", st.name(imp.String()), now)
		if v, ok := imported[imp.Var]; ok {
			fmt.Printf("      this release imported: %q\n", v)
		} else if released {
			fmt.Printf("      this release did not import it (set by the target, or released before the label)\n")
		}
	}
}

// debugImports shows every import of the compose projects running on this
// host against what their current release has: a release keeps what it
// imported, so a changed export needs the importing projects released again.
func debugImports(st style, color bool) int {
	host, err := cli.Running()
	if err != nil {
		cli.Fprint(os.Stderr, red, err.Error(), color)
		return 1
	}
	states := host.ImportStates()
	fmt.Println(st.head("Imports of the compose projects running on this host:"))
	if len(states) == 0 {
		fmt.Println("  none")
		return 0
	}
	stale := 0
	for _, s := range states {
		now := ""
		switch {
		case s.Err != nil:
			now = st.warn(s.Err.Error())
		case !s.Now.Found:
			now = st.warn(s.Now.Why(s.Import))
		default:
			now = fmt.Sprintf("%q from %s", s.Now.Value, s.Now.Project)
		}
		state := ""
		switch {
		case s.Stale():
			state = st.warn("STALE: release " + s.Project + " again")
			stale++
		case s.Set:
			state = fmt.Sprintf("set by the target: %q", s.Release)
		case s.Imported:
			state = st.ok("current")
		case !s.Released:
			state = st.dim("not a release")
		default:
			state = st.warn("not imported by its current release: release it again")
		}
		fmt.Printf("  %s  %s\n", st.name(s.Project), s.Import.String())
		if s.Imported {
			fmt.Printf("      release: %q\n", s.Release)
		}
		fmt.Printf("      now:     %s\n      %s\n", now, state)
	}
	if stale > 0 {
		fmt.Printf("\n%d stale: each release keeps what it imported, so release those projects again.\n", stale)
	}
	return 0
}

func labelList(labels map[string]map[string]string) []string {
	var out []string
	for service, ls := range labels {
		var kv []string
		for k, v := range ls {
			kv = append(kv, k+"="+v)
		}
		sort.Strings(kv)
		out = append(out, service+": "+strings.Join(kv, ", "))
	}
	sort.Strings(out)
	return out
}

// tilde shows paths under the home directory as ~/...
func tilde(s string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return strings.ReplaceAll(s, home+"/", "~/")
	}
	return s
}

func orNone(list []string) string {
	if len(list) == 0 {
		return "none"
	}
	return strings.Join(list, ", ")
}
