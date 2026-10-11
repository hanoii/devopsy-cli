package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cloneFakeDocker logs every call, with its compose project, to
// $DOCKER_LOG, answers compose config and ps for a project whose app is
// labeled pause and db stop, and copies what the rsync containers would:
// within one host, or from the daemon's mount (remembered) across hosts.
const cloneFakeDocker = `#!/bin/sh
echo "${COMPOSE_PROJECT_NAME:-} $*" >> "$DOCKER_LOG"
if [ "$1" = compose ] && [ "$2" = -f ]; then dir=$(dirname "$3"); fi
for a; do last2=${last:-}; last=$a; done
case "$*" in
  *" config --format json")
    printf '{"services":{"app":{"labels":{"devopsy.clone":"pause"},"volumes":[{"type":"bind","source":"%s/mnt/storage"}]},"db":{"labels":{"devopsy.clone":"stop"},"volumes":[{"type":"bind","source":"%s/mnt/db"}]}}}\n' "$dir" "$dir" ;;
  *" stop db") [ "${FAIL_STOP:-}" != "${COMPOSE_PROJECT_NAME:-}" ] || exit 1 ;;
  *" ps --services --status running") echo app; echo db ;;
  *" ps --services --status paused") echo app ;;
  *" ps --services --all --status exited") echo db ;;
  *" ps --all --format json db") echo '{"Service":"db","State":"running","Health":"healthy"}' ;;
  "run --rm -d "*)
    for a; do case $a in *:/mnt:ro) echo "${a%:/mnt:ro}" > "$DOCKER_LOG.daemon" ;; esac; done
    echo cid123 ;;
  "inspect "*) echo 172.17.0.9 ;;
  "run --rm --network host "*)
    [ -z "${FAIL_RSYNC:-}" ] || exit 23
    for a; do case $a in *:/mnt) dst=${a%:/mnt} ;; esac; done
    src=$(cat "$DOCKER_LOG.daemon"); d=${last#/mnt/}
    mkdir -p "$dst/$d" && cp -R "$src/$d." "$dst/$d" ;;
  "run --rm -v "*)
    [ -z "${FAIL_RSYNC:-}" ] || exit 23
    for a; do case $a in *:/src:ro) src=${a%:/src:ro} ;; *:/dst) dst=${a%:/dst} ;; esac; done
    d=${last2#/src/}
    mkdir -p "$dst/$d" && cp -R "$src/$d." "$dst/$d" ;;
esac
exit 0
`

// cloneFakeSSH runs a session's script here (its last argument), logging
// the options of each, and answers ssh -G from the destination. Like a
// server, it forwards the agent only when asked, and never with
// FAKE_RESTRICT (a restricted key).
const cloneFakeSSH = `#!/bin/sh
if [ "$1" = -G ]; then
  case $2 in *@*) echo "user ${2%@*}" ;; esac
  echo "hostname ${2#*@}"; echo port 22; echo "proxyjump ${FAKE_PROXYJUMP:-none}"
  exit 0
fi
opts=
for a; do [ -n "${s+x}" ] && opts="$opts $s"; s=$a; done
echo "$opts" >> "$SSH_LOG"
case "$opts" in *ForwardAgent=yes*) ;; *) unset SSH_AUTH_SOCK ;; esac
[ -z "${FAKE_RESTRICT:-}" ] || unset SSH_AUTH_SOCK
exec sh -c "$s"
`

// cloneSetup is a project with prod and its child dev on a fake server
// (this machine, root under tmp), and a run function.
type cloneSetup struct {
	t                *testing.T
	root, project    string
	dot, sshLog, log string
	env              []string
}

