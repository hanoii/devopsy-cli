package cli

import (
	"errors"

	"github.com/compose-spec/compose-go/v2/dotenv"
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

// build is Build without the machine's real server settings file.
func build(cwd string, args []string, environ []string) (*Plan, error) {
	return Build(cwd, args, append([]string{"DEVOPSY_SERVER_ENV="}, environ...))
}

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
	plan, err := build(root, []string{"config", "--services", "x y"}, nil)
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
	plan, err := build(root, []string{"ps"}, nil)
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
	plan, err := build(root, []string{"ps"}, nil)
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
	plan, err := build(root, []string{"ps"}, []string{"FOO=caller"})
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
	plan, err := build(root, []string{"deploy", "a b", "c"}, nil)
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
	plan, err = build(root, []string{"notexec"}, nil)
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
	plan, err := build(root, []string{"up", "-d"}, []string{"DEVOPSY_CLI_COMMAND=up"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Path != "docker" {
		t.Fatalf("recursed into the custom command: %+v", plan)
	}
	// ...while another custom command still runs.
	plan, err = build(root, []string{"top"}, []string{"DEVOPSY_CLI_COMMAND=up"})
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
		_, err := build(root, argv, nil)
		var h *Help
		if !errors.As(err, &h) || h.Code != code || !strings.Contains(h.Text, "  deploy\n") {
			t.Errorf("%q: got %v", args, err)
		}
	}
}

func TestBuildMissingCompose(t *testing.T) {
	root := project(t, "app", map[string]string{".env": "A=1\n"})
	_, err := build(root, []string{"ps"}, nil)
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

func TestBuildPublicHost(t *testing.T) {
	root := project(t, "My App", map[string]string{"compose.yaml": minimalCompose})
	server := filepath.Join(t.TempDir(), "devopsy.env")
	if err := os.WriteFile(server, []byte("DEVOPSY_PUBLIC_DOMAIN=vm1.example.com\nFOO=server\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	plan, err := Build(root, []string{"ps"}, []string{"DEVOPSY_SERVER_ENV=" + server})
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"DEVOPSY_PROJECT_NAME": "myapp",
		"DEVOPSY_PUBLIC_HOST":  "myapp.vm1.example.com",
		"FOO":                  "server",
	} {
		if got, _ := envValue(t, plan.Env, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	// Without a server domain: <name>.localhost.
	plan, err = build(root, []string{"ps"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := envValue(t, plan.Env, "DEVOPSY_PUBLIC_HOST"); got != "myapp.localhost" {
		t.Errorf("local host %q", got)
	}

	// The project's .env and the caller win over the server file.
	root2 := project(t, "app", map[string]string{
		"compose.yaml": minimalCompose,
		".env":         "FOO=project\n",
	})
	plan, err = Build(root2, []string{"ps"}, []string{"DEVOPSY_SERVER_ENV=" + server, "DEVOPSY_PUBLIC_HOST=custom.example.org"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := envValue(t, plan.Env, "FOO"); got != "project" {
		t.Errorf("FOO = %q, want project", got)
	}
	if got, _ := envValue(t, plan.Env, "DEVOPSY_PUBLIC_HOST"); got != "custom.example.org" {
		t.Errorf("caller's DEVOPSY_PUBLIC_HOST not kept: %q", got)
	}
}

func TestBuildProjectNameFromCompose(t *testing.T) {
	root := project(t, "app", map[string]string{
		"compose.yaml": "name: shop-${STAGE:-dev}\n" + minimalCompose,
		".env":         "STAGE=prod\n",
	})
	plan, err := build(root, []string{"ps"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := envValue(t, plan.Env, "DEVOPSY_PROJECT_NAME"); got != "shop-prod" {
		t.Errorf("DEVOPSY_PROJECT_NAME = %q, want shop-prod", got)
	}
	if got, _ := envValue(t, plan.Env, "DEVOPSY_PUBLIC_HOST"); got != "shop-prod.localhost" {
		t.Errorf("DEVOPSY_PUBLIC_HOST = %q", got)
	}
}

func TestHostRule(t *testing.T) {
	got, err := HostRule([]string{"shop.vm1.example.com", "Example.org", "", "example.org", "*.shop.example.org"})
	if err != nil {
		t.Fatal(err)
	}
	want := "Host(`shop.vm1.example.com`) || Host(`example.org`) || Host(`*.shop.example.org`)"
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	for _, bad := range []string{"a`b.com", "with space.com", "-bad.com", "a..b"} {
		if _, err := HostRule([]string{bad}); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestBuildHostRule(t *testing.T) {
	root := project(t, "shop", map[string]string{
		"compose.yaml": minimalCompose,
		".env":         "DEVOPSY_DOMAINS=\"example.org, www.example.org\"\n",
	})
	plan, err := build(root, []string{"ps"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "Host(`shop.localhost`) || Host(`example.org`) || Host(`www.example.org`)"
	if got, _ := envValue(t, plan.Env, "DEVOPSY_HOST_RULE"); got != want {
		t.Fatalf("got %s", got)
	}

	root = project(t, "shop", map[string]string{
		"compose.yaml": minimalCompose,
		".env":         "DEVOPSY_DOMAINS=bad`host\n",
	})
	var e *ExitError
	if _, err := build(root, []string{"ps"}, nil); !errors.As(err, &e) {
		t.Fatalf("invalid domain: %v", err)
	}
}

func TestPrintEnvRoundTrip(t *testing.T) {
	root := project(t, "shop", map[string]string{
		"compose.yaml": minimalCompose,
		".env":         "PLAIN=a b\nTRICKY=placeholder\nDEVOPSY_DOMAINS=example.org\n",
	})
	// The caller's value wins and is printed, quotes, $ and backslash included.
	_, err := build(root, []string{"print-env"}, []string{"CALLER_ONLY=1", "PLAIN=caller", `TRICKY=it's "x" $HOME \ end`})
	var out *Output
	if !errors.As(err, &out) {
		t.Fatalf("want Output, got %v", err)
	}
	if strings.Contains(out.Text, "CALLER_ONLY") {
		t.Errorf("caller-only variable printed:\n%s", out.Text)
	}
	parsed, err := dotenv.UnmarshalWithLookup(out.Text, nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.Text)
	}
	for k, want := range map[string]string{
		"PLAIN":                "caller",
		"TRICKY":               `it's "x" $HOME \ end`,
		"COMPOSE_PROJECT_NAME": "shop",
		"DEVOPSY_PUBLIC_HOST":  "shop.localhost",
		"DEVOPSY_HOST_RULE":    "Host(`shop.localhost`) || Host(`example.org`)",
	} {
		if parsed[k] != want {
			t.Errorf("%s = %q, want %q\n%s", k, parsed[k], want, out.Text)
		}
	}
}

func TestBuildTargetEnvPrecedence(t *testing.T) {
	root := project(t, "shop", map[string]string{
		"compose.yaml": minimalCompose,
		".env":         "OVERRIDE=from_env\n",
		"target.env":   "DEVOPSY_DOMAINS='example.org'\nOVERRIDE='from_target'\nFROM_TARGET='yes'\n",
	})
	server := filepath.Join(t.TempDir(), "devopsy.env")
	if err := os.WriteFile(server, []byte("FROM_TARGET=server\nDEVOPSY_PUBLIC_DOMAIN=vm1.example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := Build(root, []string{"ps"}, []string{"DEVOPSY_SERVER_ENV=" + server})
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"OVERRIDE":          "from_env",
		"FROM_TARGET":       "yes",
		"DEVOPSY_HOST_RULE": "Host(`shop.vm1.example.com`) || Host(`example.org`)",
	} {
		if got, _ := envValue(t, plan.Env, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}
