// Package cli decides what a devopsy invocation runs: a project's custom
// command or `docker compose`. It only builds a Plan; the caller executes it,
// which keeps this package testable.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"go.yaml.in/yaml/v3"
)

// ProjectDirName is the directory devopsy looks for.
const ProjectDirName = ".devopsy"

// ExitError carries an exit code and a message for the user.
type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string { return e.Msg }

// Plan is the process to replace devopsy with.
type Plan struct {
	// Path is the executable: a custom command, or "docker" looked up in PATH.
	Path string
	// Args includes argv[0].
	Args []string
	Env  []string
	// Notice is printed to stderr before executing, if not empty.
	Notice string
}

// Help is returned when devopsy only has to print usage.
type Help struct {
	Text string
	Code int
}

func (h *Help) Error() string { return h.Text }

// FindProjectDir walks up from dir until it finds a .devopsy directory.
func FindProjectDir(dir string) (string, error) {
	for {
		candidate := filepath.Join(dir, ProjectDirName)
		if fi, err := os.Stat(candidate); err == nil && fi.IsDir() {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", &ExitError{Code: 100, Msg: ProjectDirName + "/ not found in this directory or any parent."}
		}
		dir = parent
	}
}

// Env is an ordered set of environment variables.
type Env struct {
	keys   []string
	values map[string]string
}

// NewEnv builds an Env from os.Environ-style entries.
func NewEnv(environ []string) *Env {
	e := &Env{values: map[string]string{}}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		e.Set(k, v)
	}
	return e
}

// Lookup returns a variable and whether it is set.
func (e *Env) Lookup(k string) (string, bool) {
	v, ok := e.values[k]
	return v, ok
}

// Set sets a variable, keeping first-set order.
func (e *Env) Set(k, v string) {
	if _, ok := e.values[k]; !ok {
		e.keys = append(e.keys, k)
	}
	e.values[k] = v
}

// Environ returns KEY=value entries.
func (e *Env) Environ() []string {
	out := make([]string, 0, len(e.keys))
	for _, k := range e.keys {
		out = append(out, k+"="+e.values[k])
	}
	return out
}

// LoadDotenv adds the variables of file that are not already set, so the
// caller's environment wins, as in compose. Values are parsed and
// interpolated by compose's own parser.
func LoadDotenv(env *Env, file string) error {
	if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	vars, err := dotenv.ReadFile(file, env.Lookup)
	if err != nil {
		return &ExitError{Code: 1, Msg: fmt.Sprintf("%s: %v", file, err)}
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := env.Lookup(k); !ok {
			env.Set(k, vars[k])
		}
	}
	return nil
}

var projectNameChars = regexp.MustCompile("[a-z0-9_-]")

// NormalizeProjectName is compose's own normalization (compose-go loader).
func NormalizeProjectName(s string) string {
	s = strings.ToLower(s)
	s = strings.Join(projectNameChars.FindAllString(s, -1), "")
	return strings.TrimLeft(s, "_-")
}

// HasTopLevelName reports whether a compose file sets `name:`.
func HasTopLevelName(composeFile string) (bool, error) {
	data, err := os.ReadFile(composeFile)
	if err != nil {
		return false, err
	}
	var doc struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return false, &ExitError{Code: 1, Msg: fmt.Sprintf("%s: %v", composeFile, err)}
	}
	return doc.Name != "", nil
}

// CustomCommands lists the executable files in commands/.
func CustomCommands(projectDir string) []string {
	entries, err := os.ReadDir(filepath.Join(projectDir, "commands"))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if isExecutableFile(filepath.Join(projectDir, "commands", e.Name())) {
			names = append(names, e.Name())
		}
	}
	return names
}

func isExecutableFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
}

func usage(projectDir string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Usage: devopsy <command> [args...]\n\n")
	fmt.Fprintf(&b, "Runs a custom command from %s/commands/ if one exists,\n", projectDir)
	fmt.Fprintf(&b, "otherwise passes everything to docker compose.\n")
	if cmds := CustomCommands(projectDir); len(cmds) > 0 {
		fmt.Fprintf(&b, "\nCustom commands:\n")
		for _, c := range cmds {
			fmt.Fprintf(&b, "  %s\n", c)
		}
	}
	return b.String()
}

// Build decides what to run for args (without argv[0]), from cwd and the
// caller's environment.
func Build(cwd string, args []string, environ []string) (*Plan, error) {
	projectDir, err := FindProjectDir(cwd)
	if err != nil {
		return nil, err
	}
	composeFile := filepath.Join(projectDir, "compose.yaml")
	if _, err := os.Stat(composeFile); err != nil {
		return nil, &ExitError{Code: 1, Msg: composeFile + " not found."}
	}

	env := NewEnv(environ)
	env.Set("DEVOPSY_PROJECT_DIR", projectDir)
	if err := LoadDotenv(env, filepath.Join(projectDir, ".env")); err != nil {
		return nil, err
	}

	// Without a top-level name, compose would name every project after the
	// .devopsy directory and they would all collide. Use the directory that
	// contains it, as compose does for a compose.yaml at a project's root.
	if v, _ := env.Lookup("COMPOSE_PROJECT_NAME"); v == "" {
		named, err := HasTopLevelName(composeFile)
		if err != nil {
			return nil, err
		}
		if !named {
			env.Set("COMPOSE_PROJECT_NAME", NormalizeProjectName(filepath.Base(filepath.Dir(projectDir))))
		}
	}

	if len(args) == 0 {
		return nil, &Help{Text: usage(projectDir), Code: 1}
	}
	switch args[0] {
	case "help", "-h", "--help":
		return nil, &Help{Text: usage(projectDir), Code: 0}
	}

	// A custom command can call `devopsy <same name>` to reach the compose
	// command it wraps, without recursing into itself.
	current, _ := env.Lookup("DEVOPSY_CLI_COMMAND")
	custom := filepath.Join(projectDir, "commands", args[0])
	if current != args[0] && isExecutableFile(custom) {
		env.Set("DEVOPSY_CLI_COMMAND", args[0])
		return &Plan{
			Path: custom,
			Args: append([]string{custom}, args[1:]...),
			Env:  env.Environ(),
		}, nil
	}

	composeArgs := []string{"docker", "compose", "-f", composeFile}
	for _, name := range []string{"compose.override.yaml", "compose.override.yml"} {
		override := filepath.Join(projectDir, name)
		if fi, err := os.Stat(override); err == nil && fi.Mode().IsRegular() {
			composeArgs = append(composeArgs, "-f", override)
			break
		}
	}
	composeArgs = append(composeArgs, args...)
	return &Plan{
		Path:   "docker",
		Args:   composeArgs,
		Env:    env.Environ(),
		Notice: fmt.Sprintf("Running '%s'...", strings.Join(composeArgs, " ")),
	}, nil
}

// Fprint writes msg to w, in color when color is set.
func Fprint(w io.Writer, color string, msg string, useColor bool) {
	if useColor && color != "" {
		fmt.Fprintf(w, "%s%s\033[0m\n", color, msg)
		return
	}
	fmt.Fprintln(w, msg)
}
