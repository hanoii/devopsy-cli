package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ShellLabel marks the service `devopsy --shell` opens by default, and
// ShellUserLabel the user a service's shell runs as, when not its own: an
// image whose entrypoint drops from root, which compose exec skips.
const (
	ShellLabel     = "devopsy.shell"
	ShellUserLabel = "devopsy.shell.user"
)

// shellCommand runs bash where the image has it, else sh.
const shellCommand = "if command -v bash >/dev/null 2>&1; then exec bash; else exec sh; fi"

// RunningServices lists the project's running services, for --shell when no
// service is named or labeled; replaceable in tests.
var RunningServices = func(p *loadedProject) ([]string, error) {
	plan := p.compose([]string{"ps", "--services", "--status", "running"})
	cmd := exec.Command("docker", plan.Args[1:]...)
	cmd.Env = plan.Env
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// shell plans `devopsy --shell [service] [exec options...] [-- command...]`:
// a shell in a container, as its service's user, or with a command after
// `--`, that command, run directly like docker compose exec runs it. The
// project's shell capability (capabilities/shell/open) replaces it, with the
// same arguments. Otherwise the service is the one named, else the one
// labeled devopsy.shell=true, else the only one running.
func shell(p *loadedProject, args []string) (*Plan, error) {
	secrets := NewSecrets(p.env, p.dotenvFile)
	open := filepath.Join(p.dir, "capabilities", "shell", "open")
	if isExecutableFile(open) {
		cmdArgs := append([]string{open}, args...)
		return &Plan{
			Path:    open,
			Args:    cmdArgs,
			Env:     p.env.Environ(),
			Verbose: maskAll(secrets, append(p.verbose, "devopsy: running '"+strings.Join(cmdArgs, " ")+"'")),
		}, nil
	}

	var command []string
	if i := slices.Index(args, "--"); i >= 0 {
		args, command = args[:i], args[i+1:]
	}
	service := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		service, args = args[0], args[1:]
	}
	services, labeled, users, err := composeServices(p)
	if err != nil {
		return nil, &ExitError{Code: 1, Msg: "--shell: " + err.Error()}
	}
	if service == "" {
		switch {
		case len(labeled) == 1:
			service = labeled[0]
		case len(labeled) > 1:
			return nil, &ExitError{Code: 1, Msg: fmt.Sprintf("--shell: several services are labeled %s=true (%s): label one, or name it: devopsy --shell <service>", ShellLabel, strings.Join(labeled, ", "))}
		default:
			running, err := RunningServices(p)
			if err == nil && len(running) == 1 {
				service = running[0]
			} else {
				return nil, &ExitError{Code: 1, Msg: fmt.Sprintf("--shell: which service? Name it (devopsy --shell <service>), or label it %s=true in compose.yaml. Services: %s", ShellLabel, strings.Join(services, ", "))}
			}
		}
	}
	if user := users[service]; user != "" && !hasUserOption(args) {
		args = append([]string{"--user", user}, args...)
	}
	run := []string{"sh", "-c", shellCommand}
	what := "a shell"
	if len(command) > 0 {
		run, what = command, strings.Join(command, " ")
	}
	plan := p.compose(append(append(append([]string{"exec"}, args...), service), run...))
	plan.Notice = secrets.Mask(fmt.Sprintf("Running 'docker compose exec %s' (%s)...", strings.TrimSpace(strings.Join(args, " ")+" "+service), what))
	plan.Verbose = maskAll(secrets, p.verbose)
	return plan, nil
}

// hasUserOption reports whether exec options already choose a user.
func hasUserOption(args []string) bool {
	for _, a := range args {
		if a == "-u" || a == "--user" || strings.HasPrefix(a, "--user=") || (strings.HasPrefix(a, "-u") && len(a) > 2) {
			return true
		}
	}
	return false
}

// Services lists the services in the compose files of the project at
// projectDir (its .devopsy/), for completion; nil when it cannot tell.
func Services(projectDir string) []string {
	if projectDir == "" {
		return nil
	}
	services, _, _, err := composeServices(&loadedProject{dir: projectDir, composeFile: filepath.Join(projectDir, "compose.yaml")})
	if err != nil {
		return nil
	}
	return services
}

// composeServices reads the services of the project's compose file and its
// override, those labeled devopsy.shell=true, both sorted, and each
// service's devopsy.shell.user. Labels are lists ("k=v") or maps, as compose
// allows.
func composeServices(p *loadedProject) (services, labeled []string, users map[string]string, err error) {
	files := []string{p.composeFile}
	for _, name := range []string{"compose.override.yaml", "compose.override.yml"} {
		if fi, err := os.Stat(filepath.Join(p.dir, name)); err == nil && fi.Mode().IsRegular() {
			files = append(files, filepath.Join(p.dir, name))
			break
		}
	}
	all := map[string]bool{}
	marked := map[string]bool{}
	users = map[string]string{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, nil, nil, err
		}
		var doc struct {
			Services map[string]struct {
				Labels yaml.Node `yaml:"labels"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", file, err)
		}
		for name, s := range doc.Services {
			all[name] = true
			if v, ok := labelValue(s.Labels, ShellLabel); ok {
				marked[name] = v == "true"
			}
			if v, ok := labelValue(s.Labels, ShellUserLabel); ok {
				users[name] = v
			}
		}
	}
	for name := range all {
		services = append(services, name)
		if marked[name] {
			labeled = append(labeled, name)
		}
	}
	sort.Strings(services)
	sort.Strings(labeled)
	return services, labeled, users, nil
}

// labelValue finds key in a compose labels node, a list of "k=v" or a map.
func labelValue(n yaml.Node, key string) (string, bool) {
	switch n.Kind {
	case yaml.SequenceNode:
		for _, item := range n.Content {
			if k, v, _ := strings.Cut(item.Value, "="); k == key {
				return strings.TrimSpace(v), true
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				return strings.TrimSpace(n.Content[i+1].Value), true
			}
		}
	}
	return "", false
}
