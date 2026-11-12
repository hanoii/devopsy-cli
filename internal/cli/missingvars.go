package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/template"
	"github.com/compose-spec/compose-go/v2/types"
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

// composeModel is the project's compose files as compose reads them, without interpolating
// them, through compose-go's loader: what they include and extend is followed, an
// include's own .env and env_file too. Schema and consistency are left to compose, which
// may be newer than devopsy's compose-go.
func composeModel(projectDir string, opts ...func(*loader.Options)) (map[string]any, error) {
	details := types.ConfigDetails{WorkingDir: projectDir, ConfigFiles: types.ToConfigFiles(composeFiles(projectDir)), Environment: types.Mapping{}}
	return loader.LoadModelWithContext(context.Background(), details, append([]func(*loader.Options){func(o *loader.Options) {
		o.SkipInterpolation = true
		o.SkipValidation = true
		o.SkipConsistencyCheck = true
		o.SkipResolveEnvironment = true
		o.SkipResolveLabels = true
		o.SetProjectName("devopsy", false)
	}}, opts...)...)
}

// ComposeVariables lists the variables the project's compose files interpolate, those of
// the files they include and extend too, read on the operator's machine with no Docker.
// What the loader cannot follow (an include from a registry or a git repository, or whose
// path is a variable, which compose's own config --variables fails on too) is said, and
// the files are read again without their includes and extends.
func ComposeVariables(projectDir string) ([]string, error) {
	model, err := composeModel(projectDir)
	if err != nil {
		var again error
		if model, again = composeModel(projectDir, func(o *loader.Options) { o.SkipInclude, o.SkipExtends = true, true }); again != nil {
			return nil, again
		}
		report.Warn("devopsy cannot follow what the compose files include or extend, and checks their own variables only", err.Error())
	}
	var names []string
	for name := range template.ExtractVariables(model, template.DefaultPattern) {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// MissingVars says whether p's compose files read, as the installed compose reads them
// (config -q), and when they do not, the variables they require (config --variables) that
// p's environment leaves unset or empty, and what compose said. Compose refuses every
// command while one is missing, so releases check them before going live. It runs where
// the release runs, a server's compose deciding, never devopsy's own reading of the files.
func MissingVars(p *loadedProject) (missing []string, said []string, err error) {
	if _, stderr, err := composeOut(p, "config", "-q"); err == nil {
		return nil, nil, nil
	} else if stderr == "" {
		return nil, nil, fmt.Errorf("docker compose config: %v", err)
	} else {
		said = strings.Split(stderr, "\n")
	}
	out, _, err := composeOut(p, "config", "--variables", "--format", "json")
	if err != nil {
		return nil, said, nil
	}
	var vars map[string]struct{ Required bool }
	if json.Unmarshal([]byte(out), &vars) != nil {
		return nil, said, nil
	}
	for name, v := range vars {
		if value, _ := p.env.Lookup(name); v.Required && value == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing, said, nil
}

// composeOut runs compose as p's commands do, its files and environment, the docker of
// that environment's PATH, and returns its stdout and trimmed stderr.
func composeOut(p *loadedProject, args ...string) (string, string, error) {
	plan := p.compose(args)
	cmd := exec.Command(lookPathIn("docker", plan.Env), plan.Args[1:]...)
	cmd.Env = plan.Env
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), strings.TrimSpace(stderr.String()), err
}

// lookPathIn is name in env's PATH, else name.
func lookPathIn(name string, env []string) string {
	for _, kv := range env {
		if dirs, ok := strings.CutPrefix(kv, "PATH="); ok {
			for _, dir := range filepath.SplitList(dirs) {
				if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
					return filepath.Join(dir, name)
				}
			}
		}
	}
	return name
}

// MissingVarsIn is MissingVars for the project at cwd, its variables
// loaded from environ and its files and computed by its env capability,
// as --missing-vars has them: each name missing, and compose's words
// when its files read for another reason.
func MissingVarsIn(cwd string, environ []string) ([]string, error) {
	p, err := loadProject(cwd, environ)
	if err != nil {
		return nil, err
	}
	if err := p.computeEnv(); err != nil {
		return nil, err
	}
	missing, said, err := MissingVars(p)
	if err == nil && len(missing) == 0 && len(said) > 0 {
		err = errors.New("docker compose config: " + strings.Join(said, "; "))
	}
	return missing, err
}

// missingVars is the hidden `devopsy --missing-vars`: nothing when every
// required variable is set, else the list and how to set them, failing.
func missingVars(p *loadedProject) error {
	missing, said, err := MissingVars(p)
	if err != nil {
		return &ExitError{Code: 1, Msg: "--missing-vars: " + err.Error()}
	}
	if len(missing) == 0 && len(said) == 0 {
		return &Output{}
	}
	if len(missing) == 0 {
		return &ExitError{Code: 1, Err: &report.Failure{Msg: "compose cannot read the project's files:", Details: said}}
	}
	var lines []string
	for _, m := range missing {
		lines = append(lines, "  "+m)
	}
	lines = append(lines, said...)
	fix := report.Fix("set them in " + p.dotenvFile + " or the environment")
	// On a server, the address the operator typed (the session's), else the release's.
	lookup := func(k string) string { v, _ := p.env.Lookup(k); return v }
	if cmd := remote.SessionFrom(lookup).In(remote.ReleaseNames(p.dir)).On("--vars set <KEY>..."); cmd != "" {
		fix = report.Fix("set them in the environment's shared/.env (--vars-instance or --vars-project for the instance's or the project's .env), then try again:", cmd)
	}
	return &ExitError{Code: 1, Err: &report.Failure{Msg: "compose requires variables that are not set:", Details: lines, Remedies: []report.Remedy{fix}}}
}
