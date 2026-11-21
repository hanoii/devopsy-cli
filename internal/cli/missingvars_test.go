package cli

import (
	"errors"
	"fmt"
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
		want := report.Fix("set EMPTY and TOKEN in the environment's shared/.env (--vars-instance or --vars-project for the instance's or the project's .env), then try again:", "devopsy @vm1:own/prod --vars set EMPTY TOKEN")
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

// Explain words what a change leaves missing from what the edit did and what the env
// capability said it needs, never more: an input the edit took is set again and the
// change is why ("so"); a change that changed nothing is "as before"; one whose outcome is
// unknown (--missing-vars, --inherit) says neither. A chain of needs is followed down to
// an input with a value, where the capability computed nothing and nothing is offered. A
// file's empty line is said with that file: at the edited file or below it the fix goes
// there, a nearer one is to remove; a variable the capability computes or needs others
// for, emptied by a file, is unset at the edited file, never set by hand. An input an
// import empties, or one devopsy sets itself, is said and never set.
func TestExplain(t *testing.T) {
	t.Parallel()
	env, project := Edit{File: ".env"}, Edit{File: "project.env"}
	took := func(e Edit, keys ...string) Edit {
		e.Known, e.Gone = true, map[string]bool{}
		for _, k := range keys {
			e.Gone[k] = true
		}
		return e
	}
	unchanged := Edit{File: ".env", Known: true, Unchanged: true}
	needs := func(kv ...string) map[string][]string {
		n := map[string][]string{}
		for i := 0; i < len(kv); i += 2 {
			n[kv[i]] = strings.Split(kv[i+1], " ")
		}
		return n
	}
	for _, c := range []struct {
		name   string
		m      Missing
		edit   Edit
		detail string
		fix    string
		caused bool
		before bool
	}{
		{"an input the edit removed", Missing{Names: []string{"U"}, Needs: needs("U", "A")}, took(env, "A"),
			"compose needs U, which the project's env capability computes from A, not set", "set A again", true, false},
		{"the same, already gone", Missing{Names: []string{"U"}, Needs: needs("U", "A")}, unchanged,
			"compose needs U, which the project's env capability computes from A, not set", "set A", false, true},
		{"the same, no edit known", Missing{Names: []string{"U"}, Needs: needs("U", "A")}, env,
			"compose needs U, which the project's env capability computes from A, not set", "set A", false, false},
		{"a chain down to an input set", Missing{Names: []string{"X"}, Needs: needs("X", "Y", "Y", "Z"), Has: map[string]bool{"Y": true}}, env,
			"compose needs X, which the project's env capability computes from Y, but Y is set and it computed nothing", "", false, false},
		{"a chain followed", Missing{Names: []string{"X"}, Needs: needs("X", "Y", "Y", "Z")}, took(env, "Z"),
			"compose needs X, which the project's env capability computes from Z, not set", "set Z again", true, false},
		{"several inputs, one set", Missing{Names: []string{"X"}, Needs: needs("X", "A B"), Has: map[string]bool{"A": true}}, env,
			"compose needs X, which the project's env capability computes from A and B, B not set", "set B", false, false},
		{"computed, emptied by the edit", Missing{Names: []string{"X"}, Empty: map[string]string{"X": ".env"}, Computed: map[string]bool{"X": true}}, took(env, "X"),
			"compose needs X, which the environment's .env sets empty, hiding what the project's env capability computes", "unset X again", true, false},
		{"needing others, emptied in the edited file", Missing{Names: []string{"X"}, Needs: needs("X", "A"), Empty: map[string]string{"X": ".env"}}, env,
			"compose needs X, which the environment's .env sets empty, over what the project's env capability computes from A", "unset X, then set A", false, false},
		{"computed, emptied by a nearer file", Missing{Names: []string{"X"}, Empty: map[string]string{"X": ".env"}, Computed: map[string]bool{"X": true}}, took(project, "A"),
			"compose needs X, which the environment's .env sets empty: its line wins over a value below it, so remove that line there", "", false, false},
		{"a plain one emptied by a nearer file", Missing{Names: []string{"X"}, Empty: map[string]string{"X": ".env"}}, took(project, "X"),
			"compose needs X, which the environment's .env sets empty: its line wins over a value below it, so remove that line there", "", false, false},
		{"a plain one emptied below", Missing{Names: []string{"X"}, Empty: map[string]string{"X": "project.env"}}, env,
			"compose needs X, which the project's .env sets empty", "set X", false, false},
		{"a plain one the edit emptied", Missing{Names: []string{"X"}, Empty: map[string]string{"X": ".env"}}, took(env, "X"),
			"compose needs X, which the environment's .env sets empty", "set X again", true, false},
		{"an input a nearer file empties", Missing{Names: []string{"X"}, Needs: needs("X", "A"), Empty: map[string]string{"A": ".env"}}, took(project, "A"),
			"the environment's .env sets A empty: its line wins over a value below it, so remove that line there", "", false, false},
		{"an input an import empties", Missing{Names: []string{"X"}, Needs: needs("X", "A"), Imported: map[string]bool{"A": true}}, env,
			"A is imported, and empty where it comes from", "", false, false},
		{"an input devopsy sets", Missing{Names: []string{"X"}, Needs: needs("X", "DEVOPSY_UID")}, env,
			"DEVOPSY_UID", "", false, false},
		{"plain ones", Missing{Names: []string{"A", "B"}}, took(env, "A"),
			"compose needs B, which is not set", "set A again, then set B", true, false},
		{"needs lines that do not read", Missing{Names: []string{"A"}, Malformed: []int{3, 5}}, env,
			"lines 3 and 5 of the env capability's output start # needs: and do not read, so devopsy ignores them", "set A", false, false},
	} {
		e := c.m.Explain(c.edit)
		if !slices.ContainsFunc(e.Details, func(d string) bool { return strings.Contains(d, c.detail) }) {
			t.Errorf("%s: details %q, want %q", c.name, e.Details, c.detail)
		}
		var words []string
		for _, s := range e.Steps() {
			words = append(words, s.Words)
		}
		if got := strings.Join(words, ", then "); got != c.fix || e.Caused != c.caused || e.AsBefore != c.before {
			t.Errorf("%s: fix %q caused %v as before %v, want %q %v %v", c.name, got, e.Caused, e.AsBefore, c.fix, c.caused, c.before)
		}
	}
}

// One fix of several environments names each key once: a key set again in one and
// plainly in another is set again, once.
func TestExplainedMerge(t *testing.T) {
	t.Parallel()
	a := Explained{SetAgain: []string{"A"}, Set: []string{"B"}}
	b := Explained{Set: []string{"A", "C"}, Unset: []string{"X"}, UnsetAgain: []string{"X"}, Caused: true}
	m := a.Merge(b)
	if fmt.Sprint(m.SetAgain, m.Set, m.UnsetAgain, m.Unset) != "[A] [B C] [X] []" || !m.Caused {
		t.Errorf("merged: %+v", m)
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

// What the env capability needs for a missing variable travels with it from the server's
// check to the operator: the variables it needs, those it did not compute either down to
// one with a value, whether each has a value or an import set it empty, the files that
// set a missing one or an input empty (never the capability's own empty value nor an
// import's), a computed one a file's empty line hides, and the needs lines devopsy could
// not read, by line number; a line naming what is no variable's name is dropped on the way
// back.
func TestMissingCarriesNeeds(t *testing.T) {
	t.Parallel()
	said := "required variable URL is missing a value\nrequired variable HIDDEN is missing a value\nrequired variable BLANK is missing a value"
	root := project(t, "shop", map[string]string{
		"compose.yaml":              minimalCompose,
		"config.yaml":               "project: shop\n",
		"bin/docker*":               fakeCompose(said, `{"URL":{"Required":true},"HIDDEN":{"Required":true},"BLANK":{"Required":true}}`),
		"capabilities/env/compute*": "#!/bin/sh\necho '# needs: URL AUTH_USERS'\necho '# needs: AUTH_USERS AUTH SALT'\necho '# needs: LONELY'\necho \"HIDDEN='v'\"\necho \"BLANK=''\"\n",
		".env":                      "HIDDEN=\nSALT=\n",
		"target.env":                "COMPOSE_PROJECT_NAME='shop-own-prod'\nDEVOPSY_TARGET='vm1:own/prod'\n" + ImportedMarker + "\nAUTH=''\n",
	})
	p, err := loadProject(root, []string{"PATH=" + filepath.Join(root, ProjectDirName, "bin") + ":/usr/bin:/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.computeEnv(); err != nil {
		t.Fatal(err)
	}
	m, err := missingOf(p)
	if err != nil || m == nil {
		t.Fatalf("%+v %v", m, err)
	}
	got := ParseMissing(append(m.Lines(), "needs URL bad-name", "has bad-name", "imported $X"))
	if got == nil || fmt.Sprint(got.Needs) != fmt.Sprint(map[string][]string{"AUTH_USERS": {"AUTH", "SALT"}, "URL": {"AUTH_USERS"}}) ||
		len(got.Has) != 0 || fmt.Sprint(got.Imported) != fmt.Sprint(map[string]bool{"AUTH": true}) || !slices.Equal(got.Malformed, []int{3}) ||
		fmt.Sprint(got.Empty) != fmt.Sprint(map[string]string{"HIDDEN": ".env", "SALT": ".env"}) || fmt.Sprint(got.Computed) != fmt.Sprint(map[string]bool{"HIDDEN": true}) {
		t.Errorf("read back from %q:\n%+v", m.Lines(), got)
	}
}
