package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/cli"
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
	docker := "#!/bin/sh\ncase \"$*\" in *--help*) echo 'Usage:  docker compose ps [OPTIONS] [SERVICE...]'; exit 0 ;; *' config --images') echo x; exit 0 ;; *' config --format json') echo '{\"services\":{}}'; exit 0 ;; 'image inspect --format '*) echo 'No such image' >&2; exit 1 ;;\n  *' ps -a --format {{.Name}} '*) [ -z \"${FAKE_PS:-}\" ] || { printf \"$FAKE_PS\"; exit 0; } ;;\n  'inspect -f '*) [ -z \"${FAKE_HEALTH:-}\" ] || { echo \"$FAKE_HEALTH $4\"; exit 0; } ;; esac\nfor a in \"$@\"; do printf '[%s]' \"$a\"; done\necho\necho \"project=${COMPOSE_PROJECT_NAME:-unset} foo=${FOO:-unset}\"\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(docker), 0o755); err != nil {
		t.Fatal(err)
	}
	// The custom commands below call devopsy recursively.
	if err := os.Symlink(binary, filepath.Join(bin, "devopsy")); err != nil {
		t.Fatal(err)
	}
	return bin
}

// releaseFile is name beside the code/ that current points at.
func releaseFile(t *testing.T, current, name string) string {
	t.Helper()
	code, err := filepath.EvalSymlinks(current)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(filepath.Dir(code), name)
}

func runDevopsy(t *testing.T, dir string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	cmd.Env = append([]string{"PATH=" + fakeBin(t) + ":/usr/bin:/bin", "DEVOPSY_HOME=" + t.TempDir(), "HOME=" + t.TempDir()}, env...)
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
		"#!/bin/sh\necho \"cmd=$DEVOPSY_COMMAND_NAME args=$#:$*\"\ndevopsy show\n", 0o755)
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

	// -v: what devopsy decided, before the notice; nested devopsy calls
	// (the show command calls devopsy show) are verbose too.
	for _, flag := range []string{"-v", "-vv"} {
		out, code = runDevopsy(t, sub, nil, flag, "show")
		if code != 0 || strings.Count(out, "devopsy: debug: project "+dot) != 2 ||
			!strings.Contains(out, "devopsy: debug: running '"+dot+"/commands/show'") {
			t.Fatalf("%s (%d):\n%s", flag, code, out)
		}
	}
	out, code = runDevopsy(t, sub, []string{"DEVOPSY_VERBOSE=true"}, "ps")
	if code != 0 || !strings.Contains(out, "devopsy: debug: loaded "+dot+"/.env") {
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
	// Compose's help for a command, as devopsy's.
	if out, code := runDevopsy(t, sub, nil, "ps", "--help"); code != 0 || !strings.Contains(out, "Usage:  devopsy ps [OPTIONS] [SERVICE...]") || strings.Contains(out, "docker compose") || strings.Contains(out, "Running ") {
		t.Errorf("compose help (%d):\n%s", code, out)
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
	write(t, filepath.Join(dot, "config.yaml"), "project: app\nenvironments:\n  prod: {}\n", 0o644)
	for _, sub := range []string{"--release", "--rollback", "--releases", "--shell", "--shell-host", "--env", "--debug"} {
		out, code := runDevopsy(t, filepath.Join(tmp, "app"), nil, "@prod", sub, "--help")
		if code != 0 || !strings.Contains(out, "@<target> "+sub) {
			t.Errorf("%s --help (%d):\n%s", sub, code, out)
		}
	}
	// Server commands, on a server named alone or not resolvable at all.
	for _, addr := range []string{"vm1", "vm1:", "pr-1"} {
		for _, sub := range []string{"--ssh-keys", "--ssh-config", "--environments", "--upgrade"} {
			out, code := runDevopsy(t, filepath.Join(tmp, "app"), nil, "@"+addr, sub, "--help")
			if code != 0 || !strings.Contains(out, "Usage: devopsy @<server> "+sub) {
				t.Errorf("@%s %s --help (%d):\n%s", addr, sub, code, out)
			}
		}
	}
	// An environment's command needs an environment.
	if out, code := runDevopsy(t, filepath.Join(tmp, "app"), nil, "@vm1:", "--releases"); code == 0 || !strings.Contains(out, "--releases needs an environment: devopsy @vm1:<environment> --releases") {
		t.Errorf("@vm1: --releases (%d):\n%s", code, out)
	}
	// A lone flag devopsy does not know is a typo, whatever the address
	// (compose's flags never stand alone); with anything after it, it is
	// compose's.
	for _, addr := range []string{"@vm1:", "@prod", "@vm1:prod"} {
		if out, code := runDevopsy(t, filepath.Join(tmp, "app"), nil, addr, "--ssh-host"); code == 0 || !strings.Contains(out, "--ssh-host is not a devopsy flag (did you mean --shell-host?)") || strings.Contains(out, "needs an environment") {
			t.Errorf("%s --ssh-host (%d):\n%s", addr, code, out)
		}
	}
	if out, code := runDevopsy(t, filepath.Join(tmp, "app"), nil, "@vm1:", "--frobnicate"); code == 0 || !strings.Contains(out, "--frobnicate is not a devopsy flag;") {
		t.Errorf("@vm1: --frobnicate (%d):\n%s", code, out)
	}
	if out, code := runDevopsy(t, filepath.Join(tmp, "app"), nil, "--ssh-host"); code == 0 || !strings.Contains(out, "--ssh-host is not a devopsy flag (did you mean --shell-host?)") {
		t.Errorf("local --ssh-host (%d):\n%s", code, out)
	}
	out, code := runDevopsy(t, filepath.Join(tmp, "app"), nil, "@prod", "--help")
	if code != 0 || !strings.Contains(out, "<flag> --help") {
		t.Errorf("@prod --help (%d):\n%s", code, out)
	}
	// --debug's local topics are refused before SSH.
	env := []string{"DEVOPSY_SERVER=nowhere.invalid", "DEVOPSY_SSH_COMMAND=false"}
	for _, topic := range []string{"environments", "schema"} {
		out, code := runDevopsy(t, filepath.Join(tmp, "app"), env, "@prod", "--debug", topic)
		if code == 0 || !strings.Contains(out, "without a target") {
			t.Errorf("--debug %s (%d):\n%s", topic, code, out)
		}
	}
}

// Aliases refuse release and rollback before touching SSH, except from
// their source, and work outside a project.
func TestUserTargets(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "config.yaml"), "aliases:\n  vm1-traefik: {project: traefik, to: \"nowhere.invalid:main\"}\n", 0o644)
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(tmp, "app", ".devopsy", "compose.yaml"), "services: {}\n", 0o644)
	env := []string{"DEVOPSY_HOME=" + home}
	for _, sub := range []string{"--release", "--rollback"} {
		out, code := runDevopsy(t, filepath.Join(tmp, "app"), env, "@vm1-traefik", sub)
		if code == 0 || !strings.Contains(out, "without source") {
			t.Errorf("%s (%d):\n%s", sub, code, out)
		}
	}
	// With source, only from that directory ("~/" is the home directory),
	// where the guard lets it through to the usual release checks.
	write(t, filepath.Join(home, "config.yaml"), "aliases:\n  vm1-traefik: {source: ~/app, to: \"nowhere.invalid:main\"}\n", 0o644)
	write(t, filepath.Join(tmp, "app", ".devopsy", "config.yaml"), "project: traefik\nenvironments:\n  main: {}\n", 0o644)
	write(t, filepath.Join(tmp, "other", ".devopsy", "compose.yaml"), "services: {}\n", 0o644)
	withHome := append([]string{"HOME=" + tmp, "DEVOPSY_SSH_COMMAND=false"}, env...)
	out, code := runDevopsy(t, filepath.Join(tmp, "other"), withHome, "@vm1-traefik", "--release")
	if code == 0 || !strings.Contains(out, "releases only from its source, ~/app") || !strings.Contains(out, filepath.Join(tmp, "other")) {
		t.Errorf("release from another project (%d):\n%s", code, out)
	}
	out, code = runDevopsy(t, filepath.Join(tmp, "app"), withHome, "@vm1-traefik", "--release")
	if strings.Contains(out, "source") || strings.Contains(out, "alias") || !strings.Contains(out, "release") {
		t.Errorf("release from its source (%d):\n%s", code, out)
	}

	// devopsy's flags for servers, without a target, say so.
	out, code = runDevopsy(t, filepath.Join(tmp, "app"), withHome, "--rollback")
	if code == 0 || !strings.Contains(out, "devopsy: error: --rollback needs a target\ndevopsy: | targets: main, vm1-traefik\ndevopsy: | to fix it:\ndevopsy: | $ devopsy @<target> --rollback\n") {
		t.Errorf("--rollback without a target (%d):\n%s", code, out)
	}
	// --release without one runs in place, from defaults:, which this
	// project has none of.
	out, code = runDevopsy(t, filepath.Join(tmp, "app"), withHome, "--release")
	if code == 0 || !strings.Contains(out, "devopsy: error: the project has no release steps under defaults:") || !strings.Contains(out, "devopsy @<target> --release") {
		t.Errorf("--release without a target (%d):\n%s", code, out)
	}

	// Outside a project the target resolves (help needs no SSH).
	out, code = runDevopsy(t, t.TempDir(), withHome, "@vm1-traefik", "--help")
	if code != 0 || !strings.Contains(out, "Where each runs:") {
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
	write(t, filepath.Join(project, ".devopsy", "config.yaml"), "project: app\nenvironments:\n  prod: {}\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", ".env"), "DEVOPSY_SERVER=devopsy@from-dotenv\n", 0o644)
	// echo stands in for ssh: it prints the destination.
	out, code := runDevopsy(t, project, []string{"DEVOPSY_SSH_COMMAND=echo"}, "@prod", "ps")
	if code != 0 || !strings.Contains(out, "-o ClearAllForwardings=yes -T devopsy@from-dotenv sh -c") {
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

// With -v, each SSH session is printed; with -vv, also its script, the
// project's secrets masked. devopsy on the server is verbose too.
func TestRemoteVerbose(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(tmp, "app")
	write(t, filepath.Join(project, ".devopsy", "compose.yaml"), "services: {}\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", "config.yaml"), "project: app\nsecrets: [DB_PASSWORD]\nenvironments:\n  prod: {}\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", ".env"), "DB_PASSWORD=dotenv-secret-value\n", 0o644)
	// true stands in for ssh: only devopsy's own output remains.
	env := []string{"DEVOPSY_SSH_COMMAND=true", "DEVOPSY_SERVER=devopsy@server"}
	out, code := runDevopsy(t, project, env, "-vv", "@prod", "exec", "db", "dotenv-secret-value")
	if code != 0 || !strings.Contains(out, "devopsy: debug: ssh -T devopsy@server\ndevopsy: trace: running:\n") ||
		!strings.Contains(out, "DEVOPSY_VERBOSE=1 COMPOSE_PROJECT_NAME=") ||
		!strings.Contains(out, "'exec' 'db' '***'") || strings.Contains(out, "dotenv-secret-value") {
		t.Errorf("-vv (%d):\n%s", code, out)
	}
	out, code = runDevopsy(t, project, env, "-v", "@prod", "exec", "db", "dotenv-secret-value")
	if code != 0 || !strings.Contains(out, "devopsy: debug: ssh -T devopsy@server\n") || strings.Contains(out, "running:") {
		t.Errorf("-v (%d):\n%s", code, out)
	}
	out, code = runDevopsy(t, project, []string{"DEVOPSY_SSH_COMMAND=true", "DEVOPSY_SERVER=devopsy@server"}, "@prod", "ps")
	if code != 0 || out != "" {
		t.Errorf("not verbose by default (%d):\n%s", code, out)
	}
}

