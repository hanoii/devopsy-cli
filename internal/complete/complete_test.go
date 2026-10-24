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
	write(t, filepath.Join(home, "config.yaml"), "aliases:\n  a-traefik: {project: traefik, to: \"vm1:main\"}\n", 0o644)
	// ~/.ssh/config's hosts: patterns are none, includes are followed.
	t.Setenv("HOME", home)
	write(t, filepath.Join(home, ".ssh", "config"), "# hosts\nHost vm1 \"vm 2\" *.internal !bad\n  User devopsy\nInclude conf.d/*\nhost=builder1\nMatch all\nHost vm1\n", 0o644)
	write(t, filepath.Join(home, ".ssh", "conf.d", "orb"), "Host orb\n", 0o644)
	root := filepath.Join(t.TempDir(), "app")
	dot := filepath.Join(root, ".devopsy")
	write(t, filepath.Join(dot, "compose.yaml"), "services:\n  web:\n    image: busybox\n", 0o644)
	write(t, filepath.Join(dot, ".env"), "# comment\nTOKEN=x\nexport USER_NAME='y'\n", 0o644)
	write(t, filepath.Join(dot, "config.yaml"), "project: app\ninstances: true\nenvironments:\n  prod: {}\n  staging: {}\n", 0o644)
	write(t, filepath.Join(dot, "commands", "deploy"), "#!/bin/sh\n## Description: Roll out\n", 0o755)
	write(t, filepath.Join(dot, "commands", "image"), "#!/bin/sh\n## Run: local\n", 0o755)
	write(t, filepath.Join(dot, "commands", "release"), "#!/bin/sh\n## Run: target\n", 0o755)
	write(t, filepath.Join(dot, "commands", "push"), "#!/bin/sh\n## Run: local\n## Needs: target\n", 0o755)
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

