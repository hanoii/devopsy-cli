package cli

import (
	"fmt"
	"github.com/hanoii/devopsy-cli/internal/report"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/template"
	"go.yaml.in/yaml/v3"
)

// composeFiles are the compose files devopsy runs a project with, each
// command's -f (loadedProject.compose): compose.yaml and its override,
// never COMPOSE_FILE's, which compose ignores once -f names files.
func composeFiles(projectDir string) []string {
	files := []string{filepath.Join(projectDir, "compose.yaml")}
	for _, name := range []string{"compose.override.yaml", "compose.override.yml"} {
		if fi, err := os.Stat(filepath.Join(projectDir, name)); err == nil && fi.Mode().IsRegular() {
			files = append(files, filepath.Join(projectDir, name))
			break
		}
	}
	return files
}

// ComposeVariables lists the variables the project's compose files
// interpolate.
func ComposeVariables(projectDir string) ([]string, error) {
	var names []string
	for _, file := range composeFiles(projectDir) {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		var dict map[string]any
		if err := yaml.Unmarshal(data, &dict); err != nil {
			return nil, fmt.Errorf("%s: %v", file, err)
		}
		for name := range template.ExtractVariables(dict, nil) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// MissingVars lists the variables p's compose files require (${VAR:?msg},
// ${VAR?msg}) that its environment leaves unset (or empty, for :?), with
// compose's message for each. Compose refuses every command while one is
// missing, so releases check them before going live.
func MissingVars(p *loadedProject) ([][2]string, error) {
	files := composeFiles(p.dir)
	var missing [][2]string
	seen := map[string]bool{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		var dict map[string]any
		if err := yaml.Unmarshal(data, &dict); err != nil {
			return nil, fmt.Errorf("%s: %v", file, err)
		}
		vars := template.ExtractVariables(dict, nil)
		names := make([]string, 0, len(vars))
		for name, v := range vars {
			if v.Required {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		for _, name := range names {
			if seen[name] {
				continue
			}
			value, ok := p.env.Lookup(name)
			colon := regexp.MustCompile(`\$\{` + name + `:\?`).Match(data)
			if ok && (value != "" || !colon) {
				continue
			}
			seen[name] = true
			msg := ""
			if m := regexp.MustCompile(`\$\{` + name + `:?\?([^}]*)\}`).FindSubmatch(data); m != nil {
				msg = strings.TrimSpace(string(m[1]))
			}
			missing = append(missing, [2]string{name, msg})
		}
	}
	return missing, nil
}

// MissingVarsIn is MissingVars for the project at cwd, its variables
// loaded from environ and its files and computed by its env capability,
// as --missing-vars has them: each name and where it is required.
func MissingVarsIn(cwd string, environ []string) ([][2]string, error) {
	p, err := loadProject(cwd, environ)
	if err != nil {
		return nil, err
	}
	if err := p.computeEnv(); err != nil {
		return nil, err
	}
	return MissingVars(p)
}

// missingVars is the hidden `devopsy --missing-vars`: nothing when every
// required variable is set, else the list and how to set them, failing.
func missingVars(p *loadedProject) error {
	missing, err := MissingVars(p)
	if err != nil {
		return &ExitError{Code: 1, Msg: "--missing-vars: " + err.Error()}
	}
	if len(missing) == 0 {
		return &Output{}
	}
	width := 0
	for _, m := range missing {
		width = max(width, len(m[0]))
	}
	var lines []string
	for _, m := range missing {
		lines = append(lines, fmt.Sprintf("  %-*s  %s", width, m[0], m[1]))
	}
	fix := report.Fix("set them in " + p.dotenvFile + " or the environment")
	if target, _ := p.env.Lookup("DEVOPSY_TARGET"); target != "" {
		fix = report.Fix("set them in the environment's shared/.env (--vars-instance or --vars-project for the instance's or the project's .env), then try again:", "devopsy @"+target+" --vars set <KEY>...")
	}
	return &ExitError{Code: 1, Err: &report.Failure{Msg: "compose requires variables that are not set:", Details: lines, Fixes: []report.Remedy{fix}}}
}