// --vars end to end, with an ssh that runs the script locally.
func TestRemoteVars(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	// ssh [options] -T <host> 'sh -c <script>': run the last argument here.
	write(t, filepath.Join(bin, "fakessh"), "#!/bin/sh\nfor a; do s=$a; done\nexec sh -c \"$s\"\n", 0o755)
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n", 0o755)
	// The server's release root is its home, tmp.
	server := filepath.Join(tmp, "app", "prod")
	project := filepath.Join(tmp, "src", "app")
	write(t, filepath.Join(project, ".devopsy", "compose.yaml"), "services: {}\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", "config.yaml"), "project: app\nsecrets: [REG_PASSWORD, TOK*]\nenvironments:\n  prod: {}\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", ".env"), "DEVOPSY_SERVER=devopsy@server\nREG_USER=deploy\nREG_PASSWORD='p@ss word'\n", 0o644)
	env := []string{"DEVOPSY_SSH_COMMAND=" + filepath.Join(bin, "fakessh"), "PATH=" + bin + ":" + fakeBin(t) + ":/usr/bin:/bin", "HOME=" + tmp}
	run := func(stdin string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Dir = project
		cmd.Env = append([]string{"DEVOPSY_HOME=" + t.TempDir()}, env...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return string(out), code
	}

	// From the project's .env, then from stdin.
	if out, code := run("", "@prod", "--vars", "set", "REG_USER", "REG_PASSWORD"); code != 0 || !strings.Contains(out, "devopsy: set REG_USER, REG_PASSWORD in devopsy@server:"+server+"/shared/.env") {
		t.Fatalf("set (%d):\n%s", code, out)
	}
	if out, code := run("from-stdin\n", "@prod", "--vars", "set", "TOKEN"); code != 0 {
		t.Fatalf("set from stdin (%d):\n%s", code, out)
	}
	got, _ := os.ReadFile(filepath.Join(server, "shared", ".env"))
	if want := "REG_USER='deploy'\nREG_PASSWORD='p@ss word'\nTOKEN='from-stdin'\n"; string(got) != want {
		t.Fatalf(".env\n got %q\nwant %q", got, want)
	}

	// Secrets masked: the local config's, and the release's on the server.
	write(t, filepath.Join(server, "current", ".devopsy", "config.yaml"), "project: app\nsecrets: [REG_USER]\n", 0o644)
	if out, code := run("", "@prod", "--vars"); code != 0 || !strings.Contains(out, "REG_PASSWORD='***'\nREG_USER='***'\nTOKEN='***'\n") || strings.Contains(out, "p@ss") {
		t.Fatalf("list (%d):\n%s", code, out)
	}
	if out, code := run("", "@prod", "--vars", "--show"); code != 0 || !strings.Contains(out, "REG_PASSWORD='p@ss word'\nREG_USER='deploy'\nTOKEN='from-stdin'\n") {
		t.Fatalf("list --show (%d):\n%s", code, out)
	}
	if out, code := run("", "@prod", "--vars", "get", "REG_PASSWORD"); code != 0 || out != "p@ss word\n" {
		t.Fatalf("get (%d): %q", code, out)
	}
	// FORCE_COLOR set here: colors forced, and exported into the session
	// as devopsy's own (the fake runs it here: the remote script's own
	// output is colored).
	cmd := exec.Command(binary, "@prod", "--vars")
	cmd.Dir = project
	cmd.Env = append([]string{"DEVOPSY_HOME=" + t.TempDir(), "FORCE_COLOR=1"}, env...)
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "\x1b[") {
		t.Errorf("FORCE_COLOR: %v\n%q", err, out)
	}
	if out, code := run("", "@prod", "--vars", "unset", "TOKEN"); code != 0 || !strings.Contains(out, "devopsy: unset TOKEN") {
		t.Fatalf("unset (%d):\n%s", code, out)
	}
	if out, code := run("", "@prod", "--vars", "get", "TOKEN"); code == 0 || !strings.Contains(out, "TOKEN is not set") {
		t.Fatalf("get unset (%d):\n%s", code, out)
	}

	// --help anywhere after --vars only explains; --show is for the list
	// and set.
	if out, code := run("", "@prod", "--vars", "set", "--help"); code != 0 || !strings.Contains(out, "Usage: devopsy @<target> --vars") {
		t.Fatalf("set --help (%d):\n%s", code, out)
	}
	if out, code := run("", "@prod", "--vars", "get", "--show", "TOKEN"); code == 0 || !strings.Contains(out, "--show applies to the list and to set") {
		t.Fatalf("get --show (%d):\n%s", code, out)
	}
	// Without a terminal, --show still reads stdin.
	if out, code := run("shown\n", "@prod", "--vars", "set", "--show", "SHOWN"); code != 0 {
		t.Fatalf("set --show (%d):\n%s", code, out)
	}
	if out, _ := run("", "@prod", "--vars", "get", "SHOWN"); out != "shown\n" {
		t.Fatalf("get SHOWN: %q", out)
	}

	// A name like a secret's that secrets: does not list: refused without
	// a terminal.
	if out, code := run("x\n", "@prod", "--vars", "set", "API_KEY"); code == 0 || !strings.Contains(out, "API_KEY looks like a secret") {
		t.Fatalf("unnamed secret (%d):\n%s", code, out)
	}

	// Values never go in arguments.
	if out, code := run("", "@prod", "--vars", "set", "A=b"); code == 0 || !strings.Contains(out, "not a variable name") {
		t.Fatalf("value in argument (%d):\n%s", code, out)
	}
	// Several keys and no value anywhere: refuse rather than guess.
	if out, code := run("x", "@prod", "--vars", "set", "X1", "X2"); code == 0 || !strings.Contains(out, "X1 is not set") {
		t.Fatalf("missing values (%d):\n%s", code, out)
	}

	// An alias never takes values from the project's .env.
	home := t.TempDir()
	box := filepath.Join(tmp, "box", "live")
	write(t, filepath.Join(home, "config.yaml"), "aliases:\n  box: {project: box, to: \"devopsy@server:live\"}\n", 0o644)
	env = append(env, "DEVOPSY_HOME="+home)
	if out, code := run("other\n", "@box", "--vars", "set", "REG_USER"); code != 0 {
		t.Fatalf("user-level set (%d):\n%s", code, out)
	}
	if got, _ := os.ReadFile(filepath.Join(box, "shared", ".env")); string(got) != "REG_USER='other'\n" {
		t.Fatalf("user-level .env %q", got)
	}
}

func TestCompletion(t *testing.T) {
	root := t.TempDir()
	dot := filepath.Join(root, ".devopsy")
	write(t, filepath.Join(dot, "compose.yaml"), "services:\n  web:\n    image: busybox\n", 0o644)
	write(t, filepath.Join(dot, "config.yaml"), "project: app\nenvironments:\n  prod: {}\n", 0o644)
	write(t, filepath.Join(dot, "commands", "deploy"), "#!/bin/sh\n## Description: Roll out\n", 0o755)
	// docker's own completion, as cobra prints it: compose commands, one
	// clashing with the project's deploy.
	bin := t.TempDir()
	write(t, filepath.Join(bin, "docker"), "#!/bin/sh\n[ \"$1\" = __complete ] || exit 1\nprintf 'deploy\\tcompose deploy\\nup\\tCreate and start containers\\n:4\\n'\n", 0o755)
	env := []string{"PATH=" + bin + ":/usr/bin:/bin"}

	out, code := runDevopsy(t, root, env, "--complete", "")
	want := "@prod\tenvironment\ttarget\ndeploy\tRoll out\tproject\nup\tCreate and start containers\tcompose\n:4\n"
	if code != 0 || out != want {
		t.Fatalf("--complete (%d):\n%s", code, out)
	}
	out, _ = runDevopsy(t, root, env, "--complete", "deploy", "")
	if out != ":0\n" {
		t.Fatalf("a project command's arguments are files:\n%s", out)
	}

	out, code = runDevopsy(t, root, nil, "--completion", "fish")
	if code != 0 || !strings.Contains(out, "devopsy --complete") {
		t.Fatalf("--completion fish (%d):\n%s", code, out)
	}
	if _, code = runDevopsy(t, root, nil, "--completion", "tcsh"); code != 1 {
		t.Fatalf("--completion tcsh: %d", code)
	}
}

