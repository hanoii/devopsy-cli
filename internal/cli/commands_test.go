package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Tests never ask the machine's docker: compose's commands and the running
// services are fixed, and nothing is cached.
func init() {
	ComposeCommands = func([]string) ([]string, error) {
		return []string{"build", "config", "down", "exec", "logs", "ps", "pull", "restart", "run", "up", "version"}, nil
	}
	ComposeCommandsCache = func() string { return "" }
	RunningServices = func(*loadedProject) ([]string, error) { return nil, nil }
}

func TestParseCompletion(t *testing.T) {
	got, err := parseCompletion([]byte("attach\tAttach\nup\tCreate and start\n:4\nCompletion ended with directive: ShellCompDirectiveNoFileComp\n"))
	if err != nil || !slices.Equal(got, []string{"attach", "up"}) {
		t.Fatalf("%q %v", got, err)
	}
	// A fake or old docker: not cobra's output, so not trusted.
	for _, out := range []string{"", "[__complete][compose][]\nproject=x\n", "up\tx\n"} {
		if _, err := parseCompletion([]byte(out)); err == nil {
			t.Errorf("%q: want an error", out)
		}
	}
}

func TestUnknownWord(t *testing.T) {
	root := project(t, "app", map[string]string{
		"compose.yaml":     minimalCompose,
		"commands/deploy*": "#!/bin/sh\n",
	})
	if _, err := build(root, []string{"logs", "-f"}, nil); err != nil {
		t.Fatalf("compose command: %v", err)
	}
	if _, err := build(root, []string{"--profile", "x", "up"}, nil); err != nil {
		t.Fatalf("compose flags first pass unchecked: %v", err)
	}
	if _, err := build(root, []string{"help"}, nil); err != nil {
		t.Fatalf("help: %v", err)
	}
	var e *ExitError
	_, err := build(root, []string{"relase"}, nil)
	if !errors.As(err, &e) || e.Msg != "relase: not a project command (deploy) or a docker compose command" {
		t.Fatalf("typo: %v", err)
	}
	_, err = build(root, []string{"release"}, nil)
	if !errors.As(err, &e) || !strings.Contains(e.Msg, "devopsy @<target> --release") {
		t.Fatalf("a moved word: %v", err)
	}

	// --: straight to compose, even a word compose lacks or a project
	// command's name.
	plan, err := build(root, []string{"--", "deploy", "x"}, nil)
	if err != nil || plan.Path != "docker" || !slices.Equal(plan.Args[len(plan.Args)-2:], []string{"deploy", "x"}) || slices.Contains(plan.Args, "--") {
		t.Fatalf("--: %v %+v", err, plan)
	}

	// compose cannot be asked: pass the word on, as before.
	saved := ComposeCommands
	defer func() { ComposeCommands = saved }()
	ComposeCommands = func([]string) ([]string, error) { return nil, errors.New("no docker") }
	if _, err := build(root, []string{"newcommand"}, nil); err != nil {
		t.Fatalf("fail open: %v", err)
	}
}

// The cache answers known words without asking compose, and is refreshed
// when a word is missing, as after a compose upgrade.
func TestComposeCommandsCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "devopsy", "compose-commands")
	savedCache, savedCommands := ComposeCommandsCache, ComposeCommands
	defer func() { ComposeCommandsCache, ComposeCommands = savedCache, savedCommands }()
	ComposeCommandsCache = func() string { return cache }
	asked := 0
	list := []string{"up", "ps"}
	ComposeCommands = func([]string) ([]string, error) { asked++; return list, nil }

	if !isComposeCommand("up", nil) || asked != 1 {
		t.Fatalf("first use asks compose: asked %d", asked)
	}
	if !isComposeCommand("ps", nil) || asked != 1 {
		t.Fatalf("cached: asked %d", asked)
	}
	list = []string{"up", "ps", "watch"}
	if !isComposeCommand("watch", nil) || asked != 2 {
		t.Fatalf("new command after an upgrade: asked %d", asked)
	}
	if data, _ := os.ReadFile(cache); !strings.Contains(string(data), "watch") {
		t.Fatalf("cache not refreshed: %q", data)
	}
	if isComposeCommand("nope", nil) {
		t.Fatal("unknown word")
	}
}

