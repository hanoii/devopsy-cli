//go:build e2e

package e2e

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// image is the servers' and the operator's image (Dockerfile here).
const image = "devopsy-e2e:1"

// Containers of one lane (runLanes): names end with its id, the run's
// process id and the lane's number, so runs and lanes never meet.
var (
	run      string
	network  string
	server   string
	operator string
	// server2 is a second server, started by the lane's first test that
	// needs one (secondServer); bin is devopsy for it.
	server2 string
	bin     string
	// goarch is the daemon's architecture, which devopsy is built for.
	goarch string
)

func TestMain(m *testing.M) {
	flag.Parse()
	switch lane := os.Getenv(laneEnv); {
	case lane == "list", flag.Lookup("test.list").Value.String() != "":
		// Only the names: go test -list starts no lanes.
		os.Exit(m.Run())
	case lane != "":
		// A test of a lane runLanes started: its containers are there.
		if err := joinLane(lane); err != nil {
			fmt.Fprintln(os.Stderr, "e2e: "+err.Error())
			os.Exit(1)
		}
		os.Exit(m.Run())
	}
	code := 0
	err := setUp()
	if err == nil && lanes() == 1 {
		l := laneOf(fmt.Sprint(os.Getpid()))
		if err = startLane(l); err == nil {
			l.use()
			code = m.Run()
		}
	} else if err == nil {
		code, err = runLanes(lanes())
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: "+err.Error())
		code = 1
	}
	if os.Getenv("E2E_KEEP") == "" {
		tearDown()
	} else {
		fmt.Fprintf(os.Stderr, "e2e: kept the containers labeled %s=%d\n", runLabel, os.Getpid())
	}
	os.Exit(code)
}

// laneEnv tells a test process which lane it runs in (joinLane), or
// "list" to list the tests only.
const laneEnv = "E2E_INTERNAL_LANE"

// runLabel labels every container and network of a run with its
// process id, so tearDown finds them all, the lanes' second servers
// included.
const runLabel = "devopsy-e2e"

// lanes is E2E_LANES, else 4: how many servers and operators run tests
// at once (runLanes). 1 runs every test in this process, one after
// another, and takes no E2E_SHARD.
func lanes() int {
	if n, err := strconv.Atoi(os.Getenv("E2E_LANES")); err == nil && n > 0 {
		return n
	}
	return 4
}

// setUp builds the image and devopsy for the daemon's architecture, and
// starts the registry mirror every lane's servers pull through.
func setUp() error {
	if _, err := docker("version"); err != nil {
		return fmt.Errorf("docker is needed: %v", err)
	}
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file)
	if out, err := docker("build", "-q", "-t", image, dir); err != nil {
		return fmt.Errorf("building %s: %v\n%s", image, err, out)
	}
	arch, err := docker("info", "--format", "{{.Architecture}}")
	if err != nil {
		return err
	}
	goarch = map[string]string{"x86_64": "amd64", "aarch64": "arm64"}[strings.TrimSpace(arch)]
	if goarch == "" {
		return fmt.Errorf("unknown architecture %q", arch)
	}
	bin = filepath.Join(os.TempDir(), fmt.Sprintf("devopsy-e2e-%d", os.Getpid()))
	build := exec.Command("go", "build", "-o", bin, "./cmd/devopsy")
	build.Dir = filepath.Dir(dir)
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+goarch, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("building devopsy: %v\n%s", err, out)
	}
	return startMirror()
}

// mirror is the pull-through cache of Docker Hub the servers pull from
// (daemon.json's registry-mirrors: http://mirror:5000), on every lane's
// network. Its cache is a volume kept between runs, so a run asks Docker
// Hub only whether tags moved, and its pull limits stop mattering.
// Without it a server pulls from Docker Hub directly.
var mirror string

func startMirror() error {
	mirror = fmt.Sprintf("devopsy-e2e-mirror-%d", os.Getpid())
	if out, err := docker("run", "-d", "--name", mirror, "--label", runLabel+"="+fmt.Sprint(os.Getpid()),
		"-v", "devopsy-e2e-mirror:/var/lib/registry", "-e", "REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io", mirrorImage); err != nil {
		return fmt.Errorf("starting the registry mirror: %v\n%s", err, out)
	}
	return nil
}

// mirrorImage is the mirror's image, and the one the images tests run
// on the server as a registry of its own.
const mirrorImage = "registry:3"

// lane names one lane's containers and network: run is the lane's id,
// the run's process id and, with several lanes, the lane's number.
type lane struct{ run, network, server, operator string }

func laneOf(id string) lane {
	return lane{id, "devopsy-e2e-" + id, "devopsy-e2e-server-" + id, "devopsy-e2e-op-" + id}
}

// use makes l the lane the tests of this process use.
func (l lane) use() { run, network, server, operator = l.run, l.network, l.server, l.operator }

// server2 is l's second server's name.
func (l lane) server2() string { return "devopsy-e2e-server2-" + l.run }

