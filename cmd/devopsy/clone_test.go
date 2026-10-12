package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// cloneFakeDocker logs every call, with its compose project, to
// $DOCKER_LOG, answers compose config and ps for a project whose app is
// labeled pause and db stop (FAKE_APP and FAKE_DB change them, none for no
// label; $DOCKER_LOG.<compose project> sets them for one side, past the
// clean environment), with each container in the state its label gives
// it (paused, exited, else running), and copies what the rsync containers
// would: within one host, or from the daemon's mount (remembered) across
// hosts.
const cloneFakeDocker = `#!/bin/sh
echo "${COMPOSE_PROJECT_NAME:-} $*" >> "$DOCKER_LOG"
[ ! -f "$DOCKER_LOG.${COMPOSE_PROJECT_NAME:-}" ] || . "$DOCKER_LOG.${COMPOSE_PROJECT_NAME:-}"
if [ "$1" = compose ] && [ "$2" = -f ]; then dir=$(dirname "$3"); fi
for a; do last2=${last:-}; last=$a; done
case "$*" in
  *" config --format json")
    lab() { if [ "$1" = none ]; then echo '{}'; else echo "{\"devopsy.clone\":\"$1\"}"; fi; }
    printf '{"services":{"app":{"labels":%s,"volumes":[{"type":"bind","source":"%s/mnt/storage"}]},"db":{"labels":%s,"volumes":[{"type":"bind","source":"%s/mnt/db"}]}}}\n' "$(lab "${FAKE_APP:-pause}")" "$dir" "$(lab "${FAKE_DB:-stop}")" "$dir" ;;
  *" stop db") [ "${FAIL_STOP:-}" != "${COMPOSE_PROJECT_NAME:-}" ] || exit 1 ;;
  *" ps --services --status running") echo app; echo db ;;
  *" ps --services --status paused") echo app ;;
  *" ps --services --all --status exited") echo db ;;
  *" ps --all --format json db") echo '{"Service":"db","State":"running","Health":"healthy"}' ;;
  *" ps --all --format json")
    st() { case $1 in pause) echo paused ;; stop) echo exited ;; *) echo running ;; esac; }
    printf '{"Service":"app","State":"%s"}\n{"Service":"db","State":"%s"}\n' "$(st "${FAKE_APP:-pause}")" "$(st "${FAKE_DB:-stop}")" ;;
  "run --rm -d "*)
    for a; do case $a in *:/mnt:ro) echo "${a%:/mnt:ro}" > "$DOCKER_LOG.daemon" ;; esac; done
    echo cid123 ;;
  "inspect "*) echo 172.17.0.9 ;;
  "run --rm --network host "*)
    [ -z "${FAIL_RSYNC:-}" ] || exit "$FAIL_RSYNC"
    for a; do case $a in *:/mnt) dst=${a%:/mnt} ;; esac; done
    src=$(cat "$DOCKER_LOG.daemon"); d=${last#/mnt/}
    mkdir -p "$dst/$d" && cp -R "$src/$d." "$dst/$d" ;;
  "run --rm --name devopsy-clone-"*)
    [ -z "${FAIL_RSYNC:-}" ] || exit "$FAIL_RSYNC"
    if [ -n "${FAKE_SLOW_RSYNC:-}" ]; then touch "$FAKE_SLOW_RSYNC"; exec sleep 20; fi
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
# FAKE_SSH_DETACH: the push outlives its ssh, as on a server, where only
# its stdin closing tells it the session is gone.
case "$s" in *--clone-push*) if [ -n "${FAKE_SSH_DETACH:-}" ]; then sh -c "$s" <&0 & wait $!; exit $?; fi ;; esac
# FAKE_HOLDER_DIE: the holder's session dies once the copy started (the
# file named exists), as a dropped connection would end it.
case "$s" in *"ready for clone in"*) if [ -n "${FAKE_HOLDER_DIE:-}" ]; then
  sh -c "$s" <&0 & pid=$!
  while [ ! -e "$FAKE_HOLDER_DIE" ]; do sleep 0.1; done
  kill $pid 2>/dev/null; wait $pid 2>/dev/null; exit 255
fi ;; esac
case "$opts" in *" -R "*) [ -z "${FAKE_NO_FORWARD:-}" ] || { echo "Error: remote port forwarding failed for listen port 1" >&2; exit 255; } ;; esac
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
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\necho \"flock $*\" >> \"$DOCKER_LOG\"\nexit 0\n", 0o755)
	// The agent's keys (ssh-add -l): FAKE_AGENT=empty has none, dead no agent.
	write(t, filepath.Join(bin, "ssh-add"), "#!/bin/sh\ncase ${FAKE_AGENT:-} in empty) exit 1 ;; dead) exit 2 ;; esac\nexit 0\n", 0o755)
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
		"run --rm --name devopsy-clone-", " -v " + s.mnt("prod") + ":/src:ro -v " + s.mnt("dev") + ":/dst devopsy-rsync:1 rsync -a --delete --numeric-ids /src/db/ /dst/db/",
		"run --rm --name devopsy-clone-", " -v " + s.mnt("prod") + ":/src:ro -v " + s.mnt("dev") + ":/dst devopsy-rsync:1 rsync -a --delete --numeric-ids /src/storage/ /dst/storage/",
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
	// The push holds the parent's release lock shared and its clone lock
	// exclusively; the holder the child's release lock.
	if i, j := strings.Index(docker, "flock -s -w 600 9"), strings.Index(docker, "flock -w 600 8"); i < 0 || j < i || !strings.Contains(docker, "flock -w 600 9") {
		t.Errorf("locks:\n%s", docker)
	}
	// Files vanished during the copy (rsync's 24, a live service's
	// temporary files): a notice per entry, both entries copied, the run
	// step runs.
	s.logs()
	out, code = s.run([]string{"FAIL_RSYNC=24"}, "@dev", "--clone", "--yes")
	docker, _ = s.logs()
	if code != 0 || strings.Count(out, "some files vanished during the copy") != 2 || !strings.Contains(out, "deploy in shop-dev") || !strings.Contains(docker, "/src/storage/ /dst/storage/") {
		t.Errorf("vanished files (%d):\n%s\n%s", code, out, docker)
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

	// --release --clone clones on any release, asking first on one that
	// has data (--yes, or a terminal); --no-clone never.
	next()
	before, _ := os.ReadDir(filepath.Join(s.root, "shop", "dev", "releases"))
	if out, code := s.run(nil, "@dev", "--release", "--clone"); code == 0 || !strings.Contains(out, "add --yes when not at a terminal") || !strings.Contains(out, "Nothing released.") {
		t.Fatalf("--release --clone without --yes (%d):\n%s", code, out)
	}
	if after, _ := os.ReadDir(filepath.Join(s.root, "shop", "dev", "releases")); len(after) != len(before) {
		t.Errorf("uploaded before asking: %d releases, then %d", len(before), len(after))
	}
	if out, code := s.run(nil, "@dev", "--release", "--clone", "--yes"); code != 0 || !strings.Contains(out, "Cloning @devopsy@one:prod's data") {
		t.Fatalf("--release --clone --yes (%d):\n%s", code, out)
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

	// A first release never asks, --clone or not.
	out, code := s.run([]string{"FAIL_RSYNC=23"}, "@dev", "--release", "--clone")
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
	// An agent without keys, or none behind the socket: the same, before
	// any session.
	agent := append(env, "SSH_AUTH_SOCK=/tmp/agent.sock")
	for fake, want := range map[string]string{"empty": "holds no key: ssh-add the key devopsy@two accepts", "dead": "none answers at SSH_AUTH_SOCK (/tmp/agent.sock)"} {
		if out, code := s.run(append(agent, "FAKE_AGENT="+fake), "@devopsy@two:dev", "--release"); code == 0 || !strings.Contains(out, want) || s.current("dev") != "" {
			t.Fatalf("agent %s (%d):\n%s", fake, code, out)
		}
		if _, ssh := s.logs(); strings.Count(ssh, "\n") != 1 {
			t.Errorf("sessions before the agent check (%s):\n%s", fake, ssh)
		}
	}
	// A child behind a jump host is out of the parent's reach.
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
	if !strings.Contains(push, "-o ClearAllForwardings=yes -o ForwardAgent=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=4 -T devopsy@one") {
		t.Errorf("push session: %q\n%s", push, ssh)
	}
	// The parent's sessions to the child: never the forwarded agent, no
	// password prompt, and a short connect timeout.
	childSSH := "-o ForwardAgent=no -o BatchMode=yes -o ConnectTimeout=30 -o StrictHostKeyChecking=accept-new"
	if !strings.Contains(receive, "-o ExitOnForwardFailure=yes "+childSSH+" -o ServerAliveInterval=30 -o ServerAliveCountMax=4 -T -R 127.0.0.1:") || !strings.Contains(receive, ":172.17.0.9:873 -p 22 devopsy@two") || strings.Contains(receive, "ClearAllForwardings") {
		t.Errorf("receive session: %q\n%s", receive, ssh)
	}
	// Every session of the clone keeps itself alive: the holder, the push,
	// the push's look at the child's services (--clone-exec) and the
	// receive.
	keep := "-o ServerAliveInterval=30 -o ServerAliveCountMax=4"
	if strings.Count(ssh, keep) != 4 || !strings.Contains(ssh, keep+" -T devopsy@two\n") || !strings.Contains(ssh, childSSH+" "+keep+" -T -p 22 devopsy@two\n") {
		t.Errorf("keepalive on holder, push, exec and receive:\n%s", ssh)
	}
	for _, want := range []string{"run --rm -d -v " + s.mnt("prod") + ":/mnt:ro", "inspect -f", "run --rm --network host -v " + s.mnt("dev") + ":/mnt devopsy-rsync:1 rsync -a --delete --numeric-ids rsync://127.0.0.1:", " rm -f cid123"} {
		if !strings.Contains(docker, want) {
			t.Errorf("missing %q in:\n%s", want, docker)
		}
	}
	// Vanished files across servers: the receive script goes on too.
	s.logs()
	if out, code := s.run(append(agent, "FAIL_RSYNC=24"), "@devopsy@two:dev", "--clone", "--yes"); code != 0 || strings.Count(out, "some files vanished during the copy") != 2 || !strings.Contains(out, "deploy in shop-dev") {
		t.Errorf("vanished files across servers (%d):\n%s", code, out)
	}
	s.logs()

	// --clone-exec across servers keeps its session alive too.
	execEnv := append(agent, "DEVOPSY_CLONE_TO=devopsy@two", "DEVOPSY_CLONE_SSH_PORT=22", "DEVOPSY_CLONE_TO_DIR="+filepath.Join(s.root, "shop", "dev", "current", ".devopsy"), "DEVOPSY_CLONE_TO_PROJECT=shop-dev")
	if out, code := s.run(execEnv, "--clone-exec", "show"); code != 0 || !strings.Contains(out, "show project=shop-dev") {
		t.Fatalf("--clone-exec (%d):\n%s", code, out)
	}
	if _, ssh := s.logs(); !strings.Contains(ssh, "-o ClearAllForwardings=yes "+childSSH+" "+keep+" -T -p 22 devopsy@two") {
		t.Errorf("--clone-exec session: %s", ssh)
	}

	// A restricted key refuses the agent: the parent's side says so, and
	// the child's services come back.
	out, code = s.run(append(agent, "FAKE_RESTRICT=1"), "@devopsy@two:dev", "--clone", "--yes")
	docker, _ = s.logs()
	if code == 0 || !strings.Contains(out, "no SSH agent was forwarded to") || !strings.Contains(out, "restrict,agent-forwarding") || !strings.Contains(docker, "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" unpause app") {
		t.Fatalf("restricted key (%d):\n%s\n%s", code, out, docker)
	}

	// A key restricted on the child's server refuses the reverse forward,
	// which sshd reports as a taken port: three tries, then both causes.
	out, code = s.run(append(agent, "FAKE_NO_FORWARD=1"), "@devopsy@two:dev", "--clone", "--yes")
	docker, ssh = s.logs()
	if code == 0 || !strings.Contains(out, "refused the reverse forward 3 times") || !strings.Contains(out, "port-forwarding") || strings.Count(ssh, " -R ") != 3 || !strings.Contains(docker, " rm -f cid123") {
		t.Fatalf("refused forward (%d):\n%s\n%s\n%s", code, out, ssh, docker)
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
	out, code := s.run([]string{"FAIL_RSYNC=23"}, "@dev", "--clone", "--yes")
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
	write(t, filepath.Join(caps, "push"), "#!/bin/sh\nset -e\necho \"push from $COMPOSE_PROJECT_NAME to $DEVOPSY_CLONE_TO_PROJECT custom=$DEVOPSY_CLONE_CUSTOM args=$DEVOPSY_CLONE_ARGS\"\ndevopsy --clone-rsync storage -- --checksum\necho \"dump of $COMPOSE_PROJECT_NAME pw=$DB_PASSWORD\" | devopsy --clone-exec show\n", 0o755)
	s.releaseProd(nil)
	write(t, filepath.Join(s.root, "shop", "prod", "shared", ".env"), "DB_PASSWORD=parent-secret\n", 0o644)
	write(t, filepath.Join(s.root, "shop", "dev", "shared", ".env"), "DB_PASSWORD=child-secret\n", 0o644)

	out, code := s.run(nil, "@dev", "--release", "--", "x")
	if code == 0 {
		t.Fatalf("--release with arguments (%d):\n%s", code, out)
	}
	// db is custom: devopsy copies the rest first, then the push copies it.
	// The prepare and finish hooks run with the generic handling, around
	// it.
	custom := []string{"FAKE_DB=custom"}
	out, code = s.run(custom, "@dev", "--release")
	docker, _ := s.logs()
	rest := out
	for _, want := range []string{"prepare in shop-dev from devopsy@one:prod", "devopsy: pausing app", "Copying", "devopsy: pausing app", "rsync mnt/storage", "devopsy: unpausing app", "push from shop-prod to shop-dev custom=db args=", "show project=shop-dev pw=child-secret", "dump of shop-prod pw=parent-secret", "devopsy: unpausing app", "finish go in shop-dev", "deploy in shop-dev"} {
		i := strings.Index(rest, want)
		if code != 0 || i < 0 {
			t.Fatalf("missing %q in order (%d):\n%s", want, code, out)
		}
		rest = rest[i+len(want):]
	}
	if strings.Contains(docker, "stop db") || !strings.Contains(docker, "--numeric-ids /src/storage/ /dst/storage/") || !strings.Contains(docker, "--numeric-ids --checksum /src/storage/ /dst/storage/") || strings.Contains(docker, "/src/db/") {
		t.Errorf("docker:\n%s", docker)
	}
	// --clone's arguments are the push's: --clone-rsync only takes its own.
	out, code = s.run(custom, "@dev", "--clone", "--yes", "--", "--exclude=css")
	docker, _ = s.logs()
	if code != 0 || !strings.Contains(out, "args=--exclude=css") || strings.Contains(docker, "--checksum --exclude=css") || !strings.Contains(docker, "--numeric-ids --checksum /src/storage/") {
		t.Errorf("--clone -- args (%d):\n%s\n%s", code, out, docker)
	}
	if got := readString(filepath.Join(s.mnt("dev"), "storage", "file.txt")); got != "from prod\n" {
		t.Errorf("not copied: %q", got)
	}

	// A push without a custom service is not run; a custom service
	// without a push fails the clone.
	out, code = s.run(nil, "@dev", "--clone", "--yes")
	if code != 0 || !strings.Contains(out, "capabilities/clone/push present but no service labeled devopsy.clone=custom: not run") || strings.Contains(out, "push from") {
		t.Errorf("push without custom (%d):\n%s", code, out)
	}
	s.logs()
	if err := os.Remove(filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "capabilities", "clone", "push")); err != nil {
		t.Fatal(err)
	}
	out, code = s.run(custom, "@dev", "--clone", "--yes")
	if code == 0 || !strings.Contains(out, "db labeled devopsy.clone=custom: the parent's release needs an executable .devopsy/capabilities/clone/push") || !strings.Contains(out, "aborting the clone") {
		t.Errorf("custom without push (%d):\n%s", code, out)
	}
	s.logs()

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
	// A parent that cannot be resolved only fails what clones.
	bad := []string{"DEVOPSY_SERVER_PROD=bad:host"}
	for _, args := range [][]string{{"@dev", "--clone", "--yes"}, {"@dev", "--release"}} {
		if out, code := s.run(bad, args...); code == 0 || !strings.Contains(out, `environment dev: parent prod: server "bad:host"`) || s.current("dev") != "" {
			t.Errorf("%v, unresolved parent (%d):\n%s", args, code, out)
		}
	}
	if out, code := s.run(bad, "@dev", "--release", "--no-clone"); code != 0 || s.current("dev") == "" {
		t.Errorf("--no-clone, unresolved parent (%d):\n%s", code, out)
	}
	if out, code := s.run(bad, "@dev", "show"); code != 0 || !strings.Contains(out, "show project=shop-dev") {
		t.Errorf("a command, unresolved parent (%d):\n%s", code, out)
	}
	if err := os.RemoveAll(filepath.Join(s.root, "shop", "dev")); err != nil {
		t.Fatal(err)
	}
	s.logs()
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

// Interrupted during the copy, devopsy ends the push session first, then
// aborts: the child's services come back and its run step does not run.
func TestCloneInterrupt(t *testing.T) {
	s := newCloneSetup(t)
	started := filepath.Join(t.TempDir(), "started")
	write(t, filepath.Join(s.dot, "capabilities", "clone", "push"), "#!/bin/sh\ntouch \"$FAKE_STARTED\"\nexec sleep 20\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release", "--no-clone"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()

	// Output to a file: the push's sleep, orphaned, keeps a pipe open.
	outFile, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "@dev", "--clone", "--yes")
	cmd.Dir = s.project
	cmd.Env = append(append([]string{}, s.env...), "FAKE_STARTED="+started, "FAKE_DB=custom")
	cmd.Stdin = strings.NewReader("")
	cmd.Stdout, cmd.Stderr = outFile, outFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if i > 200 {
			_ = cmd.Process.Kill()
			t.Fatal("the push never started")
		}
		time.Sleep(50 * time.Millisecond)
	}
	start := time.Now()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	out := readString(outFile.Name())
	docker, _ := s.logs()
	if cmd.ProcessState.ExitCode() == 0 || time.Since(start) > 15*time.Second || !strings.Contains(out, "stopping the copy from @devopsy@one:prod, then aborting") || strings.Contains(out, "deploy in shop-dev") {
		t.Fatalf("interrupt (%d, %s):\n%s", cmd.ProcessState.ExitCode(), time.Since(start), out)
	}
	if !strings.Contains(docker, "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" unpause app") {
		t.Errorf("child not restored:\n%s", docker)
	}
}

// Labels opt in: without any, nothing is copied or touched; the flush hook
// runs for each live and pause service, on both sides, before it is
// paused or copied, never for stop; a failing flush aborts.
func TestCloneLabels(t *testing.T) {
	s := newCloneSetup(t)
	write(t, filepath.Join(s.dot, "capabilities", "clone", "flush"), "#!/bin/sh\necho \"flush $1 in $COMPOSE_PROJECT_NAME\"\n[ \"${FAIL_FLUSH:-}\" != \"$COMPOSE_PROJECT_NAME\" ]\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release", "--no-clone"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()

	out, code := s.run([]string{"FAKE_APP=none", "FAKE_DB=none"}, "@dev", "--clone", "--yes")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(out, "nothing to clone: no service labeled devopsy.clone") || !strings.Contains(out, "deploy in shop-dev") || strings.Contains(out, "flush") ||
		strings.Contains(docker, "pause") || strings.Contains(docker, "rsync") {
		t.Fatalf("no labels (%d):\n%s\n%s", code, out, docker)
	}

	out, code = s.run(nil, "@dev", "--clone", "--yes")
	s.logs()
	rest := out
	for _, want := range []string{"flush app in shop-dev", "devopsy: pausing app", "devopsy: stopping db", "Copying", "flush app in shop-prod", "devopsy: pausing app", "rsync mnt/db", "deploy in shop-dev"} {
		i := strings.Index(rest, want)
		if code != 0 || i < 0 {
			t.Fatalf("missing %q in order (%d):\n%s", want, code, out)
		}
		rest = rest[i+len(want):]
	}
	if strings.Contains(out, "flush db") {
		t.Errorf("flush for a stop service:\n%s", out)
	}
	out, code = s.run([]string{"FAKE_DB=live"}, "@dev", "--clone", "--yes")
	s.logs()
	if code != 0 || strings.Count(out, "flush db in") != 2 || strings.Count(out, "flush app in") != 2 {
		t.Errorf("flush for live services (%d):\n%s", code, out)
	}

	// A failing flush aborts like a failing prepare: on the child before
	// the copy, on the parent through the push.
	for _, side := range []string{"shop-dev", "shop-prod"} {
		out, code = s.run([]string{"FAIL_FLUSH=" + side}, "@dev", "--clone", "--yes")
		docker, _ = s.logs()
		if code == 0 || !strings.Contains(out, "flush app in "+side) || strings.Contains(out, "deploy in shop-dev") || !strings.Contains(docker, "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" unpause app") {
			t.Errorf("failing flush on %s (%d):\n%s\n%s", side, code, out, docker)
		}
	}
}

// A service the parent copies that the child leaves running (custom or no
// label there) fails the push before the parent pauses anything: the
// child restores its services; live on the child says running is fine.
func TestCloneMismatch(t *testing.T) {
	s := newCloneSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release", "--no-clone"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()
	for _, label := range []string{"custom", "none"} {
		write(t, s.log+".shop-dev", "FAKE_DB="+label+"\n", 0o644)
		out, code := s.run(nil, "@dev", "--clone", "--yes")
		docker, _ := s.logs()
		want := "labeled devopsy.clone=custom"
		if label == "none" {
			want = "without a devopsy.clone label"
		}
		if code == 0 || !strings.Contains(out, "the child's db is running, "+want+", and mnt/db would change under it: the parent copies it for db (devopsy.clone=stop)") ||
			strings.Contains(out, "deploy in shop-dev") || strings.Count(docker, "shop-prod compose") != 1 || !strings.Contains(docker, "shop-prod compose -f "+filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml")+" --profile * config") ||
			strings.Contains(docker, "rsync") || !strings.Contains(docker, "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" unpause app") {
			t.Errorf("child db %s (%d):\n%s\n%s", label, code, out, docker)
		}
	}
	write(t, s.log+".shop-dev", "FAKE_DB=live\n", 0o644)
	out, code := s.run(nil, "@dev", "--clone", "--yes")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(out, "deploy in shop-dev") || !strings.Contains(docker, "/src/db/ /dst/db/") {
		t.Errorf("child db live (%d):\n%s\n%s", code, out, docker)
	}
	// The other way round, the child stops what the parent leaves to its
	// push: nothing to refuse.
	os.Remove(s.log + ".shop-dev")
	write(t, filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "capabilities", "clone", "push"), "#!/bin/sh\necho push\n", 0o755)
	out, code = s.run([]string{"FAKE_DB=custom"}, "@dev", "--clone", "--yes")
	s.logs()
	if code != 0 || !strings.Contains(out, "deploy in shop-dev") {
		t.Errorf("parent custom (%d):\n%s", code, out)
	}
	os.Remove(s.log + ".shop-dev")
}

// An inherit: entry conditioned on the parent's clone labels (NAME:
// {<service>: <policy>}) is inherited only when they hold; a service the
// parent does not have is an error.
func TestCloneInheritWhen(t *testing.T) {
	s := newCloneSetup(t)
	write(t, filepath.Join(s.dot, "config.yaml"), "project: shop\ndefaults:\n  mode: image\n  release: deploy\nenvironments:\n  prod: {inherit: [SHARED_KEY, {DB_PASSWORD: {db: stop}}, {OTHER: {db: custom, app: pause}}]}\n  dev: {parent: prod}\n", 0o644)
	s.releaseProd(nil)
	write(t, filepath.Join(s.root, "shop", "prod", "shared", ".env"), "SHARED_KEY=s\nDB_PASSWORD=p\nOTHER=o\n", 0o600)
	childEnv := filepath.Join(s.root, "shop", "dev", "shared", ".env")

	out, code := s.run(nil, "@dev", "--release")
	if code != 0 || !strings.Contains(out, "Inherited SHARED_KEY, DB_PASSWORD from @devopsy@one:prod's shared .env") || !strings.Contains(out, "Not inherited from @devopsy@one:prod, its services' clone labels say: OTHER (db is stop).") {
		t.Fatalf("db stop (%d):\n%s", code, out)
	}
	if got := readString(childEnv); got != "SHARED_KEY='s'\nDB_PASSWORD='p'\n" {
		t.Errorf("child's shared/.env: %q", got)
	}
	s.logs()
	out, code = s.run([]string{"FAKE_DB=live"}, "@dev", "--clone", "--yes")
	if code != 0 || !strings.Contains(out, "Inherited SHARED_KEY from") || !strings.Contains(out, "clone labels say: DB_PASSWORD (db is live), OTHER (db is live).") {
		t.Errorf("db live (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "--debug", "environments"); code != 0 || !strings.Contains(out, "SHARED_KEY, DB_PASSWORD (db: stop), OTHER (app: pause, db: custom)") {
		t.Errorf("--debug environments (%d):\n%s", code, out)
	}
	write(t, filepath.Join(s.dot, "config.yaml"), "project: shop\ndefaults:\n  mode: image\n  release: deploy\nenvironments:\n  prod: {inherit: [{X: {nope: stop}}]}\n  dev: {parent: prod}\n", 0o644)
	if out, code := s.run(nil, "@dev", "--clone", "--yes"); code == 0 || !strings.Contains(out, "inherit: X (nope: stop): @devopsy@one:prod has no service nope") {
		t.Errorf("unknown service (%d):\n%s", code, out)
	}
}

// Interrupted during the copy, the parent's push stops at once, though
// its session outlives devopsy's ssh: the closed stdin tells it. It
// removes the rsync container, restores its services, and never runs the
// capability's push for the custom services.
func TestCloneInterruptCopy(t *testing.T) {
	s := newCloneSetup(t)
	tmp := t.TempDir()
	started, pushed := filepath.Join(tmp, "started"), filepath.Join(tmp, "pushed")
	write(t, filepath.Join(s.dot, "capabilities", "clone", "push"), "#!/bin/sh\ntouch \"$FAKE_PUSHED\"\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release", "--no-clone"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()

	outFile, err := os.CreateTemp(tmp, "out")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "@dev", "--clone", "--yes")
	cmd.Dir = s.project
	cmd.Env = append(append([]string{}, s.env...), "FAKE_SLOW_RSYNC="+started, "FAKE_PUSHED="+pushed, "FAKE_DB=custom", "FAKE_SSH_DETACH=1")
	cmd.Stdin = strings.NewReader("")
	cmd.Stdout, cmd.Stderr = outFile, outFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if i > 200 {
			_ = cmd.Process.Kill()
			t.Fatal("the copy never started")
		}
		time.Sleep(50 * time.Millisecond)
	}
	start := time.Now()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	if cmd.ProcessState.ExitCode() == 0 {
		t.Fatalf("interrupted clone succeeded:\n%s", readString(outFile.Name()))
	}
	restored := "shop-prod compose -f " + filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml") + " unpause app"
	for !strings.Contains(readString(s.log), restored) {
		if time.Since(start) > 10*time.Second {
			t.Fatalf("the parent did not restore in time:\n%s\n%s", readString(s.log), readString(outFile.Name()))
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	if !strings.Contains(readString(s.log), "rm -f devopsy-clone-") {
		t.Errorf("the rsync container was not removed:\n%s", readString(s.log))
	}
	if _, err := os.Stat(pushed); err == nil {
		t.Errorf("the capability's push ran after the interrupt")
	}
	// What the parent did once nobody read its session is in its log.
	if log := readString(filepath.Join(s.root, "shop", "prod", ".devopsy-clone.log")); !strings.Contains(log, ": the clone's session is gone: restoring what was paused or stopped") || !strings.Contains(log, "devopsy: unpausing app") {
		t.Errorf("the parent's clone log:\n%s", log)
	}
}

// The holder's session ends during the copy: the push is stopped, the
// parent restored, nothing answered, no run step.
func TestCloneHolderGone(t *testing.T) {
	s := newCloneSetup(t)
	started := filepath.Join(t.TempDir(), "started")
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release", "--no-clone"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()
	out, code := s.run([]string{"FAKE_SLOW_RSYNC=" + started, "FAKE_HOLDER_DIE=" + started}, "@dev", "--clone", "--yes")
	docker, _ := s.logs()
	if code == 0 || !strings.Contains(out, "ended during the copy (255): stopping the copy from @devopsy@one:prod") || strings.Contains(out, "deploy in shop-dev") {
		t.Fatalf("holder gone (%d):\n%s", code, out)
	}
	restored := "shop-prod compose -f " + filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml") + " unpause app"
	if !strings.Contains(docker, restored) || !strings.Contains(docker, "rm -f devopsy-clone-") {
		t.Errorf("the parent was not restored or the copy not stopped:\n%s", docker)
	}
}
