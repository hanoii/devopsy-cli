package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// End-to-end: build the binary and run it against a fake docker that prints
// what it received.

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "devopsy-test")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "devopsy")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func fakeBin(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	docker := "#!/bin/sh\nfor a in \"$@\"; do printf '[%s]' \"$a\"; done\necho\necho \"project=${COMPOSE_PROJECT_NAME:-unset} foo=${FOO:-unset}\"\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0o755); err != nil {
		t.Fatal(err)
	}
	// The custom commands below call devopsy recursively.
	if err := os.Symlink(binary, filepath.Join(bin, "devopsy")); err != nil {
		t.Fatal(err)
	}
	return bin
}

func runDevopsy(t *testing.T, dir string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + fakeBin(t) + ":/usr/bin:/bin", "DEVOPSY_SERVER_ENV=", "DEVOPSY_HOME=" + t.TempDir()}, env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func TestEndToEnd(t *testing.T) {
	// Resolved, as the binary sees its working directory (macOS /var symlink).
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(tmp, "My Proj")
	dot := filepath.Join(root, ".devopsy")
	write(t, filepath.Join(dot, "compose.yaml"), "services:\n  a:\n    image: busybox\n", 0o644)
	write(t, filepath.Join(dot, ".env"), "FOO=from_env\n", 0o644)
	// Calls compose's `show` through devopsy, which the guard allows.
	write(t, filepath.Join(dot, "commands", "show"),
		"#!/bin/sh\necho \"cmd=$DEVOPSY_CLI_COMMAND args=$#:$*\"\ndevopsy show\n", 0o755)
	// No shebang: run with sh.
	write(t, filepath.Join(dot, "commands", "bare"), "echo bare ran\n", 0o755)
	sub := filepath.Join(root, "sub", "dir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	out, code := runDevopsy(t, sub, nil, "config", "--services", "x y")
	if code != 0 || !strings.Contains(out, "[compose][-f]["+dot+"/compose.yaml][config][--services][x y]") ||
		!strings.Contains(out, "project=myproj foo=from_env") {
		t.Fatalf("passthrough (%d):\n%s", code, out)
	}

	out, code = runDevopsy(t, sub, []string{"FOO=caller"}, "show", "a b", "c")
	if code != 0 || !strings.Contains(out, "cmd=show args=2:a b c") ||
		!strings.Contains(out, "[show]") || !strings.Contains(out, "foo=caller") {
		t.Fatalf("custom command (%d):\n%s", code, out)
	}

	out, code = runDevopsy(t, sub, nil, "bare")
	if code != 0 || !strings.Contains(out, "bare ran") {
		t.Fatalf("no-shebang command (%d):\n%s", code, out)
	}

	out, code = runDevopsy(t, t.TempDir(), nil, "ps")
	if code != 100 || !strings.Contains(out, ".devopsy/ not found") {
		t.Fatalf("outside a project (%d):\n%s", code, out)
	}

	out, code = runDevopsy(t, t.TempDir(), nil, "--version")
	if code != 0 || !strings.Contains(out, "devopsy dev") || !strings.Contains(out, "docker compose") {
		t.Fatalf("--version outside a project (%d):\n%s", code, out)
	}

	// The word goes to compose, devopsy prints nothing of its own.
	out, code = runDevopsy(t, sub, nil, "version")
	if code != 0 || strings.Contains(out, "devopsy dev") || !strings.Contains(out, "[version]") {
		t.Fatalf("version word (%d):\n%s", code, out)
	}

	// --verbose: what devopsy decided, before the notice; nested devopsy calls
	// (the show command calls devopsy show) are verbose too.
	for _, flag := range []string{"--verbose", "-v"} {
		out, code = runDevopsy(t, sub, nil, flag, "show")
		if code != 0 || strings.Count(out, "devopsy: project "+dot) != 2 ||
			!strings.Contains(out, "devopsy: running '"+dot+"/commands/show'") {
			t.Fatalf("%s (%d):\n%s", flag, code, out)
		}
	}
	out, code = runDevopsy(t, sub, []string{"DEVOPSY_VERBOSE=true"}, "ps")
	if code != 0 || !strings.Contains(out, "devopsy: loaded "+dot+"/.env") {
		t.Fatalf("DEVOPSY_VERBOSE (%d):\n%s", code, out)
	}
	out, code = runDevopsy(t, sub, nil, "ps")
	if code != 0 || strings.Contains(out, "devopsy: project") {
		t.Fatalf("not verbose by default (%d):\n%s", code, out)
	}

	// Bare devopsy: help, anywhere, exit 0.
	out, code = runDevopsy(t, sub, nil)
	if code != 0 || !strings.Contains(out, "Project commands (") || !strings.Contains(out, "show") {
		t.Fatalf("bare devopsy in a project (%d):\n%s", code, out)
	}
	out, code = runDevopsy(t, t.TempDir(), nil)
	if code != 0 || !strings.Contains(out, "Not in a devopsy project") {
		t.Fatalf("bare devopsy outside (%d):\n%s", code, out)
	}
}

// `@target <subcommand> --help` explains and never touches the server, even
// for a target that does not exist.
func TestRemoteSubcommandHelp(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dot := filepath.Join(tmp, "app", ".devopsy")
	write(t, filepath.Join(dot, "compose.yaml"), "services: {}\n", 0o644)
	write(t, filepath.Join(dot, "targets.yaml"), "prod:\n  host: nowhere.invalid\n  path: /srv/app\n", 0o644)
	for _, sub := range []string{"release", "rollback", "releases", "domains", "--shell"} {
		out, code := runDevopsy(t, filepath.Join(tmp, "app"), nil, "@prod", sub, "--help")
		if code != 0 || !strings.Contains(out, "Usage: devopsy @<target> "+sub) {
			t.Errorf("%s --help (%d):\n%s", sub, code, out)
		}
	}
	out, code := runDevopsy(t, filepath.Join(tmp, "app"), nil, "@prod", "--help")
	if code != 0 || !strings.Contains(out, "<command> --help") {
		t.Errorf("@prod --help (%d):\n%s", code, out)
	}
}

// User-level targets refuse release and rollback before touching SSH, and
// work outside a project.
func TestUserTargets(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "targets.yaml"), "vm1-traefik:\n  host: nowhere.invalid\n  path: /srv/traefik\n", 0o644)
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(tmp, "app", ".devopsy", "compose.yaml"), "services: {}\n", 0o644)
	env := []string{"DEVOPSY_HOME=" + home}
	for _, sub := range []string{"release", "rollback"} {
		out, code := runDevopsy(t, filepath.Join(tmp, "app"), env, "@vm1-traefik", sub)
		if code == 0 || !strings.Contains(out, "is a user-level target") {
			t.Errorf("%s (%d):\n%s", sub, code, out)
		}
	}
	// Outside a project the target resolves (help needs no SSH).
	out, code := runDevopsy(t, t.TempDir(), env, "@vm1-traefik", "--help")
	if code != 0 || !strings.Contains(out, "On a server") {
		t.Errorf("outside a project (%d):\n%s", code, out)
	}
	out, code = runDevopsy(t, t.TempDir(), env, "@missing", "ps")
	if code == 0 || !strings.Contains(out, "vm1-traefik") {
		t.Errorf("missing target (%d):\n%s", code, out)
	}
}

