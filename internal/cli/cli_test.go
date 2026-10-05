package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// project creates dir/<name>/.devopsy with the given files (path: content).
// Paths ending in "*" are written executable, without the star.
func project(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	for path, content := range files {
		mode := os.FileMode(0o644)
		if strings.HasSuffix(path, "*") {
			path = strings.TrimSuffix(path, "*")
			mode = 0o755
		}
		full := filepath.Join(root, ProjectDirName, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func envValue(t *testing.T, env []string, key string) (string, bool) {
	t.Helper()
	for _, kv := range env {
		if k, v, _ := strings.Cut(kv, "="); k == key {
			return v, true
		}
	}
	return "", false
}

const minimalCompose = "services:\n  web:\n    image: busybox\n"

func TestFindProjectDirFromSubdirectory(t *testing.T) {
	root := project(t, "app", map[string]string{"compose.yaml": minimalCompose})
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := FindProjectDir(sub)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, ProjectDirName); got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestFindProjectDirMissing(t *testing.T) {
	_, err := FindProjectDir(t.TempDir())
	var e *ExitError
	if !errors.As(err, &e) || e.Code != 100 {
		t.Fatalf("want ExitError 100, got %v", err)
	}
}

func TestBuildComposePassthrough(t *testing.T) {
	root := project(t, "My Proj", map[string]string{"compose.yaml": minimalCompose})
	plan, err := Build(root, []string{"config", "--services", "x y"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	compose := filepath.Join(root, ProjectDirName, "compose.yaml")
	want := []string{"docker", "compose", "-f", compose, "config", "--services", "x y"}
	if !slices.Equal(plan.Args, want) {
		t.Fatalf("args\n got %q\nwant %q", plan.Args, want)
	}
	if name, _ := envValue(t, plan.Env, "COMPOSE_PROJECT_NAME"); name != "myproj" {
		t.Fatalf("project name %q, want myproj (compose's normalization)", name)
	}
	if dir, _ := envValue(t, plan.Env, "DEVOPSY_PROJECT_DIR"); dir != filepath.Join(root, ProjectDirName) {
		t.Fatalf("DEVOPSY_PROJECT_DIR %q", dir)
	}
}

func TestBuildOverrideFile(t *testing.T) {
	root := project(t, "app", map[string]string{
		"compose.yaml":         minimalCompose,
		"compose.override.yml": "services: {}\n",
	})
	plan, err := Build(root, []string{"ps"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	override := filepath.Join(root, ProjectDirName, "compose.override.yml")
	if !slices.Contains(plan.Args, override) {
		t.Fatalf("override missing from %q", plan.Args)
	}
}

func TestBuildKeepsTopLevelName(t *testing.T) {
	root := project(t, "app", map[string]string{"compose.yaml": "name: fixed\n" + minimalCompose})
	plan, err := Build(root, []string{"ps"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := envValue(t, plan.Env, "COMPOSE_PROJECT_NAME"); ok {
		t.Fatal("COMPOSE_PROJECT_NAME set although compose.yaml has a name")
	}
}

func TestBuildDotenvCallerWins(t *testing.T) {
	root := project(t, "app", map[string]string{
		"compose.yaml": minimalCompose,
		".env":         "# comment\nFOO=from_env\nBAR=\"quoted value\"\nexport BAZ=baz\nREF=${BAR}-x\n",
	})
	plan, err := Build(root, []string{"ps"}, []string{"FOO=caller"})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"FOO": "caller",
		"BAR": "quoted value",
		"BAZ": "baz",
		"REF": "quoted value-x",
	} {
		if got, _ := envValue(t, plan.Env, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestBuildCustomCommand(t *testing.T) {
	root := project(t, "app", map[string]string{
		"compose.yaml":     minimalCompose,
		"commands/deploy*": "#!/bin/sh\n",
		"commands/notexec": "#!/bin/sh\n",
	})
	plan, err := Build(root, []string{"deploy", "a b", "c"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cmd := filepath.Join(root, ProjectDirName, "commands", "deploy")
	if plan.Path != cmd || !slices.Equal(plan.Args, []string{cmd, "a b", "c"}) {
		t.Fatalf("plan %+v", plan)
	}
	if v, _ := envValue(t, plan.Env, "DEVOPSY_CLI_COMMAND"); v != "deploy" {
		t.Fatalf("DEVOPSY_CLI_COMMAND %q", v)
	}

	// Not executable: compose.
	plan, err = Build(root, []string{"notexec"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Path != "docker" {
		t.Fatalf("non-executable file ran as a command: %+v", plan)
	}
}

func TestBuildCustomCommandRecursionGuard(t *testing.T) {
	root := project(t, "app", map[string]string{
		"compose.yaml":  minimalCompose,
		"commands/up*":  "#!/bin/sh\n",
		"commands/top*": "#!/bin/sh\n",
	})
	// Inside commands/up, `devopsy up` reaches compose...
	plan, err := Build(root, []string{"up", "-d"}, []string{"DEVOPSY_CLI_COMMAND=up"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Path != "docker" {
		t.Fatalf("recursed into the custom command: %+v", plan)
	}
	// ...while another custom command still runs.
	plan, err = Build(root, []string{"top"}, []string{"DEVOPSY_CLI_COMMAND=up"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(plan.Path, "/commands/top") {
		t.Fatalf("custom command not run: %+v", plan)
	}
}

func TestBuildHelp(t *testing.T) {
	root := project(t, "app", map[string]string{
		"compose.yaml":     minimalCompose,
		"commands/deploy*": "#!/bin/sh\n",
	})
	for args, code := range map[string]int{"": 1, "help": 0, "--help": 0} {
		var argv []string
		if args != "" {
			argv = []string{args}
		}
		_, err := Build(root, argv, nil)
		var h *Help
		if !errors.As(err, &h) || h.Code != code || !strings.Contains(h.Text, "  deploy\n") {
			t.Errorf("%q: got %v", args, err)
		}
	}
}

func TestBuildMissingCompose(t *testing.T) {
	root := project(t, "app", map[string]string{".env": "A=1\n"})
	_, err := Build(root, []string{"ps"}, nil)
	var e *ExitError
	if !errors.As(err, &e) || e.Code != 1 {
		t.Fatalf("want ExitError 1, got %v", err)
	}
}

func TestNormalizeProjectName(t *testing.T) {
	for in, want := range map[string]string{
		"My Proj":    "myproj",
		"_-app.v2":   "appv2",
		"traefik-ok": "traefik-ok",
	} {
		if got := NormalizeProjectName(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
