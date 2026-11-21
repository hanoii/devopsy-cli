package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"

	"github.com/compose-spec/compose-go/v2/dotenv"
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
		// Masked whole before its lines: a secret of several lines (a key) matches no
		// single one, and compose interpolates a :? message, its other errors quoting values.
		stderr = p.newSecrets().Mask(stderr)
		// Compose says a variable's warning once per reading of the files.
		for _, line := range strings.Split(stderr, "\n") {
			if !slices.Contains(said, line) {
				said = append(said, line)
			}
		}
	}
	out, _, err := composeOut(p, "config", "--variables", "--format", "json")
	if err != nil {
		return nil, said, nil
	}
	var vars map[string]struct{ Required bool }
	if json.Unmarshal([]byte(out), &vars) != nil {
		return nil, said, nil
	}
	var unset []string
	for name, v := range vars {
		if value, _ := p.env.Lookup(name); v.Required && value == "" {
			unset = append(unset, name)
		}
	}
	sort.Strings(unset)
	// Compose's lines are the verdict: one it reads from elsewhere (an include's env_file)
	// is unset here and set there, and compose names only what it misses. Lines it words
	// otherwise name none: then every one unset here.
	for _, n := range unset {
		if slices.ContainsFunc(said, func(l string) bool { return strings.Contains(l, "required variable "+n+" ") }) {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		missing = unset
	}
	return missing, said, nil
}

// VarsSet is the --vars set of keys, each by its name: a fix that works
// when copied, asking each value.
func VarsSet(keys []string) string { return VarsEdit("--vars", "set", keys) }

// VarsEdit is flag's (--vars, --vars-instance, --vars-project) verb of keys, by their names.
func VarsEdit(flag, verb string, keys []string) string {
	return flag + " " + verb + " " + strings.Join(keys, " ")
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

// Missing is what compose requires that a project's variables leave unset or empty: the
// --vars follow-up's (CheckMissing) and --missing-vars'.
type Missing struct {
	Names []string
	// Said are compose's lines naming one of them, masked: compose stops at the first, and
	// interpolates a :? message, values included.
	Said []string
	// Before is how the variables were before an edit, nil when unknown.
	Before *MissingBefore
	// Unknown says, in words, why Before is nil when it was asked for, a capability's
	// stderr on the lines after.
	Unknown string
	// Empty are the ones a file other than the edited one sets empty (by name, the file's
	// in .devopsy/): its line wins, so no key of the edited file brings them back.
	Empty map[string]string
}

// MissingBefore is how the variables Missing names were before an edit.
type MissingBefore struct {
	// Computed are the ones the env capability computed then, Set the ones set then
	// (computed or from a file's interpolation).
	Computed, Set map[string]bool
	// Changed are the keys the edit changed in its file: removed, added or changed.
	Changed map[string]string
}

// ComposeUnreadable is compose failing on a project's files for another reason than a
// missing variable, its words masked.
type ComposeUnreadable struct{ Said []string }

func (u *ComposeUnreadable) Error() string {
	return "docker compose config: " + strings.Join(u.Said, "; ")
}

// missingOf is the Missing of p's environment as loaded, its capability run: nil when
// compose requires nothing unset, an error when its files fail for another reason, its
// words masked.
func missingOf(p *loadedProject) (*Missing, error) {
	names, said, err := MissingVars(p)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 && len(said) > 0 {
		return nil, &ComposeUnreadable{Said: said}
	}
	if len(names) == 0 {
		return nil, nil
	}
	m := &Missing{Names: names}
	for _, line := range said {
		// From its "required variable": where compose found it is no help to set it.
		if i := strings.Index(line, "required variable "); i >= 0 && slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(line[i:], "required variable "+n+" ") }) {
			m.Said = append(m.Said, line[i:])
		}
	}
	// One a file sets empty: that file's line wins over any value below it.
	for _, n := range names {
		if v, ok := p.env.Lookup(n); ok && v == "" && p.env.origin[n] != "" {
			if m.Empty == nil {
				m.Empty = map[string]string{}
			}
			m.Empty[n] = filepath.Base(p.env.origin[n])
		}
	}
	return m, nil
}