// release and rollback run the environment's steps in phases: before
// (local, then remote in the current release), the upload, prepare (in the
// new release), the check of compose's required variables, the switch, run,
// after (remote, then local). With an ssh that runs the scripts here.
func TestReleaseSteps(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	write(t, filepath.Join(bin, "fakessh"), "#!/bin/sh\nfor a; do s=$a; done\nexec sh -c \"$s\"\n", 0o755)
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n", 0o755)
	// GNU mv -T, which macOS lacks: replace the symlink, never move into it.
	write(t, filepath.Join(bin, "mv"), "#!/bin/sh\nif [ \"$1\" = -Tf ]; then rm -f \"$3\"; exec /bin/mv \"$2\" \"$3\"; fi\nexec /bin/mv \"$@\"\n", 0o755)
	// The server's release root is its home, tmp.
	server := filepath.Join(tmp, "app", "prod")
	project := filepath.Join(tmp, "src", "app")
	dot := filepath.Join(project, ".devopsy")
	write(t, filepath.Join(dot, "compose.yaml"), "services:\n  web:\n    image: x\n    environment:\n      SECRET: ${SECRET:?generated by secrets}\n      NEEDED: ${NEEDED:?set it per environment}\n", 0o644)
	targets := "project: app\nenvironments:\n prod:\n  mode: image\n  env:\n    SITE: one\n" +
		"  release:\n    before: [local: check, remote: mark before]\n    prepare: secrets\n    run: deploy --fast\n    after: [remote: mark after, local: -v done]\n"
	write(t, filepath.Join(dot, "config.yaml"), targets, 0o644)
	write(t, filepath.Join(dot, ".env"), "DEVOPSY_SERVER=devopsy@server\n", 0o644)
	write(t, filepath.Join(dot, "commands", "check"), "#!/bin/sh\n## Run: local\necho \"check target=$DEVOPSY_TARGET env=$DEVOPSY_ENVIRONMENT instance=$DEVOPSY_INSTANCE command=$DEVOPSY_COMMAND_TARGET site=$SITE\"\nexit ${FAIL_BEFORE:-0}\n", 0o755)
	write(t, filepath.Join(dot, "commands", "mark"), "#!/bin/sh\necho \"mark $*\"\n", 0o755)
	write(t, filepath.Join(dot, "commands", "secrets"), "#!/bin/sh\ngrep -q '^SECRET=' \"$DEVOPSY_PROJECT_DIR/.env\" || echo SECRET=generated >> \"$DEVOPSY_PROJECT_DIR/.env\"\n", 0o755)
	write(t, filepath.Join(dot, "commands", "deploy"), "#!/bin/sh\n## Run: target\necho \"deploy $* in $(basename \"$(readlink \"$DEVOPSY_PROJECT_DIR/..\" 2>/dev/null || dirname \"$DEVOPSY_PROJECT_DIR\")\")\"\n[ \"$1\" = --back ] || exit ${FAIL_REMOTE:-0}\n", 0o755)
	write(t, filepath.Join(dot, "commands", "done"), "#!/bin/sh\n## Run: local\n## Needs: target\necho \"after ran at=$DEVOPSY_COMMAND_TARGET target=$DEVOPSY_TARGET\"\n", 0o755)
	env := []string{"DEVOPSY_SSH_COMMAND=" + filepath.Join(bin, "fakessh"), "PATH=" + bin + ":" + fakeBin(t) + ":/usr/bin:/bin", "HOME=" + tmp}
	run := func(extra []string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Dir = project
		cmd.Env = append(append([]string{"DEVOPSY_HOME=" + t.TempDir()}, env...), extra...)
		out, _ := cmd.CombinedOutput()
		return string(out), cmd.ProcessState.ExitCode()
	}
	current := func() string {
		l, _ := os.Readlink(filepath.Join(server, "current"))
		return l
	}

	if out, code := run(nil, "@prod", "--release", "deploy"); code == 0 || !strings.Contains(out, "--release takes no command") {
		t.Fatalf("release with a command (%d):\n%s", code, out)
	}

	// Commands' scopes: a local one never runs on the server, as a step or
	// a word, refused before connecting.
	write(t, filepath.Join(dot, "config.yaml"), strings.Replace(targets, "run: deploy --fast", "run: check", 1), 0o644)
	if out, code := run(nil, "@prod", "--release"); code == 0 || !strings.Contains(out, "a remote step: check runs only on your machine") {
		t.Fatalf("local command as a remote step (%d):\n%s", code, out)
	}
	write(t, filepath.Join(dot, "config.yaml"), strings.Replace(targets, "local: -v done", "local: -v deploy", 1), 0o644)
	if out, code := run(nil, "@prod", "--release"); code == 0 || !strings.Contains(out, "a local step: deploy runs only on a target") {
		t.Fatalf("target command as a local step (%d):\n%s", code, out)
	}
	write(t, filepath.Join(dot, "config.yaml"), targets, 0o644)
	if out, code := run(nil, "@prod", "check"); code == 0 || !strings.Contains(out, "check runs only on your machine") {
		t.Fatalf("local command on a target (%d):\n%s", code, out)
	}
	// A local command that needs an address runs locally with it, as typed,
	// and nothing of the target's.
	write(t, filepath.Join(dot, "commands", "addr"), "#!/bin/sh\n## Run: local\n## Needs: target\necho \"addr=$DEVOPSY_COMMAND_TARGET target=$DEVOPSY_TARGET env=$DEVOPSY_ENVIRONMENT instance=$DEVOPSY_INSTANCE site=$SITE $*\"\n", 0o755)
	write(t, filepath.Join(dot, "commands", "tunnel"), "#!/bin/sh\n## Run: local\n## Needs: server\necho \"tunnel=$DEVOPSY_COMMAND_TARGET\"\n", 0o755)
	if out, code := run(nil, "@prod", "addr", "x"); code != 0 || !strings.Contains(out, "addr=prod target= env= instance= site= x") {
		t.Fatalf("local command with a target (%d):\n%s", code, out)
	}
	if out, code := run(nil, "addr"); code == 0 || !strings.Contains(out, "addr needs a target") {
		t.Fatalf("local command without its target (%d):\n%s", code, out)
	}
	for _, a := range []string{"devopsy@server:", "prod"} {
		if out, code := run(nil, "@"+a, "tunnel"); code != 0 || !strings.Contains(out, "tunnel="+a+"\n") {
			t.Fatalf("local command with a server, @%s (%d):\n%s", a, code, out)
		}
	}
	if _, err := os.Stat(server); !os.IsNotExist(err) {
		t.Fatalf("local commands touched the server: %v", err)
	}

	// A variable named like a secret that secrets: does not list stops
	// the release before anything runs, --yes or not; public: lets it go.
	if out, code := run(nil, "@prod", "--release", "--yes"); code == 0 || !strings.Contains(out, "devopsy: error: SECRET looks like a secret, but secrets: does not list it\n") || strings.Contains(out, "check target") {
		t.Fatalf("unnamed secret (%d):\n%s", code, out)
	}
	targets = "public: [SECRET]\n" + targets
	write(t, filepath.Join(dot, "config.yaml"), targets, 0o644)

	// A failing local before step stops before the server. Local steps get
	// the target's env, never its address unless they need it: a set
	// DEVOPSY_TARGET means a release on a server.
	if out, code := run([]string{"FAIL_BEFORE=3"}, "@prod", "--release"); code == 0 || !strings.Contains(out, "check target= env= instance= command= site=one") || !strings.Contains(out, "nothing changed on devopsy@server:prod") {
		t.Fatalf("before failure (%d):\n%s", code, out)
	}
	if _, err := os.Stat(server); !os.IsNotExist(err) {
		t.Fatalf("before failure touched the server: %v", err)
	}

	// prepare generates SECRET; NEEDED is missing: listed, nothing live.
	out, code := run(nil, "@prod", "--release")
	if code == 0 || !strings.Contains(out, "NEEDED  set it per environment") || strings.Contains(out, "SECRET ") || !strings.Contains(out, "devopsy @devopsy@server:prod --vars set") || current() != "" || strings.Contains(out, "deploy --fast") {
		t.Fatalf("missing variable (%d), current %q:\n%s", code, current(), out)
	}
	if !strings.Contains(out, "remote before steps skipped") {
		t.Errorf("first release ran remote before steps:\n%s", out)
	}
	if data, _ := os.ReadFile(filepath.Join(server, "shared", ".env")); !strings.Contains(string(data), "SECRET=generated") {
		t.Fatalf("prepare did not run: %q", data)
	}

	write(t, filepath.Join(server, "shared", ".env"), "SECRET=generated\nNEEDED=1\n", 0o644)
	out, code = run(nil, "@prod", "--release")
	if code != 0 || !strings.Contains(out, "deploy --fast in") || !strings.Contains(out, "mark after") || !strings.Contains(out, "after ran at=devopsy@server:prod target=\n") {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	first := current()
	if first == "" {
		t.Fatal("no current release")
	}

	// Both runs are logged on the server, in their releases, the failed one
	// too.
	out, code = run(nil, "@prod", "--log")
	if code != 0 || !strings.Contains(out, "devopsy dev: release of devopsy@server:prod") || !strings.Contains(out, "deploy --fast in") || !strings.Contains(out, "release exited 0") {
		t.Fatalf("--log (%d):\n%s", code, out)
	}
	entries, _ := os.ReadDir(filepath.Join(server, "releases"))
	if len(entries) != 2 {
		t.Fatalf("releases: %v", entries)
	}
	if out, code := run(nil, "@prod", "--log", entries[0].Name()); code != 0 || !strings.Contains(out, "NEEDED  set it per environment") || !strings.Contains(out, "release exited 1") {
		t.Fatalf("--log <id> (%d):\n%s", code, out)
	}

	// A failing run goes back to the previous release and restarts it with
	// its own run step, from its own config: the first release has no
	// rollback:, so its release: one, whatever the checkout says now. After
	// steps are skipped; remote before steps ran in it.
	// Before going back, what the failed release's containers were left as
	// goes into the server's log: each one's state, then the health checks
	// and logs of those neither running and healthy nor exited 0 (a
	// restarting one, whatever its last exit code).
	write(t, filepath.Join(dot, "config.yaml"), targets+"  rollback: deploy --back\n", 0o644)
	out, code = run([]string{"FAIL_REMOTE=4", `FAKE_PS=app-prod-web-1 web running 0 unhealthy\napp-prod-db-1 db running 0 healthy\napp-prod-init-1 init exited 0 \napp-prod-cron-1 cron exited 2 \napp-prod-worker-1 worker restarting 0 \n`, `FAKE_HEALTH=18:15:01 exit 1: "connection refused\\n"`}, "@prod", "--release")
	if code != 4 || !strings.Contains(out, "mark before") || !strings.Contains(out, "current is back to") || !strings.Contains(out, "with its own run step") || strings.Contains(out, "deploy --back in") || strings.Count(out, "deploy --fast in") < 2 || strings.Contains(out, "after ran") || current() != first {
		t.Fatalf("run failure (%d), current %s, want %s:\n%s", code, current(), first, out)
	}
	report := "devopsy: the containers as the failed run left them:\ndevopsy: | web: running, unhealthy\ndevopsy: | db: running, healthy\ndevopsy: | init: exited (0)\ndevopsy: | cron: exited (2)\ndevopsy: | worker: restarting (0)\n" +
		"devopsy: the last health checks of app-prod-web-1:\ndevopsy: | 18:15:01 exit 1: \"connection refused\\n\" app-prod-web-1\n" +
		"devopsy: the last health checks of app-prod-cron-1:\ndevopsy: | 18:15:01 exit 1: \"connection refused\\n\" app-prod-cron-1\n" +
		"devopsy: the last health checks of app-prod-worker-1:\ndevopsy: | 18:15:01 exit 1: \"connection refused\\n\" app-prod-worker-1\n" +
		"devopsy: the last logs of web cron worker:\n[compose]"
	if i := strings.Index(out, report); i < 0 || !strings.Contains(out[i:], "[--profile][*][logs][--no-color][--tail][100][web][cron][worker]\n") || i > strings.Index(out, "current is back to") {
		t.Errorf("what the failed run left, before going back:\n%s", out)
	}
	if out, code := run(nil, "@prod", "--log"); code != 0 || !strings.Contains(out, "devopsy: | cron: exited (2)") || !strings.Contains(out, "[logs][--no-color][--tail][100][web][cron][worker]") {
		t.Errorf("--log of the failed run (%d):\n%s", code, out)
	}

	// rollback: back to the release before, running its own rollback:.
	if out, code := run(nil, "@prod", "--release"); code != 0 || current() == first {
		t.Fatalf("second release (%d):\n%s", code, out)
	}
	second := current()
	if out, code := run(nil, "@prod", "--release"); code != 0 || current() == second {
		t.Fatalf("third release (%d):\n%s", code, out)
	}
	third := current()
	if out, code := run(nil, "@prod", "--rollback"); code != 0 || !strings.Contains(out, "deploy --back in") || current() != second {
		t.Fatalf("rollback (%d), current %s, want %s:\n%s", code, current(), second, out)
	}
	// The release it left is marked: never kept nor rolled back to again.
	if _, err := os.Stat(filepath.Join(server, filepath.Dir(third), remote.RolledBackFile)); err != nil {
		t.Errorf("the release rolled back from is not marked: %v", err)
	}
	if out, code := run(nil, "@prod", "--releases"); code != 0 || !strings.Contains(out, "ROLLED BACK") {
		t.Errorf("--releases (%d):\n%s", code, out)
	}
	// The rollback's log is appended to the restored release's.
	if data, err := os.ReadFile(filepath.Join(server, filepath.Dir(second), remote.LogFile)); err != nil || !strings.Contains(string(data), "release exited 0") || !strings.Contains(string(data), "rollback exited 0") {
		t.Fatalf("rollback log: %v\n%s", err, data)
	}

	// The release rolled back to needs rollback: steps of its own: the
	// first has none, whatever the checkout has. Nothing changes.
	if out, code := run(nil, "@prod", "--rollback"); code == 0 || !strings.Contains(out, "gives environment prod no rollback run step") || current() != second {
		t.Fatalf("rollback without steps (%d), current %s:\n%s", code, current(), out)
	}

	// Unknown keys are refused, not ignored.
	write(t, filepath.Join(dot, "config.yaml"), strings.Replace(targets, "run:", "runs:", 1), 0o644)
	if out, code := run(nil, "@prod", "--release"); code == 0 || !strings.Contains(out, `unknown key "runs"`) {
		t.Fatalf("unknown key (%d):\n%s", code, out)
	}
}

// A project command calling devopsy @target inherits the local project's
// COMPOSE_PROJECT_NAME, and a shell may export one: the target's own
// name always wins.
func TestNestedRemoteProjectName(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(tmp, "app")
	write(t, filepath.Join(project, ".devopsy", "compose.yaml"), "services: {}\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", "config.yaml"), "project: app\nenvironments:\n  prod: {}\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", ".env"), "DEVOPSY_SERVER=devopsy@server\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", "commands", "nested"), "#!/bin/sh\nexec devopsy @prod ps\n", 0o755)
	out, code := runDevopsy(t, project, []string{"DEVOPSY_SSH_COMMAND=echo"}, "nested")
	if code != 0 || !strings.Contains(out, `COMPOSE_PROJECT_NAME='\''app-prod'\'' exec devopsy`) {
		t.Errorf("(%d):\n%s", code, out)
	}
	out, code = runDevopsy(t, project, []string{"DEVOPSY_SSH_COMMAND=echo", "COMPOSE_PROJECT_NAME=mine"}, "@prod", "ps")
	if code != 0 || !strings.Contains(out, `COMPOSE_PROJECT_NAME='\''app-prod'\'' exec devopsy`) || strings.Contains(out, "mine") {
		t.Errorf("exported COMPOSE_PROJECT_NAME (%d):\n%s", code, out)
	}
}

