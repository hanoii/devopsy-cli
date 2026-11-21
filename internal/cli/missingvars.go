package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/uids"

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
	// Empty are those of Names, and of the variables they need, that a file sets empty, by
	// the file's envFiles name: what an import (Imported) or the env capability sets empty
	// is no file's line.
	Empty map[string]string
	// Needs are the variables the env capability said it needs for one missing, and for
	// each of those it did not compute either (readNeeds); Has are those of them with a
	// value; Imported are those of them and of Names an import sets empty. Computed are those
	// of them and of Names the env capability printed a value for that a file's empty line
	// hides. Malformed are the line
	// numbers of its needs lines devopsy could not read.
	Needs                   map[string][]string
	Has, Imported, Computed map[string]bool
	Malformed               []int
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
	m := &Missing{Names: names, Malformed: p.needsMalformed}
	for _, line := range said {
		// From its "required variable": where compose found it (one service or another)
		// is no help to set it, so two services' same line is one.
		if i := strings.Index(line, "required variable "); i >= 0 && slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(line[i:], "required variable "+n+" ") }) {
			m.Said = appendNew(m.Said, line[i:])
		}
	}
	m.needsOf(p)
	for _, n := range names {
		if v, _ := p.env.Lookup(n); v == "" && len(p.env.defs[n]) > 0 && p.env.defs[n][0].Kind == EnvImported {
			if m.Imported == nil {
				m.Imported = map[string]bool{}
			}
			m.Imported[n] = true
		}
	}
	keys := slices.Clone(names)
	for _, in := range m.Needs {
		keys = append(keys, in...)
	}
	capability := capabilityFile(p.dir)
	for _, k := range keys {
		v, ok := p.env.Lookup(k)
		origin := p.env.origin[k]
		if !ok || v != "" || origin == "" || origin == capability || m.Imported[k] {
			continue
		}
		file := p.fileNames[origin]
		if file == "" {
			file = filepath.Base(origin)
		}
		if m.Empty == nil {
			m.Empty = map[string]string{}
		}
		m.Empty[k] = file
	}
	for _, n := range keys {
		if m.Empty[n] != "" && slices.ContainsFunc(p.env.defs[n], func(d EnvDef) bool { return d.Kind == EnvComputed && d.Value != "" }) {
			if m.Computed == nil {
				m.Computed = map[string]bool{}
			}
			m.Computed[n] = true
		}
	}
	return m, nil
}

// needsOf records in m what the env capability said it needs for each missing variable,
// and for each variable needed that it did not compute either, transitively, and how
// each needed one is set.
func (m *Missing) needsOf(p *loadedProject) {
	todo := slices.Clone(m.Names)
	for len(todo) > 0 {
		x := todo[0]
		todo = todo[1:]
		in, ok := p.needs[x]
		if !ok || m.Needs[x] != nil {
			continue
		}
		if m.Needs == nil {
			m.Needs, m.Has, m.Imported = map[string][]string{}, map[string]bool{}, map[string]bool{}
		}
		m.Needs[x] = in
		for _, n := range in {
			if v, _ := p.env.Lookup(n); v != "" {
				m.Has[n] = true
			} else if defs := p.env.defs[n]; len(defs) > 0 && defs[0].Kind == EnvImported {
				m.Imported[n] = true
			}
			todo = append(todo, n)
		}
	}
}

