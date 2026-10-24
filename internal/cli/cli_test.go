package cli

import (
	"errors"
	"github.com/hanoii/devopsy-cli/internal/images"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/dotenv"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// project creates dir/<name>/.devopsy with the given files (path: content)
// and returns dir/<name>. Paths ending in "*" are written executable,
// without the star. A "target.env" makes it a release: the root is then
// dir/<name>/code, the file its devopsy-target.env beside it.
func project(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if env, ok := files["target.env"]; ok {
		root = filepath.Join(root, remote.CodeDir)
		write(t, releaseTargetEnv(root), env)
	}
	for path, content := range files {
		if path == "target.env" {
			continue
		}
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

// releaseTargetEnv is the target env file of the release whose code/ is
// root.
func releaseTargetEnv(root string) string {
	return filepath.Join(filepath.Dir(root), remote.TargetEnvFile)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
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
	t.Parallel()
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
	t.Parallel()
	_, err := FindProjectDir(t.TempDir())
	var e *ExitError
	if !errors.As(err, &e) || e.Code != 100 {
		t.Fatalf("want ExitError 100, got %v", err)
	}
}

func TestBuildComposePassthrough(t *testing.T) {
	t.Parallel()
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
	// ColorEnv: a terminal at the other end of the pipe (a session from a
	// terminal), so Compose colors its output; FORCE_COLOR too, unless 0,
	// and NO_COLOR wins.
	for _, c := range []struct {
		env     []string
		colored bool
	}{
		{[]string{ColorEnv + "=1"}, true},
		{[]string{"FORCE_COLOR=1"}, true},
		{[]string{"FORCE_COLOR=0"}, false},
		{[]string{"FORCE_COLOR=false"}, false},
		{[]string{"FORCE_COLOR=1", "NO_COLOR=1"}, false},
	} {
		plan, err = build(root, []string{"ps"}, c.env)
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"docker", "compose", "-f", compose, "ps"}
		if c.colored {
			want = []string{"docker", "compose", "--ansi", "always", "-f", compose, "ps"}
		}
		if !slices.Equal(plan.Args, want) {
			t.Fatalf("%q: args\n got %q\nwant %q", c.env, plan.Args, want)
		}
	}
}

// A command that starts containers reads labels (Plan.Starts) only when
// a compose file's YAML may carry devopsy.mnt.owner: the label, or a
// file it includes or extends; never a comment that says so
// (cmd/devopsy's TestMntOwnersLocal runs one). Only the project's own
// files, the ones the labels are read from: a -f the command adds is
// in neither. Never for --dry-run, and never in a release on a server,
// where --mnt-owners owns that (review of 2026-10-23, round 2: a
// rollback's up failed on labels this devopsy could not read).
func TestBuildStarts(t *testing.T) {
	t.Parallel()
	owned := minimalCompose + "    labels: [\"devopsy.mnt.owner=1:1\"]\n"
	root := project(t, "p", map[string]string{"compose.yaml": minimalCompose, "extra.yaml": owned})
	inRelease := project(t, "p", map[string]string{"compose.yaml": owned, "target.env": "COMPOSE_PROJECT_NAME=p-prod\n"})
	ownedRoot := project(t, "p", map[string]string{"compose.yaml": owned})
	for _, c := range []struct {
		root, args string
		starts     bool
	}{
		{root, "-f " + filepath.Join(root, ProjectDirName, "extra.yaml") + " up -d", false},
		{inRelease, "up -d", false},
		{ownedRoot, "--dry-run up -d", false},
		{ownedRoot, "up --dry-run -d", false},
		{ownedRoot, "up --dry-run=false -d", true},
		{ownedRoot, "run --rm a -- sh --dry-run", true},
		// Past run's service, everything is the container's command
		// (compose stops parsing its own flags there, -- or not):
		// `docker compose run --rm a echo ran --dry-run` prints "ran
		// --dry-run" (checked with Compose on Docker 29.8), so the
		// container starts and Docker makes a missing entry root's
		// (review of 2026-10-23, round 4: dryRun took it for compose's).
		{ownedRoot, "run --rm a composer update --dry-run", true},
		{ownedRoot, "--profile x run --rm a sh --dry-run", true},
		// Before the service it is compose's, and so is it anywhere
		// after the other commands, up to a -- (each checked with Compose
		// 5.5, by the containers left); a flag's value is never one, and
		// the last one says, as compose parses a bool flag.
		{ownedRoot, "run --rm --dry-run a sh", false},
		{ownedRoot, "run --rm -e X --dry-run a sh", false},
		{ownedRoot, "run --rm --name --dry-run a sh", true},
		{ownedRoot, "run --rm --entrypoint=sh -u 1 --dry-run=true a", false},
		{ownedRoot, "up -d a --dry-run", false},
		{ownedRoot, "up -d a -- --dry-run", true},
		{ownedRoot, "up --exit-code-from --dry-run", true},
		{ownedRoot, "up --dry-run=0 -d", true},
		{ownedRoot, "up --dry-run=T -d", false},
		{ownedRoot, "--dry-run up --dry-run=false", true},
		{ownedRoot, "restart a --dry-run", false},
		// restart starts containers too, and Docker makes a missing bind
		// source then, as root (checked with Compose on Docker 29: an
		// entry removed while the service was stopped came back on
		// restart; review of 2026-10-23, round 3).
		{ownedRoot, "restart", true},
	} {
		plan, err := build(c.root, strings.Fields(c.args), nil)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Starts != c.starts {
			t.Errorf("%s in %s: Starts %v", c.args, c.root, plan.Starts)
		}
	}

	// COMPOSE_FILE, in .devopsy/.env or the caller's environment, names
	// files compose never reads: every command names the project's with
	// -f (composeFiles), which compose prefers (checked with Compose 5.5:
	// `-f a.yaml config` with COMPOSE_FILE=a.yaml:b.yaml leaves b.yaml
	// out), ComposeConfig's included. So its labels are none, and
	// mayOwnMnt reads exactly the files the command and the labels'
	// reader run on (review of 2026-10-23, round 4, asked whether it
	// missed COMPOSE_FILE's).
	viaEnv := project(t, "p", map[string]string{"compose.yaml": minimalCompose, "extra.yaml": owned, "compose.override.yaml": minimalCompose, ".env": "COMPOSE_FILE=compose.yaml:extra.yaml\n"})
	plan, err := build(viaEnv, []string{"up", "-d"}, []string{"COMPOSE_FILE=compose.yaml:extra.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(viaEnv, ProjectDirName)
	if want := []string{"docker", "compose", "-f", filepath.Join(dir, "compose.yaml"), "-f", filepath.Join(dir, "compose.override.yaml"), "up", "-d"}; plan.Starts || !slices.Equal(plan.Args, want) || !slices.Equal(composeFiles(dir), []string{want[3], want[5]}) {
		t.Errorf("COMPOSE_FILE: Starts %v, args %q, composeFiles %q", plan.Starts, plan.Args, composeFiles(dir))
	}

	for compose, want := range map[string]bool{
		minimalCompose: false,
		"include:\n  - other.yaml\n" + minimalCompose:                                      true,
		minimalCompose + "    extends: {file: base.yaml, service: app}\n":                  true,
		minimalCompose + "    labels:\n      devopsy.mnt.owner: 1:1\n":                     true,
		minimalCompose + "    labels: [\"devopsy.mnt.owner=1:1\"]\n":                       true,
		"# include: and extends: in a comment\n" + minimalCompose:                          false,
		minimalCompose + "    image: x # devopsy.mnt.owner, included\n":                    false,
		minimalCompose + "    environment: {INCLUDED: extends}\n    # devopsy.mnt.owner\n": false,
	} {
		root := project(t, "p", map[string]string{"compose.yaml": compose})
		for args, starts := range map[string]bool{"up -d": want, "--profile x run --rm a": want, "ps": false} {
			plan, err := build(root, strings.Fields(args), nil)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Starts != starts {
				t.Errorf("%s with\n%s: Starts %v", args, compose, plan.Starts)
			}
		}
	}
}

// A script in the arguments is only counted in the notice; -vv has it whole.
func TestBuildNoticeScript(t *testing.T) {
	t.Parallel()
	root := project(t, "app", map[string]string{"compose.yaml": minimalCompose, "config.yaml": "secrets: [API_TOKEN]\n"})
	script := "\n  echo one\n  SECRETVALUE123 two\n"
	plan, err := build(root, []string{"run", "--rm", "web", "sh", "-c", script, "sh", "x"}, []string{"API_TOKEN=SECRETVALUE123"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Args[len(plan.Args)-3] != script {
		t.Fatalf("args changed: %q", plan.Args)
	}
	if !strings.HasSuffix(plan.Notice, "run --rm web sh -c <script: 2 lines> sh x'") {
		t.Fatalf("notice %s", plan.Notice)
	}
	if !strings.Contains(plan.NoticeFull, "echo one\n  *** two") {
		t.Fatalf("full notice %q", plan.NoticeFull)
	}
	plan, err = build(root, []string{"ps"}, nil)
	if err != nil || plan.NoticeFull != "" || !strings.HasSuffix(plan.Notice, " ps'") {
		t.Fatalf("plain notice %q, full %q (%v)", plan.Notice, plan.NoticeFull, err)
	}
}

func TestBuildOverrideFile(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	if v, _ := envValue(t, plan.Env, "DEVOPSY_COMMAND_NAME"); v != "deploy" {
		t.Fatalf("DEVOPSY_COMMAND_NAME %q", v)
	}

	// Not executable: not a command, and not compose's either.
	if _, err = build(root, []string{"notexec"}, nil); err == nil || !strings.Contains(err.Error(), "notexec: not a project command (deploy)") {
		t.Fatalf("non-executable file: %v", err)
	}
}

// ## Run: and ## Needs: say where a command runs: local never in a
// release, target only there, and a local one that needs an address gets
// it from the devopsy that read it, in DEVOPSY_COMMAND_TARGET.
func TestBuildCommandRun(t *testing.T) {
	t.Parallel()
	root := project(t, remote.CodeDir, map[string]string{
		"compose.yaml":     minimalCompose,
		"commands/image*":  "#!/bin/sh\n## Run: local\n",
		"commands/deploy*": "#!/bin/sh\n## Run: Target\n",
		"commands/both*":   "#!/bin/sh\n",
		"commands/push*":   "#!/bin/sh\n## Run: local\n## Needs: target\n",
		"commands/tunnel*": "#!/bin/sh\n## Run: local\n## Needs: server\n",
		"commands/typo*":   "#!/bin/sh\n## Run: server\n",
		"commands/odd*":    "#!/bin/sh\n## Needs: server\n",
	})
	release := releaseTargetEnv(root)
	addressed := []string{commandTargetInternal + "=vm1:prod", CommandTargetEnv + "=stale"}
	for _, c := range []struct {
		release bool
		env     []string
		cmd     string
		err     string
		target  string
	}{
		{false, nil, "image", "", ""},
		{false, nil, "both", "", ""},
		{false, nil, "deploy", "deploy runs only on a target (## Run: target in ", ""},
		{false, nil, "push", "push needs a target (## Needs: target in ", ""},
		{false, nil, "tunnel", "tunnel needs a server (## Needs: server in ", ""},
		{false, nil, "typo", "## Run: server is not local or target", ""},
		{false, nil, "odd", "## Needs: server takes ## Run: local", ""},
		{false, addressed, "push", "", "vm1:prod"},
		{false, addressed, "tunnel", "", "vm1:prod"},
		{false, addressed, "image", "image runs only on your machine and takes no address", ""},
		// A command without Needs never sees an outer one's address.
		{false, []string{CommandTargetEnv + "=stale"}, "both", "", ""},
		{true, nil, "deploy", "", ""},
		{true, nil, "both", "", ""},
		{true, nil, "image", "image runs only on your machine (## Run: local in ", ""},
	} {
		if c.release {
			if err := os.WriteFile(release, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		} else {
			os.Remove(release)
		}
		plan, err := build(root, []string{c.cmd}, c.env)
		if (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s (release %v): %v, want %q", c.cmd, c.release, err, c.err)
			continue
		}
		if err != nil {
			continue
		}
		got, _ := envValue(t, plan.Env, CommandTargetEnv)
		if got != c.target {
			t.Errorf("%s: %s %q, want %q", c.cmd, CommandTargetEnv, got, c.target)
		}
		if _, ok := envValue(t, plan.Env, commandTargetInternal); ok {
			t.Errorf("%s: %s reached the command", c.cmd, commandTargetInternal)
		}
	}
}

func TestBuildCustomCommandRecursionGuard(t *testing.T) {
	t.Parallel()
	root := project(t, "app", map[string]string{
		"compose.yaml":  minimalCompose,
		"commands/up*":  "#!/bin/sh\n",
		"commands/top*": "#!/bin/sh\n",
	})
	// Inside commands/up, `devopsy up` reaches compose...
	plan, err := build(root, []string{"up", "-d"}, []string{"DEVOPSY_COMMAND_NAME=up"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Path != "docker" {
		t.Fatalf("recursed into the custom command: %+v", plan)
	}
	// ...while another custom command still runs.
	plan, err = build(root, []string{"top"}, []string{"DEVOPSY_COMMAND_NAME=up"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(plan.Path, "/commands/top") {
		t.Fatalf("custom command not run: %+v", plan)
	}
}

func TestUsage(t *testing.T) {
	t.Parallel()
	root := project(t, "app", map[string]string{
		"compose.yaml":     minimalCompose,
		"commands/deploy*": "#!/bin/sh\n## Description: Pull and roll out\n",
		"commands/plain*":  "#!/bin/sh\n",
		"commands/image*":  "#!/bin/sh\n## Run: local\n",
		"commands/go*":     "#!/bin/sh\n## Run: target\n",
		"commands/push*":   "#!/bin/sh\n## Description: Push it\n## Run: local\n## Needs: server\n",
	})
	text := Usage(filepath.Join(root, ProjectDirName))
	for _, want := range []string{"Where each runs:", "Project commands (", "  [@t] deploy", "Pull and roll out", "  [@t] plain\n", "       image\n", "    @t go\n", "    @s push (runs locally)          Push it", "anything else is an error", "Releases:", "    @t --release [--from @<target>] [--accept-image <svc>] [--yes]", "  [@t] --shell [service]", "    @s --shell-host", "       --ssh-keys-generate", "       --version", "Setup and tools:"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	// Project commands, then devopsy's flags.
	if strings.Index(text, "Project commands (") > strings.Index(text, "Releases:") {
		t.Errorf("project commands come first:\n%s", text)
	}
	// Every line fits a terminal.
	for _, l := range strings.Split(text, "\n") {
		if len(l) > 80 && !strings.Contains(l, "Project commands (") {
			t.Errorf("over 80 columns: %q", l)
		}
	}
	if !strings.Contains(Usage(""), "Not in a devopsy project") {
		t.Error("outside a project")
	}
}

func TestBuildWordsAreNotBuiltins(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	root := project(t, "app", map[string]string{".env": "A=1\n"})
	_, err := build(root, []string{"ps"}, nil)
	var e *ExitError
	if !errors.As(err, &e) || e.Code != 1 {
		t.Fatalf("want ExitError 1, got %v", err)
	}
}

func TestNormalizeProjectName(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	root := project(t, "shop", map[string]string{
		"compose.yaml": minimalCompose,
		".env":         "SITE_URL=https://override.example.org\n",
	})
	compute := filepath.Join(root, ".devopsy", "capabilities", "env", "compute")
	if err := os.MkdirAll(filepath.Dir(compute), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"[ -z \"$DEVOPSY_INTERNAL_ENV_COMPUTE\" ] && exit 9\n" +
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
	if _, ok := envValue(t, plan.Env, "DEVOPSY_INTERNAL_ENV_COMPUTE"); ok {
		t.Error("the guard leaked into the command's environment")
	}
	_, err = build(root, []string{"--env"}, nil)
	if o, ok := err.(*Output); !ok || !strings.Contains(o.Text, "SITE_HOST='shop.localhost'") {
		t.Errorf("--env: %v", err)
	}
	// Inside the capability, devopsy does not run it again.
	plan, err = build(root, []string{"ps"}, []string{"DEVOPSY_INTERNAL_ENV_COMPUTE=1"})
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// Secrets never reach the notice or the verbose lines: the values of the
// variables config.yaml's secrets: names, wherever they are set. Nothing
// else is masked.
func TestBuildMasksSecrets(t *testing.T) {
	t.Parallel()
	root := project(t, "app", map[string]string{
		"compose.yaml":     minimalCompose,
		"config.yaml":      "secrets: [DB_PASSWORD, OVERRIDDEN, \"*_TOKEN\", SHORT]\n",
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
	t.Parallel()
	root := project(t, "app", map[string]string{"compose.yaml": minimalCompose, ".env": "A=1\n"})
	plan, err := build(root, []string{"ps"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ProjectDirName)
	want := []string{"project " + dir, "loaded " + filepath.Join(dir, ".env")}
	if !slices.Equal(plan.Verbose, want) {
		t.Fatalf("verbose\n got %q\nwant %q", plan.Verbose, want)
	}
}

func TestVerboseLevel(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]int{"1": 1, "true": 1, "TRUE": 1, "yes": 1, "on": 1, "2": 2, "3": 2, "": 0, "0": 0, "false": 0, "no": 0, "-1": 0} {
		if got := VerboseLevel(v); got != want {
			t.Errorf("VerboseLevel(%q) = %d", v, got)
		}
	}
}

// DotenvLine writes a line compose's parser reads back as the value it
// was given: --vars set, inherit and devopsy-target.env write with it,
// and every devopsy command on the server reads those files.
//
// Bug: a value without a quote or a newline is written single-quoted as
// it is, and compose's parser takes a backslash before the closing quote
// for an escaped quote: K='x\' is "unterminated quoted value", so a
// password ending in a backslash, set with --vars set, breaks the
// environment's shared/.env for every command (--vars included).
func TestDotenvLineRoundTrip(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"", "a b", " lead", "trail ", "a #b", "$HOME", `a\nb`, "it's", "a\nb", `"`, "'", `x\`, `\`, `x\\`} {
		line := DotenvLine("K", v)
		got, err := dotenv.UnmarshalWithLookup(line+"\n", func(string) (string, bool) { return "looked up", true })
		if err != nil || got["K"] != v {
			t.Errorf("%q: %s reads back as %q (%v)", v, line, got["K"], err)
		}
	}
}

// In a release, devopsy's own compose file comes between the project's
// compose.yaml and the operator's override, so the operator's hand still
// wins; a listing asks for the project's files alone.
func TestReleaseComposeFile(t *testing.T) {
	t.Parallel()
	rel := filepath.Join(t.TempDir(), "releases", "1")
	dir := filepath.Join(rel, "code", ".devopsy")
	write(t, filepath.Join(dir, "compose.yaml"), "services: {}\n")
	write(t, filepath.Join(dir, "compose.override.yaml"), "services: {}\n")
	write(t, filepath.Join(rel, "devopsy-target.env"), "")
	write(t, filepath.Join(rel, images.ComposeFile), "services: {}\n")
	files := func(environ ...string) []string {
		t.Helper()
		p, err := Compose(filepath.Join(rel, "code"), []string{"ps"}, environ)
		if err != nil {
			t.Fatal(err)
		}
		var fs []string
		for i, a := range p.Args {
			if a == "-f" {
				fs = append(fs, filepath.Base(p.Args[i+1]))
			}
		}
		return fs
	}
	if got := strings.Join(files(), " "); got != "compose.yaml "+images.ComposeFile+" compose.override.yaml" {
		t.Errorf("files: %s", got)
	}
	if got := strings.Join(files(images.NoReleaseComposeEnv+"=1"), " "); got != "compose.yaml compose.override.yaml" {
		t.Errorf("files for a listing: %s", got)
	}
}
