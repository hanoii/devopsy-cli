package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/template"
	"go.yaml.in/yaml/v3"
)

// MissingVars lists the variables p's compose files require (${VAR:?msg},
// ${VAR?msg}) that its environment leaves unset (or empty, for :?), with
// compose's message for each. Compose refuses every command while one is
// missing, so releases check them before going live.
func MissingVars(p *loadedProject) ([][2]string, error) {
	files := []string{p.composeFile}
	for _, name := range []string{"compose.override.yaml", "compose.override.yml"} {
		if fi, err := os.Stat(filepath.Join(p.dir, name)); err == nil && fi.Mode().IsRegular() {
			files = append(files, filepath.Join(p.dir, name))
			break
		}
	}
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
	var b strings.Builder
	b.WriteString("devopsy: compose requires variables that are not set here:\n")
	width := 0
	for _, m := range missing {
		width = max(width, len(m[0]))
	}
	for _, m := range missing {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, m[0], m[1])
	}
	if target, _ := p.env.Lookup("DEVOPSY_TARGET"); target != "" {
		fmt.Fprintf(&b, "Set them, then try again: devopsy @%s --vars set <KEY>...\n(--instance or --project for the instance's or the project's .env).", target)
	} else {
		b.WriteString("Set them in " + p.dotenvFile + " or the environment.")
	}
	return &ExitError{Code: 1, Msg: b.String()}
}
