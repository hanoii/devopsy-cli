package cli

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// fakeCompose is a project's bin/docker whose compose config -q says said and fails, or
// passes when said is "", and whose config --variables --format json prints vars.
func fakeCompose(said, vars string) string {
	return "#!/bin/sh\ncase \"$*\" in\n" +
		"  *' config -q') [ -z '" + said + "' ] || { echo '" + said + "' >&2; exit 1; } ;;\n" +
		"  *' config --variables --format json') echo '" + vars + "' ;;\n" +
		"  *) echo \"unexpected: $*\" >&2; exit 2 ;;\nesac\n"
}

// --missing-vars goes by the installed compose: the files reading is the verdict, whatever
// --variables lists (a variable an include's env_file sets is listed required, and set);
// when they do not read, the required variables unset or empty are listed with what
// compose said, a set one and an optional one left out; when none is, compose's words
// alone, a line compose said twice once, its message the variables missing as --vars'
// follow-up words them. Compose's words are kept only where they name a missing variable,
// from its "required variable", after "compose says:", and masked, on either path: compose
// interpolates a :? message, and its other errors quote values. On a target, the fix names the environment's
// shared/.env, which --vars set writes, before the other two levels, and sets the
// variables missing by their names, a command that works when copied.
func TestMissingVars(t *testing.T) {
	t.Parallel()
	vars := `{"TOKEN":{"Required":true},"EMPTY":{"Required":true},"SET":{"Required":true},"OPT":{"Required":false}}`
	said := "error while interpolating services.app.environment.EMPTY: required variable EMPTY is missing a value\nerror while interpolating services.app.environment.TOKEN: required variable TOKEN is missing a value: the token for hunter2secret"
	masked := "compose says: required variable TOKEN is missing a value: the token for ***"
	warned := `level=warning msg="The TOKEN variable is not set. Defaulting to a blank string."`
	for _, c := range []struct {
		name, said, vars string
		details          []string
		fix              bool
	}{
		{"reads", "", vars, nil, false},
		{"missing", warned + "\n" + warned + "\n" + said, vars, []string{"compose says: required variable EMPTY is missing a value", masked}, true},
		{"unreadable", "services.app.ports must be a list, not hunter2secret", `{"SET":{"Required":true}}`, []string{"services.app.ports must be a list, not ***"}, false},
	} {
		root := project(t, "tunnel", map[string]string{
			"compose.yaml": minimalCompose,
			"config.yaml":  "project: tunnel\nsecrets: [KEY]\n",
			"bin/docker*":  fakeCompose(c.said, c.vars),
			"target.env":   "COMPOSE_PROJECT_NAME='tunnel-own-prod'\nDEVOPSY_TARGET='vm1:own/prod'\n",
		})
		p, err := loadProject(root, []string{"PATH=" + filepath.Join(root, ProjectDirName, "bin") + ":/usr/bin:/bin", "SET=1", "EMPTY=", "KEY=hunter2secret"})
		if err != nil {
			t.Fatal(err)
		}
		err = missingVars(p)
		var failure *report.Failure
		if c.details == nil {
			if _, ok := err.(*Output); !ok {
				t.Errorf("%s: %v, want nothing missing", c.name, err)
			}
			continue
		}
		if !errors.As(err, &failure) {
			t.Fatalf("%s: %v, want a failure", c.name, err)
		}
		if !slices.Equal(failure.Details, c.details) {
			t.Errorf("%s: details %q, want %q", c.name, failure.Details, c.details)
		}
		if !c.fix {
			if !strings.Contains(failure.Msg, "cannot read") || len(failure.Remedies) != 0 {
				t.Errorf("%s: %q %+v, want compose's words and no fix", c.name, failure.Msg, failure.Remedies)
			}
			continue
		}
		if failure.Msg != "compose needs EMPTY and TOKEN, which are not set" {
			t.Errorf("%s: message %q", c.name, failure.Msg)
		}
		want := report.Fix("set them in the environment's shared/.env (--vars-instance or --vars-project for the instance's or the project's .env), then try again:", "devopsy @vm1:own/prod --vars set EMPTY TOKEN")
		if len(failure.Remedies) != 1 || failure.Remedies[0].Text != want.Text || len(failure.Remedies[0].Cmds) != 1 || failure.Remedies[0].Cmds[0] != want.Cmds[0] {
			t.Errorf("%s: fixes %+v\nwant: %+v", c.name, failure.Remedies, want)
		}
	}
}

