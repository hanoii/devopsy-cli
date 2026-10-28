package cli

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// EnvTraceFlag is the hidden `devopsy --env-trace [KEY...]`, run in a
// release (or a plain directory) on a server: TraceEnv as JSON
// (EnvTrace), for what --vars says about a variable's value and where it
// comes from. --debug env prints the same trace for people.
const EnvTraceFlag = "--env-trace"

// The levels of an EnvDef (Level), nearest first as the loader reads
// them.
const (
	LevelCaller   = "caller"
	LevelDevopsy  = "devopsy"
	LevelEnv      = "env"
	LevelInherit  = "inherit"
	LevelInstance = "instance"
	LevelProject  = "project"
	LevelParent   = "parent"
	LevelTarget   = "target"
	LevelImported = "imported"
	LevelComputed = "computed"
)

// Level is where d comes from, by what the loader read (loadProject):
// the environment's own .env (shared/.env on a server, a plain
// directory's .devopsy/.env, a restore's pending one), its inherited
// variables, the instance's or the project's .env, its copy of its
// parent's server's (remote.ParentEnvFile), the release's
// devopsy-target.env and what it imported, the env capability, devopsy
// itself or the caller's environment.
func (d EnvDef) Level() string {
	switch d.Kind {
	case EnvCaller:
		return LevelCaller
	case EnvDevopsy:
		return LevelDevopsy
	case EnvImported:
		return LevelImported
	case EnvComputed:
		return LevelComputed
	}
	switch base := filepath.Base(d.Source); {
	// A sync's pending copy is named after its pending inherit file.
	case base == remote.ParentEnvFile || strings.HasSuffix(base, "."+remote.ParentEnvFile):
		return LevelParent
	case base == remote.InheritFile || strings.HasPrefix(base, ".devopsy-sync.inherit."):
		return LevelInherit
	case base == "instance.env":
		return LevelInstance
	case base == "project.env":
		return LevelProject
	case base == remote.TargetEnvFile || base == remote.TargetReloadFile:
		return LevelTarget
	}
	return LevelEnv
}

// EnvTraced is EnvTrace's output: the environment's parent (its
// DEVOPSY_PARENT), and each variable's definitions by precedence, the
// first its value.
type EnvTraced struct {
	Parent string         `json:"parent,omitempty"`
	Vars   []EnvTracedVar `json:"vars"`
}

// EnvTracedVar is a variable and its definitions; none: not set.
type EnvTracedVar struct {
	Name string         `json:"name"`
	Defs []EnvTracedDef `json:"defs"`
}

// EnvTracedDef is one definition: its Level and value. The value is
// bytes, which JSON carries in base64: as a string, JSON would replace
// what is not UTF-8 (a value in Latin-1) and --vars get print another.
type EnvTracedDef struct {
	Level string `json:"level"`
	Value []byte `json:"value"`
}

// EnvTrace is TraceEnv for the names given (every variable --env prints
// without), as one line of JSON: values unmasked, since --vars get
// prints them and the rest only names levels.
func EnvTrace(cwd string, environ, names []string) ([]byte, error) {
	// The parent's name too, which says where inherited values come
	// from: asked for with the rest, so the env capability runs once.
	asked := names
	if len(names) > 0 && !slices.Contains(names, "DEVOPSY_PARENT") {
		asked = append(slices.Clone(names), "DEVOPSY_PARENT")
	}
	_, vars, _, err := TraceEnv(cwd, environ, asked)
	if err != nil {
		return nil, err
	}
	out := EnvTraced{Vars: []EnvTracedVar{}}
	for i, v := range vars {
		if v.Name == "DEVOPSY_PARENT" && len(v.Defs) > 0 {
			out.Parent = v.Defs[0].Value
		}
		if len(names) > 0 && i >= len(names) {
			break
		}
		tv := EnvTracedVar{Name: v.Name, Defs: []EnvTracedDef{}}
		for _, d := range v.Defs {
			tv.Defs = append(tv.Defs, EnvTracedDef{Level: d.Level(), Value: []byte(d.Value)})
		}
		out.Vars = append(out.Vars, tv)
	}
	return json.Marshal(out)
}

// ParseEnvTrace reads EnvTrace's output.
func ParseEnvTrace(b []byte) (EnvTraced, error) {
	var t EnvTraced
	err := json.Unmarshal(b, &t)
	return t, err
}

// Lookup returns name's definitions, nil when it is not set.
func (t EnvTraced) Lookup(name string) []EnvTracedDef {
	for _, v := range t.Vars {
		if v.Name == name {
			return v.Defs
		}
	}
	return nil
}