// --debug targets shows computed targets and where each value came from.
func TestDebugTargets(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dot := filepath.Join(tmp, "app", ".devopsy")
	write(t, filepath.Join(dot, "compose.yaml"), "services:\n  web:\n    image: x\n    labels:\n      - devopsy.shell=true\n", 0o644)
	write(t, filepath.Join(dot, "config.yaml"), "project: app\ndefaults:\n  mode: image\n  env:\n    A: one\nenvironments:\n  prod:\n    inherit: [DB]\n    env:\n      B: two\n  dev: {parent: prod}\n", 0o644)
	env := []string{"DEVOPSY_HOME=" + t.TempDir(), "DEVOPSY_SERVER=h"}
	out, code := runDevopsy(t, filepath.Join(tmp, "app"), env, "--debug", "environments", "prod")
	for _, want := range []string{"prod  (", "mode      image  (defaults in ", "env       A='one'  (defaults in ", "env       B='two'  (" + filepath.Join(dot, "config.yaml")} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("missing %q (%d):\n%s", want, code, out)
		}
	}
	if !strings.Contains(out, "inherit   DB  (") {
		t.Errorf("inherit:\n%s", out)
	}
	out, code = runDevopsy(t, filepath.Join(tmp, "app"), env, "--debug", "environments", "dev")
	if code != 0 || !strings.Contains(out, "parent    prod h:prod  (") {
		t.Errorf("parent (%d):\n%s", code, out)
	}
	out, code = runDevopsy(t, filepath.Join(tmp, "app"), env[:1], "--debug", "environments", "dev")
	if code != 0 || !strings.Contains(out, "parent    prod no server yet") {
		t.Errorf("parent without a server (%d):\n%s", code, out)
	}
	// --sync fails before any connection, with why and the two fixes.
	out, code = runDevopsy(t, filepath.Join(tmp, "app"), env[:1], "@h:dev", "--sync", "--yes")
	if code == 0 || !strings.Contains(out, "devopsy: error: dev's parent, prod, has no server\ndevopsy: | devopsy reads a parent's server from variables only:") ||
		!strings.Contains(out, "devopsy: | to fix it, do one of these:\ndevopsy: | 1. set prod's server in .devopsy/.env, as the templates do: DEVOPSY_SERVER_PROD=<server> (or DEVOPSY_SERVER for every environment)\ndevopsy: | 2. sync from an address instead:\ndevopsy: |    $ devopsy @h:dev --sync --from @h:prod\n") {
		t.Errorf("--sync without a parent's server (%d):\n%s", code, out)
	}
	out, code = runDevopsy(t, filepath.Join(tmp, "app"), env, "--debug")
	if code != 0 || !regexp.MustCompile(`(?m)^environments +dev, prod$`).MatchString(out) || !strings.Contains(out, "web: devopsy.shell=true") {
		t.Errorf("summary (%d):\n%s", code, out)
	}
	out, code = runDevopsy(t, filepath.Join(tmp, "app"), env, "--debug", "environments", "--yaml")
	if code != 0 || !strings.Contains(out, "prod:\n  target: h:prod\n  server: h\n  path: app/prod\n  mode: image\n") || !strings.Contains(out, "    A: one") ||
		!strings.Contains(out, "  parent:\n    name: prod\n    target: h:prod\n") || !strings.Contains(out, "  inherit:\n    - DB\n") {
		t.Errorf("--yaml (%d):\n%s", code, out)
	}
	out, code = runDevopsy(t, filepath.Join(tmp, "app"), env, "--debug", "capabilities")
	if code != 0 || !strings.Contains(out, "open [service]") || !strings.Contains(out, "does not implement it") || !strings.Contains(out, "finish parent|child go|abort") || !strings.Contains(out, "environment:\n    every action gets DEVOPSY_DATA_MNT") {
		t.Errorf("capabilities (%d):\n%s", code, out)
	}
}