// CheckMissing is the --vars follow-up's check for the project at cwd, its variables
// loaded from environ and its files and computed by its env capability: what compose
// requires that they leave unset, nil for nothing. With before, what its file held before
// the edit, the project is loaded again with it standing in, only when something is
// missing, to tell what the edit changed and how each was set before.
func CheckMissing(cwd string, environ []string, before *Before) (*Missing, error) {
	p, err := loadProject(cwd, environ)
	if err != nil {
		return nil, err
	}
	if err := p.computeEnv(); err != nil {
		return nil, err
	}
	m, err := missingOf(p)
	if err != nil || m == nil || before == nil {
		return m, err
	}
	for n, f := range m.Empty {
		if f == before.File {
			delete(m.Empty, n)
		}
	}
	was, err := loadProjectBefore(cwd, environ, before)
	if err != nil {
		// A parser's words quote the line, value included, and nothing masks an old one.
		m.Unknown = "devopsy could not load the variables as they were before this change"
		if _, ok := err.(*beforeError); ok {
			m.Unknown = "the file before this change does not parse"
		}
		return m, nil
	}
	if err := was.computeEnv(); err != nil {
		// The capability's stderr may print an old value the current secrets lack.
		m.Unknown = "devopsy could not tell whether the env capability computed what is missing: " + was.newSecrets().Mask(p.newSecrets().Mask(err.Error()))
		return m, nil
	}
	b := &MissingBefore{Computed: map[string]bool{}, Set: map[string]bool{}, Changed: changedKeys(before.Content, filepath.Join(p.dir, before.File))}
	capability := capabilityFile(was.dir)
	for _, n := range m.Names {
		if v, _ := was.env.Lookup(n); v != "" {
			b.Set[n] = true
			b.Computed[n] = was.env.origin[n] == capability
		}
	}
	m.Before = b
	return m, nil
}

// changedKeys are the keys whose values differ between old and file as it is now, as
// written, never as interpolated (X=${B} is not changed by B's removal): removed, added or
// changed.
func changedKeys(old []byte, file string) map[string]string {
	now, _ := os.ReadFile(file)
	parse := func(b []byte) map[string]string {
		// No $ left, nothing is interpolated; both sides alike, so equal stays equal.
		b = bytes.ReplaceAll(b, []byte("$"), []byte("\x01"))
		vars, err := dotenv.UnmarshalBytesWithLookup(b, func(string) (string, bool) { return "", false })
		if err != nil {
			return map[string]string{}
		}
		return vars
	}
	was, is := parse(old), parse(now)
	changed := map[string]string{}
	for k, v := range was {
		if w, ok := is[k]; !ok {
			changed[k] = "removed"
		} else if w != v {
			changed[k] = "changed"
		}
	}
	for k := range is {
		if _, ok := was[k]; !ok {
			changed[k] = "added"
		}
	}
	return changed
}

// missingVars is the hidden `devopsy --missing-vars`: nothing when every
// required variable is set, else what is missing and the fix, failing.
func missingVars(p *loadedProject) error {
	m, err := missingOf(p)
	var u *ComposeUnreadable
	if errors.As(err, &u) {
		return &ExitError{Code: 1, Err: &report.Failure{Msg: "compose cannot read the project's files:", Details: u.Said}}
	}
	if err != nil {
		return &ExitError{Code: 1, Msg: "--missing-vars: " + err.Error()}
	}
	if m == nil {
		return &Output{}
	}
	// As --vars' follow-up words it, with nothing known of before.
	e := m.Explain(nil)
	failure := &report.Failure{Msg: e.Details[0], Details: e.Details[1:]}
	steps := e.Steps()
	if len(steps) == 0 {
		return &ExitError{Code: 1, Err: failure}
	}
	words := make([]string, len(steps))
	for i, s := range steps {
		words[i] = s.Words
	}
	fix := report.Fix(strings.Join(words, ", then ") + " in " + p.dotenvFile + " or the environment")
	// On a server, the address the operator typed (the session's), else the release's.
	lookup := func(k string) string { v, _ := p.env.Lookup(k); return v }
	in := remote.SessionFrom(lookup).In(remote.ReleaseNames(p.dir))
	var cmds []string
	for _, s := range steps {
		if cmd := in.On(VarsEdit("--vars", s.Verb, s.Keys)); cmd != "" {
			cmds = append(cmds, cmd)
		}
	}
	if len(cmds) == len(steps) {
		fix = report.Fix(strings.Join(words, ", then ")+" in the environment's shared/.env (--vars-instance or --vars-project for the instance's or the project's .env), then try again:", cmds...)
	}
	failure.Remedies = []report.Remedy{fix}
	return &ExitError{Code: 1, Err: failure}
}