// CheckMissing is the --vars follow-up's check for the project at cwd, its variables
// loaded from environ and its files and computed by its env capability: what compose
// requires that they leave unset, nil for nothing.
func CheckMissing(cwd string, environ []string) (*Missing, error) {
	p, err := loadProject(cwd, environ)
	if err != nil {
		return nil, err
	}
	if err := p.computeEnv(); err != nil {
		return nil, err
	}
	return missingOf(p)
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
	// As --vars' follow-up words it, at the environment's own file, no edit known.
	e := m.Explain(Edit{File: ".env"})
	failure := &report.Failure{Msg: e.Details[0], Details: e.Details[1:]}
	if len(e.Steps()) == 0 {
		return &ExitError{Code: 1, Err: failure}
	}
	words, args := e.Fix("--vars")
	// The environment's own file only, when a step unsets or a key's empty line is there:
	// the environment's variables, or the other two levels, would not win over it.
	or, others := " or the environment", " (--vars-instance or --vars-project for the instance's or the project's .env)"
	if slices.ContainsFunc(e.Steps(), func(s FixStep) bool {
		return s.Verb == "unset" || slices.ContainsFunc(s.Keys, func(k string) bool { return m.Empty[k] == ".env" })
	}) {
		or, others = "", ""
	}
	fix := report.Fix(words + " in " + p.dotenvFile + or)
	// On a server, the address the operator typed (the session's), else the release's.
	lookup := func(k string) string { v, _ := p.env.Lookup(k); return v }
	in := remote.SessionFrom(lookup).In(remote.ReleaseNames(p.dir))
	var cmds []string
	for _, a := range args {
		if cmd := in.On(a); cmd != "" {
			cmds = append(cmds, cmd)
		}
	}
	if len(cmds) == len(args) {
		fix = report.Fix(words+" in the environment's shared/.env"+others+", then try again:", cmds...)
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
// "missing <name>" and compose's "said <line>"; "empty <name> <file>" for one a file sets
// empty; what the env capability needs, "needs <name> <needed>...", "has <needed>" for one
// with a value, "imported <needed>" for one an import sets empty; "computed <name>" for
// one it computed that a file's empty line hides; and "malformed <line number>".
func (m *Missing) Lines() []string {
	var out []string
	for _, n := range m.Names {
		out = append(out, "missing "+n)
	}
	for _, l := range m.Said {
		out = append(out, "said "+l)
	}
	for _, n := range slices.Sorted(maps.Keys(m.Empty)) {
		out = append(out, "empty "+n+" "+m.Empty[n])
	}
	for _, x := range slices.Sorted(maps.Keys(m.Needs)) {
		out = append(out, "needs "+x+" "+strings.Join(m.Needs[x], " "))
	}
	for _, n := range slices.Sorted(maps.Keys(m.Has)) {
		out = append(out, "has "+n)
	}
	for _, n := range slices.Sorted(maps.Keys(m.Imported)) {
		out = append(out, "imported "+n)
	}
	for _, n := range slices.Sorted(maps.Keys(m.Computed)) {
		out = append(out, "computed "+n)
	}
	for _, n := range m.Malformed {
		out = append(out, "malformed "+strconv.Itoa(n))
	}
	return out
}

// ParseMissing reads Lines back, nil with no "missing" line. A line naming what is no
// variable's name is dropped.
func ParseMissing(lines []string) *Missing {
	m := &Missing{}
	name := remote.VarNameRe.MatchString
	flag := func(set *map[string]bool, n string) {
		if name(n) {
			if *set == nil {
				*set = map[string]bool{}
			}
			(*set)[n] = true
		}
	}
	for _, l := range lines {
		word, rest, _ := strings.Cut(l, " ")
		switch word {
		case "missing":
			if name(rest) {
				m.Names = append(m.Names, rest)
			}
		case "said":
			m.Said = append(m.Said, rest)
		case "empty":
			n, f, _ := strings.Cut(rest, " ")
			if name(n) {
				if m.Empty == nil {
					m.Empty = map[string]string{}
				}
				m.Empty[n] = f
			}
		case "needs":
			names := strings.Split(rest, " ")
			if len(names) > 1 && !slices.ContainsFunc(names, func(n string) bool { return !name(n) }) {
				if m.Needs == nil {
					m.Needs = map[string][]string{}
				}
				m.Needs[names[0]] = names[1:]
			}
		case "has":
			flag(&m.Has, rest)
		case "imported":
			flag(&m.Imported, rest)
		case "computed":
			flag(&m.Computed, rest)
		case "malformed":
			if n, err := strconv.Atoi(rest); err == nil {
				m.Malformed = append(m.Malformed, n)
			}
		}
	}
	if len(m.Names) == 0 {
		return nil
	}
	return m
}

// Edit is what a change of variables did, as Explain words what it leaves missing: the
// file it edited, by its envFiles name (.env for an environment's change, and for
// --missing-vars, which edits none), and when Known, from the edit's own script, the keys
// it removed or emptied (Gone) and whether it changed nothing (Unchanged). --inherit's and
// --missing-vars' are not known.
type Edit struct {
	File      string
	Known     bool
	Gone      map[string]bool
	Unchanged bool
}

// Explained is what --vars' follow-up and --missing-vars say of a Missing: why each is
// missing, compose's words, and the keys of its one fix: to unset (again when the edit
// emptied them), then to set (again when the edit removed or emptied them). Caused: the
// edit removed or emptied what is missing or what it needs; AsBefore: the edit changed
// nothing, so what is missing was missing before it.
type Explained struct {
	Details                          []string
	UnsetAgain, Unset, SetAgain, Set []string
	Caused, AsBefore                 bool
}

// Explain words m after edit. A variable the env capability needs others for is
// missing for want of them, down to one with a value, and the fix sets those, never the
// variable itself, which bypasses the capability; with all of them set, the capability
// computed nothing, and nothing is offered. A line that sets a key empty is said with its
// file: a fix at the edited file goes on that file or on one below it, which it wins
// over; a nearer one still wins, so its line is to remove. What the capability computes
// or says it needs, a file setting it empty, is to unset at the edited file, else to
// remove: setting it by hand bypasses the capability. devopsy only says what it saw: what
// the edit did, what the capability printed.
func (m *Missing) Explain(edit Edit) Explained {
	e := Explained{AsBefore: edit.Known && edit.Unchanged}
	nearer := func(f string) bool { return envFileRank(f) < envFileRank(edit.File) }
	gone := func(k string) bool { return edit.Known && edit.Gone[k] }
	var notes []string
	set := func(k string) {
		if err := uids.Reserved(k); err != nil {
			notes = appendNew(notes, err.Error())
			return
		}
		if gone(k) {
			e.SetAgain = appendNew(e.SetAgain, k)
		} else {
			e.Set = appendNew(e.Set, k)
		}
	}
	unset := func(k string) {
		if gone(k) {
			e.UnsetAgain = appendNew(e.UnsetAgain, k)
		} else {
			e.Unset = appendNew(e.Unset, k)
		}
	}
	remove := ": its line wins over a value below it, so remove that line there"
	var own, never []string
	// key words and fixes k, a missing variable (top) or an input of one, by one rule:
	// its words a detail of its own when top, else a note.
	var key func(k string, top bool)
	key = func(k string, top bool) {
		all, missing, through := m.inputsOf(k)
		f := m.Empty[k]
		computes := len(m.Needs[k]) > 0 || m.Computed[k]
		// What the edit took of k, or of what k needs, is why, but under a nearer file's
		// line, which was winning already.
		if f == "" || !nearer(f) {
			e.Caused = e.Caused || gone(k) || slices.ContainsFunc(through, gone)
		}
		say := func(what string) {
			if top {
				e.Details = append(e.Details, "compose needs "+k+", "+what)
			} else {
				notes = appendNew(notes, k+", "+what)
			}
		}
		hiding := "hiding what the project's env capability computes"
		if len(all) > 0 {
			hiding = "over what the project's env capability computes from " + report.JoinAnd(all)
		}
		switch {
		case m.Imported[k]:
			say("which the project imports, and is empty where it comes from")
		case f != "" && computes && f == edit.File:
			say("which " + fileSays(f) + " sets empty, " + hiding)
			unset(k)
			for _, n := range missing {
				key(n, false)
			}
		case f != "" && (computes || nearer(f)):
			say("which " + fileSays(f) + " sets empty" + remove)
		case f != "":
			say("which " + fileSays(f) + " sets empty")
			set(k)
		case len(all) > 0 && len(missing) == 0:
			say("which the project's env capability computes from " + report.JoinAnd(all) + ", but " + report.JoinAnd(all) + " " + are(all) + " set and it computed nothing")
		case len(all) > 0:
			unsetWords := ", not set"
			if len(missing) < len(all) {
				unsetWords = ", " + report.JoinAnd(missing) + " not set"
			}
			say("which the project's env capability computes from " + report.JoinAnd(all) + unsetWords)
			for _, n := range missing {
				key(n, false)
			}
		case top && gone(k):
			own = append(own, k)
		case top:
			never = append(never, k)
		default:
			set(k)
		}
	}
	for _, x := range m.Names {
		key(x, true)
	}
	if len(own) > 0 {
		e.Details = append(e.Details, "compose needs "+report.JoinAnd(own))
	}
	if len(never) > 0 {
		e.Details = append(e.Details, "compose needs "+report.JoinAnd(never)+", which "+are(never)+" not set")
	}
	for _, x := range append(own, never...) {
		set(x)
	}
	// A key unset for one variable is never set for another: that bypasses the
	// capability the unset gives it back to.
	e.Set = slices.DeleteFunc(e.Set, func(k string) bool { return slices.Contains(e.Unset, k) || slices.Contains(e.UnsetAgain, k) })
	e.SetAgain = slices.DeleteFunc(e.SetAgain, func(k string) bool { return slices.Contains(e.Unset, k) || slices.Contains(e.UnsetAgain, k) })
	e.Details = append(e.Details, notes...)
	e.Details = append(e.Details, m.SaidLines()...)
	if len(m.Malformed) > 0 {
		e.Details = append(e.Details, NeedsMalformed(m.Malformed))
	}
	e.Caused = e.Caused && !e.AsBefore
	return e
}

// inputsOf are the variables the env capability needs for x, each that it did not
// compute either replaced by what it needs, down to one with a value (the capability saw
// it, and computed nothing) or one a file's or an import's empty line sets (which comes
// first), and those of them with none; through are those it replaced; nil when it said
// none.
func (m *Missing) inputsOf(x string) (all, missing, through []string) {
	seen := map[string]bool{x: true}
	var walk func(n string)
	walk = func(n string) {
		if seen[n] {
			return
		}
		seen[n] = true
		if !m.Has[n] && m.Empty[n] == "" && !m.Imported[n] && len(m.Needs[n]) > 0 {
			through = append(through, n)
			for _, i := range m.Needs[n] {
				walk(i)
			}
			return
		}
		all = append(all, n)
		if !m.Has[n] {
			missing = append(missing, n)
		}
	}
	for _, n := range m.Needs[x] {
		walk(n)
	}
	return all, missing, through
}

// Merge adds o's fix to e's, as one fix of several environments: each key once, one set
// or unset again anywhere said once as again, one unset anywhere never set (that bypasses
// the capability it gives the key back to). Details stay e's; as before only when both are.
func (e Explained) Merge(o Explained) Explained {
	e.UnsetAgain = appendNew(e.UnsetAgain, o.UnsetAgain...)
	e.SetAgain = appendNew(e.SetAgain, o.SetAgain...)
	e.Unset = slices.DeleteFunc(appendNew(e.Unset, o.Unset...), func(k string) bool { return slices.Contains(e.UnsetAgain, k) })
	e.Set = slices.DeleteFunc(appendNew(e.Set, o.Set...), func(k string) bool { return slices.Contains(e.SetAgain, k) })
	unsets := func(k string) bool { return slices.Contains(e.Unset, k) || slices.Contains(e.UnsetAgain, k) }
	e.Set, e.SetAgain = slices.DeleteFunc(e.Set, unsets), slices.DeleteFunc(e.SetAgain, unsets)
	e.Caused, e.AsBefore = e.Caused || o.Caused, e.AsBefore && o.AsBefore
	return e
}

// Fix is e's fix as words ("unset X, then set A") and each step's arguments of flag
// (--vars, --vars-instance, --vars-project): a caller adds only where and how it says it.
func (e Explained) Fix(flag string) (string, []string) {
	var words, args []string
	for _, s := range e.Steps() {
		words = append(words, s.Words)
		args = append(args, VarsEdit(flag, s.Verb, s.Keys))
	}
	return strings.Join(words, ", then "), args
}

// FixStep is one command of a missing variable's fix: its words and the --vars verb
// and keys it runs.
type FixStep struct {
	Words, Verb string
	Keys        []string
}

// Steps are e's fix, every command needed, the keys by name: those to unset again, to
// unset, to set again, then to set; none when no key brings it back, which the details
// say. The --vars follow-up and --missing-vars each add only their flag, address and
// words.
func (e Explained) Steps() []FixStep {
	var steps []FixStep
	add := func(words, verb string, keys []string) {
		if len(keys) > 0 {
			steps = append(steps, FixStep{Words: words, Verb: verb, Keys: keys})
		}
	}
	add("unset "+report.JoinAnd(e.UnsetAgain)+" again", "unset", e.UnsetAgain)
	add("unset "+report.JoinAnd(e.Unset), "unset", e.Unset)
	add("set "+report.JoinAnd(e.SetAgain)+" again", "set", e.SetAgain)
	add("set "+report.JoinAnd(e.Set), "set", e.Set)
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
	case remote.TargetEnvFile:
		return "the release's " + remote.TargetEnvFile
	}
	return f
}

func are(ns []string) string {
	if len(ns) == 1 {
		return "is"
	}
	return "are"
}

// appendNew appends what list lacks of vs.
func appendNew(list []string, vs ...string) []string {
	for _, v := range vs {
		if !slices.Contains(list, v) {
			list = append(list, v)
		}
	}
	return list
}