// --debug env traces each variable: what sets it and what that overrides.
func TestDebugEnv(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A release: its code/ through current, its target env beside it.
	rel := filepath.Join(tmp, "releases", "1")
	if err := os.MkdirAll(filepath.Join(rel, remote.CodeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("releases", "1", remote.CodeDir), filepath.Join(tmp, "current")); err != nil {
		t.Fatal(err)
	}
	dot := filepath.Join(tmp, "current", ".devopsy")
	write(t, filepath.Join(dot, "compose.yaml"), "services:\n  web:\n    image: x\n", 0o644)
	write(t, filepath.Join(dot, "config.yaml"), "project: shop\nsecrets: [DB_PASS*]\n", 0o644)
	write(t, filepath.Join(tmp, "shared", ".env"), "LEVEL=shared\nDB_PASSWORD=hunter2hunter2\nCALLED=file\n", 0o644)
	if err := os.Symlink(filepath.Join(tmp, "shared", ".env"), filepath.Join(dot, ".env")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dot, "project.env"), "LEVEL=project\nDB_PASSWORD=oldoldoldold\n", 0o644)
	write(t, filepath.Join(rel, remote.TargetEnvFile), "COMPOSE_PROJECT_NAME='shop-prod'\nLEVEL=target\n"+cli.ImportedMarker+"\nDEVOPSY_WILDCARD_DOMAIN='vm1.example.com'\n", 0o644)
	write(t, filepath.Join(dot, "capabilities", "env", "compute"), "#!/bin/sh\necho \"SITE_URL='https://$COMPOSE_PROJECT_NAME.$DEVOPSY_WILDCARD_DOMAIN'\"\necho LEVEL=computed\n", 0o755)
	dir := filepath.Join(tmp, "current")
	// PWD as a shell sets it: commands run through current.
	out, code := runDevopsy(t, dir, []string{"CALLED=caller", "PWD=" + dir}, "--debug", "env")
	for _, want := range []string{
		"COMPOSE_PROJECT_NAME='shop-prod'\n  set by      devopsy-target.env\n",
		"DEVOPSY_WILDCARD_DOMAIN='vm1.example.com'\n  set by      devopsy-target.env, imported at release\n",
		"SITE_URL='https://shop-prod.vm1.example.com'\n  set by      capabilities/env/compute, computed\n",
		"LEVEL='shared'\n  set by      .env (-> " + filepath.Join(tmp, "shared", ".env") + ")\n  overridden  project.env: 'project'\n  overridden  devopsy-target.env: 'target'\n  overridden  capabilities/env/compute, computed: 'computed'\n",
		"CALLED='caller'\n  set by      the caller's environment\n  overridden  .env (",
		"DB_PASSWORD='***'\n",
		"overridden  project.env: '***'\n",
		"DEVOPSY_PROJECT_DIR='" + dot + "'\n  set by      devopsy, from where it found .devopsy/\n",
	} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("missing %q (%d):\n%s", want, code, out)
		}
	}
	out, code = runDevopsy(t, dir, nil, "--debug", "env", "--show", "DB_PASSWORD", "NOPE", "PATH")
	if code != 0 || !strings.Contains(out, "DB_PASSWORD='hunter2hunter2'\n") || !strings.Contains(out, "project.env: 'oldoldoldold'") ||
		!strings.Contains(out, "NOPE not set\n") || !strings.Contains(out, "\n  set by      the caller's environment\n") || strings.Contains(out, "LEVEL") {
		t.Errorf("--show (%d):\n%s", code, out)
	}
	// --env masks the same, --show shows everything.
	out, code = runDevopsy(t, dir, nil, "--env")
	if code != 0 || !strings.Contains(out, "DB_PASSWORD='***'\n") || !strings.Contains(out, "LEVEL='shared'\n") || strings.Contains(out, "hunter2") {
		t.Errorf("--env (%d):\n%s", code, out)
	}
	out, code = runDevopsy(t, dir, nil, "--env", "--show")
	if code != 0 || !strings.Contains(out, "DB_PASSWORD='hunter2hunter2'\n") {
		t.Errorf("--env --show (%d):\n%s", code, out)
	}
	if out, code = runDevopsy(t, dir, nil, "--env", "x"); code == 0 || !strings.Contains(out, "usage: devopsy --env [--show]") {
		t.Errorf("--env x (%d):\n%s", code, out)
	}
	// Without release files, devopsy names the compose project itself.
	write(t, filepath.Join(tmp, "app", ".devopsy", "compose.yaml"), "services:\n  web:\n    image: x\n", 0o644)
	out, code = runDevopsy(t, filepath.Join(tmp, "app"), nil, "--debug", "env")
	if code != 0 || !strings.Contains(out, "COMPOSE_PROJECT_NAME='app'\n  set by      devopsy, from the name of the directory with .devopsy/\n") {
		t.Errorf("local (%d):\n%s", code, out)
	}
}

// --prepare-release, --debug labels and --debug imports against a docker
// that answers ps and inspect like a host running a proxy.
func TestRolesEndToEnd(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	release := filepath.Join(tmp, "releases", "1")
	rel := filepath.Join(release, remote.CodeDir)
	dot := filepath.Join(rel, ".devopsy")
	targetEnv := filepath.Join(release, remote.TargetEnvFile)
	write(t, filepath.Join(dot, "compose.yaml"), "services:\n  web:\n    image: x\n    labels: [devopsy.import.DEVOPSY_WILDCARD_DOMAIN=proxy/WILDCARD_DOMAIN]\n", 0o644)
	write(t, targetEnv, "COMPOSE_PROJECT_NAME='shop'\n", 0o644)
	bin := t.TempDir()
	docker := `#!/bin/sh
case "$1" in
  ps) echo aaa; echo bbb ;;
  inspect)
    echo 'aaa {"com.docker.compose.project":"traefik-main","devopsy.role":"proxy","devopsy.export.WILDCARD_DOMAIN":"vm1.example.com"}'
    echo 'bbb {"com.docker.compose.project":"shop","com.docker.compose.project.working_dir":"` + dot + `","devopsy.import.DEVOPSY_WILDCARD_DOMAIN":"proxy/WILDCARD_DOMAIN"}'
    ;;
esac
`
	write(t, filepath.Join(bin, "docker"), docker, 0o755)
	env := []string{"PATH=" + bin + ":/usr/bin:/bin"}

	out, code := runDevopsy(t, rel, env, "--prepare-release")
	data, _ := os.ReadFile(targetEnv)
	if code != 0 || !strings.Contains(out, "DEVOPSY_WILDCARD_DOMAIN=vm1.example.com, from proxy (traefik-main)") || !strings.HasSuffix(string(data), "DEVOPSY_WILDCARD_DOMAIN='vm1.example.com'\n") {
		t.Fatalf("--prepare-release (%d):\n%s\n%s", code, out, data)
	}
	out, code = runDevopsy(t, rel, env, "--debug", "labels")
	if code != 0 || !strings.Contains(out, "role proxy  held by traefik-main") || !strings.Contains(out, "exports WILDCARD_DOMAIN=vm1.example.com") || !strings.Contains(out, `this release imported: "vm1.example.com"`) || !strings.Contains(out, "devopsy.data=mnt|stream|none") || !strings.Contains(out, "devopsy.data.stop=never|own|import|always") || !strings.Contains(out, "devopsy.data.exclude=<entry>") {
		t.Errorf("--debug labels (%d):\n%s", code, out)
	}
	out, code = runDevopsy(t, rel, env, "--debug", "imports")
	if code != 0 || !strings.Contains(out, "shop  devopsy.import.DEVOPSY_WILDCARD_DOMAIN=proxy/WILDCARD_DOMAIN") || !strings.Contains(out, "current") {
		t.Errorf("--debug imports (%d):\n%s", code, out)
	}
	// The proxy now exports another domain: the release is stale.
	write(t, filepath.Join(bin, "docker"), strings.Replace(docker, `"devopsy.export.WILDCARD_DOMAIN":"vm1.example.com"`, `"devopsy.export.WILDCARD_DOMAIN":"vm2.example.com"`, 1), 0o755)
	out, _ = runDevopsy(t, rel, env, "--debug", "imports")
	if !strings.Contains(out, "STALE: release shop again") {
		t.Errorf("stale:\n%s", out)
	}
}

// Build mode uploads what git lists: outside a repository a release fails
// before any session, the role check's included, and says how to fix it.
func TestReleaseBuildModeNeedsGit(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(tmp, "app")
	dot := filepath.Join(project, ".devopsy")
	write(t, filepath.Join(dot, "compose.yaml"), "services:\n  web:\n    image: x\n    labels: [devopsy.role=proxy]\n", 0o644)
	write(t, filepath.Join(dot, "config.yaml"), "project: app\nenvironments:\n prod:\n  release: deploy\n", 0o644)
	bin := t.TempDir()
	sshLog := filepath.Join(tmp, "ssh.log")
	write(t, filepath.Join(bin, "fakessh"), "#!/bin/sh\necho \"$*\" >> "+sshLog+"\n", 0o755)
	out, code := runDevopsy(t, project, []string{"DEVOPSY_SSH_COMMAND=" + filepath.Join(bin, "fakessh"), "GIT_CEILING_DIRECTORIES=" + tmp}, "@devopsy@server:prod", "--release")
	if code == 0 || !strings.Contains(out, "devopsy: error: build mode uploads the files git lists, and "+project+" is in no git repository: nothing changed\n") || !strings.Contains(out, "$ git init "+project) {
		t.Errorf("a build-mode release outside git (%d):\n%s", code, out)
	}
	if data, _ := os.ReadFile(sshLog); len(data) != 0 {
		t.Errorf("sessions opened before the failure:\n%s", data)
	}
}