func TestShell(t *testing.T) {
	compose := "services:\n  web:\n    image: x\n    labels:\n      - traefik.enable=true\n      - devopsy.shell=true\n  db:\n    image: y\n"
	root := project(t, "app", map[string]string{"compose.yaml": compose})
	tail := []string{"sh", "-c", shellCommand}

	plan, err := build(root, []string{"--shell"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.Args[len(plan.Args)-5:], append([]string{"exec", "web"}, tail...)) {
		t.Fatalf("labeled service: %q", plan.Args)
	}
	plan, err = build(root, []string{"--shell", "db", "--user", "root"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.Args[len(plan.Args)-7:], append([]string{"exec", "--user", "root", "db"}, tail...)) {
		t.Fatalf("named service with options: %q", plan.Args)
	}

	// devopsy.shell.user: the service's shell user, unless given.
	userCompose := "services:\n  app:\n    image: x\n    labels:\n      devopsy.shell: \"true\"\n      devopsy.shell.user: app\n  db:\n    image: y\n"
	withUser := project(t, "withuser", map[string]string{"compose.yaml": userCompose})
	for args, want := range map[string][]string{
		"--shell":                 {"exec", "--user", "app", "app"},
		"--shell app --user root": {"exec", "--user", "root", "app"},
		"--shell app -uroot":      {"exec", "-uroot", "app"},
		"--shell db":              {"exec", "db"},
	} {
		plan, err := build(withUser, strings.Fields(args), nil)
		if err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		got := plan.Args[len(plan.Args)-len(want)-3 : len(plan.Args)-3]
		if !slices.Equal(got, want) {
			t.Errorf("%s: %q, want %q", args, got, want)
		}
	}

	// A command after --: run directly, in the same service and user.
	for args, want := range map[string][]string{
		"--shell -- drush status":        {"exec", "--user", "app", "app", "drush", "status"},
		"--shell db --user root -- ls /": {"exec", "--user", "root", "db", "ls", "/"},
		"--shell app -- sh -c a b":       {"exec", "--user", "app", "app", "sh", "-c", "a", "b"},
		"--shell --":                     {"exec", "--user", "app", "app", "sh", "-c", shellCommand},
	} {
		plan, err := build(withUser, strings.Fields(args), nil)
		if err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		if got := plan.Args[len(plan.Args)-len(want):]; !slices.Equal(got, want) {
			t.Errorf("%s: %q, want %q", args, got, want)
		}
	}

	// Map labels in the override, and two labeled services.
	write := func(p, s string) {
		if err := os.WriteFile(filepath.Join(root, ProjectDirName, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("compose.override.yaml", "services:\n  db:\n    labels:\n      devopsy.shell: \"true\"\n")
	if _, err := build(root, []string{"--shell"}, nil); err == nil || !strings.Contains(err.Error(), "several services") {
		t.Fatalf("two labeled: %v", err)
	}

	// No label: the only running service, else an error naming them.
	bare := project(t, "bare", map[string]string{"compose.yaml": "services:\n  a:\n    image: x\n  b:\n    image: y\n"})
	if _, err := build(bare, []string{"--shell"}, nil); err == nil || !strings.Contains(err.Error(), "Services: a, b") {
		t.Fatalf("no label: %v", err)
	}
	saved := RunningServices
	defer func() { RunningServices = saved }()
	RunningServices = func(*loadedProject) ([]string, error) { return []string{"b"}, nil }
	if plan, err := build(bare, []string{"--shell"}, nil); err != nil || !slices.Contains(plan.Args, "b") {
		t.Fatalf("only running: %v %q", err, plan)
	}

	// The project's shell capability replaces it, with the arguments.
	own := project(t, "own", map[string]string{"compose.yaml": compose, "capabilities/shell/open*": "#!/bin/sh\n"})
	plan, err = build(own, []string{"--shell", "db", "--", "ls"}, nil)
	open := filepath.Join(own, ProjectDirName, "capabilities", "shell", "open")
	if err != nil || plan.Path != open || !slices.Equal(plan.Args, []string{open, "db", "--", "ls"}) {
		t.Fatalf("capability: %v %+v", err, plan)
	}

	if got := Services(filepath.Join(root, ProjectDirName)); !slices.Equal(got, []string{"db", "web"}) {
		t.Fatalf("services %q", got)
	}
}
