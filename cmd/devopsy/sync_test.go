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

// syncFakeDocker logs every call, with its compose project, to
// $DOCKER_LOG, answers compose config and ps for a project whose app is
// labeled pause and db stop (FAKE_APP and FAKE_DB change them, none for no
// label; $DOCKER_LOG.<compose project> sets them for one side, past the
// clean environment), with each container in the state its label gives
// it (paused, exited, else running), and copies what the rsync containers
// would: within one host, or from the daemon's mount (remembered) across
// hosts.
const syncFakeDocker = `#!/bin/sh
echo "${COMPOSE_PROJECT_NAME:-} $*" >> "$DOCKER_LOG"
[ ! -f "$DOCKER_LOG.${COMPOSE_PROJECT_NAME:-}" ] || . "$DOCKER_LOG.${COMPOSE_PROJECT_NAME:-}"
if [ "$1" = compose ] && [ "$2" = -f ]; then dir=$(dirname "$3"); fi
for a; do last2=${last:-}; last=$a; done
# FAKE_ABSENT names services without a container (a first release).
absent() { case " ${FAKE_ABSENT:-} " in *" $1 "*) return 0 ;; esac; return 1; }
case "$*" in
  *" config --format json")
    lab() { if [ "$1" = none ]; then echo '{}'; else echo "{\"devopsy.sync\":\"$1\"}"; fi; }
    printf '{"name":"%s","services":{"app":{"labels":%s,"volumes":[{"type":"bind","source":"%s/mnt%s"}]},"db":{"labels":%s,"volumes":[{"type":"bind","source":"%s/mnt/db"}]}},"networks":{"default":{"name":"%s_default"},"traefik":{"name":"traefik","external":true}}}\n' "${COMPOSE_PROJECT_NAME:-}" "$(lab "${FAKE_APP:-pause}")" "$dir" "${FAKE_APP_MOUNT-/storage}" "$(lab "${FAKE_DB:-stop}")" "$dir" "${COMPOSE_PROJECT_NAME:-}" ;;
  # FAKE_NETWORK_OWNER: every network exists and is that project's.
  "network inspect "*) [ -n "${FAKE_NETWORK_OWNER:-}" ] || exit 1; echo "$FAKE_NETWORK_OWNER" ;;
  *" stop db") [ "${FAIL_STOP:-}" != "${COMPOSE_PROJECT_NAME:-}" ] || exit 1 ;;
  *" ps --services --status running") absent app || echo app; absent db || [ -n "${FAKE_DB_DOWN:-}" ] || echo db ;;
  *" ps --services --status paused") absent app || echo app ;;
  *" ps --services --status restarting") [ -z "${FAKE_DB_RESTARTING:-}" ] || echo db ;;
  *" ps --services --all --status exited") absent db || echo db ;;
  *" ps --all --format json db") echo '{"Service":"db","State":"running","Health":"healthy"}' ;;
  *" ps --all --format json")
    st() { case $1 in pause) echo paused ;; stop) echo exited ;; *) echo running ;; esac; }
    absent app || printf '{"Service":"app","State":"%s"}\n' "$(st "${FAKE_APP:-pause}")"
    absent db || printf '{"Service":"db","State":"%s"}\n' "$(st "${FAKE_DB:-stop}")" ;;
  "run --rm -d "*)
    for a; do case $a in *:/mnt:ro) echo "${a%:/mnt:ro}" > "$DOCKER_LOG.daemon" ;; RSYNCD_CONF=*) printf '%s\n' "${a#RSYNCD_CONF=}" > "$DOCKER_LOG.rsyncd" ;; esac; done
    echo cid123 ;;
  "inspect "*) echo 172.17.0.9 ;;
  "run --rm --network host "*)
    [ -z "${FAIL_RSYNC:-}" ] || exit "$FAIL_RSYNC"
    for a; do case $a in *:/mnt) dst=${a%:/mnt} ;; esac; done
    src=$(cat "$DOCKER_LOG.daemon"); d=${last#/mnt/}
    # Only a module the daemon was given (its name, or mnt for all of it).
    m=${last2#rsync://127.0.0.1:*/}; m=${m%/}; f=${m#*/}; m=${m%%/*}
    grep -qx "\[$m\]" "$DOCKER_LOG.rsyncd" || { echo "@ERROR: Unknown module '$m'" >&2; exit 5; }
    if [ "$f" != "$m" ]; then cp "$src/$f" "$dst/"; else mkdir -p "$dst/$d" && cp -R "$src/$d." "$dst/$d"; fi ;;
  "run --rm --name devopsy-sync-"*)
    [ -z "${FAIL_RSYNC:-}" ] || exit "$FAIL_RSYNC"
    if [ -n "${FAKE_SLOW_RSYNC:-}" ]; then touch "$FAKE_SLOW_RSYNC"; exec sleep 20; fi
    for a; do case $a in *:/src:ro) src=${a%:/src:ro} ;; *:/dst) dst=${a%:/dst} ;; esac; done
    d=${last2#/src/}
    if [ -f "$src/$d" ]; then cp "$src/$d" "$dst/"; else mkdir -p "$dst/$d" && cp -R "$src/$d." "$dst/$d"; fi ;;
esac
exit 0
`

// syncFakeSSH runs a session's script here (its last argument), logging
// the options of each, and answers ssh -G from the destination. Like a
// server, it forwards the agent only when asked, and never with
// FAKE_RESTRICT (a restricted key).
const syncFakeSSH = `#!/bin/sh
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
# its stdin closing tells it the session is gone. A background job's
# stdin is /dev/null before its own redirections apply (dash), hence fd 3.
exec 3<&0
case "$s" in *--sync-push*) if [ -n "${FAKE_SSH_DETACH:-}" ]; then sh -c "$s" <&3 & wait $!; exit $?; fi ;; esac
# FAKE_HOLDER_DIE: the holder's session dies once the copy started (the
# file named exists), as a dropped connection would end it.
case "$s" in *"ready for sync in"*) if [ -n "${FAKE_HOLDER_DIE:-}" ]; then
  sh -c "$s" <&3 & pid=$!
  while [ ! -e "$FAKE_HOLDER_DIE" ]; do sleep 0.1; done
  # dash runs the inner sh as a child instead of exec'ing it: kill both.
  pkill -P $pid 2>/dev/null; kill $pid 2>/dev/null; wait $pid 2>/dev/null; exit 255
fi ;; esac
case "$opts" in *" -R "*) [ -z "${FAKE_NO_FORWARD:-}" ] || { echo "Error: remote port forwarding failed for listen port 1" >&2; exit 255; } ;; esac
exec sh -c "$s"
`

