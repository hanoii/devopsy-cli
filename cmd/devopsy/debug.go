package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// debugTopics are what `devopsy --debug <topic>` explains.
var debugTopics = []string{"targets", "capabilities", "labels"}

// runDebug implements `devopsy --debug [targets [name] | capabilities |
// labels]`: what devopsy sees and computes, to explain its behavior. On a
// server (`devopsy @<target> --debug`), the server's view.
func runDebug(cwd string, args []string, color bool) int {
	projectDir, _ := cli.FindProjectDir(cwd)
	topic := ""
	if len(args) > 0 {
		topic = args[0]
	}
	switch topic {
	case "":
		debugSummary(projectDir)
	case "targets":
		name := ""
		if len(args) > 1 {
			name = args[1]
		}
		return debugTargets(projectDir, name, color)
	case "capabilities":
		debugCapabilities(projectDir)
	case "labels":
		return debugLabels(projectDir, color)
	default:
		cli.Fprint(os.Stderr, red, fmt.Sprintf("--debug: no topic %q: %s", topic, strings.Join(debugTopics, ", ")), color)
		return 1
	}
	return 0
}

func debugSummary(projectDir string) {
	self, _ := os.Executable()
	fmt.Printf("devopsy %s (%s)\n", version, self)
	compose := "docker compose: not available"
	if out, err := exec.Command("docker", "compose", "version", "--short").Output(); err == nil {
		compose = "docker compose " + strings.TrimSpace(string(out))
	}
	fmt.Printf("%s, %s\n", dockerVersion(), compose)
	fmt.Printf("user-level targets: %s\n", remote.UserTargetsFile())
	if projectDir == "" {
		fmt.Println("project: none (no .devopsy/ here or above)")
	} else {
		files := []string{"compose.yaml"}
		for _, f := range []string{"compose.override.yaml", "compose.override.yml", ".env", "target.env", remote.TargetsFile, remote.LocalTargetsFile} {
			if _, err := os.Stat(filepath.Join(projectDir, f)); err == nil {
				files = append(files, f)
			}
		}
		fmt.Printf("project: %s (%s)\n", projectDir, strings.Join(files, ", "))
		fmt.Printf("  commands: %s\n", orNone(cli.CustomCommands(projectDir)))
	}
	var project, user []string
	for _, t := range remote.Targets(projectDir) {
		if t.User {
			user = append(user, t.Name)
		} else {
			project = append(project, t.Name)
		}
	}
	fmt.Printf("  targets: %s; user-level: %s\n", orNone(project), orNone(user))
	var caps []string
	for _, c := range cli.Capabilities {
		if have := cli.ImplementedActions(projectDir, c); len(have) > 0 {
			caps = append(caps, c.Name+" ("+strings.Join(have, ", ")+")")
		}
	}
	fmt.Printf("  capabilities implemented: %s\n", orNone(caps))
	if projectDir != "" {
		if labels, err := cli.DevopsyLabels(projectDir); err == nil {
			fmt.Printf("  devopsy labels: %s\n", orNone(labelList(labels)))
		}
	}
	fmt.Printf("\nMore: devopsy --debug targets [name] | capabilities | labels\n")
}

// debugTargets shows targets as devopsy computes them, and where each value
// came from.
func debugTargets(projectDir, name string, color bool) int {
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
	for i, n := range names {
		if i > 0 {
			fmt.Println()
		}
		t, err := remote.LoadTarget(projectDir, n, projectEnv)
		if err != nil {
			fmt.Printf("%s\n  error: %v\n", n, err)
			continue
		}
		kind := ""
		if t.User {
			kind = ", user-level"
		}
		fmt.Printf("%s  (%s%s)\n", t.Name, tilde(t.File), kind)
		row := func(field, value string) {
			if value == "" {
				return
			}
			from := t.From[field]
			if from != "" {
				from = "  (" + tilde(from) + ")"
			}
			fmt.Printf("  %-9s %s%s\n", field, value, from)
		}
		row("host", t.Host)
		row("path", t.Path)
		row("mode", t.Mode)
		row("source", t.Source)
		row("release", steps(t.Release))
		row("rollback", steps(t.Rollback))
		keys := make([]string, 0, len(t.Env))
		for k := range t.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			from := t.From["env."+k]
			if from != "" {
				from = "  (" + tilde(from) + ")"
			}
			fmt.Printf("  env       %s%s\n", cli.DotenvLine(k, t.Env[k]), from)
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

func debugCapabilities(projectDir string) {
	fmt.Println("Capabilities devopsy calls: .devopsy/capabilities/<name>/<action>, executables a")
	fmt.Println("project ships to implement them (devopsy-cli README, Capabilities).")
	for _, c := range cli.Capabilities {
		fmt.Printf("\n%s\n  implemented by: %s\n  called by:      %s\n", c.Name, c.Who, c.When)
		for _, a := range c.Actions {
			fmt.Printf("  %s\n      %s\n", a.Usage, a.Does)
		}
		switch have := cli.ImplementedActions(projectDir, c); {
		case projectDir == "":
		case len(have) == 0:
			fmt.Println("  this project: does not implement it")
		default:
			fmt.Printf("  this project: implements %s\n", strings.Join(have, ", "))
		}
	}
}

func debugLabels(projectDir string, color bool) int {
	if projectDir == "" {
		cli.Fprint(os.Stderr, red, "--debug labels: not in a devopsy project", color)
		return 1
	}
	labels, err := cli.DevopsyLabels(projectDir)
	if err != nil {
		cli.Fprint(os.Stderr, red, err.Error(), color)
		return 1
	}
	fmt.Println("Labels devopsy reads, on compose services:")
	fmt.Printf("  %-20s %s\n", cli.ShellLabel+"=true", "the service --shell opens by default")
	fmt.Printf("  %-20s %s\n\n", cli.ShellUserLabel+"=<user>", "the user of its shell and --shell commands, unless --user")
	if len(labels) == 0 {
		fmt.Println("This project sets none.")
		return 0
	}
	for _, l := range labelList(labels) {
		fmt.Println("  " + l)
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