// startLane starts l: its network, operator and server, and lets the
// operator reach the server's deploy user over SSH.
func startLane(l lane) error {
	if out, err := docker("network", "create", "--label", runLabel+"="+fmt.Sprint(os.Getpid()), l.network); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	if mirror != "" {
		if out, err := docker("network", "connect", "--alias", "mirror", l.network, mirror); err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
	}
	if err := l.start(l.operator, "operator"); err != nil {
		return err
	}
	// The operator's key, authorized for the deploy user of both servers,
	// and an agent holding it, for syncs across servers (agent()).
	if out, err := docker("exec", l.operator, "sh", "-c", `ssh-keygen -q -t ed25519 -N '' -f /root/.ssh/id_ed25519 && printf 'Host server server2\n  User devopsy\n  StrictHostKeyChecking accept-new\n' >/root/.ssh/config && chmod 600 /root/.ssh/config && ssh-agent -a /tmp/agent.sock >/dev/null && SSH_AUTH_SOCK=/tmp/agent.sock ssh-add -q`); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return l.startServer(l.server, "server")
}

// joinLane takes the lane runLanes started for this process: lane is
// "<id> <devopsy>".
func joinLane(lane string) error {
	id, devopsy, ok := strings.Cut(lane, " ")
	if !ok {
		return fmt.Errorf("%s: %q", laneEnv, lane)
	}
	bin = devopsy
	l := laneOf(id)
	l.use()
	// A second server an earlier test of the lane started.
	if out, err := docker("inspect", "-f", "{{.State.Running}}", l.server2()); err == nil && strings.TrimSpace(out) == "true" {
		server2 = l.server2()
	}
	return nil
}

// runLanes starts n lanes and runs each test the flags select in a
// process of its own, on the first lane free, the slowest first (by
// their last times, timesFile). Each test's output is printed whole
// once it ends: with -v always, else when it fails.
func runLanes(n int) (int, error) {
	tests, err := listTests()
	if err != nil {
		return 0, err
	}
	if tests, err = shard(tests); err != nil {
		return 0, err
	}
	times := readTimes()
	sort.SliceStable(tests, func(i, j int) bool { return slowFirst(times, tests[i], tests[j]) })
	if len(tests) < n {
		n = len(tests)
	}
	ids := make(chan string, n)
	var started sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		started.Add(1)
		go func() {
			defer started.Done()
			id := fmt.Sprintf("%d-%d", os.Getpid(), i)
			if errs[i] = startLane(laneOf(id)); errs[i] == nil {
				ids <- id
			}
		}()
	}
	started.Wait()
	if err := errors.Join(errs...); err != nil {
		return 0, err
	}
	var mu sync.Mutex
	failed := false
	var done sync.WaitGroup
	queue := make(chan string, len(tests))
	for _, name := range tests {
		queue <- name
	}
	close(queue)
	for range n {
		done.Add(1)
		go func() {
			defer done.Done()
			id := <-ids
			for name := range queue {
				resetLane(laneOf(id))
				start := time.Now()
				out, err := runTest(id, name)
				took := time.Since(start)
				mu.Lock()
				if err != nil {
					failed = true
				} else {
					times[name] = took.Seconds()
				}
				if err != nil || testing.Verbose() {
					os.Stdout.Write(out)
				}
				if err != nil {
					fmt.Printf("e2e: %s failed in lane %s after %.0fs: %v\n", name, id, took.Seconds(), err)
				}
				mu.Unlock()
			}
		}()
	}
	done.Wait()
	writeTimes(times)
	if failed {
		fmt.Println("FAIL")
		return 1, nil
	}
	fmt.Println("PASS")
	return 0, nil
}

// resetLane undoes on l what a test before may have left that the next
// one would trip on, whatever their order: devopsy, ssh and terminals
// still running on the operator, a dropped or slowed connection, the
// deploy user's login shell.
func resetLane(l lane) {
	_, _ = docker("exec", l.operator, "sh", "-c", "pkill -x devopsy; pkill -x ssh; pkill -x script; iptables -F; tc qdisc del dev eth0 root 2>/dev/null; true")
	_, _ = docker("exec", l.server, "sh", "-c", "tc qdisc del dev eth0 root 2>/dev/null; usermod -s /bin/sh devopsy; true")
}

// listTests lists the tests -test.run selects, never -test.skip's.
func listTests() ([]string, error) {
	pattern := "."
	if f := flag.Lookup("test.run"); f != nil && f.Value.String() != "" {
		pattern, _, _ = strings.Cut(f.Value.String(), "/")
	}
	cmd := exec.Command(os.Args[0], "-test.list", pattern)
	cmd.Env = append(os.Environ(), laneEnv+"=list")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("listing the tests: %v", err)
	}
	var tests []string
	skip := flag.Lookup("test.skip")
	for _, name := range strings.Fields(string(out)) {
		if !strings.HasPrefix(name, "Test") {
			continue
		}
		if skip != nil && skip.Value.String() != "" {
			if re, err := regexp.Compile(skip.Value.String()); err == nil && re.MatchString(name) {
				continue
			}
		}
		tests = append(tests, name)
	}
	return tests, nil
}