func newCloneSetup(t *testing.T) *cloneSetup {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(tmp, "bin")
	write(t, filepath.Join(bin, "fakessh"), cloneFakeSSH, 0o755)
	write(t, filepath.Join(bin, "docker"), cloneFakeDocker, 0o755)
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n", 0o755)
	write(t, filepath.Join(bin, "mv"), "#!/bin/sh\nif [ \"$1\" = -Tf ]; then rm -f \"$3\"; exec /bin/mv \"$2\" \"$3\"; fi\nexec /bin/mv \"$@\"\n", 0o755)
	if err := os.Symlink(binary, filepath.Join(bin, "devopsy")); err != nil {
		t.Fatal(err)
	}
	s := &cloneSetup{t: t, root: filepath.Join(tmp, "srv"), project: filepath.Join(tmp, "app"), sshLog: filepath.Join(tmp, "ssh.log"), log: filepath.Join(tmp, "docker.log")}
	home := filepath.Join(tmp, "home")
	write(t, filepath.Join(home, "config.yaml"), "releases:\n  root: "+s.root+"\n", 0o644)
	s.dot = filepath.Join(s.project, ".devopsy")
	write(t, filepath.Join(s.dot, "compose.yaml"), "services:\n  app:\n    image: x\n  db:\n    image: y\n", 0o644)
	write(t, filepath.Join(s.dot, "config.yaml"), "project: shop\ndefaults:\n  mode: image\n  release: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n  solo: {}\n  bare: {parent: prod, release: {before: [local: deploy]}}\n", 0o644)
	write(t, filepath.Join(s.dot, ".env"), "DEVOPSY_SERVER=devopsy@one\n", 0o644)
	write(t, filepath.Join(s.dot, "commands", "deploy"), "#!/bin/sh\necho \"deploy in $COMPOSE_PROJECT_NAME parent=${DEVOPSY_PARENT:-} ${DEVOPSY_PARENT_TARGET:-}\"\n", 0o755)
	write(t, filepath.Join(s.dot, "commands", "show"), "#!/bin/sh\necho \"show project=$COMPOSE_PROJECT_NAME pw=${DB_PASSWORD:-}\"\ncat\n", 0o755)
	s.env = []string{"DEVOPSY_SSH_COMMAND=" + filepath.Join(bin, "fakessh"), "PATH=" + bin + ":/usr/bin:/bin", "HOME=" + tmp, "DEVOPSY_HOME=" + home,
		"SSH_LOG=" + s.sshLog, "DOCKER_LOG=" + s.log}
	return s
}

func (s *cloneSetup) run(extra []string, args ...string) (string, int) {
	s.t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir = s.project
	cmd.Env = append(append([]string{}, s.env...), extra...)
	cmd.Stdin = strings.NewReader("")
	out, _ := cmd.CombinedOutput()
	return string(out), cmd.ProcessState.ExitCode()
}

// logs returns and empties the docker and ssh logs.
func (s *cloneSetup) logs() (docker, ssh string) {
	d, _ := os.ReadFile(s.log)
	h, _ := os.ReadFile(s.sshLog)
	os.Remove(s.log)
	os.Remove(s.sshLog)
	return string(d), string(h)
}

func (s *cloneSetup) current(env string) string {
	l, _ := os.Readlink(filepath.Join(s.root, "shop", env, "current"))
	return l
}

func (s *cloneSetup) mnt(env string) string {
	return filepath.Join(s.root, "shop", env, "shared", "mnt")
}

func (s *cloneSetup) releaseProd(extra []string, args ...string) {
	s.t.Helper()
	if out, code := s.run(extra, append([]string{"@prod", "--release"}, args...)...); code != 0 {
		s.t.Fatalf("prod release (%d):\n%s", code, out)
	}
	write(s.t, filepath.Join(s.mnt("prod"), "storage", "file.txt"), "from prod\n", 0o644)
	write(s.t, filepath.Join(s.mnt("prod"), "db", "ibdata"), "rows\n", 0o644)
	s.logs()
}