// Not parallel: its setup sets DEVOPSY_HOME and HOME.
func TestComplete(t *testing.T) {
	root := setup(t)
	cases := []struct {
		words     []string
		want      []string
		delegate  []string
		directive int
	}{
		{[]string{""}, []string{"@prod", "@staging", "@a-traefik", "deploy", "image"}, []string{""}, DirectiveNoFileComp},
		{[]string{"@"}, []string{"@prod", "@staging", "@a-traefik", "@vm1:", "@vm 2:", "@orb:", "@builder1:"}, nil, DirectiveNoFileComp},
		{[]string{"@vm"}, []string{"@vm1:", "@vm 2:"}, nil, DirectiveNoFileComp | DirectiveNoSpace},
		{[]string{"@b"}, []string{"@builder1:"}, nil, DirectiveNoFileComp | DirectiveNoSpace},
		{[]string{"--up"}, []string{"--upgrade"}, []string{"--up"}, DirectiveNoFileComp},
		{[]string{"-v", "@st"}, []string{"@staging"}, nil, DirectiveNoFileComp},
		{[]string{"de"}, []string{"deploy"}, []string{"de"}, DirectiveNoFileComp},
		{[]string{"deploy", ""}, nil, nil, 0},
		{[]string{"logs", ""}, nil, []string{"logs", ""}, 0},
		{[]string{"--completion", "z"}, []string{"zsh"}, nil, DirectiveNoFileComp},
		{[]string{"--env", ""}, []string{"--show"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "de"}, []string{"deploy"}, []string{"de"}, DirectiveNoFileComp},
		// Commands by scope: local ones here, target ones after @.
		{[]string{"im"}, []string{"image"}, []string{"im"}, DirectiveNoFileComp},
		{[]string{"rel"}, nil, []string{"rel"}, DirectiveNoFileComp},
		{[]string{"@prod", "im"}, nil, []string{"im"}, DirectiveNoFileComp},
		{[]string{"@prod", "rel"}, []string{"release"}, []string{"rel"}, DirectiveNoFileComp},
		{[]string{"pu"}, nil, []string{"pu"}, DirectiveNoFileComp},
		{[]string{"@prod", "pu"}, []string{"push"}, []string{"pu"}, DirectiveNoFileComp},
		{[]string{"@prod", "--rel"}, []string{"--release", "--release-sync", "--release-no-sync", "--releases"}, nil, DirectiveNoFileComp},
		{[]string{"@vm1:"}, []string{"@vm1:prod", "@vm1:staging"}, nil, DirectiveNoFileComp},
		{[]string{"@vm1:b/st"}, []string{"@vm1:b/staging"}, nil, DirectiveNoFileComp},
		{[]string{"@b/"}, []string{"@b/prod", "@b/staging"}, nil, DirectiveNoFileComp},
		{[]string{"@vm1:b/prod", "--sh"}, []string{"--shell", "--shell-host"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--"}, []string{"--release", "--release-sync", "--release-no-sync", "--rollback", "--releases", "--log", "--destroy", "--sync", "--reset", "--inherit", "--export", "--import", "--env", "--vars", "--vars-instance", "--vars-project", "--status", "--shell", "--shell-host", "--environments", "--debug", "--ssh-keys", "--ssh-config", "--upgrade", "--help"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--ssh-keys", ""}, []string{"list", "add", "remove"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--releases", ""}, []string{"remove", "--help"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--releases", "remove", ""}, []string{"failed", "--yes", "--help"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--releases", "remove", "2026", "--"}, []string{"--yes", "--help"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--ssh-keys", "add", "k.pub", "--"}, []string{"--restricted", "--help"}, nil, DirectiveNoFileComp},
		{[]string{"--ssh-keys-generate", "x", "-"}, []string{"--out", "--help"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--release", ""}, []string{"--from", "--accept-image", "--yes", "--help"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--release", "--from", "@st"}, []string{"@staging"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--release", "--from", "@staging", ""}, []string{"--accept-image", "--yes"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--release-sync", "--"}, []string{"--from", "--accept-image", "--yes", "--help"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--release", "deploy", ""}, nil, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--sy"}, []string{"--sync"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--sync", ""}, []string{"--from", "--accept-image", "--yes", "--help"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--reset", ""}, []string{"--from", "--accept-image", "--yes", "--help"}, nil, DirectiveNoFileComp},
		// --from takes an address, offered as the first word's, aliases
		// aside, and always with its @.
		{[]string{"@prod", "--sync", "--from", ""}, []string{"@prod", "@staging"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--sync", "--from", "@"}, []string{"@prod", "@staging", "@vm1:", "@vm 2:", "@orb:", "@builder1:"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--sync", "--from", "@vm"}, []string{"@vm1:", "@vm 2:"}, nil, DirectiveNoFileComp | DirectiveNoSpace},
		{[]string{"@prod", "--sync", "--from", "@vm1:st"}, []string{"@vm1:staging"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--sync", "--from", "st"}, nil, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--sync", "--from", "staging", ""}, []string{"--accept-image", "--yes"}, nil, DirectiveNoFileComp},
		// --export and --import: the service is the user's to type.
		{[]string{"@prod", "--export", ""}, []string{"--help"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--export", "database", ""}, nil, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--import", "database", ""}, []string{"--yes", "--resolved", "--accept-image"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--import", "database", "--yes", ""}, []string{"--resolved", "--accept-image"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--import", "database", "--accept-image", ""}, []string{"web"}, nil, DirectiveNoFileComp},
		{[]string{"--import", ""}, []string{"--help", "--yes"}, nil, DirectiveNoFileComp},
		{[]string{"--export", "-"}, []string{"--help"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--rollback", "-"}, []string{"--help"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--vars", ""}, []string{"get", "set", "unset"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--vars", "set", "TOKEN", ""}, []string{"USER_NAME"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--shell", ""}, []string{"web"}, nil, DirectiveNoFileComp},
		// --accept-image takes a service.
		{[]string{"@prod", "--sync", "--accept-image", ""}, []string{"web"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--release", "--accept-image", "web", ""}, []string{"--from", "--yes"}, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--shell-host", ""}, nil, nil, DirectiveNoFileComp},
		{[]string{"@prod", "--debug", ""}, []string{"env", "capabilities", "labels", "imports"}, nil, DirectiveNoFileComp},
		{[]string{"--shell", "w"}, []string{"web"}, nil, DirectiveNoFileComp},
		{[]string{"--sh"}, []string{"--shell"}, []string{"--sh"}, DirectiveNoFileComp},
		{[]string{"@prod", "logs", ""}, nil, []string{"logs", ""}, 0},
		// A user-level target is another project: no local commands, .env
		// or release.
		{[]string{"@a-traefik", "dep"}, nil, []string{"dep"}, DirectiveNoFileComp},
		{[]string{"@a-traefik", "--vars", "set", ""}, nil, nil, DirectiveNoFileComp},
		{[]string{"@a-traefik", "--release", "--h"}, []string{"--help"}, nil, DirectiveNoFileComp},
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
	t.Parallel()
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

// A user-level target with source completes as its source's project, from
// any directory: its commands and its services for --shell.
//
// Not parallel: its setup sets DEVOPSY_HOME and HOME.
func TestCompleteUserTargetSource(t *testing.T) {
	root := setup(t)
	home := os.Getenv("DEVOPSY_HOME")
	write(t, filepath.Join(home, "config.yaml"), "aliases:\n  a-traefik: {source: "+root+", to: \"vm1:prod\"}\n", 0o644)
	elsewhere := t.TempDir()
	for _, c := range []struct {
		words []string
		want  []string
	}{
		{[]string{"@a-traefik", "--shell", ""}, []string{"web"}},
		{[]string{"@a-traefik", "de"}, []string{"deploy"}},
		// Its values never come from the source's .env.
		{[]string{"@a-traefik", "--vars", "set", ""}, nil},
	} {
		r := Complete(elsewhere, c.words, os.Environ())
		if got := values(r); !slices.Equal(got, c.want) {
			t.Errorf("%q: candidates %q, want %q", c.words, got, c.want)
		}
	}
	// Without source, nothing of the project's.
	write(t, filepath.Join(home, "config.yaml"), "aliases:\n  a-traefik: {project: traefik, to: \"vm1:main\"}\n", 0o644)
	if got := values(Complete(elsewhere, []string{"@a-traefik", "--shell", ""}, os.Environ())); len(got) != 0 {
		t.Errorf("without source: %q", got)
	}
}
