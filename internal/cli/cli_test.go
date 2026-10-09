package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/dotenv"
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

// build is Build, as the tests call it.
func build(cwd string, args []string, environ []string) (*Plan, error) {
	return Build(cwd, args, environ)
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

	// Not executable: not a command, and not compose's either.
	if _, err = build(root, []string{"notexec"}, nil); err == nil || !strings.Contains(err.Error(), "notexec: not a project command (deploy)") {
		t.Fatalf("non-executable file: %v", err)
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

func TestUsage(t *testing.T) {
	root := project(t, "app", map[string]string{
		"compose.yaml":     minimalCompose,
		"commands/deploy*": "#!/bin/sh\n## Description: Pull and roll out\n",
		"commands/plain*":  "#!/bin/sh\n",
	})
	text := Usage(filepath.Join(root, ProjectDirName))
	for _, want := range []string{"Built-in:", "--version", "--env", "On a server", "Project commands (", "deploy  Pull and roll out", "  plain", "Anything else is an error", "--shell [service]", "--shell-host", "--release"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if !strings.Contains(Usage(""), "Not in a devopsy project") {
		t.Error("outside a project")
	}
}

func TestBuildWordsAreNotBuiltins(t *testing.T) {
	root := project(t, "app", map[string]string{"compose.yaml": minimalCompose})
	// version and help are compose commands now; built-ins are flags.
	for _, word := range []string{"version", "help"} {
		plan, err := build(root, []string{word}, nil)
		if err != nil || plan.Path != "docker" || plan.Args[len(plan.Args)-1] != word {
			t.Errorf("%s: %+v %v", word, plan, err)
		}
	}
	for _, flag := range []string{"--help", "-h"} {
		var h *Help
		if _, err := build(root, []string{flag}, nil); !errors.As(err, &h) || h.Code != 0 {
			t.Errorf("%s: %v", flag, err)
		}
	}
	for _, env := range []string{"--env"} {
		var out *Output
		if _, err := build(root, []string{env}, nil); !errors.As(err, &out) || !strings.Contains(out.Text, "COMPOSE_PROJECT_NAME='app'") {
			t.Errorf("%s: %v", env, err)
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

func TestBuildProjectName(t *testing.T) {
	cases := []struct {
		files   map[string]string
		environ []string
		want    string
	}{
		// The directory containing .devopsy, as compose would for a
		// compose.yaml at the root.
		{map[string]string{"compose.yaml": minimalCompose}, nil, "myapp"},
		// compose's name:, interpolated.
		{map[string]string{"compose.yaml": "name: shop-${STAGE:-dev}\n" + minimalCompose, ".env": "STAGE=prod\n"}, nil, "shop-prod"},
		// The config's project, over compose's name:.
		{map[string]string{"compose.yaml": "name: other\n" + minimalCompose, "config.yaml": "project: shop\n"}, nil, "shop"},
		// A release's target.env, over the config's project.
		{map[string]string{"compose.yaml": minimalCompose, "config.yaml": "project: shop\n", "target.env": "COMPOSE_PROJECT_NAME='shop-prod'\n"}, nil, "shop-prod"},
		// The caller wins.
		{map[string]string{"compose.yaml": minimalCompose, "config.yaml": "project: shop\n"}, []string{"COMPOSE_PROJECT_NAME=mine"}, "mine"},
	}
	for i, c := range cases {
		root := project(t, "My App", c.files)
		plan, err := build(root, []string{"ps"}, c.environ)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := envValue(t, plan.Env, "COMPOSE_PROJECT_NAME"); got != c.want {
			t.Errorf("%d: COMPOSE_PROJECT_NAME = %q, want %q", i, got, c.want)
		}
	}
}

func TestEnvCapability(t *testing.T) {
	root := project(t, "shop", map[string]string{
		"compose.yaml": minimalCompose,
		".env":         "SITE_URL=https://override.example.org\n",
	})
	compute := filepath.Join(root, ".devopsy", "capabilities", "env", "compute")
	if err := os.MkdirAll(filepath.Dir(compute), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"[ -z \"$DEVOPSY_ENV_COMPUTE\" ] && exit 9\n" +
		"echo \"SITE_HOST='$COMPOSE_PROJECT_NAME.localhost'\"\n" +
		"echo \"SITE_URL='https://$COMPOSE_PROJECT_NAME.localhost'\"\n"
	if err := os.WriteFile(compute, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	plan, err := build(root, []string{"ps"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := envValue(t, plan.Env, "SITE_HOST"); got != "shop.localhost" {
		t.Errorf("SITE_HOST = %q", got)
	}
	// What is set already wins.
	if got, _ := envValue(t, plan.Env, "SITE_URL"); got != "https://override.example.org" {
		t.Errorf("SITE_URL = %q", got)
	}
	if _, ok := envValue(t, plan.Env, "DEVOPSY_ENV_COMPUTE"); ok {
		t.Error("the guard leaked into the command's environment")
	}
	_, err = build(root, []string{"--env"}, nil)
	if o, ok := err.(*Output); !ok || !strings.Contains(o.Text, "SITE_HOST='shop.localhost'") {
		t.Errorf("--env: %v", err)
	}
	// Inside the capability, devopsy does not run it again.
	plan, err = build(root, []string{"ps"}, []string{"DEVOPSY_ENV_COMPUTE=1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := envValue(t, plan.Env, "SITE_HOST"); ok {
		t.Error("ran inside itself")
	}
	// A failure names the script and says what it printed.
	if err := os.WriteFile(compute, []byte("#!/bin/sh\necho broken >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var e *ExitError
	if _, err := build(root, []string{"ps"}, nil); !errors.As(err, &e) || !strings.Contains(e.Msg, compute) || !strings.Contains(e.Msg, "broken") {
		t.Errorf("failure: %v", err)
	}
	// Help needs no environment.
	if _, err := build(root, nil, nil); err == nil {
		t.Error("no help")
	} else if _, ok := err.(*Help); !ok {
		t.Errorf("help: %v", err)
	}
}

func TestPrintEnvRoundTrip(t *testing.T) {
	root := project(t, "shop", map[string]string{
		"compose.yaml": minimalCompose,
		".env":         "PLAIN=a b\nTRICKY=placeholder\nDEVOPSY_DOMAINS=example.org\n",
	})
	// The caller's value wins and is printed, quotes, $ and backslash included.
	_, err := build(root, []string{"--env"}, []string{"CALLER_ONLY=1", "PLAIN=caller", `TRICKY=it's "x" $HOME \ end`})
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
		"DEVOPSY_DOMAINS":      "example.org",
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
		"target.env":   "DEVOPSY_DOMAINS='example.org'\nOVERRIDE='from_target'\nFROM_TARGET='yes'\nDEVOPSY_WILDCARD_DOMAIN='vm1.example.com'\n",
	})
	plan, err := build(root, []string{"ps"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"OVERRIDE":    "from_env",
		"FROM_TARGET": "yes",
	} {
		if got, _ := envValue(t, plan.Env, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestBuildCapability(t *testing.T) {
	root := project(t, "traefik", map[string]string{
		"compose.yaml":                 minimalCompose,
		".env":                         "API_TOKEN=s3cr3t-value\n",
		"capabilities/domains/facts":   "#!/bin/sh\n",
		"capabilities/domains/notexec": "#!/bin/sh\n",
	})
	if err := os.Chmod(filepath.Join(root, ".devopsy", "capabilities", "domains", "facts"), 0o755); err != nil {
		t.Fatal(err)
	}
	plan, err := build(root, []string{"--capability", "domains", "facts", "--all"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, ".devopsy", "capabilities", "domains", "facts")
	if plan.Path != want || strings.Join(plan.Args, " ") != want+" --all" {
		t.Fatalf("plan %s %v", plan.Path, plan.Args)
	}
	if got, _ := envValue(t, plan.Env, "API_TOKEN"); got != "s3cr3t-value" {
		t.Errorf("project env not loaded: %q", got)
	}
	for args, wantErr := range map[string]string{
		"--capability":                 "usage",
		"--capability domains":         "usage",
		"--capability domains retry":   "does not provide the domains capability's retry action",
		"--capability domains notexec": "does not provide",
		"--capability ../x facts":      "not a capability",
		"--capability domains ../x":    "not a capability",
	} {
		_, err := build(root, strings.Fields(args), nil)
		if e, ok := err.(*ExitError); !ok || !strings.Contains(e.Msg, wantErr) {
			t.Errorf("%s: %v, want %q", args, err, wantErr)
		}
	}
}

// Secrets never reach the notice or the verbose lines: values from .env,
// even when the caller sets them, and variables named like secrets.
func TestBuildMasksSecrets(t *testing.T) {
	root := project(t, "app", map[string]string{
		"compose.yaml":     minimalCompose,
		".env":             "DB_PASSWORD=from-dotenv-secret\nOVERRIDDEN=dotenv-value-x\nSHORT=abc\n",
		"commands/deploy*": "#!/bin/sh\n",
	})
	caller := []string{"OVERRIDDEN=caller-value-xyz", "API_TOKEN=caller-token-123", "PLAIN=caller-plain-value"}
	args := []string{"exec", "db", "mariadb", "-pfrom-dotenv-secret", "caller-value-xyz", "caller-token-123", "abc", "caller-plain-value"}
	plan, err := build(root, args, caller)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"from-dotenv-secret", "caller-value-xyz", "caller-token-123"} {
		if strings.Contains(plan.Notice, secret) {
			t.Fatalf("notice shows %q: %s", secret, plan.Notice)
		}
	}
	// Short values and ordinary variables stay readable; the arguments
	// themselves are untouched.
	if !strings.Contains(plan.Notice, "-p*** *** *** abc caller-plain-value'") {
		t.Fatalf("notice %s", plan.Notice)
	}
	if !slices.Contains(plan.Args, "-pfrom-dotenv-secret") {
		t.Fatalf("args were masked: %q", plan.Args)
	}

	plan, err = build(root, []string{"deploy", "from-dotenv-secret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	last := plan.Verbose[len(plan.Verbose)-1]
	if !strings.HasSuffix(last, "commands/deploy ***'") {
		t.Fatalf("verbose %q", plan.Verbose)
	}
}

func TestBuildVerbose(t *testing.T) {
	root := project(t, "app", map[string]string{"compose.yaml": minimalCompose, ".env": "A=1\n"})
	plan, err := build(root, []string{"ps"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ProjectDirName)
	want := []string{"devopsy: project " + dir, "devopsy: loaded " + filepath.Join(dir, ".env")}
	if !slices.Equal(plan.Verbose, want) {
		t.Fatalf("verbose\n got %q\nwant %q", plan.Verbose, want)
	}
}

func TestIsVerbose(t *testing.T) {
	for v, want := range map[string]bool{"1": true, "true": true, "TRUE": true, "yes": true, "on": true, "": false, "0": false, "false": false, "no": false} {
		if got := IsVerbose(v); got != want {
			t.Errorf("IsVerbose(%q) = %v", v, got)
		}
	}
}