// syncSetup is a project with prod and its child dev on a fake server
// (this machine, root under tmp), and a run function.
type syncSetup struct {
	t                *testing.T
	root, project    string
	dot, sshLog, log string
	env              []string
}

func newSyncSetup(t *testing.T) *syncSetup {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(tmp, "bin")
	write(t, filepath.Join(bin, "fakessh"), syncFakeSSH, 0o755)
	write(t, filepath.Join(bin, "docker"), syncFakeDocker, 0o755)
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\necho \"flock $*\" >> \"$DOCKER_LOG\"\nexit 0\n", 0o755)
	// The agent's keys (ssh-add -l): FAKE_AGENT=empty has none, dead no agent.
	write(t, filepath.Join(bin, "ssh-add"), "#!/bin/sh\ncase ${FAKE_AGENT:-} in empty) exit 1 ;; dead) exit 2 ;; esac\nexit 0\n", 0o755)
	write(t, filepath.Join(bin, "mv"), "#!/bin/sh\nif [ \"$1\" = -Tf ]; then rm -f \"$3\"; exec /bin/mv \"$2\" \"$3\"; fi\nexec /bin/mv \"$@\"\n", 0o755)
	if err := os.Symlink(binary, filepath.Join(bin, "devopsy")); err != nil {
		t.Fatal(err)
	}
	s := &syncSetup{t: t, root: filepath.Join(tmp, "srv"), project: filepath.Join(tmp, "app"), sshLog: filepath.Join(tmp, "ssh.log"), log: filepath.Join(tmp, "docker.log")}
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

func (s *syncSetup) run(extra []string, args ...string) (string, int) {
	s.t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir = s.project
	cmd.Env = append(append([]string{}, s.env...), extra...)
	cmd.Stdin = strings.NewReader("")
	out, _ := cmd.CombinedOutput()
	return string(out), cmd.ProcessState.ExitCode()
}

// logs returns and empties the docker and ssh logs.
func (s *syncSetup) logs() (docker, ssh string) {
	d, _ := os.ReadFile(s.log)
	h, _ := os.ReadFile(s.sshLog)
	os.Remove(s.log)
	os.Remove(s.sshLog)
	return string(d), string(h)
}

func (s *syncSetup) current(env string) string {
	l, _ := os.Readlink(filepath.Join(s.root, "shop", env, "current"))
	return l
}

func (s *syncSetup) mnt(env string) string {
	return filepath.Join(s.root, "shop", env, "shared", "mnt")
}