// A target's host can come from the project's .env, read before connecting.
func TestTargetHostFromDotenv(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(tmp, "app")
	write(t, filepath.Join(project, ".devopsy", "compose.yaml"), "services: {}\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", "targets.yaml"), "prod:\n  path: /srv/app-prod\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", ".env"), "DEVOPSY_TARGET_HOST=devopsy@from-dotenv\n", 0o644)
	// echo stands in for ssh: it prints the destination.
	out, code := runDevopsy(t, project, []string{"DEVOPSY_SSH_COMMAND=echo"}, "@prod", "ps")
	if code != 0 || !strings.Contains(out, "-T devopsy@from-dotenv sh -c") {
		t.Errorf("(%d):\n%s", code, out)
	}
}

// Builds from source (version "dev" here) never replace themselves.
func TestUpgradeDevelopmentBuild(t *testing.T) {
	out, code := runDevopsy(t, t.TempDir(), nil, "--upgrade")
	if code == 0 || !strings.Contains(out, "development build") {
		t.Errorf("(%d):\n%s", code, out)
	}
	out, _ = runDevopsy(t, t.TempDir(), nil, "--help")
	if !strings.Contains(out, "--upgrade") {
		t.Errorf("help does not list --upgrade:\n%s", out)
	}
}

func TestTargetEnv(t *testing.T) {
	tg := &remote.Target{Name: "prod", Env: map[string]string{"DEVOPSY_DOMAINS": "example.org"}}
	got := string(targetEnv(tg, "app-prod", "0123abc"))
	for _, want := range []string{"COMPOSE_PROJECT_NAME='app-prod'\n", "DEVOPSY_RELEASE_COMMIT='0123abc'\n", "DEVOPSY_DOMAINS='example.org'\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Outside git there is no commit; a target's own value wins.
	if got := string(targetEnv(tg, "", "")); strings.Contains(got, "DEVOPSY_RELEASE_COMMIT") {
		t.Errorf("no commit:\n%s", got)
	}
	tg.Env["DEVOPSY_RELEASE_COMMIT"] = "pinned"
	if got := string(targetEnv(tg, "", "0123abc")); strings.Count(got, "DEVOPSY_RELEASE_COMMIT") != 1 || !strings.Contains(got, "'pinned'") {
		t.Errorf("target override:\n%s", got)
	}
}

// With --verbose, the SSH command and its script are printed, secrets from
// the project's .env masked, and devopsy on the server is verbose too.
func TestRemoteVerbose(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(tmp, "app")
	write(t, filepath.Join(project, ".devopsy", "compose.yaml"), "services: {}\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", "targets.yaml"), "prod:\n  host: devopsy@server\n  path: /srv/app-prod\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", ".env"), "DB_PASSWORD=dotenv-secret-value\n", 0o644)
	// true stands in for ssh: only devopsy's own output remains.
	out, code := runDevopsy(t, project, []string{"DEVOPSY_SSH_COMMAND=true"}, "-v", "@prod", "exec", "db", "dotenv-secret-value")
	if code != 0 || !strings.Contains(out, "devopsy: ssh -T devopsy@server, running:") ||
		!strings.Contains(out, "DEVOPSY_VERBOSE=1 COMPOSE_PROJECT_NAME=") ||
		!strings.Contains(out, "'exec' 'db' '***'") || strings.Contains(out, "dotenv-secret-value") {
		t.Errorf("(%d):\n%s", code, out)
	}
	out, code = runDevopsy(t, project, []string{"DEVOPSY_SSH_COMMAND=true"}, "@prod", "ps")
	if code != 0 || out != "" {
		t.Errorf("not verbose by default (%d):\n%s", code, out)
	}
}
