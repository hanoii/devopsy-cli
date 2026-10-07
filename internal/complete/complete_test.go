package complete

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// setup creates a project with a deploy command, a .env and targets, plus a
// user-level target, named to sort first: the project's still come first.
func setup(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DEVOPSY_HOME", home)
	write(t, filepath.Join(home, "targets.yaml"), "a-traefik:\n  host: devopsy@vm1\n  path: /srv/traefik\n", 0o644)
	root := filepath.Join(t.TempDir(), "app")
	dot := filepath.Join(root, ".devopsy")
	write(t, filepath.Join(dot, "compose.yaml"), "services:\n  web:\n    image: busybox\n", 0o644)
	write(t, filepath.Join(dot, ".env"), "# comment\nTOKEN=x\nexport USER_NAME='y'\n", 0o644)
	write(t, filepath.Join(dot, "targets.yaml"), "prod:\n  path: /srv/app-prod\nstaging:\n  path: /srv/app-staging\n", 0o644)
	write(t, filepath.Join(dot, "commands", "deploy"), "#!/bin/sh\n## Description: Roll out\n", 0o755)
	return root
}

func values(r Result) []string {
	var out []string
	for _, c := range r.Candidates {
		out = append(out, c.Value)
	}
	return out
}

// delegated is the words docker's completion gets after `compose -f ...`.
func delegated(t *testing.T, r Result) []string {
	t.Helper()
	if r.Delegate == nil {
		return nil
	}
	args := r.Delegate.Args
	if !slices.Equal(args[:3], []string{"docker", "__complete", "compose"}) {
		t.Fatalf("delegate %q", args)
	}
	args = args[3:]
	for len(args) > 1 && args[0] == "-f" {
		args = args[2:]
	}
	return args
}

func TestComplete(t *testing.T) {
	root := setup(t)
	cases := []struct {
		words     []string
		want      []string
		delegate  []string
		directive int
	}{
		{[]string{""}, []string{"@prod", "@staging", "@a-traefik", "deploy"}, []string{""}, DirectiveNoFileComp},
		{[]string{"@"}, []string{"@prod", "@staging", "@a-traefik"}, nil, DirectiveNoFileComp},
		{[]string{"--up"}, []string{"--upgrade"}, []string{"--up"}, DirectiveNoFileComp},
		{[]string{"-v", "@st"}, []string{"@staging"}, nil, DirectiveNoFileComp},
		{[]string{"de"}, []string{"deploy"}, []string{"de"}, DirectiveNoFileComp},
		{[]string{"deploy", ""}, nil, nil, 0},
		{[]string{"logs", ""}, nil, []string{"logs", ""}, 0},
		{[]string{"--completion", "z"}, []string{"zsh"}, nil, DirectiveNoFileComp},
		{[]string{"--env", ""}, nil, nil, DirectiveNoFileComp},
		{[]string{"@prod", "rel"}, []string{"release", "releases"}, []string{"rel"}, DirectiveNoFileComp},
		{[]string{"@prod", "--"}, []string{"--shell", "--vars", "--help"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "release", ""}, []string{"--help"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "release", "deploy", ""}, nil, nil, DirectiveNoFileComp},
		{[]string{"@prod", "rollback", "-"}, []string{"--help"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "domains", ""}, []string{"--retry"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--vars", ""}, []string{"get", "set", "unset"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--vars", "set", "TOKEN", ""}, []string{"USER_NAME"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--shell", ""}, nil, nil, DirectiveNoFileComp},
		{[]string{"@prod", "logs", ""}, nil, []string{"logs", ""}, 0},
		// A user-level target is another project: no local commands, .env
		// or release.
		{[]string{"@a-traefik", "dep"}, nil, []string{"dep"}, DirectiveNoFileComp},
		{[]string{"@a-traefik", "--vars", "set", ""}, nil, nil, DirectiveNoFileComp},
		{[]string{"@a-traefik", "release", ""}, []string{"--help"}, nil, DirectiveNoFileComp},
	}
	for _, c := range cases {
		r := Complete(root, c.words, os.Environ())
		if got := values(r); !slices.Equal(got, c.want) {
			t.Errorf("%q: candidates %q, want %q", c.words, got, c.want)
		}
		if got := delegated(t, r); !slices.Equal(got, c.delegate) {
			t.Errorf("%q: delegated %q, want %q", c.words, got, c.delegate)
		}
		if r.Directive != c.directive {
			t.Errorf("%q: directive %d, want %d", c.words, r.Directive, c.directive)
		}
	}

	// The project's files and environment go to docker.
	r := Complete(root, []string{"logs", ""}, os.Environ())
	if !slices.Contains(r.Delegate.Args, filepath.Join(root, ".devopsy", "compose.yaml")) {
		t.Errorf("no compose file in %q", r.Delegate.Args)
	}
	if !slices.Contains(r.Delegate.Env, "TOKEN=x") {
		t.Error("the project's .env is not in docker's environment")
	}

	// Outside a project: user-level targets, and compose without files.
	r = Complete(t.TempDir(), []string{""}, os.Environ())
	if got := values(r); !slices.Equal(got, []string{"@a-traefik"}) {
		t.Errorf("outside a project: %q", got)
	}
	if got := r.Delegate.Args; !slices.Equal(got, []string{"docker", "__complete", "compose", ""}) {
		t.Errorf("outside a project: delegate %q", got)
	}
}

func TestScripts(t *testing.T) {
	for _, shell := range Shells {
		script := Script(shell)
		if !strings.Contains(script, "devopsy "+Flag) {
			t.Errorf("%s: script does not call devopsy %s", shell, Flag)
		}
		if _, err := exec.LookPath(shell); err != nil {
			continue
		}
		flag := "-n"
		if shell == "fish" {
			flag = "--no-execute"
		}
		cmd := exec.Command(shell, flag)
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s", shell, err, out)
		}
	}
	if Script("powershell") != "" {
		t.Error("powershell has a script")
	}
}
