package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

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
	write(t, filepath.Join(dot, "config.yaml"), "project: app\nenvironments:\n  prod: {}\n", 0o644)
	for _, sub := range []string{"--release", "--rollback", "--releases", "--shell", "--shell-host", "--env", "--debug"} {
		out, code := runDevopsy(t, filepath.Join(tmp, "app"), nil, "@prod", sub, "--help")
		if code != 0 || !strings.Contains(out, "Usage: devopsy @<target> "+sub) {
			t.Errorf("%s --help (%d):\n%s", sub, code, out)
		}
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
	out, code = runDevopsy(t, filepath.Join(tmp, "app"), withHome, "--release")
	if code == 0 || !strings.Contains(out, "--release needs a target: devopsy @<target> --release (targets: main, vm1-traefik)") {
		t.Errorf("--release without a target (%d):\n%s", code, out)
	}

	// Outside a project the target resolves (help needs no SSH).
	out, code = runDevopsy(t, t.TempDir(), withHome, "@vm1-traefik", "--help")
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
	write(t, filepath.Join(project, ".devopsy", "config.yaml"), "project: app\nenvironments:\n  prod: {}\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", ".env"), "DEVOPSY_SERVER=devopsy@from-dotenv\n", 0o644)
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
	write(t, filepath.Join(project, ".devopsy", "config.yaml"), "project: app\nenvironments:\n  prod: {}\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", ".env"), "DB_PASSWORD=dotenv-secret-value\n", 0o644)
	// true stands in for ssh: only devopsy's own output remains. Values from
	// .env are masked, so the server comes from the environment here.
	out, code := runDevopsy(t, project, []string{"DEVOPSY_SSH_COMMAND=true", "DEVOPSY_SERVER=devopsy@server"}, "-v", "@prod", "exec", "db", "dotenv-secret-value")
	if code != 0 || !strings.Contains(out, "devopsy: ssh -T devopsy@server, running:") ||
		!strings.Contains(out, "DEVOPSY_VERBOSE=1 COMPOSE_PROJECT_NAME=") ||
		!strings.Contains(out, "'exec' 'db' '***'") || strings.Contains(out, "dotenv-secret-value") {
		t.Errorf("(%d):\n%s", code, out)
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
	// ssh -T <host> 'sh -c <script>': run the last argument here.
	write(t, filepath.Join(bin, "fakessh"), "#!/bin/sh\nshift 2\nexec sh -c \"$1\"\n", 0o755)
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n", 0o755)
	server := filepath.Join(tmp, "srv", "app-prod")
	project := filepath.Join(tmp, "app")
	write(t, filepath.Join(project, ".devopsy", "compose.yaml"), "services: {}\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", "config.yaml"), "project: app\nenvironments:\n  prod:\n    path: "+server+"\n", 0o644)
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
	if out, code := run("", "@prod", "--vars", "set", "REG_USER", "REG_PASSWORD"); code != 0 || !strings.Contains(out, "Set REG_USER, REG_PASSWORD in devopsy@server:"+server+"/shared/.env") {
		t.Fatalf("set (%d):\n%s", code, out)
	}
	if out, code := run("from-stdin\n", "@prod", "--vars", "set", "TOKEN"); code != 0 {
		t.Fatalf("set from stdin (%d):\n%s", code, out)
	}
	got, _ := os.ReadFile(filepath.Join(server, "shared", ".env"))
	if want := "REG_USER='deploy'\nREG_PASSWORD='p@ss word'\nTOKEN='from-stdin'\n"; string(got) != want {
		t.Fatalf(".env\n got %q\nwant %q", got, want)
	}

	if out, code := run("", "@prod", "--vars"); code != 0 || !strings.Contains(out, "REG_PASSWORD\nREG_USER\nTOKEN\n") || strings.Contains(out, "p@ss") {
		t.Fatalf("list (%d):\n%s", code, out)
	}
	if out, code := run("", "@prod", "--vars", "get", "REG_PASSWORD"); code != 0 || out != "p@ss word\n" {
		t.Fatalf("get (%d): %q", code, out)
	}
	if out, code := run("", "@prod", "--vars", "unset", "TOKEN"); code != 0 || !strings.Contains(out, "Unset TOKEN") {
		t.Fatalf("unset (%d):\n%s", code, out)
	}
	if out, code := run("", "@prod", "--vars", "get", "TOKEN"); code == 0 || !strings.Contains(out, "TOKEN is not set") {
		t.Fatalf("get unset (%d):\n%s", code, out)
	}

	// --help anywhere after --vars only explains; --show is for set only.
	if out, code := run("", "@prod", "--vars", "set", "--help"); code != 0 || !strings.Contains(out, "Usage: devopsy @<target> --vars") {
		t.Fatalf("set --help (%d):\n%s", code, out)
	}
	if out, code := run("", "@prod", "--vars", "get", "--show", "TOKEN"); code == 0 || !strings.Contains(out, "--show only applies to set") {
		t.Fatalf("get --show (%d):\n%s", code, out)
	}
	// Without a terminal, --show still reads stdin.
	if out, code := run("shown\n", "@prod", "--vars", "set", "--show", "SHOWN"); code != 0 {
		t.Fatalf("set --show (%d):\n%s", code, out)
	}
	if out, _ := run("", "@prod", "--vars", "get", "SHOWN"); out != "shown\n" {
		t.Fatalf("get SHOWN: %q", out)
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
	write(t, filepath.Join(dot, "config.yaml"), "project: app\nenvironments:\n  prod:\n    path: /srv/app-prod\n", 0o644)
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
	write(t, filepath.Join(bin, "fakessh"), "#!/bin/sh\nshift 2\nexec sh -c \"$1\"\n", 0o755)
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n", 0o755)
	// GNU mv -T, which macOS lacks: replace the symlink, never move into it.
	write(t, filepath.Join(bin, "mv"), "#!/bin/sh\nif [ \"$1\" = -Tf ]; then rm -f \"$3\"; exec /bin/mv \"$2\" \"$3\"; fi\nexec /bin/mv \"$@\"\n", 0o755)
	server := filepath.Join(tmp, "srv", "app-prod")
	project := filepath.Join(tmp, "app")
	dot := filepath.Join(project, ".devopsy")
	write(t, filepath.Join(dot, "compose.yaml"), "services:\n  web:\n    image: x\n    environment:\n      SECRET: ${SECRET:?generated by secrets}\n      NEEDED: ${NEEDED:?set it per environment}\n", 0o644)
	targets := "project: app\nenvironments:\n prod:\n  path: " + server + "\n  mode: image\n  env:\n    SITE: one\n" +
		"  release:\n    before: [local: check, remote: mark before]\n    prepare: secrets\n    run: deploy --fast\n    after: [remote: mark after, local: done]\n"
	write(t, filepath.Join(dot, "config.yaml"), targets, 0o644)
	write(t, filepath.Join(dot, ".env"), "DEVOPSY_SERVER=devopsy@server\n", 0o644)
	write(t, filepath.Join(dot, "commands", "check"), "#!/bin/sh\necho \"check target=$DEVOPSY_TARGET site=$SITE\"\nexit ${FAIL_BEFORE:-0}\n", 0o755)
	write(t, filepath.Join(dot, "commands", "mark"), "#!/bin/sh\necho \"mark $*\"\n", 0o755)
	write(t, filepath.Join(dot, "commands", "secrets"), "#!/bin/sh\ngrep -q '^SECRET=' \"$DEVOPSY_PROJECT_DIR/.env\" || echo SECRET=generated >> \"$DEVOPSY_PROJECT_DIR/.env\"\n", 0o755)
	write(t, filepath.Join(dot, "commands", "deploy"), "#!/bin/sh\necho \"deploy $* in $(basename \"$(readlink \"$DEVOPSY_PROJECT_DIR/..\" 2>/dev/null || dirname \"$DEVOPSY_PROJECT_DIR\")\")\"\n[ \"$1\" = --back ] || exit ${FAIL_REMOTE:-0}\n", 0o755)
	write(t, filepath.Join(dot, "commands", "done"), "#!/bin/sh\necho after ran\n", 0o755)
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
	next := func() { time.Sleep(1100 * time.Millisecond) } // release ids are per second

	if out, code := run(nil, "@prod", "--release", "deploy"); code == 0 || !strings.Contains(out, "--release takes no command") {
		t.Fatalf("release with a command (%d):\n%s", code, out)
	}

	// A failing local before step stops before the server.
	if out, code := run([]string{"FAIL_BEFORE=3"}, "@prod", "--release"); code == 0 || !strings.Contains(out, "check target=devopsy@server:prod site=one") || !strings.Contains(out, "nothing changed on devopsy@server:prod") {
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

	next()
	write(t, filepath.Join(server, "shared", ".env"), "SECRET=generated\nNEEDED=1\n", 0o644)
	out, code = run(nil, "@prod", "--release")
	if code != 0 || !strings.Contains(out, "deploy --fast in") || !strings.Contains(out, "mark after") || !strings.Contains(out, "after ran") {
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

	// A failing run goes back to the previous release, restarts it with its
	// rollback's run step, and skips after. Remote before steps ran in it.
	write(t, filepath.Join(dot, "config.yaml"), targets+"  rollback: deploy --back\n", 0o644)
	next()
	out, code = run([]string{"FAIL_REMOTE=4"}, "@prod", "--release")
	if code != 4 || !strings.Contains(out, "mark before") || !strings.Contains(out, "current is back to") || !strings.Contains(out, "deploy --back in") || strings.Contains(out, "after ran") || current() != first {
		t.Fatalf("run failure (%d), current %s, want %s:\n%s", code, current(), first, out)
	}

	// rollback: back to the first release, running its run step.
	next()
	if out, code := run(nil, "@prod", "--release"); code != 0 || current() == first {
		t.Fatalf("second release (%d):\n%s", code, out)
	}
	if out, code := run(nil, "@prod", "--rollback"); code != 0 || !strings.Contains(out, "deploy --back in") || current() != first {
		t.Fatalf("rollback (%d), current %s, want %s:\n%s", code, current(), first, out)
	}
	// The rollback's log is appended to the restored release's.
	if data, err := os.ReadFile(filepath.Join(server, first, ".devopsy-log")); err != nil || !strings.Contains(string(data), "release exited 0") || !strings.Contains(string(data), "rollback exited 0") {
		t.Fatalf("rollback log: %v\n%s", err, data)
	}

	// rollback needs its own steps.
	write(t, filepath.Join(dot, "config.yaml"), targets, 0o644)
	if out, code := run(nil, "@prod", "--rollback"); code == 0 || !strings.Contains(out, "environment prod has no rollback steps") || !strings.Contains(out, "rollback: up -d --wait") {
		t.Fatalf("rollback without steps (%d):\n%s", code, out)
	}

	// Unknown keys are refused, not ignored.
	write(t, filepath.Join(dot, "config.yaml"), strings.Replace(targets, "run:", "runs:", 1), 0o644)
	if out, code := run(nil, "@prod", "--release"); code == 0 || !strings.Contains(out, `unknown key "runs"`) {
		t.Fatalf("unknown key (%d):\n%s", code, out)
	}
}

// A project command calling devopsy @target inherits the local project's
// COMPOSE_PROJECT_NAME; the target's own name must win.
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
}

// --debug targets shows computed targets and where each value came from.
func TestDebugTargets(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dot := filepath.Join(tmp, "app", ".devopsy")
	write(t, filepath.Join(dot, "compose.yaml"), "services:\n  web:\n    image: x\n    labels:\n      - devopsy.shell=true\n", 0o644)
	write(t, filepath.Join(dot, "config.yaml"), "project: app\ndefaults:\n  mode: image\n  env:\n    A: one\nenvironments:\n  prod:\n    path: /srv/app\n    env:\n      B: two\n", 0o644)
	env := []string{"DEVOPSY_HOME=" + t.TempDir(), "DEVOPSY_SERVER=h"}
	out, code := runDevopsy(t, filepath.Join(tmp, "app"), env, "--debug", "environments", "prod")
	for _, want := range []string{"prod  (", "mode      image  (defaults in ", "env       A='one'  (defaults in ", "env       B='two'  (" + filepath.Join(dot, "config.yaml")} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("missing %q (%d):\n%s", want, code, out)
		}
	}
	out, code = runDevopsy(t, filepath.Join(tmp, "app"), env, "--debug")
	if code != 0 || !regexp.MustCompile(`(?m)^environments +prod$`).MatchString(out) || !strings.Contains(out, "web: devopsy.shell=true") {
		t.Errorf("summary (%d):\n%s", code, out)
	}
	out, code = runDevopsy(t, filepath.Join(tmp, "app"), env, "--debug", "environments", "--yaml")
	if code != 0 || !strings.Contains(out, "prod:\n  target: h:prod\n  server: h\n  path: /srv/app\n  mode: image\n") || !strings.Contains(out, "    A: one") {
		t.Errorf("--yaml (%d):\n%s", code, out)
	}
	out, code = runDevopsy(t, filepath.Join(tmp, "app"), env, "--debug", "capabilities")
	if code != 0 || !strings.Contains(out, "open [service]") || !strings.Contains(out, "does not implement it") {
		t.Errorf("capabilities (%d):\n%s", code, out)
	}
}

