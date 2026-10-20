//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// image is the servers' and the operator's image (Dockerfile here).
const image = "devopsy-e2e:1"

// Containers of one run: names end with its id, so runs never meet.
var (
	run      string
	network  string
	server   string
	operator string
	// server2 is a second server, started by the first test that needs
	// one (secondServer); bin is devopsy for it.
	server2 string
	bin     string
	// goarch is the daemon's architecture, which devopsy is built for.
	goarch string
)

func TestMain(m *testing.M) {
	code, err := setUp()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: "+err.Error())
		code = 1
	} else {
		code = m.Run()
	}
	if os.Getenv("E2E_KEEP") == "" {
		tearDown()
	} else {
		fmt.Fprintf(os.Stderr, "e2e: kept %s and %s on %s\n", server, operator, network)
	}
	os.Exit(code)
}

// setUp builds the image and devopsy for the daemon's architecture,
// starts a server and an operator, and lets the operator reach the
// server's deploy user over SSH.
func setUp() (int, error) {
	if _, err := docker("version"); err != nil {
		return 0, fmt.Errorf("docker is needed: %v", err)
	}
	run = fmt.Sprint(os.Getpid())
	network, server, operator = "devopsy-e2e-"+run, "devopsy-e2e-server-"+run, "devopsy-e2e-op-"+run
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file)
	if out, err := docker("build", "-q", "-t", image, dir); err != nil {
		return 0, fmt.Errorf("building %s: %v\n%s", image, err, out)
	}
	arch, err := docker("info", "--format", "{{.Architecture}}")
	if err != nil {
		return 0, err
	}
	goarch = map[string]string{"x86_64": "amd64", "aarch64": "arm64"}[strings.TrimSpace(arch)]
	if goarch == "" {
		return 0, fmt.Errorf("unknown architecture %q", arch)
	}
	bin = filepath.Join(os.TempDir(), "devopsy-e2e-"+run)
	build := exec.Command("go", "build", "-o", bin, "./cmd/devopsy")
	build.Dir = filepath.Dir(dir)
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("building devopsy: %v\n%s", err, out)
	}

	if out, err := docker("network", "create", network); err != nil {
		return 0, fmt.Errorf("%v: %s", err, out)
	}
	if err := start(operator, "operator"); err != nil {
		return 0, err
	}
	// The operator's key, authorized for the deploy user of both servers,
	// and an agent holding it, for syncs across servers (agent()).
	if out, err := docker("exec", operator, "sh", "-c", `ssh-keygen -q -t ed25519 -N '' -f /root/.ssh/id_ed25519 && printf 'Host server server2\n  User devopsy\n  StrictHostKeyChecking accept-new\n' >/root/.ssh/config && chmod 600 /root/.ssh/config && ssh-agent -a /tmp/agent.sock >/dev/null && SSH_AUTH_SOCK=/tmp/agent.sock ssh-add -q`); err != nil {
		return 0, fmt.Errorf("%v: %s", err, out)
	}
	if err := startServer(server, "server"); err != nil {
		return 0, err
	}
	return 0, nil
}