// shard keeps E2E_SHARD's share of tests, sorted by name: "<k>/<n>"
// takes every n-th from the k-th, for CI's jobs. Round robin by name,
// never by time, so a test stays in its shard whatever its times, and
// the sync tests, next to each other, spread over the shards.
func shard(tests []string) ([]string, error) {
	spec := os.Getenv("E2E_SHARD")
	if spec == "" {
		return tests, nil
	}
	ks, ns, _ := strings.Cut(spec, "/")
	k, err1 := strconv.Atoi(ks)
	n, err2 := strconv.Atoi(ns)
	if err1 != nil || err2 != nil || n < 1 || k < 1 || k > n {
		return nil, fmt.Errorf("E2E_SHARD=%s: want <k>/<n>, 1 <= k <= n", spec)
	}
	sorted := slices.Sorted(slices.Values(tests))
	var mine []string
	for i, name := range sorted {
		if i%n == k-1 {
			mine = append(mine, name)
		}
	}
	return mine, nil
}

// runTest runs the test name alone in lane id, with -test.run's subtest
// part if any, and returns its output.
func runTest(id, name string) ([]byte, error) {
	pattern := "^" + name + "$"
	if f := flag.Lookup("test.run"); f != nil {
		if _, sub, ok := strings.Cut(f.Value.String(), "/"); ok {
			pattern += "/" + sub
		}
	}
	cmd := exec.Command(os.Args[0], "-test.run", pattern, "-test.v", "-test.count=1", "-test.timeout=20m")
	cmd.Env = append(os.Environ(), laneEnv+"="+id+" "+bin)
	out, err := cmd.CombinedOutput()
	// The test binary's own last line: the run's is runLanes'.
	out = bytes.TrimSuffix(bytes.TrimSuffix(out, []byte("PASS\n")), []byte("FAIL\n"))
	return out, err
}

// slowFirst orders tests by their last times, slowest first; a test
// without one first of all (it may be slow), else by name.
func slowFirst(times map[string]float64, a, b string) bool {
	ta, oka := times[a]
	tb, okb := times[b]
	if oka != okb {
		return !oka
	}
	if ta != tb {
		return ta > tb
	}
	return a < b
}

// timesFile keeps each test's last time that passed, "<name> <seconds>"
// per line, for runLanes' order: in the user's cache directory, never
// in the checkout.
func timesFile() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "devopsy-e2e", "times")
}

func readTimes() map[string]float64 {
	times := map[string]float64{}
	data, err := os.ReadFile(timesFile())
	if err != nil {
		return times
	}
	for _, line := range strings.Split(string(data), "\n") {
		name, secs, ok := strings.Cut(line, " ")
		if f, err := strconv.ParseFloat(secs, 64); ok && err == nil {
			times[name] = f
		}
	}
	return times
}

func writeTimes(times map[string]float64) {
	file := timesFile()
	if file == "" || os.MkdirAll(filepath.Dir(file), 0o755) != nil {
		return
	}
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(times)) {
		fmt.Fprintf(&b, "%s %.1f\n", name, times[name])
	}
	_ = os.WriteFile(file, []byte(b.String()), 0o644)
}

// start runs the image as role (server or operator) in a container
// named name, reached as role on l's network, with devopsy.
func (l lane) start(name, role string) error {
	runID, _, _ := strings.Cut(l.run, "-")
	args := []string{"run", "-d", "--privileged", "--name", name, "--label", runLabel + "=" + runID, "--hostname", role, "--network", l.network, "--network-alias", role}
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

// startServer starts a server of l reached as alias, authorizes the
// operator's key for its deploy user, and waits for its dockerd and SSH.
func (l lane) startServer(name, alias string) error {
	if err := l.start(name, alias); err != nil {
		return err
	}
	pub, err := docker("exec", l.operator, "cat", "/root/.ssh/id_ed25519.pub")
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
	if out, err := docker("exec", l.operator, "ssh", alias, "true"); err != nil {
		return fmt.Errorf("ssh to %s: %v\n%s", alias, err, out)
	}
	return nil
}

// secondServer starts server2, once per lane, for the tests that sync
// across servers.
func secondServer(t *testing.T) {
	t.Helper()
	if server2 != "" {
		return
	}
	l := laneOf(run)
	if err := l.startServer(l.server2(), "server2"); err != nil {
		_, _ = docker("rm", "-f", "-v", l.server2())
		t.Fatal(err)
	}
	server2 = l.server2()
}

// tearDown removes the run's containers and networks, every lane's.
func tearDown() {
	filter := "label=" + runLabel + "=" + fmt.Sprint(os.Getpid())
	if out, err := docker("ps", "-aq", "--filter", filter); err == nil && strings.TrimSpace(out) != "" {
		_, _ = docker(append([]string{"rm", "-f", "-v"}, strings.Fields(out)...)...)
	}
	if out, err := docker("network", "ls", "-q", "--filter", filter); err == nil && strings.TrimSpace(out) != "" {
		_, _ = docker(append([]string{"network", "rm"}, strings.Fields(out)...)...)
	}
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