// --prepare-release, --debug labels and --debug imports against a docker
// that answers ps and inspect like a host running a proxy.
func TestRolesEndToEnd(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(tmp, "releases", "1")
	dot := filepath.Join(rel, ".devopsy")
	write(t, filepath.Join(dot, "compose.yaml"), "services:\n  web:\n    image: x\n    labels: [devopsy.import.DEVOPSY_WILDCARD_DOMAIN=proxy/WILDCARD_DOMAIN]\n", 0o644)
	write(t, filepath.Join(dot, "target.env"), "COMPOSE_PROJECT_NAME='shop'\n", 0o644)
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
	data, _ := os.ReadFile(filepath.Join(dot, "target.env"))
	if code != 0 || !strings.Contains(out, "DEVOPSY_WILDCARD_DOMAIN=vm1.example.com, from proxy (traefik-main)") || !strings.HasSuffix(string(data), "DEVOPSY_WILDCARD_DOMAIN='vm1.example.com'\n") {
		t.Fatalf("--prepare-release (%d):\n%s\n%s", code, out, data)
	}
	out, code = runDevopsy(t, rel, env, "--debug", "labels")
	if code != 0 || !strings.Contains(out, "role proxy  held by traefik-main") || !strings.Contains(out, "exports WILDCARD_DOMAIN=vm1.example.com") || !strings.Contains(out, `this release imported: "vm1.example.com"`) {
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

// Instances, the server's release root, the project's .env, --instances and
// --destroy, with an ssh that runs the scripts here.
func TestInstancesEndToEnd(t *testing.T) {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	write(t, filepath.Join(bin, "fakessh"), "#!/bin/sh\nshift 2\nexec sh -c \"$1\"\n", 0o755)
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n", 0o755)
	write(t, filepath.Join(bin, "mv"), "#!/bin/sh\nif [ \"$1\" = -Tf ]; then rm -f \"$3\"; exec /bin/mv \"$2\" \"$3\"; fi\nexec /bin/mv \"$@\"\n", 0o755)
	root := filepath.Join(tmp, "srv")
	home := filepath.Join(tmp, "home")
	write(t, filepath.Join(home, "config.yaml"), "releases:\n  root: "+root+"\n", 0o644)
	project := filepath.Join(tmp, "app")
	dot := filepath.Join(project, ".devopsy")
	write(t, filepath.Join(dot, "compose.yaml"), "services: {}\n", 0o644)
	write(t, filepath.Join(dot, "config.yaml"), "project: shop\ninstances: required\ndefaults:\n  mode: image\n  release: deploy\nenvironments:\n  prod: {}\n  \"pr-*\": {}\n", 0o644)
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
	if out, code := run([]string{"SHARED=yes"}, "@b/prod", "--vars", "--project", "set", "SHARED"); code != 0 || !strings.Contains(out, filepath.Join(root, "shop", ".env")) {
		t.Fatalf("--vars --project (%d):\n%s", code, out)
	}
	// A new instance asks first; without a terminal, --yes.
	if out, code := run(nil, "@b/prod", "--release"); code == 0 || !strings.Contains(out, `New instance: devopsy@server has no "b" instance of shop yet.`) || !strings.Contains(out, "shop/b/prod, compose project shop-b-prod") || !strings.Contains(out, "--release --yes") {
		t.Fatalf("new instance (%d):\n%s", code, out)
	}
	out, code := run(nil, "@b/prod", "--release", "--yes")
	if code != 0 || !strings.Contains(out, "to devopsy@server:b/prod") || !strings.Contains(out, "deploy shared=yes project=shop instance=b env=prod") {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	data, _ := os.ReadFile(filepath.Join(root, "shop", "b", "prod", "current", ".devopsy", "target.env"))
	if !strings.Contains(string(data), "COMPOSE_PROJECT_NAME='shop-b-prod'") || !strings.Contains(string(data), "DEVOPSY_TARGET='devopsy@server:b/prod'") {
		t.Errorf("target.env:\n%s", data)
	}
	// Known now: no question.
	if out, code := run(nil, "@b/prod", "--release"); code != 0 {
		t.Fatalf("second release (%d):\n%s", code, out)
	}
	if out, code := run([]string{"DEVOPSY_INSTANCE=c"}, "@pr-12", "--release", "--yes"); code != 0 || !strings.Contains(out, "instance=c env=pr-12") {
		t.Fatalf("pattern environment (%d):\n%s", code, out)
	}
	if out, code := run(nil, "@b/prod", "--instances"); code != 0 || !strings.Contains(out, "b") || !strings.Contains(out, "prod") || !strings.Contains(out, "c") || !strings.Contains(out, "pr-12") {
		t.Fatalf("--instances (%d):\n%s", code, out)
	}
	if out, code := run(nil, "@c/pr-12", "--destroy"); code == 0 || !strings.Contains(out, "add --yes") {
		t.Fatalf("--destroy without a terminal (%d):\n%s", code, out)
	}
	if out, code := run(nil, "@devopsy@server:c/pr-12", "--destroy", "--yes"); code != 0 || !strings.Contains(out, "removed") {
		t.Fatalf("--destroy (%d):\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(root, "shop", "c", "pr-12")); !os.IsNotExist(err) {
		t.Fatalf("still there: %v", err)
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