// Instances, the server's release root, the project's .env, --environments and
// --destroy, with an ssh that runs the scripts here.
func TestInstancesEndToEnd(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	write(t, filepath.Join(bin, "fakessh"), "#!/bin/sh\nfor a; do s=$a; done\nexec sh -c \"$s\"\n", 0o755)
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n", 0o755)
	write(t, filepath.Join(bin, "mv"), "#!/bin/sh\nif [ \"$1\" = -Tf ]; then rm -f \"$3\"; exec /bin/mv \"$2\" \"$3\"; fi\nexec /bin/mv \"$@\"\n", 0o755)
	root := filepath.Join(tmp, "srv")
	home := filepath.Join(tmp, "home")
	write(t, filepath.Join(home, "config.yaml"), "releases:\n  root: "+root+"\n", 0o644)
	project := filepath.Join(tmp, "app")
	dot := filepath.Join(project, ".devopsy")
	write(t, filepath.Join(dot, "compose.yaml"), "services: {}\n", 0o644)
	write(t, filepath.Join(dot, "config.yaml"), "project: shop\ninstances: true\ndefaults:\n  mode: image\n  release: deploy\nenvironments:\n  prod: {}\n  \"pr-*\": {}\n", 0o644)
	write(t, filepath.Join(dot, ".env"), "DEVOPSY_SERVER=devopsy@server\n", 0o644)
	write(t, filepath.Join(dot, "commands", "deploy"), "#!/bin/sh\necho \"deploy shared=$SHARED project=$DEVOPSY_PROJECT instance=$DEVOPSY_INSTANCE env=$DEVOPSY_ENVIRONMENT\"\n", 0o755)
	env := []string{"DEVOPSY_SSH_COMMAND=" + filepath.Join(bin, "fakessh"), "PATH=" + bin + ":" + fakeBin(t) + ":/usr/bin:/bin", "HOME=" + tmp, "DEVOPSY_HOME=" + home}
	run := func(extra []string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Dir = project
		cmd.Env = append(append([]string{}, env...), extra...)
		cmd.Stdin = strings.NewReader("")
		out, _ := cmd.CombinedOutput()
		return string(out), cmd.ProcessState.ExitCode()
	}

	if out, code := run(nil, "@prod", "--release"); code == 0 || !strings.Contains(out, "shop needs an instance: devopsy @[<server>:]<instance>/prod") {
		t.Fatalf("instance required (%d):\n%s", code, out)
	}
	// A shared value for the whole project on that server, before any release.
	if out, code := run([]string{"SHARED=yes"}, "@b/prod", "--vars-project", "set", "SHARED"); code != 0 || !strings.Contains(out, filepath.Join(root, "shop", ".env")) {
		t.Fatalf("--vars-project (%d):\n%s", code, out)
	}
	// On a server named alone: the checkout's project.
	if out, code := run([]string{"OTHER=1"}, "@devopsy@server", "--vars-project", "set", "OTHER"); code != 0 || !strings.Contains(out, filepath.Join(root, "shop", ".env")) {
		t.Fatalf("@server --vars-project (%d):\n%s", code, out)
	}
	if out, code := run(nil, "@devopsy@server", "--vars-instance"); code == 0 || !strings.Contains(out, "(for a server: --vars-instance needs an environment, devopsy @devopsy@server:<environment> --vars-instance)") {
		t.Fatalf("@server --vars-instance (%d):\n%s", code, out)
	}
	if out, code := run(nil, "@devopsy@server", "--ssh-host"); code == 0 || !strings.Contains(out, "--ssh-host is not a devopsy flag (did you mean --shell-host?)") || strings.Contains(out, "needs an environment") {
		t.Fatalf("@server --ssh-host (%d):\n%s", code, out)
	}
	// A server whose devopsy is older refuses before anything, the new
	// instance's question included: its refusal is no answer. Older than
	// 8, it reports no features.
	old := filepath.Join(tmp, "old")
	write(t, filepath.Join(old, "devopsy"), "#!/bin/sh\nif [ \"$1\" = --release-settings ]; then "+binary+" --release-settings | sed -e 's/^protocol=.*/protocol=1/' -e '/^features=/d'; exit; fi\nexec "+binary+" \"$@\"\n", 0o755)
	write(t, filepath.Join(bin, "oldssh"), "#!/bin/sh\nfor a; do s=$a; done\nPATH="+old+":$PATH exec sh -c \"$s\"\n", 0o755)
	if out, code := run([]string{"DEVOPSY_SSH_COMMAND=" + filepath.Join(bin, "oldssh")}, "@b/prod", "--release"); code == 0 || !strings.Contains(out, "older than this one needs: upgrade it (devopsy @devopsy@server: --upgrade)") || strings.Contains(out, "a new instance") {
		t.Fatalf("an older server (%d):\n%s", code, out)
	}
	// Older still, without --release-settings: the hint upgrades the
	// server's devopsy too, never the local one (Conventions: hints work
	// when copied).
	ancient := filepath.Join(tmp, "ancient")
	write(t, filepath.Join(ancient, "devopsy"), "#!/bin/sh\nif [ \"$1\" = --release-settings ]; then echo 'devopsy: error: unknown flag --release-settings' >&2; exit 1; fi\nexec "+binary+" \"$@\"\n", 0o755)
	write(t, filepath.Join(bin, "ancientssh"), "#!/bin/sh\nfor a; do s=$a; done\nPATH="+ancient+":$PATH exec sh -c \"$s\"\n", 0o755)
	if out, code := run([]string{"DEVOPSY_SSH_COMMAND=" + filepath.Join(bin, "ancientssh")}, "@b/prod", "--release"); code == 0 || !strings.Contains(out, "upgrade it (devopsy @devopsy@server: --upgrade)") {
		t.Fatalf("a server without --release-settings (%d):\n%s", code, out)
	}
	// DEVOPSY_PROTOCOL_CHECK=warn goes on, at the operator's risk: here to
	// the new instance's question.
	if out, code := run([]string{"DEVOPSY_SSH_COMMAND=" + filepath.Join(bin, "oldssh"), "DEVOPSY_PROTOCOL_CHECK=warn"}, "@b/prod", "--release"); !strings.Contains(out, "devopsy: warning: the server's devopsy speaks protocol 1, older than this one's") || strings.Contains(out, "lacks what this needs") || !strings.Contains(out, "a new instance") {
		t.Fatalf("an older server, warned (%d):\n%s", code, out)
	}
	// A server whose devopsy lacks a feature (here --release-id, which
	// only the upload calls) refuses only what needs it, naming it, and
	// runs the rest.
	lacks := filepath.Join(tmp, "lacks")
	write(t, filepath.Join(lacks, "devopsy"), "#!/bin/sh\nif [ \"$1\" = --release-settings ]; then "+binary+" --release-settings | sed 's/ release-id / /'; exit; fi\nexec "+binary+" \"$@\"\n", 0o755)
	write(t, filepath.Join(bin, "lacksssh"), "#!/bin/sh\nfor a; do s=$a; done\nPATH="+lacks+":$PATH exec sh -c \"$s\"\n", 0o755)
	lacking := []string{"DEVOPSY_SSH_COMMAND=" + filepath.Join(bin, "lacksssh")}
	if out, code := run(lacking, "@b/prod", "--release", "--yes"); code == 0 || !strings.Contains(out, "the server's devopsy lacks what this needs (release-id): upgrade it (devopsy @devopsy@server: --upgrade)") || strings.Contains(out, "uploaded") {
		t.Fatalf("a server lacking a feature (%d):\n%s", code, out)
	}
	if out, code := run(lacking, "@devopsy@server", "--environments"); code != 0 || strings.Contains(out, "lacks") {
		t.Fatalf("a server lacking a feature, what does not need it (%d):\n%s", code, out)
	}
	// A new instance asks first; without a terminal, --yes.
	if out, code := run(nil, "@b/prod", "--release"); code == 0 || !strings.Contains(out, `devopsy: error: a new instance: devopsy@server has no "b" instance of shop yet`) || !strings.Contains(out, "shop/b/prod, compose project shop-b-prod") || !strings.Contains(out, "--release --yes") {
		t.Fatalf("new instance (%d):\n%s", code, out)
	}
	out, code := run(nil, "@b/prod", "--release", "--yes")
	if code != 0 || !strings.Contains(out, "to devopsy@server:b/prod") || !strings.Contains(out, "deploy shared=yes project=shop instance=b env=prod") {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	data, _ := os.ReadFile(releaseFile(t, filepath.Join(root, "shop", "b", "prod", "current"), remote.TargetEnvFile))
	if !strings.Contains(string(data), "COMPOSE_PROJECT_NAME='shop-b-prod'") || !strings.Contains(string(data), "DEVOPSY_TARGET='devopsy@server:b/prod'") {
		t.Errorf("target.env:\n%s", data)
	}
	// Known now: no question. In a new second: release ids are seconds,
	// and a taken one is refused.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	if out, code := run(nil, "@b/prod", "--release"); code != 0 {
		t.Fatalf("second release (%d):\n%s", code, out)
	}
	if out, code := run([]string{"DEVOPSY_INSTANCE=c"}, "@pr-12", "--release", "--yes"); code != 0 || !strings.Contains(out, "instance=c env=pr-12") {
		t.Fatalf("pattern environment (%d):\n%s", code, out)
	}
	// Only the project's layout is listed: an environment without an
	// instance, made by hand, is not one of its environments.
	if err := os.MkdirAll(filepath.Join(root, "shop", "staging", "releases"), 0o755); err != nil {
		t.Fatal(err)
	}
	// On the server .env names, as addresses; or on the one given.
	if out, code := run(nil, "--environments"); code != 0 || !strings.HasSuffix(out, "devopsy@server:b/prod\ndevopsy@server:c/pr-12\n") {
		t.Fatalf("--environments (%d):\n%s", code, out)
	}
	// On one server, named alone or through an environment.
	if out, code := run(nil, "@other", "--environments"); code != 0 || !strings.Contains(out, "other:b/prod\n") {
		t.Fatalf("@other --environments (%d):\n%s", code, out)
	}
	if out, code := run(nil, "@b/prod", "--environments"); code != 0 || !strings.Contains(out, "devopsy@server:b/prod\n") {
		t.Fatalf("@b/prod --environments (%d):\n%s", code, out)
	}
	// Without an instance: only the server matters.
	if out, code := run(nil, "@prod", "--ssh-config"); code != 0 || !strings.Contains(out, "Host server") {
		t.Fatalf("@prod --ssh-config (%d):\n%s", code, out)
	}
	// The step is the release's own, from its config on the server: the
	// checkout's is not what runs.
	withStep := "project: shop\ninstances: true\ndefaults:\n  mode: image\n  release: deploy\n  destroy: destroy\nenvironments:\n  prod: {}\n  \"pr-*\": {}\n"
	write(t, filepath.Join(root, "shop", "c", "pr-12", "current", ".devopsy", "config.yaml"), withStep, 0o644)
	if out, code := run(nil, "@c/pr-12", "--destroy"); code == 0 || !strings.Contains(out, "add --yes") {
		t.Fatalf("--destroy without a terminal (%d):\n%s", code, out)
	}
	// The step comes from the current release: a failing one removes nothing.
	write(t, filepath.Join(root, "shop", "c", "pr-12", "current", ".devopsy", "commands", "destroy"), "#!/bin/sh\necho \"destroy env=$DEVOPSY_ENVIRONMENT\"\nexit 3\n", 0o755)
	if out, code := run(nil, "@devopsy@server:c/pr-12", "--destroy", "--yes"); code != 3 || !strings.Contains(out, "nothing removed") {
		t.Fatalf("--destroy with a failing step (%d):\n%s", code, out)
	}
	write(t, filepath.Join(root, "shop", "c", "pr-12", "current", ".devopsy", "commands", "destroy"), "#!/bin/sh\necho \"destroy env=$DEVOPSY_ENVIRONMENT\"\n", 0o755)
	write(t, filepath.Join(root, "shop", "c", "pr-12", "shared", "mnt", "db", "data"), "x", 0o644)
	out, code = run(nil, "@devopsy@server:c/pr-12", "--destroy", "--yes")
	if code != 0 || !strings.Contains(out, "destroy env=pr-12") || !strings.Contains(out, "[down][--volumes][--remove-orphans][--rmi][local]") || !strings.Contains(out, "[find][/mnt][-mindepth][1][-delete]") || !strings.Contains(out, "removed") {
		t.Fatalf("--destroy (%d):\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(root, "shop", "c", "pr-12")); !os.IsNotExist(err) {
		t.Fatalf("still there: %v", err)
	}
	// Instance c has no environment left: kept, with a warning saying
	// how; --purge removes it, also once the environment is gone. The
	// project's stays: b is still there.
	if !strings.Contains(out, "devopsy: warning: devopsy@server keeps "+filepath.Join(root, "shop", "c")) || !strings.Contains(out, "devopsy: | $ devopsy @devopsy@server:c/pr-12 --destroy --yes --purge") {
		t.Errorf("no warning about what is left:\n%s", out)
	}
	if out, code := run(nil, "@devopsy@server:c/pr-12", "--destroy", "--yes", "--purge"); code != 0 || !strings.Contains(out, "removed "+filepath.Join(root, "shop", "c")) {
		t.Fatalf("--destroy --purge (%d):\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(root, "shop", "c")); !os.IsNotExist(err) {
		t.Fatalf("instance still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "shop", ".env")); err != nil {
		t.Fatalf("--purge removed the project's .env: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "shop", "b", "prod", "current")); err != nil {
		t.Fatalf("destroy touched another environment: %v", err)
	}
}

// --init writes a config once; --debug schema documents every key.
func TestInitAndSchema(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "My_Shop")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, code := runDevopsy(t, dir, nil, "--init"); code == 0 || !strings.Contains(out, "(my-shop?)") {
		t.Fatalf("no terminal, no name (%d):\n%s", code, out)
	}
	if out, code := runDevopsy(t, dir, nil, "--init", "Bad Name"); code == 0 {
		t.Fatalf("bad name (%d):\n%s", code, out)
	}
	if out, code := runDevopsy(t, dir, nil, "--init", "shop"); code != 0 {
		t.Fatalf("--init (%d):\n%s", code, out)
	}
	data, _ := os.ReadFile(filepath.Join(dir, ".devopsy", "config.yaml"))
	if !strings.Contains(string(data), "project: shop\n") {
		t.Fatalf("config:\n%s", data)
	}
	write(t, filepath.Join(dir, ".devopsy", "config.yaml"), "project: mine\n", 0o644)
	if out, code := runDevopsy(t, dir, nil, "--init", "other"); code != 0 || !strings.Contains(out, "nothing to do") {
		t.Fatalf("existing (%d):\n%s", code, out)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, ".devopsy", "config.yaml")); string(data) != "project: mine\n" {
		t.Fatalf("existing config changed:\n%s", data)
	}
	if out, code := runDevopsy(t, dir, nil, "--debug", "schema"); code != 0 || !strings.Contains(out, "environments:") {
		t.Fatalf("schema (%d):\n%s", code, out)
	}
	if out, code := runDevopsy(t, dir, nil, "--debug", "schema", "--user"); code != 0 || !strings.Contains(out, "aliases:") {
		t.Fatalf("user schema (%d):\n%s", code, out)
	}
}

// The completion scripts in real shells, each one found on this machine:
// what a Tab offers for targets with servers and instances (bash splits
// words at ":"), and after one.
func TestCompletionShells(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dot := filepath.Join(tmp, "app", ".devopsy")
	write(t, filepath.Join(dot, "compose.yaml"), "services: {}\n", 0o644)
	write(t, filepath.Join(dot, "config.yaml"), "project: app\nenvironments:\n  prod: {}\n  staging: {}\n", 0o644)
	bin := t.TempDir()
	if err := os.Symlink(binary, filepath.Join(bin, "devopsy")); err != nil {
		t.Fatal(err)
	}
	// docker's own completion, as cobra ends it: no candidates.
	write(t, filepath.Join(bin, "docker"), "#!/bin/sh\necho :4\n", 0o755)
	path := bin + ":/usr/bin:/bin:/opt/homebrew/bin:/usr/local/bin"
	cases := []struct{ line, want string }{
		{"devopsy @vm1:", "@vm1:prod @vm1:staging"},
		{"devopsy @vm1:b/st", "@vm1:b/staging"},
		{"devopsy -v @st", "@staging"},
		{"devopsy @vm1:prod --shell-h", "--shell-host"},
	}
	run := func(t *testing.T, shell string, args ...string) string {
		t.Helper()
		cmd := exec.Command(shell, args...)
		cmd.Dir = filepath.Join(tmp, "app")
		cmd.Env = []string{"PATH=" + path, "HOME=" + tmp, "DEVOPSY_HOME=" + t.TempDir()}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", shell, err, out)
		}
		return string(out)
	}
	scripts := map[string]string{}
	for _, shell := range []string{"bash", "zsh", "fish"} {
		out, _ := runDevopsy(t, tmp, nil, "--completion", shell)
		file := filepath.Join(tmp, "devopsy."+shell)
		write(t, file, out, 0o644)
		scripts[shell] = file
	}

	for _, shell := range []string{"/bin/bash", "bash", "zsh", "fish"} {
		found, err := exec.LookPath(shell)
		if err != nil {
			t.Logf("%s: not installed, skipped", shell)
			continue
		}
		t.Run(shell, func(t *testing.T) {
			for _, c := range cases {
				var out string
				switch filepath.Base(shell) {
				case "bash":
					// bash's own word breaks, ":" included.
					out = run(t, found, "-c", `source "$1"; COMP_WORDBREAKS=$' \t\n"'"'"'@><=;|&(:'; COMP_LINE="$2"; COMP_POINT=${#2}; _devopsy; echo "${COMPREPLY[*]}"`, "sh", scripts["bash"], c.line)
					// bash completes what follows the last ":" in the word.
					if i := strings.LastIndex(c.line, ":"); i > strings.LastIndex(c.line, " ") {
						prefix := c.line[strings.LastIndex(c.line, " ")+1 : i+1]
						var words []string
						for _, w := range strings.Fields(out) {
							words = append(words, prefix+w)
						}
						out = strings.Join(words, " ")
					}
				case "zsh":
					// compsys needs a terminal: _describe prints what it gets.
					out = run(t, found, "-c", `_describe() { local a=$4 i; for i in "${(@P)a}"; do [[ $i =~ '^(([\\].|[^:\\])*)' ]] && i=$match[1]; print -r -- "${i//\\:/:}"; done; }
zstyle() { return 0 }; _files() { return 1 }; compdef() { : }
source "$1"; words=(${(z)2}); [[ $2 == *' ' ]] && words+=(''); CURRENT=${#words}; _devopsy`, "zsh", scripts["zsh"], c.line)
				case "fish":
					out = run(t, found, "-c", `source $argv[1]; complete -C $argv[2] | string replace -r '\t.*' ''`, scripts["fish"], c.line)
				}
				if got := strings.Join(strings.Fields(out), " "); got != c.want {
					t.Errorf("%q: %q, want %q", c.line, got, c.want)
				}
			}
		})
	}
}

// --ssh-keys edits the deploy user's authorized_keys on the server (here: HOME
// under tmp, the fake ssh running the scripts locally).
func TestRemoteSSHKeys(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	write(t, filepath.Join(bin, "fakessh"), "#!/bin/sh\nfor a; do s=$a; done\nexec sh -c \"$s\"\n", 0o755)
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n", 0o755)
	project := filepath.Join(tmp, "app")
	write(t, filepath.Join(project, ".devopsy", "compose.yaml"), "services: {}\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", "config.yaml"), "project: app\nenvironments:\n  prod: {}\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", ".env"), "DEVOPSY_SERVER=devopsy@server\n", 0o644)
	env := []string{"DEVOPSY_SSH_COMMAND=" + filepath.Join(bin, "fakessh"), "PATH=" + bin + ":" + fakeBin(t) + ":/usr/bin:/bin", "HOME=" + tmp}
	run := func(stdin string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Dir = project
		cmd.Env = append([]string{"DEVOPSY_HOME=" + t.TempDir()}, env...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return string(out), code
	}
	authorized := filepath.Join(tmp, ".ssh", "authorized_keys")
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g sample@test"

	if out, code := run("", "@prod", "--ssh-keys"); code != 0 || !strings.Contains(out, "has no authorized keys") {
		t.Fatalf("empty list (%d):\n%s", code, out)
	}
	// generate: a pair here, no target, then its public half restricted
	// there; the private key never sent.
	priv := filepath.Join(tmp, "ci-key")
	out, code := run("", "--ssh-keys-generate", "ci-server", "--out", priv)
	if code != 0 || !strings.Contains(out, "devopsy: made SHA256:") || !strings.Contains(out, "(ci-server)") || !strings.Contains(out, "private key "+priv+", public key "+priv+".pub") {
		t.Fatalf("generate (%d):\n%s", code, out)
	}
	if _, err := os.Stat(priv); err != nil {
		t.Errorf("private key: %v", err)
	}
	if out, code := run("", "--ssh-keys-generate", "ci-server", "--out", priv); code == 0 || !strings.Contains(out, "exists") {
		t.Errorf("generate over an existing file (%d):\n%s", code, out)
	}
	if out, code := run("", "@prod", "--ssh-keys", "generate"); code == 0 || !strings.Contains(out, "--ssh-keys-generate") {
		t.Errorf("generate with a target (%d):\n%s", code, out)
	}
	if out, code := run("", "--ssh-keys"); code == 0 || !strings.Contains(out, "needs a target") {
		t.Errorf("list without a target (%d):\n%s", code, out)
	}
	out, code = run("", "@prod", "--ssh-keys", "add", priv+".pub", "--restricted")
	if code != 0 || !strings.Contains(out, "devopsy: authorized SHA256:") || !strings.Contains(out, "(ci-server)") || !strings.Contains(out, "devopsy: | restricted:") {
		t.Fatalf("add restricted (%d):\n%s", code, out)
	}
	if got := readString(authorized); !strings.HasPrefix(got, "restrict,agent-forwarding,port-forwarding ssh-ed25519 ") || !strings.Contains(got, " ci-server\n") || strings.Contains(got, "PRIVATE") {
		t.Errorf("authorized_keys after add: %q", got)
	}
	// add a plain key, from a file and from stdin, never twice.
	pub := filepath.Join(tmp, "sample.pub")
	write(t, pub, key+"\n", 0o644)
	if out, code := run("", "@prod", "--ssh-keys", "add", pub); code != 0 || !strings.Contains(out, "devopsy: authorized SHA256:") || !strings.Contains(out, "(sample@test) for devopsy@server") || strings.Contains(out, "restricted:") {
		t.Fatalf("add (%d):\n%s", code, out)
	}
	if out, code := run(key+"\n", "@prod", "--ssh-keys", "add", "-"); code != 3 || !strings.Contains(out, "authorized already") {
		t.Errorf("add twice (%d):\n%s", code, out)
	}
	if out, code := run("", "@prod", "--ssh-keys", "add", priv); code == 0 || !strings.Contains(out, "private key") {
		t.Errorf("add a private key (%d):\n%s", code, out)
	}
	out, code = run("", "@prod", "--ssh-keys")
	if code != 0 || strings.Count(out, "SHA256:") != 2 || !strings.Contains(out, "ed25519  sample@test\n") || !strings.Contains(out, "ci-server  [restrict,agent-forwarding,port-forwarding]") {
		t.Fatalf("list (%d):\n%s", code, out)
	}
	// remove by comment, by fingerprint prefix; never the last one.
	if out, code := run("", "@prod", "--ssh-keys", "remove", "nobody@nowhere"); code != 1 || !strings.Contains(out, "matches none") {
		t.Errorf("remove unknown (%d):\n%s", code, out)
	}
	if out, code := run("", "@prod", "--ssh-keys", "remove", "sample@test"); code != 0 || !strings.Contains(out, "devopsy: removed SHA256:") || strings.Contains(readString(authorized), "sample@test") {
		t.Errorf("remove by comment (%d):\n%s", code, out)
	}
	fp := strings.TrimSpace(strings.SplitN(strings.SplitN(run2(t, run, "@prod", "--ssh-keys"), "SHA256:", 2)[1], " ", 2)[0])
	if out, code := run("", "@prod", "--ssh-keys", "remove", fp[:8]); code == 0 || !strings.Contains(out, "last key") || !strings.Contains(readString(authorized), "ci-server") {
		t.Errorf("remove the last key (%d):\n%s", code, out)
	}
}

