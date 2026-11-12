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
// alone. On a target, the fix names the environment's shared/.env, which --vars set
// writes, before the other two levels.
func TestMissingVars(t *testing.T) {
	t.Parallel()
	vars := `{"TOKEN":{"Required":true},"EMPTY":{"Required":true},"SET":{"Required":true},"OPT":{"Required":false}}`
	said := "error while interpolating services.app.environment.TOKEN: required variable TOKEN is missing a value: the token"
	for _, c := range []struct {
		name, said, vars string
		details          []string
		fix              bool
	}{
		{"reads", "", vars, nil, false},
		{"missing", said, vars, []string{"  EMPTY", "  TOKEN", said}, true},
		{"unreadable", "services.app.ports must be a list", `{"SET":{"Required":true}}`, []string{"services.app.ports must be a list"}, false},
	} {
		root := project(t, "tunnel", map[string]string{
			"compose.yaml": minimalCompose,
			"bin/docker*":  fakeCompose(c.said, c.vars),
			"target.env":   "COMPOSE_PROJECT_NAME='tunnel-own-prod'\nDEVOPSY_TARGET='vm1:own/prod'\n",
		})
		p, err := loadProject(root, []string{"PATH=" + filepath.Join(root, ProjectDirName, "bin") + ":/usr/bin:/bin", "SET=1", "EMPTY="})
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
		want := report.Fix("set them in the environment's shared/.env (--vars-instance or --vars-project for the instance's or the project's .env), then try again:", "devopsy @vm1:own/prod --vars set <KEY>...")
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