// The variables a project reads are those of its compose files and their override, and of
// what they include and extend, read with no Docker; what the loader cannot follow (an
// include whose path is a variable) leaves the project's own files' variables, that path's included.
func TestComposeVariables(t *testing.T) {
	t.Parallel()
	root := project(t, "shop", map[string]string{
		"compose.yaml":          "include:\n  - path: sub/inc.yaml\n    env_file: sub/inc.env\nservices:\n  web:\n    image: x\n    extends: {file: base.yaml, service: b}\n    user: ${MAIN:?}\n",
		"compose.override.yaml": "services:\n  web:\n    environment: [\"O=${OVERRIDE}\"]\n",
		"base.yaml":             "services:\n  b:\n    image: x\n    environment: [\"E=${FROM_EXTENDS:-1}\"]\n",
		"sub/inc.yaml":          "services:\n  c:\n    image: x\n    user: ${FROM_INCLUDE:?}\n",
		"sub/inc.env":           "FROM_INCLUDE=1\n",
	})
	got, err := ComposeVariables(filepath.Join(root, ProjectDirName))
	if want := []string{"FROM_EXTENDS", "FROM_INCLUDE", "MAIN", "OVERRIDE"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("ComposeVariables: %v %v, want %v", got, err, want)
	}
	root = project(t, "shop", map[string]string{
		"compose.yaml": "include:\n  - ${INC:-sub}/inc.yaml\nservices:\n  web:\n    image: x\n    user: ${MAIN:?}\n",
		"sub/inc.yaml": "services:\n  c:\n    image: x\n    user: ${FROM_INCLUDE:?}\n",
	})
	got, err = ComposeVariables(filepath.Join(root, ProjectDirName))
	if want := []string{"INC", "MAIN"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("an include by a variable's path: %v %v, want %v", got, err, want)
	}
}

// A project whose compose files read either uid needs a server that gives them; a variable
// named alike does not.
func TestReadsUIDs(t *testing.T) {
	t.Parallel()
	for names, want := range map[string]bool{
		"DEVOPSY_UID":           true,
		"A DEVOPSY_PROJECT_UID": true,
		"DEVOPSY_UIDS UID":      false,
		"":                      false,
	} {
		if got := ReadsUIDs(strings.Fields(names)); got != want {
			t.Errorf("%q: %v, want %v", names, got, want)
		}
	}
}

// The --vars follow-up's check loads the project again with the edited file as it was
// standing in, at its level, only when something is missing: a variable the env capability
// computed then, one a file's interpolation set then, one missing then too (a nearer file
// emptying the key, which overlaying the old value would hide), a key the edit added that
// made the capability stop, each with the keys the edit changed. The old content not
// parsing, or the capability failing on it, leaves them unknown, said in words that
// never carry an old value.
func TestCheckMissingBefore(t *testing.T) {
	t.Parallel()
	compute := "#!/bin/sh\n[ -z \"${FAIL:-}\" ] || { echo \"failing on $FAIL\" >&2; exit 1; }\n[ -z \"${A:-}\" ] || [ -n \"${OFF:-}\" ] || echo \"AU=u-$A\"\n"
	said := "error while interpolating services.app.environment.AU: required variable AU is missing a value: set A\nerror while interpolating services.app.environment.X: required variable X is missing a value"
	for _, c := range []struct {
		name, level, env, instance, old string
		missing                         []string
		set, computed                   []string
		changed                         map[string]string
		unknown                         string
	}{
		{name: "unset", level: "env", env: "X=${B}\n", old: "A=hunter2secret\nB=1\nX=${B}\n",
			missing: []string{"AU", "X"}, set: []string{"AU", "X"}, computed: []string{"AU"}, changed: map[string]string{"A": "removed", "B": "removed"}},
		{name: "added", level: "env", env: "A=1\nOFF=1\nX=1\n", old: "A=1\nX=1\n",
			missing: []string{"AU"}, set: []string{"AU"}, computed: []string{"AU"}, changed: map[string]string{"OFF": "added"}},
		{name: "nearer", level: "instance", env: "A=\nX=1\n", instance: "", old: "A=hunter2secret\n",
			missing: []string{"AU"}, changed: map[string]string{"A": "removed"}},
		{name: "unparsable", level: "env", env: "X=1\n", old: "A='hunter2secret\n",
			missing: []string{"AU"}, unknown: "the file before this change does not parse"},
		{name: "capability", level: "env", env: "X=1\n", old: "A=1\nFAIL=hunter2secret\nX=1\n",
			missing: []string{"AU"}, unknown: "failing on ***"},
	} {
		root := project(t, "shop", map[string]string{
			"compose.yaml":              minimalCompose,
			"config.yaml":               "project: shop\nsecrets: [A, FAIL]\n",
			"bin/docker*":               fakeCompose(said, `{"AU":{"Required":true},"X":{"Required":true}}`),
			"capabilities/env/compute*": compute,
			".env":                      c.env,
			"instance.env":              c.instance,
		})
		m, err := CheckMissing(root, []string{"PATH=" + filepath.Join(root, ProjectDirName, "bin") + ":/usr/bin:/bin"}, &Before{File: BeforeFiles[c.level], Content: []byte(c.old)})
		if err != nil || m == nil {
			t.Fatalf("%s: %+v %v", c.name, m, err)
		}
		if !slices.Equal(m.Names, c.missing) || len(m.Said) != len(c.missing) {
			t.Errorf("%s: missing %q, said %q", c.name, m.Names, m.Said)
		}
		if c.unknown != "" {
			if m.Before != nil || !strings.Contains(m.Unknown, c.unknown) || strings.Contains(m.Unknown, "hunter2secret") {
				t.Errorf("%s: before %+v, unknown %q, want %q and no value", c.name, m.Before, m.Unknown, c.unknown)
			}
			continue
		}
		if m.Before == nil {
			t.Fatalf("%s: unknown %q", c.name, m.Unknown)
		}
		keys := func(set map[string]bool) []string {
			var out []string
			for k, ok := range set {
				if ok {
					out = append(out, k)
				}
			}
			slices.Sort(out)
			return out
		}
		if got := keys(m.Before.Set); !slices.Equal(got, c.set) {
			t.Errorf("%s: set before %q, want %q", c.name, got, c.set)
		}
		if got := keys(m.Before.Computed); !slices.Equal(got, c.computed) {
			t.Errorf("%s: computed before %q, want %q", c.name, got, c.computed)
		}
		if len(m.Before.Changed) != len(c.changed) {
			t.Errorf("%s: changed %v, want %v", c.name, m.Before.Changed, c.changed)
		}
		for k, v := range c.changed {
			if m.Before.Changed[k] != v {
				t.Errorf("%s: changed %v, want %v", c.name, m.Before.Changed, c.changed)
			}
		}
	}
}