// SaidLines are compose's words on m, as a detail says them.
func (m *Missing) SaidLines() []string {
	var out []string
	for _, l := range m.Said {
		out = append(out, "compose says: "+l)
	}
	return out
}

// Lines are m as --check-images --hint prints it for --vars' follow-up (ParseMissing):
// "missing <name>" and compose's "said <line>"; with the variables before the edit known,
// "before", then "set <name>" for one set then, "computed <name>" for one the env
// capability computed then, and "changed <key> <removed|added|changed>"; else "unknown
// <why>" when asked for, one per line of it, so none of a capability's is read as another;
// and "empty <name> <file>" for one another file sets empty.
func (m *Missing) Lines() []string {
	var out []string
	for _, n := range m.Names {
		out = append(out, "missing "+n)
	}
	for _, l := range m.Said {
		out = append(out, "said "+l)
	}
	if m.Unknown != "" {
		for _, l := range strings.Split(m.Unknown, "\n") {
			out = append(out, "unknown "+l)
		}
	}
	for _, n := range m.Names {
		if f := m.Empty[n]; f != "" {
			out = append(out, "empty "+n+" "+f)
		}
	}
	if m.Before == nil {
		return out
	}
	out = append(out, "before")
	for _, n := range m.Names {
		if m.Before.Set[n] {
			out = append(out, "set "+n)
		}
		if m.Before.Computed[n] {
			out = append(out, "computed "+n)
		}
	}
	keys := make([]string, 0, len(m.Before.Changed))
	for k := range m.Before.Changed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, "changed "+k+" "+m.Before.Changed[k])
	}
	return out
}

// ParseMissing reads Lines back, nil with no "missing" line; Before is known after a
// "before" line.
func ParseMissing(lines []string) *Missing {
	m := &Missing{}
	b := &MissingBefore{Computed: map[string]bool{}, Set: map[string]bool{}, Changed: map[string]string{}}
	known := false
	for _, l := range lines {
		word, rest, _ := strings.Cut(l, " ")
		switch word {
		case "missing":
			m.Names = append(m.Names, rest)
		case "said":
			m.Said = append(m.Said, rest)
		case "unknown":
			m.Unknown = strings.TrimPrefix(m.Unknown+"\n"+rest, "\n")
		case "before":
			known = true
		case "empty":
			n, f, _ := strings.Cut(rest, " ")
			if m.Empty == nil {
				m.Empty = map[string]string{}
			}
			m.Empty[n] = f
		case "set":
			b.Set[rest] = true
		case "computed":
			b.Computed[rest] = true
		case "changed":
			k, kind, _ := strings.Cut(rest, " ")
			b.Changed[k] = kind
		}
	}
	if len(m.Names) == 0 {
		return nil
	}
	if known {
		m.Before = b
	}
	return m
}

// Explained is what --vars' follow-up says of a Missing: why each is missing, compose's
// words, and the keys its one fix sets again, unsets again or sets, every one needed.
type Explained struct {
	Details                       []string
	SetAgain, UnsetAgain, SetThem []string
	// AsBefore: none of them was set before the edit either, so the edit is not why.
	AsBefore bool
}