// run2 is a test helper returning only the output.
func run2(t *testing.T, run func(string, ...string) (string, int), args ...string) string {
	t.Helper()
	out, _ := run("", args...)
	return out
}

// --release without a target runs defaults:' release steps in place:
// prepare, the variables check, the images as build mode gets them, run.
// A step that only runs on a target refuses before anything runs.
func TestLocalRelease(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, ".devopsy")
	write(t, filepath.Join(dir, "compose.yaml"), "services: {app: {image: x}}\n", 0o644)
	write(t, filepath.Join(dir, "config.yaml"), "project: app\ndefaults:\n  mode: image\n  release: {before: [local: push], prepare: secrets, run: deploy}\nenvironments: {prod: {}}\n", 0o644)
	write(t, filepath.Join(dir, "commands", "secrets"), "#!/bin/sh\n## Description: s\necho SECRETS in $PWD\n", 0o755)
	write(t, filepath.Join(dir, "commands", "deploy"), "#!/bin/sh\n## Description: d\necho DEPLOY\n", 0o755)
	write(t, filepath.Join(dir, "commands", "push"), "#!/bin/sh\n## Description: p\necho PUSH\n", 0o755)
	out, code := runDevopsy(t, tmp, nil, "--release")
	if code != 0 {
		t.Fatalf("--release (%d):\n%s", code, out)
	}
	order := []string{"SECRETS in " + tmp, "[compose]", "[pull][--ignore-buildable]", "[build][--pull]", "DEPLOY"}
	at := 0
	for _, want := range order {
		i := strings.Index(out[at:], want)
		if i < 0 {
			t.Fatalf("--release: %q missing or out of order:\n%s", want, out)
		}
		at += i + len(want)
	}
	if strings.Contains(out, "PUSH") {
		t.Errorf("--release ran a before step:\n%s", out)
	}

	write(t, filepath.Join(dir, "commands", "deploy"), "#!/bin/sh\n## Description: d\n## Run: target\necho DEPLOY\n", 0o755)
	out, code = runDevopsy(t, tmp, nil, "--release")
	if code == 0 || strings.Contains(out, "SECRETS") || !strings.Contains(out, "a release step: deploy runs only on a target") {
		t.Errorf("--release with a target-only step (%d):\n%s", code, out)
	}
}