func (s *syncSetup) releaseProd(extra []string, args ...string) {
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

// A child's first release syncs its parent on the same server: services
// paused and stopped on both sides, one container copying, then deploy on
// the parent's data. Later releases do not, unless --sync; --sync
// resyncs.
func TestSyncSameHost(t *testing.T) {
	s := newSyncSetup(t)
	s.releaseProd(nil)

	out, code := s.run(nil, "@dev", "--release")
	docker, ssh := s.logs()
	if code != 0 || !strings.Contains(out, "Syncing @devopsy@one:prod's data into it") || !strings.Contains(out, "deploy in shop-dev parent=prod devopsy@one:prod") || s.current("dev") == "" {
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
		"run --rm --name devopsy-sync-", " -v " + s.mnt("prod") + ":/src:ro -v " + s.mnt("dev") + ":/dst devopsy-rsync:1 rsync -a --delete --numeric-ids /src/db/ /dst/db/",
		"run --rm --name devopsy-sync-", " -v " + s.mnt("prod") + ":/src:ro -v " + s.mnt("dev") + ":/dst devopsy-rsync:1 rsync -a --delete --numeric-ids /src/storage/ /dst/storage/",
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
	// The push holds the parent's release lock shared and its sync lock
	// exclusively; the holder the child's release lock.
	if i, j := strings.Index(docker, "flock -n -s 9"), strings.Index(docker, "flock -n 8"); i < 0 || j < i || !strings.Contains(docker, "flock -n 9\n") {
		t.Errorf("locks:\n%s", docker)
	}
	// Files vanished during the copy (rsync's 24, a live service's
	// temporary files): a notice per entry, both entries copied, the run
	// step runs.
	s.logs()
	out, code = s.run([]string{"FAIL_RSYNC=24"}, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 || strings.Count(out, "some files vanished during the copy") != 2 || !strings.Contains(out, "deploy in shop-dev") || !strings.Contains(docker, "/src/storage/ /dst/storage/") {
		t.Errorf("vanished files (%d):\n%s\n%s", code, out, docker)
	}
	data := readString(filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "target.env"))
	if !strings.Contains(data, "DEVOPSY_PARENT='prod'\n") || !strings.Contains(data, "DEVOPSY_PARENT_TARGET='devopsy@one:prod'\n") {
		t.Errorf("target.env:\n%s", data)
	}

	// A later release does not sync.
	next()
	out, code = s.run(nil, "@dev", "--release")
	docker, _ = s.logs()
	if code != 0 || strings.Contains(out, "Syncing") || strings.Contains(docker, "pause") {
		t.Fatalf("second release (%d):\n%s\n%s", code, out, docker)
	}

	// --sync asks; --yes; arguments after -- go to rsync; the log goes
	// with the current release.
	write(t, filepath.Join(s.mnt("prod"), "storage", "file.txt"), "newer\n", 0o644)
	if out, code := s.run(nil, "@dev", "--sync"); code == 0 || !strings.Contains(out, "add --yes when not at a terminal") {
		t.Fatalf("--sync without a terminal (%d):\n%s", code, out)
	}
	out, code = s.run(nil, "@dev", "--sync", "--yes", "--", "--exclude=css")
	docker, _ = s.logs()
	if code != 0 || !strings.Contains(out, "syncing into") || !strings.Contains(out, "deploy in shop-dev") || !strings.Contains(docker, "--numeric-ids --exclude=css /src/storage/") {
		t.Fatalf("--sync (%d):\n%s\n%s", code, out, docker)
	}
	if got := readString(filepath.Join(s.mnt("dev"), "storage", "file.txt")); got != "newer\n" {
		t.Errorf("not resynced: %q", got)
	}
	if out, code := s.run(nil, "@dev", "--log"); code != 0 || !strings.Contains(out, "release exited 0") || !strings.Contains(out, "sync of devopsy@one:dev") || !strings.Contains(out, "sync exited 0") {
		t.Errorf("--log (%d):\n%s", code, out)
	}

	// --release-sync syncs on any release, asking first on one that
	// has data (--yes, or a terminal); --release-no-sync never.
	next()
	before, _ := os.ReadDir(filepath.Join(s.root, "shop", "dev", "releases"))
	if out, code := s.run(nil, "@dev", "--release-sync"); code == 0 || !strings.Contains(out, "add --yes when not at a terminal") || !strings.Contains(out, "Nothing released.") {
		t.Fatalf("--release-sync without --yes (%d):\n%s", code, out)
	}
	if after, _ := os.ReadDir(filepath.Join(s.root, "shop", "dev", "releases")); len(after) != len(before) {
		t.Errorf("uploaded before asking: %d releases, then %d", len(before), len(after))
	}
	if out, code := s.run(nil, "@dev", "--release-sync", "--yes"); code != 0 || !strings.Contains(out, "Syncing @devopsy@one:prod's data") {
		t.Fatalf("--release-sync --yes (%d):\n%s", code, out)
	}
	s.logs()
	if out, code := s.run(nil, "@dev", "--rollback", "--yes"); code == 0 || !strings.Contains(out, "--rollback takes no command") {
		t.Errorf("--rollback --sync (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--sync", "--help"); code != 0 || !strings.Contains(out, "Usage: devopsy @<target> --sync [--from <target>] [--yes] [-- args...]") || !strings.Contains(out, "devopsy.sync=pause") {
		t.Errorf("--sync --help (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--release", "--help"); code != 0 || !strings.Contains(out, "--release-sync [--yes]") {
		t.Errorf("--release --help (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev"); code != 0 || !strings.Contains(out, "--sync [--from <target>]") {
		t.Errorf("@dev help (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "--sync"); code == 0 || !strings.Contains(out, "--sync needs a target") {
		t.Errorf("--sync without a target (%d):\n%s", code, out)
	}
}

// A first release whose copy fails does not go live: the release is marked
// failed, nothing is current, the services restored, and the next release
// syncs again; --release-no-sync releases without the data.
func TestReleaseFirstSync(t *testing.T) {
	s := newSyncSetup(t)
	s.releaseProd(nil)

	// Nothing of the child exists on its first release: finish creates
	// the labeled services before its hook, where a re-sync restores
	// them, and the run step's up finds them running.
	write(t, s.log+".shop-dev", "FAKE_ABSENT='app db'\n", 0o644)
	out, code := s.run(nil, "@dev", "--release")
	docker, _ := s.logs()
	os.Remove(s.log + ".shop-dev")
	if code != 0 || !strings.Contains(out, "devopsy: creating app db") || strings.Count(out, "devopsy: pausing app") != 1 || !strings.Contains(docker, "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" up -d --wait app db") || strings.Contains(docker, "shop-prod compose -f "+filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml")+" up -d") {
		t.Fatalf("first release with no containers (%d):\n%s\n%s", code, out, docker)
	}
	os.RemoveAll(filepath.Join(s.root, "shop", "dev"))

	// A first release never asks, --sync or not.
	out, code = s.run([]string{"FAIL_RSYNC=23"}, "@dev", "--release-sync")
	docker, _ = s.logs()
	if code == 0 || s.current("dev") != "" || !strings.Contains(out, "sync failed (1), no previous release to go back to") || strings.Contains(out, "deploy in shop-dev") {
		t.Fatalf("failed sync (%d), current %q:\n%s", code, s.current("dev"), out)
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
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 || strings.Contains(out, "Syncing") || s.current("dev") == "" {
		t.Fatalf("--release-no-sync (%d):\n%s", code, out)
	}
	if docker, _ := s.logs(); strings.Contains(docker, "pause") {
		t.Errorf("--release-no-sync paused:\n%s", docker)
	}
}

// A service bind-mounting a file of mnt: copied as a file, on both paths.
func TestSyncFileEntry(t *testing.T) {
	s := newSyncSetup(t)
	s.releaseProd(nil)
	write(t, filepath.Join(s.mnt("prod"), "app.ini"), "[prod]\n", 0o644)
	env := []string{"FAKE_APP_MOUNT=/app.ini"}

	out, code := s.run(env, "@dev", "--release")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(docker, "devopsy-rsync:1 rsync -a --delete --numeric-ids --inplace /src/app.ini /dst/\n") || readString(filepath.Join(s.mnt("dev"), "app.ini")) != "[prod]\n" {
		t.Fatalf("same server (%d):\n%s\n%s", code, out, docker)
	}

	env = append(env, "DEVOPSY_SERVER_PROD=devopsy@one", "SSH_AUTH_SOCK=/tmp/agent.sock")
	out, code = s.run(env, "@devopsy@two:dev", "--sync", "--yes")
	docker, _ = s.logs()
	for _, want := range []string{"[app.ini]\npath = /mnt\ninclude = /app.ini\nexclude = *\n", "--numeric-ids --inplace rsync://127.0.0.1:"} {
		if code != 0 || !strings.Contains(docker, want) {
			t.Fatalf("across servers (%d), missing %q:\n%s\n%s", code, want, out, docker)
		}
	}
	if got := readString(filepath.Join(s.mnt("dev"), "app.ini")); got != "[prod]\n" {
		t.Errorf("file not copied across servers: %q", got)
	}
}

// Across servers: the agent forwarded to the parent's, an rsync daemon
// there, the child's server reaching it through a reverse forward, and the
// daemon removed.
func TestSyncDifferentHosts(t *testing.T) {
	s := newSyncSetup(t)
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
	// Every session of the sync keeps itself alive: the holder, the push,
	// the push's look at the child's services (--sync-exec) and the
	// receive.
	keep := "-o ServerAliveInterval=30 -o ServerAliveCountMax=4"
	if strings.Count(ssh, keep) != 4 || !strings.Contains(ssh, keep+" -T devopsy@two\n") || !strings.Contains(ssh, childSSH+" "+keep+" -T -p 22 devopsy@two\n") {
		t.Errorf("keepalive on holder, push, exec and receive:\n%s", ssh)
	}
	for _, want := range []string{"run --rm -d -v " + s.mnt("prod") + ":/mnt:ro", "RSYNCD_CONF=uid = 0\ngid = 0\nuse chroot = yes\nread only = yes\n[db]\npath = /mnt/db\n[storage]\npath = /mnt/storage\n", "inspect -f", "run --rm --network host -v " + s.mnt("dev") + ":/mnt devopsy-rsync:1 rsync -a --delete --numeric-ids rsync://127.0.0.1:", "/db/ /mnt/db/", "/storage/ /mnt/storage/", " rm -f cid123"} {
		if !strings.Contains(docker, want) {
			t.Errorf("missing %q in:\n%s", want, docker)
		}
	}
	// Vanished files across servers: the receive script goes on too.
	s.logs()
	if out, code := s.run(append(agent, "FAIL_RSYNC=24"), "@devopsy@two:dev", "--sync", "--yes"); code != 0 || strings.Count(out, "some files vanished during the copy") != 2 || !strings.Contains(out, "deploy in shop-dev") {
		t.Errorf("vanished files across servers (%d):\n%s", code, out)
	}
	s.logs()

	// --sync-exec across servers keeps its session alive too.
	execEnv := append(agent, "DEVOPSY_SYNC_TO=devopsy@two", "DEVOPSY_SYNC_SSH_PORT=22", "DEVOPSY_SYNC_TO_DIR="+filepath.Join(s.root, "shop", "dev", "current", ".devopsy"), "DEVOPSY_SYNC_TO_PROJECT=shop-dev")
	if out, code := s.run(execEnv, "--sync-exec", "show"); code != 0 || !strings.Contains(out, "show project=shop-dev") {
		t.Fatalf("--sync-exec (%d):\n%s", code, out)
	}
	if _, ssh := s.logs(); !strings.Contains(ssh, "-o ClearAllForwardings=yes "+childSSH+" "+keep+" -T -p 22 devopsy@two") {
		t.Errorf("--sync-exec session: %s", ssh)
	}

	// A restricted key refuses the agent: the parent's side says so, and
	// the child's services come back.
	out, code = s.run(append(agent, "FAKE_RESTRICT=1"), "@devopsy@two:dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code == 0 || !strings.Contains(out, "no SSH agent was forwarded to") || !strings.Contains(out, "restrict,agent-forwarding") || !strings.Contains(docker, "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" unpause app") {
		t.Fatalf("restricted key (%d):\n%s\n%s", code, out, docker)
	}

	// A key restricted on the child's server refuses the reverse forward,
	// which sshd reports as a taken port: three tries, then both causes.
	out, code = s.run(append(agent, "FAKE_NO_FORWARD=1"), "@devopsy@two:dev", "--sync", "--yes")
	docker, ssh = s.logs()
	if code == 0 || !strings.Contains(out, "refused the reverse forward 3 times") || !strings.Contains(out, "port-forwarding") || strings.Count(ssh, " -R ") != 3 || !strings.Contains(docker, " rm -f cid123") {
		t.Fatalf("refused forward (%d):\n%s\n%s\n%s", code, out, ssh, docker)
	}
}

// A push that fails aborts: the child's services are restored, its run
// step skipped.
func TestSyncAbort(t *testing.T) {
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()
	out, code := s.run([]string{"FAIL_RSYNC=23"}, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code != 1 || !strings.Contains(out, "aborting the sync") || strings.Contains(out, "deploy in shop-dev") {
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
		t.Error("an aborted sync changed current")
	}

	// The parent's stop fails: what it paused or stopped comes back.
	out, code = s.run([]string{"FAIL_STOP=shop-prod"}, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 1 || !strings.Contains(out, "aborting the sync") {
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

// The sync capability replaces the generic path: prepare and finish on
// the child, push on the parent with the helpers; the child's commands run
// as the child, without the parent's environment.
func TestSyncCapability(t *testing.T) {
	s := newSyncSetup(t)
	caps := filepath.Join(s.dot, "capabilities", "sync")
	write(t, filepath.Join(caps, "prepare"), "#!/bin/sh\necho \"prepare in $COMPOSE_PROJECT_NAME from $DEVOPSY_SYNC_FROM_TARGET\"\n", 0o755)
	write(t, filepath.Join(caps, "finish"), "#!/bin/sh\necho \"finish $1 in $COMPOSE_PROJECT_NAME\"\n", 0o755)
	write(t, filepath.Join(caps, "push"), "#!/bin/sh\nset -e\necho \"push from $COMPOSE_PROJECT_NAME to $DEVOPSY_SYNC_TO_PROJECT custom=$DEVOPSY_SYNC_CUSTOM args=$DEVOPSY_SYNC_ARGS\"\ndevopsy --sync-rsync storage -- --checksum\necho \"dump of $COMPOSE_PROJECT_NAME pw=$DB_PASSWORD\" | devopsy --sync-exec show\n", 0o755)
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
	// --sync's arguments are the push's: --sync-rsync only takes its own.
	out, code = s.run(custom, "@dev", "--sync", "--yes", "--", "--exclude=css")
	docker, _ = s.logs()
	if code != 0 || !strings.Contains(out, "args=--exclude=css") || strings.Contains(docker, "--checksum --exclude=css") || !strings.Contains(docker, "--numeric-ids --checksum /src/storage/") {
		t.Errorf("--sync -- args (%d):\n%s\n%s", code, out, docker)
	}
	if got := readString(filepath.Join(s.mnt("dev"), "storage", "file.txt")); got != "from prod\n" {
		t.Errorf("not copied: %q", got)
	}

	// A push without a custom service is not run; a custom service
	// without a push fails the sync.
	out, code = s.run(nil, "@dev", "--sync", "--yes")
	if code != 0 || !strings.Contains(out, "capabilities/sync/push present but no service labeled devopsy.sync=custom: not run") || strings.Contains(out, "push from") {
		t.Errorf("push without custom (%d):\n%s", code, out)
	}
	s.logs()
	if err := os.Remove(filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "capabilities", "sync", "push")); err != nil {
		t.Fatal(err)
	}
	out, code = s.run(custom, "@dev", "--sync", "--yes")
	if code == 0 || !strings.Contains(out, "db labeled devopsy.sync=custom: the parent's release needs an executable .devopsy/capabilities/sync/push") || !strings.Contains(out, "aborting the sync") {
		t.Errorf("custom without push (%d):\n%s", code, out)
	}
	s.logs()

	// The helpers only run inside a push, on entries of mnt.
	prod := filepath.Join(s.root, "shop", "prod", "current")
	inPush := []string{"DEVOPSY_SYNC_TO_DIR=" + filepath.Join(s.root, "shop", "dev", "current", ".devopsy"), "DEVOPSY_SYNC_TO_PROJECT=shop-dev"}
	for _, args := range [][]string{{"--sync-rsync", "../x"}, {"--sync-rsync", "a/b"}, {"--sync-rsync", "missing"}} {
		cmd := exec.Command(binary, args...)
		cmd.Dir = prod
		cmd.Env = append(append([]string{}, s.env...), inPush...)
		if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "--sync-rsync") {
			t.Errorf("%v: %v\n%s", args, err, out)
		}
	}
	for _, flag := range []string{"--sync-exec", "--sync-rsync", "--sync-push"} {
		cmd := exec.Command(binary, flag, "ps")
		cmd.Dir = prod
		cmd.Env = s.env
		if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "only runs in a sync's push") {
			t.Errorf("%s outside a push: %v\n%s", flag, err, out)
		}
	}
}

// What --sync refuses before anything starts.
func TestSyncGuards(t *testing.T) {
	s := newSyncSetup(t)
	// A parent that cannot be resolved only fails what syncs.
	bad := []string{"DEVOPSY_SERVER_PROD=bad:host"}
	for _, args := range [][]string{{"@dev", "--sync", "--yes"}, {"@dev", "--release"}} {
		if out, code := s.run(bad, args...); code == 0 || !strings.Contains(out, `environment dev: parent prod: server "bad:host"`) || s.current("dev") != "" {
			t.Errorf("%v, unresolved parent (%d):\n%s", args, code, out)
		}
	}
	if out, code := s.run(bad, "@dev", "--release-no-sync"); code != 0 || s.current("dev") == "" {
		t.Errorf("--release-no-sync, unresolved parent (%d):\n%s", code, out)
	}
	if out, code := s.run(bad, "@dev", "show"); code != 0 || !strings.Contains(out, "show project=shop-dev") {
		t.Errorf("a command, unresolved parent (%d):\n%s", code, out)
	}
	if err := os.RemoveAll(filepath.Join(s.root, "shop", "dev")); err != nil {
		t.Fatal(err)
	}
	s.logs()
	if out, code := s.run(nil, "@solo", "--sync", "--yes"); code == 0 || !strings.Contains(out, "@solo is not a child") {
		t.Errorf("not a child (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@solo", "--release-sync"); code == 0 || !strings.Contains(out, "@solo is not a child") {
		t.Errorf("--release-sync, not a child (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--sync", "--yes"); code == 0 || !strings.Contains(out, "the parent @devopsy@one:prod (devopsy@one:shop/prod) has no current release") {
		t.Errorf("parent not released (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--release"); code == 0 || !strings.Contains(out, "has no current release") || !strings.Contains(out, "--release-no-sync") {
		t.Errorf("first release, parent not released (%d):\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(s.root, "shop", "dev")); !os.IsNotExist(err) {
		t.Errorf("the child was touched: %v", err)
	}
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--sync", "--yes"); code == 0 || !strings.Contains(out, "has no current release: run 'devopsy @dev --release' first") {
		t.Errorf("child not released (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--sync", "--from", "dev", "--yes"); code == 0 || !strings.Contains(out, "and its parent are the same environment") {
		t.Errorf("--from itself (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@bare", "--sync", "--yes"); code == 0 || !strings.Contains(out, "environment bare has no release steps") {
		t.Errorf("no run step (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--sync", "--bogus"); code == 0 || !strings.Contains(out, "usage: devopsy @dev --sync") {
		t.Errorf("usage (%d):\n%s", code, out)
	}
	// --from another environment of the project works as the source.
	if out, code := s.run(nil, "@solo", "--release"); code != 0 {
		t.Fatalf("solo (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	write(t, filepath.Join(s.mnt("solo"), "storage", "file.txt"), "from solo\n", 0o644)
	if out, code := s.run(nil, "@dev", "--sync", "--from", "solo", "--yes"); code != 0 || readString(filepath.Join(s.mnt("dev"), "storage", "file.txt")) != "from solo\n" {
		t.Errorf("--from solo (%d):\n%s", code, out)
	}
}

// inherit: on the parent names variables its children get from its
// server's files, written at the same level on the child's side, before the
// upload of a first release and before a --sync's copy; never with
// --release-no-sync.
func TestSyncInherit(t *testing.T) {
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "config.yaml"), "project: shop\ninstances: true\ndefaults:\n  mode: image\n  release: deploy\nenvironments:\n  prod: {inherit: [SHARED_KEY, INST_KEY, PROJ_KEY, MISSING_KEY]}\n  dev: {parent: prod}\n", 0o644)
	if out, code := s.run(nil, "@a/prod", "--release", "--yes"); code != 0 {
		t.Fatalf("prod (%d):\n%s", code, out)
	}
	shop := filepath.Join(s.root, "shop")
	write(t, filepath.Join(shop, "a", "prod", "shared", ".env"), "SHARED_KEY=s1\n", 0o600)
	write(t, filepath.Join(shop, "a", ".env"), "INST_KEY='i 1'\n", 0o600)
	write(t, filepath.Join(shop, ".env"), "PROJ_KEY=p1\nSHARED_KEY=lower\n", 0o600)
	childEnv := filepath.Join(shop, "a", "dev", "shared", ".env")
	childInherit := filepath.Join(shop, "a", "dev", "shared", "inherit.env")
	s.logs()

	// A first release, nothing of the child on the server yet, inherits
	// before the upload: every key from the parent's nearest file into the
	// child's shared/inherit.env, whole.
	out, code := s.run(nil, "@a/dev", "--release", "--yes")
	if code != 0 {
		t.Fatalf("first release (%d):\n%s", code, out)
	}
	for _, want := range []string{
		"Inherited SHARED_KEY, INST_KEY, PROJ_KEY from @devopsy@one:a/prod into @devopsy@one:a/dev's shared/inherit.env.",
		"@devopsy@one:a/prod does not set MISSING_KEY",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if i, j := strings.Index(out, "Inherited SHARED_KEY"), strings.Index(out, "Releasing "); i < 0 || j < i {
		t.Errorf("inherited after the upload:\n%s", out)
	}
	if got := readString(childInherit); got != "SHARED_KEY='s1'\nINST_KEY='i 1'\nPROJ_KEY='p1'\n" {
		t.Errorf("child's inherit.env: %q", got)
	}
	// The child's own shared/.env is untouched and wins; a key set there
	// too is named.
	write(t, childEnv, "OWN=1\nPROJ_KEY=child\n", 0o600)
	if out, code := s.run(nil, "@a/dev", "--inherit"); code != 0 || !strings.Contains(out, "@devopsy@one:a/dev's own shared .env also sets PROJ_KEY, which wins there (devopsy @dev --vars unset to inherit it).") {
		t.Errorf("shadowed key (%d):\n%s", code, out)
	}
	if got := readString(childEnv); got != "OWN=1\nPROJ_KEY=child\n" {
		t.Errorf("child's shared/.env changed: %q", got)
	}
	if strings.Contains(out, "s1") || strings.Contains(readString(filepath.Join(s.root, "shop", "a", "dev", "current", ".devopsy-log")), "s1") {
		t.Errorf("a value was printed:\n%s", out)
	}
	// The release links it, and devopsy loads it below the child's own
	// .env and above the instance's and project's.
	if _, err := os.Lstat(filepath.Join(shop, "a", "dev", "current", ".devopsy", "inherit.env")); err != nil {
		t.Errorf("inherit.env not linked into the release: %v", err)
	}
	out, code = s.run(nil, "@a/dev", "--env")
	if code != 0 || !strings.Contains(out, "SHARED_KEY='s1'\n") || !strings.Contains(out, "PROJ_KEY='child'\n") || !strings.Contains(out, "INST_KEY='i 1'\n") {
		t.Errorf("effective variables (%d):\n%s", code, out)
	}
	next := func() { time.Sleep(1100 * time.Millisecond) }

	// --release-no-sync inherits nothing.
	write(t, filepath.Join(shop, "a", "prod", "shared", ".env"), "SHARED_KEY=s2\n", 0o600)
	next()
	if out, code := s.run(nil, "@a/dev", "--release-no-sync"); code != 0 || strings.Contains(out, "Inherited") || !strings.Contains(readString(childInherit), "SHARED_KEY='s1'") {
		t.Fatalf("--release-no-sync (%d): %q\n%s", code, readString(childInherit), out)
	}

	// --inherit resyncs the variables alone: no copy, no run step.
	s.logs()
	out, code = s.run(nil, "@a/dev", "--inherit")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(out, "Inherited SHARED_KEY, INST_KEY, PROJ_KEY from") || strings.Contains(out, "deploy in") || strings.Contains(docker, "pause") || !strings.Contains(readString(childInherit), "SHARED_KEY='s2'") {
		t.Fatalf("--inherit (%d): %q\n%s\n%s", code, readString(childInherit), out, docker)
	}
	if out, code := s.run(nil, "@a/prod", "--inherit"); code == 0 || !strings.Contains(out, "is not a child") {
		t.Errorf("--inherit on prod (%d):\n%s", code, out)
	}

	// Across servers a re-sync inherits once the copy is in, before the
	// child's run step: a failed copy leaves the child's file as it was.
	write(t, filepath.Join(shop, "a", "prod", "shared", ".env"), "SHARED_KEY=s3\n", 0o600)
	env := []string{"DEVOPSY_SERVER_PROD=devopsy@one", "SSH_AUTH_SOCK=/tmp/agent.sock"}
	s.logs()
	out, code = s.run(env, "@devopsy@two:a/dev", "--sync", "--yes")
	if code != 0 {
		t.Fatalf("--sync (%d):\n%s", code, out)
	}
	if i, j, k := strings.Index(out, "Copying "), strings.Index(out, "Inherited SHARED_KEY"), strings.Index(out, "deploy in shop-a-dev"); i < 0 || j < i || k < j {
		t.Errorf("inherited before the copy or after the run step:\n%s", out)
	}
	// The holder holds the child's lock while the write happens: the
	// write takes none (it waited for the holder, which waited for it).
	if docker, _ := s.logs(); strings.Count(docker, "flock -n 9\n") != 1 {
		t.Errorf("the child's lock taken more than once during a re-sync:\n%s", docker)
	}
	if got := readString(childInherit); got != "SHARED_KEY='s3'\nINST_KEY='i 1'\nPROJ_KEY='p1'\n" {
		t.Errorf("child's inherit.env: %q", got)
	}
	write(t, filepath.Join(shop, "a", "prod", "shared", "mnt", "storage", "f"), "x\n", 0o644)
	write(t, filepath.Join(shop, "a", "prod", "shared", ".env"), "SHARED_KEY=s4\n", 0o600)
	if out, code := s.run(append(env, "FAIL_RSYNC=23"), "@devopsy@two:a/dev", "--sync", "--yes"); code == 0 || strings.Contains(out, "Inherited") || !strings.Contains(readString(childInherit), "SHARED_KEY='s3'") {
		t.Errorf("failed re-sync (%d): %q\n%s", code, readString(childInherit), out)
	}
}

// Interrupted during the copy, devopsy ends the push session first, then
// aborts: the child's services come back and its run step does not run.
func TestSyncInterrupt(t *testing.T) {
	s := newSyncSetup(t)
	started := filepath.Join(t.TempDir(), "started")
	write(t, filepath.Join(s.dot, "capabilities", "sync", "push"), "#!/bin/sh\ntouch \"$FAKE_STARTED\"\nexec sleep 20\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()

	// Output to a file: the push's sleep, orphaned, keeps a pipe open.
	outFile, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "@dev", "--sync", "--yes")
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
func TestSyncLabels(t *testing.T) {
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "capabilities", "sync", "flush"), "#!/bin/sh\necho \"flush $1 in $COMPOSE_PROJECT_NAME\"\n[ \"${FAIL_FLUSH:-}\" != \"$COMPOSE_PROJECT_NAME\" ]\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()

	out, code := s.run([]string{"FAKE_APP=none", "FAKE_DB=none"}, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(out, "nothing to sync: no service labeled devopsy.sync") || !strings.Contains(out, "deploy in shop-dev") || strings.Contains(out, "flush") ||
		strings.Contains(docker, "pause") || strings.Contains(docker, "rsync") {
		t.Fatalf("no labels (%d):\n%s\n%s", code, out, docker)
	}

	out, code = s.run(nil, "@dev", "--sync", "--yes")
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
	out, code = s.run([]string{"FAKE_DB=live"}, "@dev", "--sync", "--yes")
	s.logs()
	if code != 0 || strings.Count(out, "flush db in") != 2 || strings.Count(out, "flush app in") != 2 {
		t.Errorf("flush for live services (%d):\n%s", code, out)
	}

	// A db down before the sync (a maintenance stop) stays down: finish
	// restores what prepare recorded, and the record goes afterwards.
	out, code = s.run([]string{"FAKE_DB_DOWN=1"}, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 || strings.Contains(out, "stopping db") || strings.Contains(out, "starting db") || strings.Contains(docker, "start db") || strings.Count(out, "unpausing app") != 2 {
		t.Errorf("db down before the sync (%d):\n%s\n%s", code, out, docker)
	}
	// A db in a restart loop (an aborted copy left its datadir half
	// written) is stopped like a running one: the parent's guard counts
	// it as running.
	out, code = s.run([]string{"FAKE_DB_DOWN=1", "FAKE_DB_RESTARTING=1"}, "@dev", "--sync", "--yes")
	s.logs()
	if code != 0 || strings.Count(out, "stopping db") != 2 || strings.Count(out, "starting db") != 2 {
		t.Errorf("db restarting before the sync (%d):\n%s", code, out)
	}
	for _, env := range []string{"prod", "dev"} {
		if _, err := os.Stat(filepath.Join(s.root, "shop", env, ".devopsy-sync.services")); err == nil {
			t.Errorf("%s: the record of services to restore stays", env)
		}
	}
	// finish alone, in the parent's current release: with a record it
	// restores what the record says; without one (an older prepare, a
	// lost file) every labeled service, as before.
	record := filepath.Join(s.root, "shop", "prod", ".devopsy-sync.services")
	finish := func() (string, string) {
		t.Helper()
		cmd := exec.Command(binary, "--sync-services", "finish")
		cmd.Dir = filepath.Join(s.root, "shop", "prod", "current")
		// PWD as a shell's cd leaves it: the record lives next to current.
		cmd.Env = append(append([]string{}, s.env...), "COMPOSE_PROJECT_NAME=shop-prod", "PWD="+cmd.Dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("finish: %v\n%s", err, out)
		}
		docker, _ := s.logs()
		return string(out), docker
	}
	write(t, record, "pause app\n", 0o644)
	if out, docker := finish(); strings.Contains(docker, "start db") || !strings.Contains(docker, "unpause app") {
		t.Errorf("finish with a record:\n%s\n%s", out, docker)
	}
	if _, err := os.Stat(record); err == nil {
		t.Errorf("the record stays after finish")
	}
	if out, docker := finish(); !strings.Contains(docker, "start db") || !strings.Contains(docker, "unpause app") {
		t.Errorf("finish without a record:\n%s\n%s", out, docker)
	}

	// An app mounting mnt whole copies the skipped db's entry too: a notice.
	out, code = s.run([]string{"FAKE_APP_MOUNT=", "FAKE_DB=none"}, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 || !strings.Contains(out, "devopsy: app copies mnt whole, db (skip: db) included: devopsy.sync.exclude=db on app leaves them out") || !strings.Contains(docker, "rsync -a --delete --numeric-ids /src/./ /dst/./") {
		t.Errorf("whole mnt notice (%d):\n%s\n%s", code, out, docker)
	}
	out, code = s.run([]string{"FAKE_APP_MOUNT="}, "@dev", "--sync", "--yes")
	s.logs()
	if code != 0 || strings.Contains(out, "copies mnt whole") {
		t.Errorf("notice for a stop db under mnt whole (%d):\n%s", code, out)
	}

	// A failing flush aborts like a failing prepare: on the child before
	// the copy, on the parent through the push.
	for _, side := range []string{"shop-dev", "shop-prod"} {
		out, code = s.run([]string{"FAIL_FLUSH=" + side}, "@dev", "--sync", "--yes")
		docker, _ = s.logs()
		if code == 0 || !strings.Contains(out, "flush app in "+side) || strings.Contains(out, "deploy in shop-dev") || !strings.Contains(docker, "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" unpause app") {
			t.Errorf("failing flush on %s (%d):\n%s\n%s", side, code, out, docker)
		}
	}
}

// A service the parent copies that the child leaves running (custom or no
// label there) fails the push before the parent pauses anything: the
// child restores its services; live on the child says running is fine.
func TestSyncMismatch(t *testing.T) {
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()
	for _, label := range []string{"custom", "none"} {
		write(t, s.log+".shop-dev", "FAKE_DB="+label+"\n", 0o644)
		out, code := s.run(nil, "@dev", "--sync", "--yes")
		docker, _ := s.logs()
		want := "labeled devopsy.sync=custom"
		if label == "none" {
			want = "without a devopsy.sync label"
		}
		if code == 0 || !strings.Contains(out, "the child's db is running, "+want+", and mnt/db would change under it: the parent copies it for db (devopsy.sync=stop)") ||
			strings.Contains(out, "deploy in shop-dev") || strings.Count(docker, "shop-prod compose") != 1 || !strings.Contains(docker, "shop-prod compose -f "+filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml")+" --profile * config") ||
			strings.Contains(docker, "rsync") || !strings.Contains(docker, "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" unpause app") {
			t.Errorf("child db %s (%d):\n%s\n%s", label, code, out, docker)
		}
	}
	write(t, s.log+".shop-dev", "FAKE_DB=live\n", 0o644)
	out, code := s.run(nil, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(out, "deploy in shop-dev") || !strings.Contains(docker, "/src/db/ /dst/db/") {
		t.Errorf("child db live (%d):\n%s\n%s", code, out, docker)
	}
	// The other way round, the child stops what the parent leaves to its
	// push: nothing to refuse.
	os.Remove(s.log + ".shop-dev")
	write(t, filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "capabilities", "sync", "push"), "#!/bin/sh\necho push\n", 0o755)
	out, code = s.run([]string{"FAKE_DB=custom"}, "@dev", "--sync", "--yes")
	s.logs()
	if code != 0 || !strings.Contains(out, "deploy in shop-dev") {
		t.Errorf("parent custom (%d):\n%s", code, out)
	}
	os.Remove(s.log + ".shop-dev")
}

// Interrupted during the copy, the parent's push stops at once, though
// its session outlives devopsy's ssh: the closed stdin tells it. It
// removes the rsync container, restores its services, and never runs the
// capability's push for the custom services.
func TestSyncInterruptCopy(t *testing.T) {
	s := newSyncSetup(t)
	tmp := t.TempDir()
	started, pushed := filepath.Join(tmp, "started"), filepath.Join(tmp, "pushed")
	write(t, filepath.Join(s.dot, "capabilities", "sync", "push"), "#!/bin/sh\ntouch \"$FAKE_PUSHED\"\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()

	outFile, err := os.CreateTemp(tmp, "out")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "@dev", "--sync", "--yes")
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
			t.Fatalf("the copy never started:\n%s\n%s", readString(outFile.Name()), readString(s.log))
		}
		time.Sleep(50 * time.Millisecond)
	}
	start := time.Now()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	if cmd.ProcessState.ExitCode() == 0 {
		t.Fatalf("interrupted sync succeeded:\n%s", readString(outFile.Name()))
	}
	restored := "shop-prod compose -f " + filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml") + " unpause app"
	for !strings.Contains(readString(s.log), restored) {
		if time.Since(start) > 10*time.Second {
			t.Fatalf("the parent did not restore in time:\n%s\n%s", readString(s.log), readString(outFile.Name()))
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)
	if !strings.Contains(readString(s.log), "rm -f devopsy-sync-") {
		t.Errorf("the rsync container was not removed:\n%s", readString(s.log))
	}
	if _, err := os.Stat(pushed); err == nil {
		t.Errorf("the capability's push ran after the interrupt")
	}
	// Said where the parent's words go once its session is cut: its log.
	if log := readString(filepath.Join(s.root, "shop", "prod", ".devopsy-sync.log")); !strings.Contains(log, "devopsy: mnt/storage is half copied: a database copied raw cannot start on it until the next sync completes it") {
		t.Errorf("no word on the entry cut:\n%s", log)
	}
	// What the parent did once nobody read its session is in its log.
	if log := readString(filepath.Join(s.root, "shop", "prod", ".devopsy-sync.log")); !strings.Contains(log, ": the sync's session is gone: restoring what was paused or stopped") || !strings.Contains(log, "devopsy: unpausing app") {
		t.Errorf("the parent's sync log:\n%s", log)
	}
}

// The holder's session ends during the copy: the push is stopped, the
// parent restored, nothing answered, no run step.
func TestSyncHolderGone(t *testing.T) {
	s := newSyncSetup(t)
	started := filepath.Join(t.TempDir(), "started")
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()
	out, code := s.run([]string{"FAKE_SLOW_RSYNC=" + started, "FAKE_HOLDER_DIE=" + started}, "@dev", "--sync", "--yes")
	docker, ssh := s.logs()
	if code == 0 || !strings.Contains(out, "ended during the copy (255): stopping the copy from @devopsy@one:prod") || strings.Contains(out, "deploy in shop-dev") {
		t.Fatalf("holder gone (%d):\n%s\n%s\n%s", code, out, ssh, docker)
	}
	// The push outlives devopsy's session: its restore may still be
	// under way when devopsy has exited.
	restored := "shop-prod compose -f " + filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml") + " unpause app"
	for start := time.Now(); !strings.Contains(docker, restored); docker += readString(s.log) {
		if time.Since(start) > 10*time.Second {
			t.Fatalf("the parent was not restored:\n%s", docker)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(docker, "rm -f devopsy-sync-") {
		t.Errorf("the copy was not stopped:\n%s", docker)
	}
}

// A current that is a directory (Docker recreated a restarting
// container's bind mounts under it after a failed first release removed
// the link) fails the release before the upload, with the way out.
func TestReleaseCurrentDirectory(t *testing.T) {
	s := newSyncSetup(t)
	s.releaseProd(nil)
	current := filepath.Join(s.root, "shop", "prod", "current")
	os.Remove(current)
	if err := os.MkdirAll(filepath.Join(current, ".devopsy", "mnt"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := s.run(nil, "@prod", "--release")
	if code == 0 || !strings.Contains(out, "current is a directory, not a link to a release") || !strings.Contains(out, "rm -rf /env/current") || strings.Contains(out, "current is now") {
		t.Errorf("release over a current directory (%d):\n%s", code, out)
	}
}

// --status: the containers and the status capability's lines, locally;
// with a target, the release's record and the last sync first.
func TestStatus(t *testing.T) {
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "capabilities", "status", "show"), "#!/bin/sh\necho \"url https://$COMPOSE_PROJECT_NAME.example\"\n", 0o755)
	out, code := s.run(nil, "--status")
	if code != 0 || !strings.Contains(out, "containers\n") || !strings.Contains(out, "  app ") || !strings.Contains(out, "  db ") || !strings.Contains(out, "exited") || !strings.Contains(out, "url https://shop.example\n") {
		t.Fatalf("local status (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@prod", "--status"); code != 0 || !strings.Contains(out, "@prod (devopsy@one:shop/prod)") || !strings.Contains(out, "release     none: not released yet") {
		t.Fatalf("unreleased status (%d):\n%s", code, out)
	}
	s.releaseProd(nil)
	out, code = s.run(nil, "@prod", "--status")
	if code != 0 || !strings.Contains(out, "release     "+s.current("prod")[len("releases/"):]) || !strings.Contains(out, "just now") || !strings.Contains(out, "(1 kept)") || !strings.Contains(out, "url https://shop-prod.example\n") || strings.Contains(out, "parent") {
		t.Fatalf("prod status (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--status"); code != 0 || !strings.Contains(out, "parent      prod  never synced") {
		t.Fatalf("dev status before a sync (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--sync", "--yes"); code != 0 {
		t.Fatalf("sync (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--status"); code != 0 || !strings.Contains(out, "parent      prod  last synced just now") {
		t.Fatalf("dev status after a sync (%d):\n%s", code, out)
	}
}

// A release never takes over another project's network: the networks a
// project names are its own, or external.
func TestReleaseNetworkOwned(t *testing.T) {
	s := newSyncSetup(t)
	if out, code := s.run([]string{"FAKE_NETWORK_OWNER=other-prod"}, "@prod", "--release"); code == 0 || !strings.Contains(out, "network shop-prod_default is other-prod's") || !strings.Contains(out, "join its network as external") || s.current("prod") != "" {
		t.Errorf("another project's network (%d):\n%s", code, out)
	}
	if out, code := s.run([]string{"FAKE_NETWORK_OWNER=shop-prod"}, "@prod", "--release"); code != 0 || s.current("prod") == "" {
		t.Errorf("own network (%d):\n%s", code, out)
	}
}
