package main

import (
	"os"
	"os/exec"
	"path/filepath"
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
	cmd.Env = append([]string{"PATH=" + fakeBin(t) + ":/usr/bin:/bin", "DEVOPSY_HOME=" + t.TempDir()}, env...)
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
	for _, sub := range []string{"--release", "--rollback", "--releases", "--domains", "--shell", "--shell-host"} {
		out, code := runDevopsy(t, filepath.Join(tmp, "app"), nil, "@prod", sub, "--help")
		if code != 0 || !strings.Contains(out, "Usage: devopsy @<target> "+sub) {
			t.Errorf("%s --help (%d):\n%s", sub, code, out)
		}
	}
	out, code := runDevopsy(t, filepath.Join(tmp, "app"), nil, "@prod", "--help")
	if code != 0 || !strings.Contains(out, "<flag> --help") {
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
	for _, sub := range []string{"--release", "--rollback"} {
		out, code := runDevopsy(t, filepath.Join(tmp, "app"), env, "@vm1-traefik", sub)
		if code == 0 || !strings.Contains(out, "is a user-level target") {
			t.Errorf("%s (%d):\n%s", sub, code, out)
		}
	}
	// With source, only from that directory ("~/" is the home directory),
	// where the guard lets it through to the usual release checks.
	write(t, filepath.Join(home, "targets.yaml"), "vm1-traefik:\n  host: nowhere.invalid\n  path: /srv/traefik\n  source: ~/app\n", 0o644)
	write(t, filepath.Join(tmp, "other", ".devopsy", "compose.yaml"), "services: {}\n", 0o644)
	withHome := append([]string{"HOME=" + tmp, "DEVOPSY_SSH_COMMAND=false"}, env...)
	out, code := runDevopsy(t, filepath.Join(tmp, "other"), withHome, "@vm1-traefik", "--release")
	if code == 0 || !strings.Contains(out, "releases only from its source, ~/app") || !strings.Contains(out, filepath.Join(tmp, "other")) {
		t.Errorf("release from another project (%d):\n%s", code, out)
	}
	out, code = runDevopsy(t, filepath.Join(tmp, "app"), withHome, "@vm1-traefik", "--release")
	if strings.Contains(out, "source") || strings.Contains(out, "user-level") || !strings.Contains(out, "release") {
		t.Errorf("release from its source (%d):\n%s", code, out)
	}

	// devopsy's flags for servers, without a target, say so.
	out, code = runDevopsy(t, filepath.Join(tmp, "app"), env, "--release")
	if code == 0 || !strings.Contains(out, "--release needs a target: devopsy @<target> --release (targets: vm1-traefik)") {
		t.Errorf("--release without a target (%d):\n%s", code, out)
	}

	// Outside a project the target resolves (help needs no SSH).
	out, code = runDevopsy(t, t.TempDir(), env, "@vm1-traefik", "--help")
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
	write(t, filepath.Join(project, ".devopsy", "targets.yaml"), "prod:\n  host: devopsy@server\n  path: "+server+"\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", ".env"), "REG_USER=deploy\nREG_PASSWORD='p@ss word'\n", 0o644)
	env := []string{"DEVOPSY_SSH_COMMAND=" + filepath.Join(bin, "fakessh"), "PATH=" + bin + ":" + fakeBin(t) + ":/usr/bin:/bin"}
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

	// A user-level target never takes values from the project's .env.
	home := t.TempDir()
	box := filepath.Join(tmp, "srv", "box")
	write(t, filepath.Join(home, "targets.yaml"), "box:\n  host: devopsy@server\n  path: "+box+"\n", 0o644)
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
	write(t, filepath.Join(dot, "targets.yaml"), "prod:\n  path: /srv/app-prod\n", 0o644)
	write(t, filepath.Join(dot, "commands", "deploy"), "#!/bin/sh\n## Description: Roll out\n", 0o755)
	// docker's own completion, as cobra prints it: compose commands, one
	// clashing with the project's deploy.
	bin := t.TempDir()
	write(t, filepath.Join(bin, "docker"), "#!/bin/sh\n[ \"$1\" = __complete ] || exit 1\nprintf 'deploy\\tcompose deploy\\nup\\tCreate and start containers\\n:4\\n'\n", 0o755)
	env := []string{"PATH=" + bin + ":/usr/bin:/bin"}

	out, code := runDevopsy(t, root, env, "--complete", "")
	want := "@prod\t/srv/app-prod\ttarget\ndeploy\tRoll out\tproject\nup\tCreate and start containers\tcompose\n:4\n"
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

// release and rollback run the target's steps: before (local), the upload and
// switch, remote (on the server), after (local). With an ssh that runs the
// scripts here.
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
	write(t, filepath.Join(dot, "compose.yaml"), "services: {}\n", 0o644)
	targets := "prod:\n  host: devopsy@server\n  path: " + server + "\n  env:\n    SITE: one\n" +
		"  release:\n    before: check\n    remote: deploy --fast\n    after: [done]\n"
	write(t, filepath.Join(dot, "targets.yaml"), targets, 0o644)
	write(t, filepath.Join(dot, "commands", "check"), "#!/bin/sh\necho \"check target=$DEVOPSY_TARGET site=$SITE\"\nexit ${FAIL_BEFORE:-0}\n", 0o755)
	write(t, filepath.Join(dot, "commands", "deploy"), "#!/bin/sh\necho \"deploy $* in $(basename \"$(readlink \"$DEVOPSY_PROJECT_DIR/..\" 2>/dev/null || dirname \"$DEVOPSY_PROJECT_DIR\")\")\"\nexit ${FAIL_REMOTE:-0}\n", 0o755)
	write(t, filepath.Join(dot, "commands", "done"), "#!/bin/sh\necho after ran\n", 0o755)
	env := []string{"DEVOPSY_SSH_COMMAND=" + filepath.Join(bin, "fakessh"), "PATH=" + bin + ":" + fakeBin(t) + ":/usr/bin:/bin"}
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

	// A failing before step stops before the server.
	if out, code := run([]string{"FAIL_BEFORE=3"}, "@prod", "--release"); code == 0 || !strings.Contains(out, "check target=prod site=one") || !strings.Contains(out, "nothing changed on prod") {
		t.Fatalf("before failure (%d):\n%s", code, out)
	}
	if _, err := os.Stat(server); !os.IsNotExist(err) {
		t.Fatalf("before failure touched the server: %v", err)
	}

	out, code := run(nil, "@prod", "--release")
	if code != 0 || !strings.Contains(out, "deploy --fast in") || !strings.Contains(out, "after ran") {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	first := current()
	if first == "" {
		t.Fatal("no current release")
	}

	// A failing remote step goes back to the previous release, and skips after.
	time.Sleep(1100 * time.Millisecond) // release ids are per second
	out, code = run([]string{"FAIL_REMOTE=4"}, "@prod", "--release")
	if code != 4 || strings.Contains(out, "after ran") || current() != first {
		t.Fatalf("remote failure (%d), current %s, want %s:\n%s", code, current(), first, out)
	}

	// rollback needs its own steps.
	if out, code := run(nil, "@prod", "--rollback"); code == 0 || !strings.Contains(out, "@prod has no rollback steps") || !strings.Contains(out, "remote: up -d --wait") {
		t.Fatalf("rollback without steps (%d):\n%s", code, out)
	}
	// With rollback steps: back to the first release, running its remote.
	write(t, filepath.Join(dot, "targets.yaml"), targets+"  rollback:\n    remote: deploy --back\n", 0o644)
	time.Sleep(1100 * time.Millisecond)
	if out, code := run(nil, "@prod", "--release"); code != 0 || current() == first {
		t.Fatalf("second release (%d):\n%s", code, out)
	}
	if out, code := run(nil, "@prod", "--rollback"); code != 0 || !strings.Contains(out, "deploy --back in") || current() != first {
		t.Fatalf("rollback (%d), current %s, want %s:\n%s", code, current(), first, out)
	}

	// Unknown keys are refused, not ignored.
	write(t, filepath.Join(dot, "targets.yaml"), strings.Replace(targets, "remote:", "remotes:", 1), 0o644)
	if out, code := run(nil, "@prod", "--release"); code == 0 || !strings.Contains(out, `unknown key "remotes"`) {
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
	write(t, filepath.Join(project, ".devopsy", "targets.yaml"), "prod:\n  host: devopsy@server\n  path: /srv/app-prod\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", "commands", "nested"), "#!/bin/sh\nexec devopsy @prod ps\n", 0o755)
	out, code := runDevopsy(t, project, []string{"DEVOPSY_SSH_COMMAND=echo"}, "nested")
	if code != 0 || !strings.Contains(out, `COMPOSE_PROJECT_NAME='\''app-prod'\'' exec devopsy`) {
		t.Errorf("(%d):\n%s", code, out)
	}
}
