package main

import (
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
var debugTopics = []string{"targets", "capabilities", "labels"}

// runDebug implements `devopsy --debug [targets [name] [--yaml] |
// capabilities | labels]`: what devopsy sees and computes, to explain its
// behavior. On a server (`devopsy @<target> --debug`), the server's view.
func runDebug(cwd string, args []string, color bool) int {
	projectDir, _ := cli.FindProjectDir(cwd)
	st := styleFor(os.Stdout)
	topic := ""
	if len(args) > 0 {
		topic = args[0]
	}
	switch topic {
	case "":
		debugSummary(st, projectDir)
	case "targets":
		asYAML := slices.Contains(args[1:], "--yaml")
		name := ""
		for _, a := range args[1:] {
			if a != "--yaml" {
				name = a
			}
		}
		return debugTargets(st, projectDir, name, asYAML, color)
	case "capabilities":
		debugCapabilities(st, projectDir)
	case "labels":
		return debugLabels(st, projectDir, color)
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
	field("user targets file", tilde(remote.UserTargetsFile()))
	if projectDir == "" {
		field("project", st.warn("none (no .devopsy/ here or above)"))
	} else {
		files := []string{"compose.yaml"}
		for _, f := range []string{"compose.override.yaml", "compose.override.yml", ".env", "target.env", remote.TargetsFile, remote.LocalTargetsFile} {
			if _, err := os.Stat(filepath.Join(projectDir, f)); err == nil {
				files = append(files, f)
			}
		}
		field("project", tilde(projectDir)+" "+st.dim("("+strings.Join(files, ", ")+")"))
		field("commands", orNone(cli.CustomCommands(projectDir)))
	}
	var project, user []string
	for _, t := range remote.Targets(projectDir) {
		if t.User {
			user = append(user, t.Name)
		} else {
			project = append(project, t.Name)
		}
	}
	field("targets", orNone(project))
	field("user-level targets", orNone(user))
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
	fmt.Printf("\n%s devopsy --debug targets [name] [--yaml] | capabilities | labels\n", st.dim("More:"))
}

// yamlTarget is a target as devopsy uses it, for --debug targets --yaml.
type yamlTarget struct {
	Host     string            `yaml:"host"`
	Path     string            `yaml:"path"`
	Mode     string            `yaml:"mode"`
	Source   string            `yaml:"source,omitempty"`
	Release  *yamlSteps        `yaml:"release,omitempty"`
	Rollback *yamlSteps        `yaml:"rollback,omitempty"`
	Env      map[string]string `yaml:"env,omitempty"`
}

type yamlSteps struct {
	Before []string `yaml:"before,omitempty"`
	Remote string   `yaml:"remote"`
	After  []string `yaml:"after,omitempty"`
}

func toYAMLSteps(s *remote.Steps) *yamlSteps {
	if s == nil {
		return nil
	}
	return &yamlSteps{Before: s.Before, Remote: s.Remote, After: s.After}
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
	var names []string
	for _, t := range remote.Targets(projectDir) {
		if name == "" || t.Name == name {
			names = append(names, t.Name)
		}
	}
	if len(names) == 0 {
		msg := "no targets"
		if name != "" {
			msg = "no target " + name
		}
		cli.Fprint(os.Stderr, red, msg, color)
		return 1
	}

	if asYAML {
		// A mapping node, so the order is Targets' (a map would sort by name).
		out := &yaml.Node{Kind: yaml.MappingNode}
		for _, n := range names {
			t, err := remote.LoadTarget(projectDir, n, projectEnv)
			if err != nil {
				fmt.Fprintf(os.Stderr, "# %s: %v\n", n, err)
				continue
			}
			var value yaml.Node
			if err := value.Encode(&yamlTarget{Host: t.Host, Path: t.Path, Mode: t.Mode, Source: t.Source,
				Release: toYAMLSteps(t.Release), Rollback: toYAMLSteps(t.Rollback), Env: t.Env}); err != nil {
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
		t, err := remote.LoadTarget(projectDir, n, projectEnv)
		if err != nil {
			fmt.Printf("%s\n  %s %v\n", st.name(n), st.warn("error:"), err)
			continue
		}
		kind := ""
		if t.User {
			kind = ", user-level"
		}
		fmt.Printf("%s  %s\n", st.name(st.head(t.Name)), st.dim("("+tilde(t.File)+kind+")"))
		row := func(field, value, from string) {
			if value == "" {
				return
			}
			if from != "" {
				from = "  " + st.dim("("+tilde(from)+")")
			}
			fmt.Printf("  %s %s%s\n", st.head(fmt.Sprintf("%-9s", field)), value, from)
		}
		row("host", t.Host, t.From["host"])
		row("path", t.Path, t.From["path"])
		row("mode", t.Mode, t.From["mode"])
		row("source", t.Source, t.From["source"])
		row("release", steps(t.Release), t.From["release"])
		row("rollback", steps(t.Rollback), t.From["rollback"])
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
	parts := []string{}
	if len(s.Before) > 0 {
		parts = append(parts, "before: "+strings.Join(s.Before, ", "))
	}
	parts = append(parts, "remote: "+s.Remote)
	if len(s.After) > 0 {
		parts = append(parts, "after: "+strings.Join(s.After, ", "))
	}
	return strings.Join(parts, "; ")
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
	holders, err := cli.Containers(cli.RoleLabel)
	if err != nil {
		fmt.Println("  " + st.warn(err.Error()))
		return
	}
	roles := map[string][]string{}
	for _, c := range holders {
		r := c.Labels[cli.RoleLabel]
		if !slices.Contains(roles[r], c.Project) {
			roles[r] = append(roles[r], c.Project)
		}
	}
	if len(roles) == 0 {
		fmt.Println("  no roles")
	}
	names := make([]string, 0, len(roles))
	for r := range roles {
		names = append(names, r)
	}
	sort.Strings(names)
	for _, r := range names {
		fmt.Printf("  %s  held by %s\n", st.name("role "+r), strings.Join(roles[r], ", "))
		for _, project := range roles[r] {
			list, _ := cli.Containers("com.docker.compose.project=" + project)
			exports := map[string]bool{}
			for _, c := range list {
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
	current := map[string]string{}
	if data, err := os.ReadFile(filepath.Join(projectDir, "target.env")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(k, "#") {
				current[k] = strings.Trim(v, `'"`)
			}
		}
	}
	for _, imp := range imports {
		now := ""
		switch e, err := cli.Resolve(imp); {
		case err != nil:
			now = st.warn(err.Error())
		case !e.Found && e.Project == "":
			now = st.warn("nothing running is " + imp.Source)
		case !e.Found:
			now = st.warn(e.Project + " does not export " + imp.Key)
		default:
			now = fmt.Sprintf("%q from %s", e.Value, e.Project)
		}
		fmt.Printf("  %s\n      now: %s\n", st.name(imp.String()), now)
		if v, ok := current[imp.Var]; ok {
			fmt.Printf("      this release's target.env: %q\n", v)
		}
	}
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