// start runs the image as role (server or operator) in a container
// named name, reached as role on the network, with devopsy.
func start(name, role string) error {
	args := []string{"run", "-d", "--privileged", "--name", name, "--hostname", role, "--network", network, "--network-alias", role}
	if role != "operator" {
		args = append(args, "-v", "/var/lib/docker")
	}
	mode := "server"
	if role == "operator" {
		mode = "operator"
	}
	if out, err := docker(append(args, image, mode)...); err != nil {
		return fmt.Errorf("starting %s: %v\n%s", name, err, out)
	}
	if out, err := docker("cp", bin, name+":/usr/local/bin/devopsy"); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

// startServer starts a server reached as alias, authorizes the
// operator's key for its deploy user, and waits for its dockerd and SSH.
func startServer(name, alias string) error {
	if err := start(name, alias); err != nil {
		return err
	}
	pub, err := docker("exec", operator, "cat", "/root/.ssh/id_ed25519.pub")
	if err != nil {
		return err
	}
	add := exec.Command("docker", "exec", "-i", "-u", "devopsy", name, "sh", "-c", "cat >>~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys")
	add.Stdin = strings.NewReader(pub)
	if out, err := add.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	// The server's dockerd, then SSH all the way.
	for i := 0; ; i++ {
		if _, err := docker("exec", "-u", "devopsy", name, "docker", "version"); err == nil {
			break
		}
		if i > 60 {
			log, _ := docker("exec", name, "cat", "/var/log/dockerd.log")
			return fmt.Errorf("%s's dockerd did not start:\n%s", alias, log)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if out, err := docker("exec", operator, "ssh", alias, "true"); err != nil {
		return fmt.Errorf("ssh to %s: %v\n%s", alias, err, out)
	}
	return nil
}

// secondServer starts server2, once, for the tests that sync across
// servers.
func secondServer(t *testing.T) {
	t.Helper()
	if server2 != "" {
		return
	}
	name := "devopsy-e2e-server2-" + run
	if err := startServer(name, "server2"); err != nil {
		_, _ = docker("rm", "-f", "-v", name)
		t.Fatal(err)
	}
	server2 = name
}

func tearDown() {
	_, _ = docker("rm", "-f", "-v", server, operator)
	if server2 != "" {
		_, _ = docker("rm", "-f", "-v", server2)
	}
	_, _ = docker("network", "rm", network)
	os.Remove(bin)
}

// docker runs docker args and returns its combined output.
func docker(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

// op runs script on the operator, in dir (a project there), and returns
// its output and exit code.
func op(t *testing.T, dir, script string) (string, int) {
	t.Helper()
	return execIn(t, []string{"-w", dir, operator}, script)
}

// srv runs script on the server as the deploy user.
func srv(t *testing.T, script string) (string, int) {
	t.Helper()
	return execIn(t, []string{"-u", "devopsy", "-w", "/home/devopsy", server}, script)
}

// srv2 runs script on the second server (secondServer) as the deploy
// user.
func srv2(t *testing.T, script string) (string, int) {
	t.Helper()
	return execIn(t, []string{"-u", "devopsy", "-w", "/home/devopsy", server2}, script)
}

// agent is the operator's environment with its SSH agent, which a sync
// across servers forwards to the parent's.
const agent = "SSH_AUTH_SOCK=/tmp/agent.sock"

// loginShell makes shell the deploy user's login shell on the server
// for the test: the image's is dash (/bin/sh), devopsy-server's bash,
// and they run sshd's command differently (bash execs it, dash does
// not), which the watcher's signal depends on.
func loginShell(t *testing.T, shell string) {
	t.Helper()
	if out, err := docker("exec", server, "usermod", "-s", shell, "devopsy"); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	t.Cleanup(func() { _, _ = docker("exec", server, "usermod", "-s", "/bin/sh", "devopsy") })
}

// inTerminal starts command on the operator, in dir, on a terminal of
// its own (script), its output in log there and its exit code, once it
// ends, in log.code (terminalCode). Flushed as it comes, so a test can
// read the log while the command runs.
func inTerminal(t *testing.T, dir, command, log string) {
	t.Helper()
	op(t, "/", "rm -f "+log+" "+log+".code")
	background(t, dir, `script -qfec "`+strings.ReplaceAll(command, `"`, `\"`)+`; echo exit=\$? >`+log+`.code" `+log, "/dev/null")
}

// terminalCode waits for inTerminal's command to end and returns
// "exit=<code>".
func terminalCode(t *testing.T, log string) string {
	t.Helper()
	var out string
	eventually(t, 2*time.Minute, "devopsy's end", func() bool {
		out, _ = op(t, "/", "cat "+log+".code 2>/dev/null")
		return out != ""
	})
	return strings.TrimSpace(out)
}

func execIn(t *testing.T, where []string, script string) (string, int) {
	t.Helper()
	cmd := exec.Command("docker", append(append([]string{"exec"}, where...), "sh", "-c", script)...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	code := 0
	if err != nil {
		code = -1
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		}
	}
	return out.String(), code
}

// beat is the heartbeat's beat for the tests, seconds: E2E_BEAT, else 1,
// so a silence counts in seconds (remote.QuietBeats are 6 of them);
// E2E_BEAT=5 runs everything with the real timeouts.
func beat() int {
	if n, err := strconv.Atoi(os.Getenv("E2E_BEAT")); err == nil && n > 0 {
		return n
	}
	return 1
}

// fast is the operator's environment with the tests' beat.
func fast() string {
	return "DEVOPSY_INTERNAL_HEARTBEAT_EVERY=" + strconv.Itoa(beat())
}

// quiet is beats of the tests' beat, plus slack for what follows.
func quiet(beats int, slack time.Duration) time.Duration {
	return time.Duration(beats*beat())*time.Second + slack
}

// dropConnection cuts, silently, only the operator's ssh session whose
// command line matches pattern (a pgrep -f pattern: bracket a letter so
// it never matches itself).
func dropConnection(t *testing.T, pattern string) {
	t.Helper()
	script := `pid=$(pgrep -f '` + pattern + `' | head -n 1) && [ -n "$pid" ] && port=$(ss -tnpH | grep "pid=$pid," | awk '{print $4}' | sed 's/.*://' | head -n 1) && [ -n "$port" ] && iptables -A OUTPUT -p tcp --sport "$port" -j DROP && iptables -A INPUT -p tcp --dport "$port" -j DROP`
	if out, code := op(t, "/", script); code != 0 {
		t.Fatalf("dropping %s (%d): %s", pattern, code, out)
	}
	t.Cleanup(func() { undrop(t) })
}

// throttle slows the operator's traffic to rate (tc's), so a transfer
// lasts long enough to cut.
func throttle(t *testing.T, rate string) {
	t.Helper()
	if out, code := op(t, "/", "tc qdisc add dev eth0 root tbf rate "+rate+" burst 32kbit latency 400ms"); code != 0 {
		t.Fatalf("throttling (%d): %s", code, out)
	}
	t.Cleanup(func() { op(t, "/", "tc qdisc del dev eth0 root || true") })
}

// lockFree reports whether nothing holds the lock file path (relative to
// the deploy user's home) on the server.
func lockFree(t *testing.T, path string) bool {
	_, code := srv(t, "flock -n "+quote(path)+" true")
	return code == 0
}

// background starts command on the operator, in dir, its output in
// log there.
func background(t *testing.T, dir, command, log string) {
	t.Helper()
	if out, code := op(t, dir, "nohup sh -c "+quote(command)+" >"+log+" 2>&1 </dev/null &"); code != 0 {
		t.Fatalf("starting %s (%d): %s", command, code, out)
	}
}

// eventually waits until cond holds, failing after within.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !cond() {
		if time.Since(start) > within {
			t.Fatalf("%s: not after %s", what, within)
		}
		time.Sleep(250 * time.Millisecond)
	}
	return time.Since(start)
}

// dropSilently cuts the operator from the server without a FIN or an
// RST, as a laptop asleep: both directions dropped. undrop restores it.
func dropSilently(t *testing.T) {
	t.Helper()
	dropHost(t, "server")
}

// dropHost is dropSilently from one server only (server or server2),
// the operator's sessions to the other kept.
func dropHost(t *testing.T, host string) {
	t.Helper()
	if out, code := op(t, "/", `ip=$(getent hosts `+host+` | cut -d' ' -f1) && iptables -A OUTPUT -d "$ip" -j DROP && iptables -A INPUT -s "$ip" -j DROP`); code != 0 {
		t.Fatalf("dropping (%d): %s", code, out)
	}
	t.Cleanup(func() { undrop(t) })
}

func undrop(t *testing.T) {
	op(t, "/", "iptables -F")
}

// exists reports whether path, relative to the deploy user's home on the
// server, exists.
func exists(t *testing.T, path string) bool {
	t.Helper()
	_, code := srv(t, "test -e "+quote(path))
	return code == 0
}

// commitAll commits the project at dir on the operator: build mode packs
// what git tracks.
func commitAll(t *testing.T, dir string) {
	t.Helper()
	if out, code := op(t, dir, "git config --global --add safe.directory '*' && git init -q && git add -A && git -c user.name=e2e -c user.email=e2e@localhost commit -qm e2e"); code != 0 {
		t.Fatalf("git (%d): %s", code, out)
	}
}

// asRoot runs script as root in the server container name (server,
// server2): what the deploy user cannot do, like giving files to 10001.
func asRoot(t *testing.T, name, script string) (string, int) {
	t.Helper()
	return execIn(t, []string{"-w", "/home/devopsy", name}, script)
}

// quote quotes s for sh.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// project writes files (path to content, executables ending in a
// newline-led "#!") into a new project directory on the operator and
// returns its path there.
func project(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	local := t.TempDir()
	for p, content := range files {
		mode := os.FileMode(0o644)
		if strings.HasPrefix(content, "#!") {
			mode = 0o755
		}
		path := filepath.Join(local, p)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	dir := "/work/" + name
	if out, err := docker("exec", operator, "mkdir", "-p", "/work"); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if out, err := docker("cp", local, operator+":"+dir); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return dir
}