// The check's lines read back as they were written: the variables before the edit known
// whatever the edit changed, none included (an unset of a key the file lacks still says
// "as before" of a variable compose missed then too), and a capability's stderr of
// several lines kept whole as the reason they are unknown, none of its lines taken for
// one of the protocol's (a "set A ..." line making them known).
func TestMissingLines(t *testing.T) {
	t.Parallel()
	known := &Missing{Names: []string{"REQUIRED"}, Before: &MissingBefore{Computed: map[string]bool{}, Set: map[string]bool{}, Changed: map[string]string{}}}
	got := ParseMissing(known.Lines())
	if got == nil || got.Before == nil {
		t.Fatalf("the variables before the edit lost: %q -> %+v", known.Lines(), got)
	}
	if e := got.Explain([]string{"REQUIRED"}); !e.AsBefore || len(e.SetAgain) != 0 {
		t.Errorf("an edit that changed nothing: %+v", e)
	}
	unknown := &Missing{Names: []string{"X"}, Unknown: "devopsy could not tell whether the env capability computed what is missing: compute: exit status 1\nset A (user:password) first"}
	got = ParseMissing(strings.Split(strings.Join(unknown.Lines(), "\n"), "\n"))
	if got == nil || got.Before != nil || got.Unknown != unknown.Unknown {
		t.Fatalf("a capability's stderr: %+v", got)
	}
	if e := got.Explain(nil); e.AsBefore || !slices.Contains(e.Details, "set A (user:password) first") {
		t.Errorf("a capability's stderr explained: %+v", e)
	}
}

// Explain never offers a key the edit did not change: a variable set before is brought
// back by the keys the edit changed, so with none (a key set to the value it had, the
// capability answering otherwise) it says so in words and offers nothing; and a key the
// edit removed that was empty before too is no key of the edit's, compose having missed
// it already (an environment's empty line winning over the project's value the edit
// unset), so it is "as before", and its fix names the file that empties it, never one at
// the edited level, where the nearer line still wins.
func TestExplainOffersOnlyWhatBringsItBack(t *testing.T) {
	t.Parallel()
	m := &Missing{Names: []string{"X"}, Before: &MissingBefore{Computed: map[string]bool{"X": true}, Set: map[string]bool{"X": true}, Changed: map[string]string{}}}
	if e := m.Explain(nil); len(e.SetAgain)+len(e.UnsetAgain)+len(e.SetThem) != 0 || !slices.ContainsFunc(e.Details, func(d string) bool { return strings.Contains(d, "left its file as it was") }) {
		t.Errorf("no key changed: %+v", e)
	}
	said := "error while interpolating services.app.environment.REQUIRED: required variable REQUIRED is missing a value"
	root := project(t, "shop", map[string]string{
		"compose.yaml": minimalCompose,
		"config.yaml":  "project: shop\n",
		"bin/docker*":  fakeCompose(said, `{"REQUIRED":{"Required":true}}`),
		".env":         "REQUIRED=\n",
		"project.env":  "",
	})
	m, err := CheckMissing(root, []string{"PATH=" + filepath.Join(root, ProjectDirName, "bin") + ":/usr/bin:/bin"}, &Before{File: BeforeFiles["project"], Content: []byte("REQUIRED='1'\n")})
	if err != nil || m == nil || m.Before == nil {
		t.Fatalf("%+v %v", m, err)
	}
	if e := m.Explain([]string{"REQUIRED"}); !e.AsBefore || slices.Contains(e.SetAgain, "REQUIRED") || slices.Contains(e.SetThem, "REQUIRED") || !strings.Contains(e.Details[0], "the environment's .env sets empty") {
		t.Errorf("a key removed that was empty before too: %+v", e)
	}
}