// Explain words m. unset are the keys the operator unset, set again when the variables
// before the edit are unknown. No fix sets what the env capability or a file's
// interpolation set before: that bypasses them; the keys the edit changed bring it back.
func (m *Missing) Explain(unset []string) Explained {
	var e Explained
	var computed, derived, own, never []string
	emptied := map[string][]string{}
	for _, n := range m.Names {
		switch {
		case m.Empty[n] != "":
			emptied[m.Empty[n]] = append(emptied[m.Empty[n]], n)
		case m.Before == nil:
			if slices.Contains(unset, n) {
				own = append(own, n)
			} else {
				never = append(never, n)
			}
		case m.Before.Set[n] && m.Before.Changed[n] != "" && m.Before.Changed[n] != "added":
			own = append(own, n)
		case m.Before.Set[n] && m.Before.Computed[n]:
			computed = append(computed, n)
		case m.Before.Set[n]:
			derived = append(derived, n)
		default:
			never = append(never, n)
		}
	}
	join := func(ns []string) string { return report.JoinAnd(ns) }
	if len(computed) > 0 {
		// What the check saw: computed with the file as it was, not with it as it is.
		without := "this change"
		if len(m.Before.Changed) == 1 {
			for k := range m.Before.Changed {
				without = k
			}
		}
		e.Details = append(e.Details, "compose needs "+join(computed)+", which the project's env capability computes, and without "+without+" it computes nothing")
	}
	if len(derived) > 0 {
		e.Details = append(e.Details, "compose needs "+join(derived)+", which "+were(derived)+" set before this change and "+are(derived)+" empty now")
	}
	if len(own) > 0 {
		e.Details = append(e.Details, "compose needs "+join(own))
	}
	if len(never) > 0 {
		why := "which " + are(never) + " not set"
		if m.Before != nil {
			why = "which " + were(never) + " not set before this change either"
		}
		e.Details = append(e.Details, "compose needs "+join(never)+", "+why)
	}
	files := make([]string, 0, len(emptied))
	for f := range emptied {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		e.Details = append(e.Details, "compose needs "+join(emptied[f])+", which "+fileSays(f)+" sets empty: its line wins over a value anywhere else, so set "+join(emptied[f])+" there, or remove that line")
	}
	if m.Unknown != "" {
		e.Details = append(e.Details, strings.Split(m.Unknown, "\n")...)
	}
	e.Details = append(e.Details, m.SaidLines()...)
	e.SetAgain = append(e.SetAgain, own...)
	if len(computed)+len(derived) > 0 && len(m.Before.Changed) == 0 {
		// Nothing to set back: the edit left its file as it was (a key set to the value it had).
		e.Details = append(e.Details, "this change left its file as it was, so no key of it brings "+join(append(slices.Clone(computed), derived...))+" back")
	}
	if len(computed)+len(derived) > 0 {
		keys := make([]string, 0, len(m.Before.Changed))
		for k := range m.Before.Changed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if m.Before.Changed[k] == "added" {
				e.UnsetAgain = append(e.UnsetAgain, k)
			} else if !slices.Contains(e.SetAgain, k) {
				e.SetAgain = append(e.SetAgain, k)
			}
		}
	}
	e.SetThem = never
	// A nearer file's empty line was there before too: the edit is not why.
	e.AsBefore = m.Before != nil && len(never)+len(m.Empty) == len(m.Names)
	return e
}

// FixStep is one command of a missing variable's fix: its words and the --vars verb
// and keys it runs.
type FixStep struct {
	Words, Verb string
	Keys        []string
}

// Steps are e's fix, every command needed, the keys by name: those to set again, to
// unset again, then to set; none when no key brings it back, which the details say. The
// --vars follow-up and --missing-vars each add only their flag, address and words.
func (e Explained) Steps() []FixStep {
	var steps []FixStep
	add := func(words, verb string, keys []string) {
		if len(keys) > 0 {
			steps = append(steps, FixStep{Words: words, Verb: verb, Keys: keys})
		}
	}
	add("set "+report.JoinAnd(e.SetAgain)+" again", "set", e.SetAgain)
	add("unset "+report.JoinAnd(e.UnsetAgain)+" again", "unset", e.UnsetAgain)
	add("set "+report.JoinAnd(e.SetThem), "set", e.SetThem)
	return steps
}

// fileSays names a variables file of a release's .devopsy/ in words.
func fileSays(f string) string {
	switch f {
	case ".env":
		return "the environment's .env"
	case "instance.env":
		return "the instance's .env"
	case "project.env":
		return "the project's .env"
	case remote.InheritFile:
		return "what the environment inherits from its parent"
	case remote.ParentEnvFile:
		return "its copy of the parent's server's .env"
	}
	return f
}

func were(ns []string) string {
	if len(ns) == 1 {
		return "was"
	}
	return "were"
}

func are(ns []string) string {
	if len(ns) == 1 {
		return "is"
	}
	return "are"
}