// --destroy without a target runs defaults:' destroy step in place: with
// --yes, or after the project's name at a terminal; never without either.
func TestLocalDestroy(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, ".devopsy")
	write(t, filepath.Join(dir, "compose.yaml"), "services: {app: {image: x}}\n", 0o644)
	write(t, filepath.Join(dir, "config.yaml"), "project: app\nenvironments: {prod: {}}\n", 0o644)
	write(t, filepath.Join(dir, "commands", "destroy"), "#!/bin/sh\n## Description: d\necho DESTROYED in $PWD\n", 0o755)
	// Without a step: devopsy's part alone, mnt left alone when empty.
	out, code := runDevopsy(t, tmp, nil, "--destroy", "--yes")
	if code != 0 || strings.Contains(out, "DESTROYED") || !strings.Contains(out, "[down][--volumes][--remove-orphans][--rmi][local]") || strings.Contains(out, "[find]") {
		t.Errorf("--destroy without a step (%d):\n%s", code, out)
	}

	write(t, filepath.Join(dir, "config.yaml"), "project: app\ndefaults: {destroy: destroy}\nenvironments: {prod: {}}\n", 0o644)
	out, code = runDevopsy(t, tmp, nil, "--destroy")
	if code == 0 || strings.Contains(out, "DESTROYED") || !strings.Contains(out, "devopsy: error: nothing removed: no terminal to confirm it on") || !strings.Contains(out, "add --yes to go on without asking") {
		t.Errorf("--destroy without --yes or a terminal (%d):\n%s", code, out)
	}
	write(t, filepath.Join(dir, "mnt", "db", "data"), "x", 0o644)
	out, code = runDevopsy(t, tmp, nil, "--destroy", "--yes")
	if code != 0 || !strings.Contains(out, "DESTROYED in "+tmp) || !strings.Contains(out, "[find][/mnt][-mindepth][1][-delete]") {
		t.Errorf("--destroy --yes (%d):\n%s", code, out)
	}
	write(t, filepath.Join(dir, "commands", "destroy"), "#!/bin/sh\n## Description: d\nexit 3\n", 0o755)
	out, code = runDevopsy(t, tmp, nil, "--destroy", "--yes")
	if code != 3 || strings.Contains(out, "[down]") || !strings.Contains(out, "nothing removed") {
		t.Errorf("--destroy with a failing step (%d):\n%s", code, out)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("--destroy removed the project: %v", err)
	}
}

// --releases remove takes releases away by id, a unique prefix or failed,
// after resolving every word: the current release, or a word naming none,
// refuses them all; without a terminal it needs --yes.
func TestReleasesRemove(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	write(t, filepath.Join(bin, "fakessh"), "#!/bin/sh\nfor a; do s=$a; done\nexec sh -c \"$s\"\n", 0o755)
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n", 0o755)
	write(t, filepath.Join(bin, "mv"), "#!/bin/sh\nif [ \"$1\" = -Tf ]; then rm -f \"$3\"; exec /bin/mv \"$2\" \"$3\"; fi\nexec /bin/mv \"$@\"\n", 0o755)
	server := filepath.Join(tmp, "app", "prod")
	project := filepath.Join(tmp, "src", "app")
	dot := filepath.Join(project, ".devopsy")
	write(t, filepath.Join(dot, "compose.yaml"), "services:\n  web:\n    image: x\n", 0o644)
	write(t, filepath.Join(dot, "config.yaml"), "project: app\nenvironments:\n prod:\n  mode: image\n  release: deploy\n", 0o644)
	write(t, filepath.Join(dot, ".env"), "DEVOPSY_SERVER=devopsy@server\n", 0o644)
	write(t, filepath.Join(dot, "commands", "deploy"), "#!/bin/sh\n## Run: target\nexit ${FAIL_REMOTE:-0}\n", 0o755)
	env := []string{"DEVOPSY_SSH_COMMAND=" + filepath.Join(bin, "fakessh"), "PATH=" + bin + ":" + fakeBin(t) + ":/usr/bin:/bin", "HOME=" + tmp}
	run := func(extra []string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(binary, args...)
		cmd.Dir = project
		cmd.Env = append(append([]string{"DEVOPSY_HOME=" + t.TempDir()}, env...), extra...)
		out, _ := cmd.CombinedOutput()
		return string(out), cmd.ProcessState.ExitCode()
	}
	releases := func() []string {
		entries, _ := os.ReadDir(filepath.Join(server, "releases"))
		var ids []string
		for _, e := range entries {
			ids = append(ids, e.Name())
		}
		return ids
	}
	for i, extra := range [][]string{nil, {"FAIL_REMOTE=1"}, nil, {"FAIL_REMOTE=1"}, nil} {
		if i > 0 {
			time.Sleep(1100 * time.Millisecond) // release ids are per second
		}
		if out, code := run(extra, "@prod", "--release"); (code == 0) != (extra == nil) {
			t.Fatalf("release %d (%d):\n%s", i+1, code, out)
		}
	}
	ids := releases()
	if len(ids) != 5 {
		t.Fatalf("releases: %v", ids)
	}
	first, failed1, current := ids[0], ids[1], ids[4]

	if out, code := run(nil, "@prod", "--releases", "remove", first); code == 0 || !strings.Contains(out, "add --yes") && !strings.Contains(out, "Add --yes") {
		t.Errorf("without a terminal or --yes (%d):\n%s", code, out)
	}
	if out, code := run(nil, "@prod", "--releases", "remove", first, current, "--yes"); code == 0 || !strings.Contains(out, "release "+current+" is the current one of devopsy@server:prod: nothing removed") {
		t.Errorf("the current one (%d):\n%s", code, out)
	}
	if out, code := run(nil, "@prod", "--releases", "remove", "1999", "--yes"); code == 0 || !strings.Contains(out, "devopsy@server:prod has no release 1999: nothing removed") {
		t.Errorf("an unknown id (%d):\n%s", code, out)
	}
	if out, code := run(nil, "@prod", "--releases", "drop", first); code == 0 || !strings.Contains(out, "usage: devopsy @devopsy@server:prod --releases [remove <id|failed>... [--yes]]") {
		t.Errorf("another word (%d):\n%s", code, out)
	}
	if got := releases(); len(got) != 5 {
		t.Fatalf("a refused remove removed something: %v", got)
	}

	// failed and an id together, the id by its full name.
	out, code := run(nil, "@prod", "--releases", "remove", "failed", first, "--yes")
	if code != 0 || !strings.Contains(out, "removed release "+failed1) {
		t.Fatalf("remove failed and %s (%d):\n%s", first, code, out)
	}
	if got := strings.Join(releases(), " "); got != ids[2]+" "+ids[4] {
		t.Errorf("left %s, want %s %s", got, ids[2], ids[4])
	}
	if out, code := run(nil, "@prod", "--releases", "remove", "failed", "--yes"); code != 0 || !strings.Contains(out, "devopsy: no failed releases") {
		t.Errorf("no failed releases left (%d):\n%s", code, out)
	}
	// Each release's line, then its images (the fake docker's output).
	if out, code := run(nil, "@prod", "--releases"); code != 0 || !strings.Contains(out, "* "+current) || strings.Count(out, "\n    ") != 2 || strings.Count(out, "\n") != 4 {
		t.Errorf("listing (%d):\n%s", code, out)
	}
}