// Compose's words never carry a secret of several lines (a key in PEM, a JSON
// credential), on either path: compose's stderr is masked whole before it is split into
// lines, since Secrets.Mask replaces a value whole and compose interpolates a :? message.
func TestComposeWordsMaskSecretsOfSeveralLines(t *testing.T) {
	t.Parallel()
	key := "first-line-of-the-key\nsecond-line-of-the-key"
	for _, c := range []struct{ name, said, vars string }{
		{"missing", "error while interpolating services.app.environment.TOKEN: required variable TOKEN is missing a value: sign it with " + key, `{"TOKEN":{"Required":true}}`},
		{"unreadable", "services.app.configs: invalid content " + key, `{}`},
	} {
		root := project(t, "shop", map[string]string{
			"compose.yaml": minimalCompose,
			"config.yaml":  "project: shop\nsecrets: [KEY]\n",
			"bin/docker*":  fakeCompose(c.said, c.vars),
		})
		p, err := loadProject(root, []string{"PATH=" + filepath.Join(root, ProjectDirName, "bin") + ":/usr/bin:/bin", "KEY=" + key})
		if err != nil {
			t.Fatal(err)
		}
		var failure *report.Failure
		if err := missingVars(p); !errors.As(err, &failure) {
			t.Fatalf("%s: %v, want a failure", c.name, err)
		}
		said := failure.Msg + "\n" + strings.Join(failure.Details, "\n")
		if strings.Contains(said, "of-the-key") {
			t.Errorf("%s: a line of the secret said:\n%s", c.name, said)
		}
		// The follow-up's lines, what --check-images --hint prints for the operator.
		if m, err := missingOf(p); m != nil {
			if lines := strings.Join(m.Lines(), "\n"); strings.Contains(lines, "of-the-key") {
				t.Errorf("%s: a line of the secret in the check's lines:\n%s", c.name, lines)
			}
		} else if err != nil && strings.Contains(err.Error(), "of-the-key") {
			t.Errorf("%s: a line of the secret in the check's error: %v", c.name, err)
		}
	}
}

// What is missing is what compose names: a variable an include's env_file sets is unset in
// devopsy's environment and set for compose, which reads that file, so it is never listed
// (nor set by the --vars follow-up's fix in shared/.env, over the include's value).
func TestMissingVarsTakesComposesVerdict(t *testing.T) {
	t.Parallel()
	said := "error while interpolating services.side.environment.[]: required variable INC_NAME is missing a value: the name"
	root := project(t, "inclvars", map[string]string{
		"compose.yaml": "include:\n  - path: sub/inc.yaml\n    env_file: sub/inc.env\nservices:\n  app:\n    image: busybox\n",
		"sub/inc.yaml": "services:\n  side:\n    image: busybox\n    environment: [\"NAME=${INC_NAME:?the name}\", \"SET=${INC_SET:?}\"]\n",
		"sub/inc.env":  "INC_SET=1\n",
		"config.yaml":  "project: inclvars\n",
		"bin/docker*":  fakeCompose(said, `{"INC_NAME":{"Required":true},"INC_SET":{"Required":true}}`),
	})
	p, err := loadProject(root, []string{"PATH=" + filepath.Join(root, ProjectDirName, "bin") + ":/usr/bin:/bin"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := missingOf(p)
	if err != nil || m == nil {
		t.Fatalf("%+v %v", m, err)
	}
	if !slices.Equal(m.Names, []string{"INC_NAME"}) {
		t.Errorf("missing %q, want INC_NAME alone: the include's env_file sets INC_SET", m.Names)
	}
}
