//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	goarch := map[string]string{"x86_64": "amd64", "aarch64": "arm64"}[strings.TrimSpace(arch)]
	if goarch == "" {
		return 0, fmt.Errorf("unknown architecture %q", arch)
	}
	bin := filepath.Join(os.TempDir(), "devopsy-e2e-"+run)
	build := exec.Command("go", "build", "-o", bin, "./cmd/devopsy")
	build.Dir = filepath.Dir(dir)
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("building devopsy: %v\n%s", err, out)
	}
	defer os.Remove(bin)

	if out, err := docker("network", "create", network); err != nil {
		return 0, fmt.Errorf("%v: %s", err, out)
	}
	for _, c := range [][]string{{server, "server"}, {operator, "operator"}} {
		args := []string{"run", "-d", "--privileged", "--name", c[0], "--hostname", c[1], "--network", network, "--network-alias", c[1]}
		if c[1] == "server" {
			args = append(args, "-v", "/var/lib/docker")
		}
		if out, err := docker(append(args, image, c[1])...); err != nil {
			return 0, fmt.Errorf("starting %s: %v\n%s", c[0], err, out)
		}
		if out, err := docker("cp", bin, c[0]+":/usr/local/bin/devopsy"); err != nil {
			return 0, fmt.Errorf("%v: %s", err, out)
		}
	}
	// The operator's key, authorized for the deploy user.
	if out, err := docker("exec", operator, "sh", "-c", `ssh-keygen -q -t ed25519 -N '' -f /root/.ssh/id_ed25519 && printf 'Host server\n  User devopsy\n  StrictHostKeyChecking accept-new\n' >/root/.ssh/config && chmod 600 /root/.ssh/config`); err != nil {
		return 0, fmt.Errorf("%v: %s", err, out)
	}
	pub, err := docker("exec", operator, "cat", "/root/.ssh/id_ed25519.pub")
	if err != nil {
		return 0, err
	}
	add := exec.Command("docker", "exec", "-i", "-u", "devopsy", server, "sh", "-c", "cat >>~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys")
	add.Stdin = strings.NewReader(pub)
	if out, err := add.CombinedOutput(); err != nil {
		return 0, fmt.Errorf("%v: %s", err, out)
	}
	// The server's dockerd, then SSH all the way.
	for i := 0; ; i++ {
		if _, err := docker("exec", "-u", "devopsy", server, "docker", "version"); err == nil {
			break
		}
		if i > 60 {
			log, _ := docker("exec", server, "cat", "/var/log/dockerd.log")
			return 0, fmt.Errorf("the server's dockerd did not start:\n%s", log)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if out, err := docker("exec", operator, "ssh", "server", "true"); err != nil {
		return 0, fmt.Errorf("ssh to the server: %v\n%s", err, out)
	}
	return 0, nil
}

func tearDown() {
	_, _ = docker("rm", "-f", "-v", server, operator)
	_, _ = docker("network", "rm", network)
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