func readString(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

func next() { time.Sleep(1100 * time.Millisecond) } // release ids are per second

// A child's first release clones its parent on the same server: services
// paused and stopped on both sides, one container copying, then deploy on
// the parent's data. Later releases do not, unless --clone; --clone
// resyncs.
func TestCloneSameHost(t *testing.T) {
	s := newCloneSetup(t)
	s.releaseProd(nil)

	out, code := s.run(nil, "@dev", "--release")
	docker, ssh := s.logs()
	if code != 0 || !strings.Contains(out, "Cloning @devopsy@one:prod's data into it") || !strings.Contains(out, "deploy in shop-dev parent=prod devopsy@one:prod") || s.current("dev") == "" {
		t.Fatalf("first release (%d), current %q:\n%s", code, s.current("dev"), out)
	}
	if got := readString(filepath.Join(s.mnt("dev"), "storage", "file.txt")); got != "from prod\n" {
		t.Errorf("storage not copied: %q", got)
	}
	if got := readString(filepath.Join(s.mnt("dev"), "db", "ibdata")); got != "rows\n" {
		t.Errorf("db not copied: %q", got)
	}
	// Order on each side: pause, stop, copy, start (healthy), unpause; the
	// child's around the parent's; one container copies, no daemon.
	order := []string{"shop-dev compose", "pause app", "shop-dev compose", "stop db", "shop-prod compose", "pause app", "stop db",
		"run --rm -v " + s.mnt("prod") + ":/src:ro -v " + s.mnt("dev") + ":/dst devopsy-rsync:1 rsync -a --delete --numeric-ids /src/db/ /dst/db/",
		"run --rm -v " + s.mnt("prod") + ":/src:ro -v " + s.mnt("dev") + ":/dst devopsy-rsync:1 rsync -a --delete --numeric-ids /src/storage/ /dst/storage/",
		"shop-prod compose", "start db", "unpause app", "shop-dev compose", "start db", "unpause app"}
	rest := docker
	for _, want := range order {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("missing %q in order, after:\n%s\nall:\n%s", want, rest, docker)
		}
		rest = rest[i+len(want):]
	}
	if strings.Contains(docker, "run --rm -d") || strings.Contains(ssh, "ForwardAgent") {
		t.Errorf("same host used a daemon or the agent:\n%s\n%s", docker, ssh)
	}
	data := readString(filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "target.env"))
	if !strings.Contains(data, "DEVOPSY_PARENT='prod'\n") || !strings.Contains(data, "DEVOPSY_PARENT_TARGET='devopsy@one:prod'\n") {
		t.Errorf("target.env:\n%s", data)
	}

	// A later release does not clone.
	next()
	out, code = s.run(nil, "@dev", "--release")
	docker, _ = s.logs()
	if code != 0 || strings.Contains(out, "Cloning") || strings.Contains(docker, "pause") {
		t.Fatalf("second release (%d):\n%s\n%s", code, out, docker)
	}

	// --clone asks; --yes; arguments after -- go to rsync; the log goes
	// with the current release.
	write(t, filepath.Join(s.mnt("prod"), "storage", "file.txt"), "newer\n", 0o644)
	if out, code := s.run(nil, "@dev", "--clone"); code == 0 || !strings.Contains(out, "add --yes when not at a terminal") {
		t.Fatalf("--clone without a terminal (%d):\n%s", code, out)
	}
	out, code = s.run(nil, "@dev", "--clone", "--yes", "--", "--exclude=css")
	docker, _ = s.logs()
	if code != 0 || !strings.Contains(out, "cloning into") || !strings.Contains(out, "deploy in shop-dev") || !strings.Contains(docker, "--numeric-ids --exclude=css /src/storage/") {
		t.Fatalf("--clone (%d):\n%s\n%s", code, out, docker)
	}
	if got := readString(filepath.Join(s.mnt("dev"), "storage", "file.txt")); got != "newer\n" {
		t.Errorf("not resynced: %q", got)
	}
	if out, code := s.run(nil, "@dev", "--log"); code != 0 || !strings.Contains(out, "release exited 0") || !strings.Contains(out, "clone of devopsy@one:dev") || !strings.Contains(out, "clone exited 0") {
		t.Errorf("--log (%d):\n%s", code, out)
	}

	// --release --clone clones on any release; --no-clone never.
	next()
	if out, code := s.run(nil, "@dev", "--release", "--clone"); code != 0 || !strings.Contains(out, "Cloning @devopsy@one:prod's data") {
		t.Fatalf("--release --clone (%d):\n%s", code, out)
	}
	s.logs()
	if out, code := s.run(nil, "@dev", "--release", "--clone", "--no-clone"); code == 0 || !strings.Contains(out, "not both") {
		t.Errorf("--clone --no-clone (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--rollback", "--clone"); code == 0 || !strings.Contains(out, "--rollback takes no --clone") {
		t.Errorf("--rollback --clone (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--clone", "--help"); code != 0 || !strings.Contains(out, "Usage: devopsy @<target> --clone [--from <target>] [--yes] [-- args...]") || !strings.Contains(out, "devopsy.clone=pause") {
		t.Errorf("--clone --help (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--release", "--help"); code != 0 || !strings.Contains(out, "[--clone | --no-clone]") {
		t.Errorf("--release --help (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev"); code != 0 || !strings.Contains(out, "--clone [--from <target>]") {
		t.Errorf("@dev help (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "--clone"); code == 0 || !strings.Contains(out, "--clone needs a target") {
		t.Errorf("--clone without a target (%d):\n%s", code, out)
	}
}

// A first release whose copy fails does not go live: the release is marked
// failed, nothing is current, the services restored, and the next release
// clones again; --no-clone releases without the data.
func TestReleaseFirstClone(t *testing.T) {
	s := newCloneSetup(t)
	s.releaseProd(nil)

	out, code := s.run([]string{"FAIL_RSYNC=1"}, "@dev", "--release")
	docker, _ := s.logs()
	if code == 0 || s.current("dev") != "" || !strings.Contains(out, "clone failed (1), no previous release to go back to") || strings.Contains(out, "deploy in shop-dev") {
		t.Fatalf("failed clone (%d), current %q:\n%s", code, s.current("dev"), out)
	}
	entries, _ := os.ReadDir(filepath.Join(s.root, "shop", "dev", "releases"))
	if len(entries) != 1 || readString(filepath.Join(s.root, "shop", "dev", "releases", entries[0].Name(), ".devopsy-failed")) != "" {
		t.Fatalf("releases: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(s.root, "shop", "dev", "releases", entries[0].Name(), ".devopsy-failed")); err != nil {
		t.Errorf("not marked failed: %v", err)
	}
	// The parent's services come back too.
	if !strings.Contains(docker, "shop-prod compose -f "+filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml")+" unpause app") {
		t.Errorf("parent not restored:\n%s", docker)
	}

	next()
	if out, code := s.run(nil, "@dev", "--release", "--no-clone"); code != 0 || strings.Contains(out, "Cloning") || s.current("dev") == "" {
		t.Fatalf("--no-clone (%d):\n%s", code, out)
	}
	if docker, _ := s.logs(); strings.Contains(docker, "pause") {
		t.Errorf("--no-clone paused:\n%s", docker)
	}
}

// Across servers: the agent forwarded to the parent's, an rsync daemon
// there, the child's server reaching it through a reverse forward, and the
// daemon removed.
func TestCloneDifferentHosts(t *testing.T) {
	s := newCloneSetup(t)
	s.releaseProd(nil)
	env := []string{"DEVOPSY_SERVER_PROD=devopsy@one"}

	// No agent here: nothing starts.
	if out, code := s.run(env, "@devopsy@two:dev", "--release"); code == 0 || !strings.Contains(out, "eval $(ssh-agent)") || s.current("dev") != "" {
		t.Fatalf("no agent (%d):\n%s", code, out)
	}
	if _, ssh := s.logs(); strings.Count(ssh, "\n") != 1 {
		t.Errorf("sessions before the agent check (only the first-release check expected):\n%s", ssh)
	}
	// A child behind a jump host is out of the parent's reach.
	agent := append(env, "SSH_AUTH_SOCK=/tmp/agent.sock")
	if out, code := s.run(append(agent, "FAKE_PROXYJUMP=bastion"), "@devopsy@two:dev", "--release"); code == 0 || !strings.Contains(out, "relay through this machine is not supported yet") {
		t.Fatalf("proxyjump (%d):\n%s", code, out)
	}
	s.logs()

	out, code := s.run(agent, "@devopsy@two:dev", "--release")
	docker, ssh := s.logs()
	if code != 0 || !strings.Contains(out, "from devopsy@one to devopsy@two") || !strings.Contains(out, "deploy in shop-dev parent=prod devopsy@one:prod") {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	if got := readString(filepath.Join(s.mnt("dev"), "storage", "file.txt")); got != "from prod\n" {
		t.Errorf("not copied: %q\n%s", got, docker)
	}
	var push, receive string
	for _, line := range strings.Split(ssh, "\n") {
		switch {
		case strings.Contains(line, "ForwardAgent=yes"):
			push = line
		case strings.Contains(line, " -R "):
			receive = line
		}
	}
	if !strings.Contains(push, "-o ClearAllForwardings=yes -o ForwardAgent=yes -T devopsy@one") {
		t.Errorf("push session: %q\n%s", push, ssh)
	}
	if !strings.Contains(receive, "-o ExitOnForwardFailure=yes -T -R 127.0.0.1:") || !strings.Contains(receive, ":172.17.0.9:873 -p 22 devopsy@two") || strings.Contains(receive, "ClearAllForwardings") {
		t.Errorf("receive session: %q\n%s", receive, ssh)
	}
	for _, want := range []string{"run --rm -d -v " + s.mnt("prod") + ":/mnt:ro", "inspect -f", "run --rm --network host -v " + s.mnt("dev") + ":/mnt devopsy-rsync:1 rsync -a --delete --numeric-ids rsync://127.0.0.1:", " rm -f cid123"} {
		if !strings.Contains(docker, want) {
			t.Errorf("missing %q in:\n%s", want, docker)
		}
	}

	// A restricted key refuses the agent: the parent's side says so, and
	// the child's services come back.
	out, code = s.run(append(agent, "FAKE_RESTRICT=1"), "@devopsy@two:dev", "--clone", "--yes")
	docker, _ = s.logs()
	if code == 0 || !strings.Contains(out, "no SSH agent was forwarded to") || !strings.Contains(out, "restrict,agent-forwarding") || !strings.Contains(docker, "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" unpause app") {
		t.Fatalf("restricted key (%d):\n%s\n%s", code, out, docker)
	}
}

// A push that fails aborts: the child's services are restored, its run
// step skipped.
func TestCloneAbort(t *testing.T) {
	s := newCloneSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()
	out, code := s.run([]string{"FAIL_RSYNC=1"}, "@dev", "--clone", "--yes")
	docker, _ := s.logs()
	if code != 1 || !strings.Contains(out, "aborting the clone") || strings.Contains(out, "deploy in shop-dev") {
		t.Fatalf("abort (%d):\n%s", code, out)
	}
	rest := docker
	for _, want := range []string{"shop-dev compose", "pause app", "shop-prod compose", "unpause app", "shop-dev compose", "start db", "unpause app"} {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("missing %q in order:\n%s", want, docker)
		}
		rest = rest[i+len(want):]
	}
	if s.current("dev") == "" {
		t.Error("an aborted clone changed current")
	}

	// The parent's stop fails: what it paused or stopped comes back.
	out, code = s.run([]string{"FAIL_STOP=shop-prod"}, "@dev", "--clone", "--yes")
	docker, _ = s.logs()
	if code != 1 || !strings.Contains(out, "aborting the clone") {
		t.Fatalf("parent's stop fails (%d):\n%s", code, out)
	}
	rest = docker
	for _, want := range []string{"shop-prod compose", "stop db", "shop-prod compose", "start db", "shop-prod compose", "unpause app"} {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("parent not restored, missing %q in order:\n%s", want, docker)
		}
		rest = rest[i+len(want):]
	}
}

// The clone capability replaces the generic path: prepare and finish on
// the child, push on the parent with the helpers; the child's commands run
// as the child, without the parent's environment.
func TestCloneCapability(t *testing.T) {
	s := newCloneSetup(t)
	caps := filepath.Join(s.dot, "capabilities", "clone")
	write(t, filepath.Join(caps, "prepare"), "#!/bin/sh\necho \"prepare in $COMPOSE_PROJECT_NAME from $DEVOPSY_CLONE_FROM_TARGET\"\n", 0o755)
	write(t, filepath.Join(caps, "finish"), "#!/bin/sh\necho \"finish $1 in $COMPOSE_PROJECT_NAME\"\n", 0o755)
	write(t, filepath.Join(caps, "push"), "#!/bin/sh\nset -e\necho \"push from $COMPOSE_PROJECT_NAME to $DEVOPSY_CLONE_TO_PROJECT args=$DEVOPSY_CLONE_ARGS\"\ndevopsy --clone-rsync storage -- --checksum\necho \"dump of $COMPOSE_PROJECT_NAME pw=$DB_PASSWORD\" | devopsy --clone-exec show\n", 0o755)
	s.releaseProd(nil)
	write(t, filepath.Join(s.root, "shop", "prod", "shared", ".env"), "DB_PASSWORD=parent-secret\n", 0o644)
	write(t, filepath.Join(s.root, "shop", "dev", "shared", ".env"), "DB_PASSWORD=child-secret\n", 0o644)

	out, code := s.run(nil, "@dev", "--release", "--", "x")
	if code == 0 {
		t.Fatalf("--release with arguments (%d):\n%s", code, out)
	}
	out, code = s.run(nil, "@dev", "--release")
	docker, _ := s.logs()
	for _, want := range []string{"prepare in shop-dev from devopsy@one:prod", "push from shop-prod to shop-dev args=", "show project=shop-dev pw=child-secret", "dump of shop-prod pw=parent-secret", "finish go in shop-dev", "deploy in shop-dev"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("missing %q (%d):\n%s", want, code, out)
		}
	}
	if strings.Contains(docker, "pause") || !strings.Contains(docker, "--numeric-ids --checksum /src/storage/ /dst/storage/") || strings.Contains(docker, "/src/db/") {
		t.Errorf("docker:\n%s", docker)
	}
	// --clone's arguments are the push's: --clone-rsync only takes its own.
	out, code = s.run(nil, "@dev", "--clone", "--yes", "--", "--exclude=css")
	docker, _ = s.logs()
	if code != 0 || !strings.Contains(out, "args=--exclude=css") || strings.Contains(docker, "--exclude=css") || !strings.Contains(docker, "--numeric-ids --checksum /src/storage/") {
		t.Errorf("--clone -- args (%d):\n%s\n%s", code, out, docker)
	}
	if got := readString(filepath.Join(s.mnt("dev"), "storage", "file.txt")); got != "from prod\n" {
		t.Errorf("not copied: %q", got)
	}

	// The helpers only run inside a push, on entries of mnt.
	prod := filepath.Join(s.root, "shop", "prod", "current")
	inPush := []string{"DEVOPSY_CLONE_TO_DIR=" + filepath.Join(s.root, "shop", "dev", "current", ".devopsy"), "DEVOPSY_CLONE_TO_PROJECT=shop-dev"}
	for _, args := range [][]string{{"--clone-rsync", "../x"}, {"--clone-rsync", "a/b"}, {"--clone-rsync", "missing"}} {
		cmd := exec.Command(binary, args...)
		cmd.Dir = prod
		cmd.Env = append(append([]string{}, s.env...), inPush...)
		if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "--clone-rsync") {
			t.Errorf("%v: %v\n%s", args, err, out)
		}
	}
	for _, flag := range []string{"--clone-exec", "--clone-rsync", "--clone-push"} {
		cmd := exec.Command(binary, flag, "ps")
		cmd.Dir = prod
		cmd.Env = s.env
		if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "only runs in a clone's push") {
			t.Errorf("%s outside a push: %v\n%s", flag, err, out)
		}
	}
}

// What --clone refuses before anything starts.
func TestCloneGuards(t *testing.T) {
	s := newCloneSetup(t)
	if out, code := s.run(nil, "@solo", "--clone", "--yes"); code == 0 || !strings.Contains(out, "@solo is not a child") {
		t.Errorf("not a child (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@solo", "--release", "--clone"); code == 0 || !strings.Contains(out, "@solo is not a child") {
		t.Errorf("--release --clone, not a child (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--clone", "--yes"); code == 0 || !strings.Contains(out, "the parent @devopsy@one:prod (devopsy@one:shop/prod) has no current release") {
		t.Errorf("parent not released (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--release"); code == 0 || !strings.Contains(out, "has no current release") || !strings.Contains(out, "--no-clone") {
		t.Errorf("first release, parent not released (%d):\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(s.root, "shop", "dev")); !os.IsNotExist(err) {
		t.Errorf("the child was touched: %v", err)
	}
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--clone", "--yes"); code == 0 || !strings.Contains(out, "has no current release: run 'devopsy @dev --release' first") {
		t.Errorf("child not released (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--clone", "--from", "dev", "--yes"); code == 0 || !strings.Contains(out, "and its parent are the same environment") {
		t.Errorf("--from itself (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@bare", "--clone", "--yes"); code == 0 || !strings.Contains(out, "environment bare has no release steps") {
		t.Errorf("no run step (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--clone", "--bogus"); code == 0 || !strings.Contains(out, "usage: devopsy @dev --clone") {
		t.Errorf("usage (%d):\n%s", code, out)
	}
	// --from another environment of the project works as the source.
	if out, code := s.run(nil, "@solo", "--release"); code != 0 {
		t.Fatalf("solo (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--release", "--no-clone"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	write(t, filepath.Join(s.mnt("solo"), "storage", "file.txt"), "from solo\n", 0o644)
	if out, code := s.run(nil, "@dev", "--clone", "--from", "solo", "--yes"); code != 0 || readString(filepath.Join(s.mnt("dev"), "storage", "file.txt")) != "from solo\n" {
		t.Errorf("--from solo (%d):\n%s", code, out)
	}
}

// inherit: on the parent names variables its children get from its
// server's files, written at the same level on the child's side, before the
// upload of a first release and before a --clone's copy; never with
// --no-clone.
func TestCloneInherit(t *testing.T) {
	s := newCloneSetup(t)
	write(t, filepath.Join(s.dot, "config.yaml"), "project: shop\ndefaults:\n  mode: image\n  release: deploy\nenvironments:\n  prod: {inherit: [SHARED_KEY, INST_KEY, PROJ_KEY, MISSING_KEY]}\n  dev: {parent: prod}\n", 0o644)
	if out, code := s.run(nil, "@a/prod", "--release", "--yes"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	shop := filepath.Join(s.root, "shop")
	write(t, filepath.Join(shop, "a", "prod", "shared", ".env"), "SHARED_KEY=s1\n", 0o600)
	write(t, filepath.Join(shop, "a", ".env"), "INST_KEY='i 1'\n", 0o600)
	write(t, filepath.Join(shop, ".env"), "PROJ_KEY=p1\nSHARED_KEY=lower\n", 0o600)
	childEnv := filepath.Join(shop, "a", "dev", "shared", ".env")
	write(t, childEnv, "OWN=1\nSHARED_KEY=child\n", 0o600)
	s.logs()

	// Same server: shared/.env is written, the instance's and project's are
	// the parent's own files.
	out, code := s.run(nil, "@a/dev", "--release", "--yes")
	if code != 0 {
		t.Fatalf("first release (%d):\n%s", code, out)
	}
	for _, want := range []string{
		"Inherited SHARED_KEY from @devopsy@one:a/prod's shared .env into devopsy@one:" + childEnv,
		"Inherited INST_KEY from @devopsy@one:a/prod: the same instance .env on devopsy@one.",
		"Inherited PROJ_KEY from @devopsy@one:a/prod: the same project .env on devopsy@one.",
		"@devopsy@one:a/prod does not set MISSING_KEY",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if i, j := strings.Index(out, "Inherited SHARED_KEY"), strings.Index(out, "Releasing "); i < 0 || j < i {
		t.Errorf("inherited after the upload:\n%s", out)
	}
	if got := readString(childEnv); got != "OWN=1\nSHARED_KEY='s1'\n" {
		t.Errorf("child's shared/.env: %q", got)
	}
	if strings.Contains(out, "s1") || strings.Contains(readString(filepath.Join(s.root, "shop", "a", "dev", "current", ".devopsy-log")), "s1") {
		t.Errorf("a value was printed:\n%s", out)
	}

	// --no-clone inherits nothing.
	write(t, filepath.Join(shop, "a", "prod", "shared", ".env"), "SHARED_KEY=s2\n", 0o600)
	next()
	if out, code := s.run(nil, "@a/dev", "--release", "--no-clone"); code != 0 || strings.Contains(out, "Inherited") || readString(childEnv) != "OWN=1\nSHARED_KEY='s1'\n" {
		t.Fatalf("--no-clone (%d): %q\n%s", code, readString(childEnv), out)
	}

	// Across servers each level is written on the child's, before the copy.
	env := []string{"DEVOPSY_SERVER_PROD=devopsy@one", "SSH_AUTH_SOCK=/tmp/agent.sock"}
	out, code = s.run(env, "@devopsy@two:a/dev", "--clone", "--yes")
	if code != 0 {
		t.Fatalf("--clone (%d):\n%s", code, out)
	}
	for _, want := range []string{
		"Inherited SHARED_KEY from @devopsy@one:a/prod's shared .env into devopsy@two:" + childEnv,
		"Inherited INST_KEY from @devopsy@one:a/prod's instance .env into devopsy@two:" + filepath.Join(shop, "a", ".env"),
		"Inherited PROJ_KEY from @devopsy@one:a/prod's project .env into devopsy@two:" + filepath.Join(shop, ".env"),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if i, j := strings.Index(out, "Inherited SHARED_KEY"), strings.Index(out, "Copying "); i < 0 || j < i {
		t.Errorf("inherited after the copy:\n%s", out)
	}
	if got := readString(childEnv); got != "OWN=1\nSHARED_KEY='s2'\n" {
		t.Errorf("child's shared/.env: %q", got)
	}
	if got := readString(filepath.Join(shop, "a", ".env")); got != "INST_KEY='i 1'\n" {
		t.Errorf("instance .env: %q", got)
	}
}
