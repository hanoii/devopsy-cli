package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// syncFakeDocker logs every call, with its compose project, to
// $DOCKER_LOG, answers compose config and ps for a project whose app is
// labeled rsync (devopsy.sync=rsync) and db rsync and stopped
// (devopsy.sync.stop=true), app depending on db (FAKE_APP and FAKE_DB
// change them: rsync, stream, each with +stop, or none for no label;
// $DOCKER_LOG.<compose project> sets them for one side, past the clean
// environment), each container running until a stop (remembered, until
// a start or an up), exited with FAKE_DB_DOWN, restarting with
// FAKE_DB_RESTARTING, and copies what the rsync containers would: within one host,
// or from the daemon's mount (remembered) across hosts. The tar
// containers of --export and --import run tar here. FAKE_ORPHANS names
// services the project no longer has, whose containers are left exited:
// ps lists them, and start refuses them, as Compose does. FAKE_ONEOFF
// names services with a one-off container running (a hook's run --rm),
// which ps lists under the service, labeled, as Compose does.
const syncFakeDocker = `#!/bin/sh
echo "${COMPOSE_PROJECT_NAME:-} $*" >> "$DOCKER_LOG"
[ ! -f "$DOCKER_LOG.${COMPOSE_PROJECT_NAME:-}" ] || . "$DOCKER_LOG.${COMPOSE_PROJECT_NAME:-}"
if [ "$1" = compose ] && [ "$2" = -f ]; then dir=$(dirname "$3"); fi
for a; do last2=${last:-}; last=$a; done
# FAKE_ABSENT names services without a container (a first release),
# until an up creates them.
absent() {
  ! grep -qx "$1" "$DOCKER_LOG.up.${COMPOSE_PROJECT_NAME:-}" 2>/dev/null || return 1
  case " ${FAKE_ABSENT:-} " in *" $1 "*) return 0 ;; esac; return 1
}
lab() {
  case $1 in none) echo '{}' ;; *+stop) echo "{\"devopsy.sync\":\"${1%+stop}\",\"devopsy.sync.stop\":\"true\"}" ;; *) echo "{\"devopsy.sync\":\"$1\"}" ;; esac
}
stopped="$DOCKER_LOG.stopped.${COMPOSE_PROJECT_NAME:-}"
# st: a service's container state, none without one.
st() {
  if absent "$1"; then echo none
  elif grep -qx "$1" "$stopped" 2>/dev/null; then echo exited
  elif [ "$1" = db ] && [ -n "${FAKE_DB_RESTARTING:-}" ]; then echo restarting
  elif [ "$1" = db ] && [ -n "${FAKE_DB_DOWN:-}" ]; then echo exited
  else echo running
  fi
}
# services after a compose command (stop, start, up -d --wait).
after() { at=$1; shift; w=0; for a; do [ "$w" = 1 ] && echo "$a"; [ "$a" = "$at" ] && w=1; done; }
case "$*" in
  *" config --format json")
    printf '{"name":"%s","services":{"app":{"labels":%s,"depends_on":{"db":{"condition":"service_started"}},"volumes":[{"type":"bind","source":"%s/mnt%s"}]},"db":{"labels":%s,"volumes":[{"type":"bind","source":"%s/mnt/db"}]}},"networks":{"default":{"name":"%s_default"},"traefik":{"name":"traefik","external":true}}}\n' "${COMPOSE_PROJECT_NAME:-}" "$(lab "${FAKE_APP:-rsync}")" "$dir" "${FAKE_APP_MOUNT-/storage}" "$(lab "${FAKE_DB:-rsync+stop}")" "$dir" "${COMPOSE_PROJECT_NAME:-}" ;;
  # FAKE_NETWORK_OWNER: every network exists and is that project's.
  "network inspect "*) [ -n "${FAKE_NETWORK_OWNER:-}" ] || exit 1; echo "$FAKE_NETWORK_OWNER" ;;
  *" stop "*)
    after stop "$@" >> "$stopped"
    # FAIL_STOP: a stop that fails partway, its services stopped.
    [ "${FAIL_STOP:-}" != "${COMPOSE_PROJECT_NAME:-}" ] || exit 1 ;;
  *" start "*)
    # FAIL_START: a start that fails, its services left stopped.
    [ "${FAIL_START:-}" != "${COMPOSE_PROJECT_NAME:-}" ] || exit 1
    for s in $(after start "$@"); do case $s in app|db) ;; *) echo "no such service: $s" >&2; exit 1 ;; esac; done
    for s in $(after start "$@"); do grep -vx "$s" "$stopped" > "$stopped.new" 2>/dev/null; mv "$stopped.new" "$stopped"; done ;;
  *" ps --services --status running") for s in app db; do [ "$(st $s)" != running ] || echo $s; done ;;
  *" ps --all --format json "*)
    for s in $(after json "$@"); do
      printf '{"Service":"%s","State":"running","Health":"healthy"}\n' "$s"
      case " ${FAKE_ONEOFF:-} " in *" $s "*) printf '{"Name":"%s-%s-run-1","Service":"%s","State":"running","Labels":"com.docker.compose.oneoff=True"}\n' "${COMPOSE_PROJECT_NAME:-}" "$s" "$s" ;; esac
    done ;;
  # FAKE_SYNC_CONTAINERS: containers a sync left (devopsy-sync-<id>-...),
  # listed by a name filter.
  "ps -aq --filter name="*)
    for c in ${FAKE_SYNC_CONTAINERS:-}; do case $c in "${4#name=}"*) echo "$c" ;; esac; done ;;
  *" ps --all --format json")
    for s in app db; do [ "$(st $s)" = none ] || printf '{"Service":"%s","State":"%s"}\n' "$s" "$(st $s)"; done
    for s in ${FAKE_ORPHANS:-}; do printf '{"Service":"%s","State":"exited"}\n' "$s"; done
    for s in ${FAKE_ONEOFF:-}; do printf '{"Name":"%s-%s-run-1","Service":"%s","State":"running","Labels":"com.docker.compose.project=%s,com.docker.compose.oneoff=True"}\n' "${COMPOSE_PROJECT_NAME:-}" "$s" "$s" "${COMPOSE_PROJECT_NAME:-}"; done ;;
  *" up -d --wait "*)
    [ "${FAIL_UP:-}" != "${COMPOSE_PROJECT_NAME:-}" ] || exit 1
    # FAKE_UP_DEPS: as Compose does, an up of app starts db, which it
    # depends on, too, unless --no-deps.
    ups=$(after --wait "$@")
    case "$* " in
      *" --no-deps "*) ups=$(after --no-deps "$@") ;;
      *) [ -z "${FAKE_UP_DEPS:-}" ] || case " $ups " in *" app "*) ups="db $ups" ;; esac ;;
    esac
    for s in $ups; do
      echo "$s" >> "$DOCKER_LOG.up.${COMPOSE_PROJECT_NAME:-}"
      grep -vx "$s" "$stopped" > "$stopped.new" 2>/dev/null; mv "$stopped.new" "$stopped"
    done ;;
  "run --rm -d "*)
    for a; do case $a in *:/mnt:ro) echo "${a%:/mnt:ro}" > "$DOCKER_LOG.daemon" ;; RSYNCD_CONF=*) printf '%s\n' "${a#RSYNCD_CONF=}" > "$DOCKER_LOG.rsyncd" ;; esac; done
    echo cid123 ;;
  "inspect "*) echo 172.17.0.9 ;;
  "run --rm --name devopsy-sync-"*"-receive --network host "*)
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
  "run --rm --name devopsy-export-"*)
    [ -z "${FAIL_TAR:-}" ] || exit "$FAIL_TAR"
    while [ $# -gt 0 ]; do case $1 in *:/mnt:ro) src=${1%:/mnt:ro} ;; --numeric-owner) shift; break ;; esac; shift; done
    COPYFILE_DISABLE=1 exec tar -C "$src" -cf - "$@" ;;
  "run --rm --name devopsy-import-"*)
    [ -z "${FAIL_TAR:-}" ] || exit "$FAIL_TAR"
    while [ $# -gt 0 ]; do case $1 in *:/mnt) dst=${1%:/mnt} ;; -c) shift 3; break ;; esac; shift; done
    for e; do if [ "$e" = . ]; then find "$dst" -mindepth 2 -maxdepth 2 -exec rm -rf {} +; find "$dst" -mindepth 1 -maxdepth 1 ! -type d -exec rm -f {} +; elif [ -d "$dst/$e" ] && [ ! -L "$dst/$e" ]; then find "$dst/$e" -mindepth 1 -maxdepth 1 -exec rm -rf {} +; fi; done
    exec tar -C "$dst" -xf - ;;
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
# FAKE_IMPORT_DETACH: the same for a stream's import session.
case "$s" in *sync-services*import*) if [ -n "${FAKE_IMPORT_DETACH:-}" ]; then sh -c "$s" <&3 & wait $!; exit $?; fi ;; esac
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
	released         map[string]int64
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
	write(t, filepath.Join(s.dot, "config.yaml"), "project: shop\ndefaults:\n  mode: image\n  release: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n  solo: {}\n  slow: {release: {prepare: [slow], run: deploy}}\n  bare: {parent: prod, release: {before: [local: deploy]}}\n", 0o644)
	write(t, filepath.Join(s.dot, ".env"), "DEVOPSY_SERVER=devopsy@one\n", 0o644)
	write(t, filepath.Join(s.dot, "commands", "deploy"), "#!/bin/sh\necho \"deploy in $COMPOSE_PROJECT_NAME parent=${DEVOPSY_PARENT:-} ${DEVOPSY_PARENT_TARGET:-}\"\nif [ -n \"${FAKE_SLOW_RUN:-}\" ]; then touch \"$FAKE_SLOW_RUN\"; sleep 3; echo \"deployed slowly\"; fi\n", 0o755)
	write(t, filepath.Join(s.dot, "commands", "slow"), "#!/bin/sh\n## Description: a slow prepare step\nif [ -n \"${FAKE_SLOW_PREPARE:-}\" ]; then touch \"$FAKE_SLOW_PREPARE\"; exec sleep 20; fi\n", 0o755)
	write(t, filepath.Join(s.dot, "commands", "show"), "#!/bin/sh\necho \"show project=$COMPOSE_PROJECT_NAME pw=${DB_PASSWORD:-}\"\ncat\n", 0o755)
	s.env = []string{"DEVOPSY_SSH_COMMAND=" + filepath.Join(bin, "fakessh"), "PATH=" + bin + ":/usr/bin:/bin", "HOME=" + tmp, "DEVOPSY_HOME=" + home,
		"SSH_LOG=" + s.sshLog, "DOCKER_LOG=" + s.log}
	return s
}

func (s *syncSetup) run(extra []string, args ...string) (string, int) {
	s.t.Helper()
	s.spaceReleases(args)
	cmd := exec.Command(binary, args...)
	cmd.Dir = s.project
	cmd.Env = append(append([]string{}, s.env...), extra...)
	cmd.Stdin = strings.NewReader("")
	out, _ := cmd.CombinedOutput()
	return string(out), cmd.ProcessState.ExitCode()
}

// spaceReleases waits for a new second before a second release of the
// same target: release ids are seconds, and a taken one is refused.
func (s *syncSetup) spaceReleases(args []string) {
	if len(args) < 2 || !strings.HasPrefix(args[1], "--release") {
		return
	}
	if s.released == nil {
		s.released = map[string]int64{}
	}
	for s.released[args[0]] == time.Now().Unix() {
		time.Sleep(20 * time.Millisecond)
	}
	s.released[args[0]] = time.Now().Unix()
}

// logs returns and empties the docker and ssh logs.
func (s *syncSetup) logs() (docker, ssh string) {
	d, _ := os.ReadFile(s.log)
	h, _ := os.ReadFile(s.sshLog)
	os.Remove(s.log)
	os.Remove(s.sshLog)
	return string(d), string(h)
}

// labels sets FAKE_APP and FAKE_DB (vars, lines of sh) for both sides,
// through the fake docker's files: the child's devopsy on the same server
// gets a clean environment. Empty removes them.
func (s *syncSetup) labels(vars string) {
	s.t.Helper()
	for _, p := range []string{"shop-dev", "shop-prod"} {
		if vars == "" {
			os.Remove(s.log + "." + p)
			continue
		}
		write(s.t, s.log+"."+p, vars+"\n", 0o644)
	}
}

// current is the release env's current points at: releases/<id>.
func (s *syncSetup) current(env string) string {
	l, _ := os.Readlink(filepath.Join(s.root, "shop", env, "current"))
	return strings.TrimSuffix(l, "/"+remote.CodeDir)
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

// A child's first release syncs its parent on the same server: the
// services labeled devopsy.sync.stop=true stopped on both sides, one
// container copying, then deploy on the parent's data. Later releases do not, unless --sync; --sync
// resyncs.
func TestSyncSameHost(t *testing.T) {
	s := newSyncSetup(t)
	s.releaseProd(nil)

	out, code := s.run(nil, "@dev", "--release")
	docker, ssh := s.logs()
	if code != 0 || !strings.Contains(out, "devopsy: syncing @devopsy@one:prod's data into it") || !strings.Contains(out, "deploy in shop-dev parent=prod devopsy@one:prod") || s.current("dev") == "" {
		t.Fatalf("first release (%d), current %q:\n%s", code, s.current("dev"), out)
	}
	if got := readString(filepath.Join(s.mnt("dev"), "storage", "file.txt")); got != "from prod\n" {
		t.Errorf("storage not copied: %q", got)
	}
	if got := readString(filepath.Join(s.mnt("dev"), "db", "ibdata")); got != "rows\n" {
		t.Errorf("db not copied: %q", got)
	}
	// Order on each side: stop, copy, start (healthy); the child's around
	// the parent's; one container copies, no daemon. app, labeled rsync
	// alone, keeps running. A first release then creates every labeled
	// service, whatever containers it found.
	order := []string{"shop-dev compose", "stop db", "shop-prod compose", "stop db",
		"run --rm --name devopsy-sync-", " -v " + s.mnt("prod") + ":/src:ro -v " + s.mnt("dev") + ":/dst devopsy-util:1 rsync -a --delete --numeric-ids /src/db/ /dst/db/",
		"run --rm --name devopsy-sync-", " -v " + s.mnt("prod") + ":/src:ro -v " + s.mnt("dev") + ":/dst devopsy-util:1 rsync -a --delete --numeric-ids /src/storage/ /dst/storage/",
		"shop-prod compose", "start db", "shop-dev compose", "up -d --wait app db"}
	rest := docker
	for _, want := range order {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("missing %q in order, after:\n%s\nall:\n%s", want, rest, docker)
		}
		rest = rest[i+len(want):]
	}
	if strings.Contains(docker, "run --rm -d") || strings.Contains(ssh, "ForwardAgent") || strings.Contains(docker, "stop app") {
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
	data := readString(releaseFile(t, filepath.Join(s.root, "shop", "dev", "current"), remote.TargetEnvFile))
	if !strings.Contains(data, "DEVOPSY_PARENT='prod'\n") || !strings.Contains(data, "DEVOPSY_PARENT_TARGET='devopsy@one:prod'\n") {
		t.Errorf("target.env:\n%s", data)
	}

	// A later release does not sync.
	next()
	out, code = s.run(nil, "@dev", "--release")
	docker, _ = s.logs()
	if code != 0 || strings.Contains(out, "devopsy: syncing") || strings.Contains(docker, "stop db") {
		t.Fatalf("second release (%d):\n%s\n%s", code, out, docker)
	}

	// --sync asks; --yes; arguments after -- go to rsync; the log goes
	// with the current release.
	write(t, filepath.Join(s.mnt("prod"), "storage", "file.txt"), "newer\n", 0o644)
	if out, code := s.run(nil, "@dev", "--sync"); code == 0 || !strings.Contains(out, "add --yes to go on without asking") {
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
	if out, code := s.run(nil, "@dev", "--release-sync"); code == 0 || !strings.Contains(out, "add --yes to go on without asking") || !strings.Contains(out, "devopsy: error: nothing released") {
		t.Fatalf("--release-sync without --yes (%d):\n%s", code, out)
	}
	if after, _ := os.ReadDir(filepath.Join(s.root, "shop", "dev", "releases")); len(after) != len(before) {
		t.Errorf("uploaded before asking: %d releases, then %d", len(before), len(after))
	}
	if out, code := s.run(nil, "@dev", "--release-sync", "--yes"); code != 0 || !strings.Contains(out, "devopsy: syncing @devopsy@one:prod's data") {
		t.Fatalf("--release-sync --yes (%d):\n%s", code, out)
	}
	s.logs()
	// A rollback copies nothing: --from is refused (--yes is accepted).
	if out, code := s.run(nil, "@dev", "--rollback", "--from", "@prod"); code == 0 || !strings.Contains(out, "--rollback takes no command") {
		t.Errorf("--rollback --from (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--sync", "--help"); code != 0 || !strings.Contains(out, "Usage: devopsy @<target> --sync [--from @<target>] [--yes] [-- args...]") || !strings.Contains(out, "devopsy.sync=stream") {
		t.Errorf("--sync --help (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--release", "--help"); code != 0 || !strings.Contains(out, "--release-sync [--from @<target>] [--yes]") {
		t.Errorf("--release --help (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev"); code != 0 || !strings.Contains(out, "--sync [--from @<target>]") {
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
	// every labeled service before its hook, where a re-sync restores
	// the stopped ones, and the run step's up finds them running.
	write(t, s.log+".shop-dev", "FAKE_ABSENT='app db'\n", 0o644)
	out, code := s.run(nil, "@dev", "--release")
	docker, _ := s.logs()
	os.Remove(s.log + ".shop-dev")
	if code != 0 || !strings.Contains(out, "devopsy: creating app db\n") || strings.Count(out, "devopsy: stopping db") != 1 || !strings.Contains(docker, "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" up -d --wait app db\n") || strings.Contains(docker, "shop-prod compose -f "+filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml")+" up -d") {
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
	if len(entries) != 1 || readString(filepath.Join(s.root, "shop", "dev", "releases", entries[0].Name(), remote.FailedFile)) != "" {
		t.Fatalf("releases: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(s.root, "shop", "dev", "releases", entries[0].Name(), remote.FailedFile)); err != nil {
		t.Errorf("not marked failed: %v", err)
	}
	// The parent's services come back too.
	if !strings.Contains(docker, "shop-prod compose -f "+filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml")+" start db") {
		t.Errorf("parent not restored:\n%s", docker)
	}

	// The copy was cut: the child's data is a partial copy, which only
	// a sync completes.
	if !fileExists(filepath.Join(s.root, "shop", "dev", remote.SyncIncompleteFile)) || !strings.Contains(out, "the copy was cut") {
		t.Errorf("a cut copy not marked partial:\n%s", out)
	}
	next()
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code == 0 || !strings.Contains(out, "did not complete") || !strings.Contains(out, "release it: a first release syncs\ndevopsy: | $ devopsy @devopsy@one:dev --release\n") || !strings.Contains(out, ": nothing changed\n") {
		t.Fatalf("--release-no-sync on a partial copy (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--release"); code != 0 || !strings.Contains(out, "deploy in shop-dev") || s.current("dev") == "" || fileExists(filepath.Join(s.root, "shop", "dev", remote.SyncIncompleteFile)) {
		t.Fatalf("a first release again, syncing (%d):\n%s", code, out)
	}
	next()
	s.logs()
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 || strings.Contains(out, "devopsy: syncing") || s.current("dev") == "" {
		t.Fatalf("--release-no-sync (%d):\n%s", code, out)
	}
	if docker, _ := s.logs(); strings.Contains(docker, "stop db") {
		t.Errorf("--release-no-sync stopped:\n%s", docker)
	}
}

// --from on a release: the parent's address when only the address names
// its server, as --sync --from; only on a child's release that syncs.
func TestReleaseFrom(t *testing.T) {
	s := newSyncSetup(t)
	s.releaseProd(nil)
	write(t, filepath.Join(s.dot, ".env"), "", 0o644) // prod's server from no variable

	out, code := s.run(nil, "@devopsy@one:dev", "--release")
	for _, want := range []string{
		"devopsy: error: dev's parent, prod, has no server\n",
		"devopsy: | to fix it, do one of these:\ndevopsy: | 1. set prod's server in .devopsy/.env",
		"\ndevopsy: | 2. name prod's address for this release:\ndevopsy: |    $ devopsy @devopsy@one:dev --release --from @devopsy@one:prod\n",
		"\ndevopsy: | 3. release without prod's data for now, and sync it whenever you are ready:\ndevopsy: |    $ devopsy @devopsy@one:dev --release-no-sync\ndevopsy: |    $ devopsy @devopsy@one:dev --sync --from @devopsy@one:prod\n",
	} {
		if code == 0 || !strings.Contains(out, want) || s.current("dev") != "" {
			t.Fatalf("no parent server, missing %q (%d):\n%s", want, code, out)
		}
	}
	s.logs()

	if out, code := s.run(nil, "@devopsy@one:dev", "--release", "--from", "@devopsy@one:prod"); code != 0 || !strings.Contains(out, "deploy in shop-dev") || readString(filepath.Join(s.mnt("dev"), "storage", "file.txt")) != "from prod\n" {
		t.Fatalf("--release --from (%d):\n%s", code, out)
	}
	s.logs()
	// Released: a plain release no longer syncs, so --from has no use there.
	if out, code := s.run(nil, "@devopsy@one:dev", "--release", "--from", "devopsy@one:prod"); code == 0 || !strings.Contains(out, "devopsy: | $ devopsy @devopsy@one:dev --release-sync --from @devopsy@one:prod\n") {
		t.Fatalf("--from on a released child (%d):\n%s", code, out)
	}
	write(t, filepath.Join(s.mnt("prod"), "storage", "file.txt"), "newer\n", 0o644)
	if out, code := s.run(nil, "@devopsy@one:dev", "--release-sync", "--from", "devopsy@one:prod", "--yes"); code != 0 || readString(filepath.Join(s.mnt("dev"), "storage", "file.txt")) != "newer\n" {
		t.Fatalf("--release-sync --from (%d):\n%s", code, out)
	}
	s.logs()
	if out, code := s.run(nil, "@devopsy@one:solo", "--release", "--from", "@devopsy@one:prod"); code == 0 || !strings.Contains(out, "--from: @devopsy@one:solo is not a child") {
		t.Fatalf("--from without a parent (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@devopsy@one:dev", "--release-no-sync", "--from", "@devopsy@one:prod"); code == 0 || !strings.Contains(out, "--from: --release-no-sync never syncs") {
		t.Fatalf("--release-no-sync --from (%d):\n%s", code, out)
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
	if code != 0 || !strings.Contains(docker, "devopsy-util:1 rsync -a --delete --numeric-ids --inplace /src/app.ini /dst/\n") || readString(filepath.Join(s.mnt("dev"), "app.ini")) != "[prod]\n" {
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
	if !strings.Contains(push, "-o ClearAllForwardings=yes -o ForwardAgent=yes -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -T devopsy@one") {
		t.Errorf("push session: %q\n%s", push, ssh)
	}
	// The parent's sessions to the child: never the forwarded agent, no
	// password prompt, and a short connect timeout.
	childSSH := "-o ForwardAgent=no -o BatchMode=yes -o ConnectTimeout=30 -o StrictHostKeyChecking=accept-new"
	if !strings.Contains(receive, "-o ExitOnForwardFailure=yes "+childSSH+" -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -T -R 127.0.0.1:") || !strings.Contains(receive, ":172.17.0.9:873 -p 22 devopsy@two") || strings.Contains(receive, "ClearAllForwardings") {
		t.Errorf("receive session: %q\n%s", receive, ssh)
	}
	// Every session of the release keeps itself alive: the upload, the
	// holder, the push, the push's look at the child's services
	// (--sync-exec), the receive, and the log's save.
	keep := "-o ServerAliveInterval=15 -o ServerAliveCountMax=3"
	if strings.Count(ssh, keep) != 6 || !strings.Contains(ssh, "-o ConnectTimeout=30 "+keep+" -T devopsy@two\n") || !strings.Contains(ssh, keep+" -T devopsy@two\n") || !strings.Contains(ssh, childSSH+" "+keep+" -T -p 22 devopsy@two\n") {
		t.Errorf("keepalive on upload, holder, push, exec, receive and the log's save:\n%s", ssh)
	}
	for _, want := range []string{"run --rm -d -v " + s.mnt("prod") + ":/mnt:ro", "RSYNCD_CONF=uid = 0\ngid = 0\nuse chroot = yes\nread only = yes\n[db]\npath = /mnt/db\n[storage]\npath = /mnt/storage\n", "inspect -f", " --network host -v " + s.mnt("dev") + ":/mnt devopsy-util:1 rsync -a --delete --numeric-ids rsync://127.0.0.1:", "/db/ /mnt/db/", "/storage/ /mnt/storage/", " rm -f cid123"} {
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
	if code == 0 || !strings.Contains(out, "no SSH agent was forwarded to") || !strings.Contains(out, "restrict,agent-forwarding") || !strings.Contains(docker, "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" start db") {
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
	// The parent's stop fails, before anything is written: what it
	// stopped comes back, and so does the child.
	out, code := s.run([]string{"FAIL_STOP=shop-prod"}, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code != 1 || !strings.Contains(out, "aborting the sync") || !strings.Contains(out, "before writing anything") {
		t.Fatalf("parent's stop fails (%d):\n%s", code, out)
	}
	rest := docker
	for _, want := range []string{"shop-prod compose", "stop db", "shop-prod compose", "start db", "shop-dev compose", "start db"} {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("parent not restored, missing %q in order:\n%s", want, docker)
		}
		rest = rest[i+len(want):]
	}

	// The rsync fails: the child's data is a partial copy, its stopped
	// services stay down, the parent's come back.
	out, code = s.run([]string{"FAIL_RSYNC=23"}, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 1 || !strings.Contains(out, "aborting the sync") || !strings.Contains(out, "the copy was cut") || !strings.Contains(out, "db stay down") || strings.Contains(out, "deploy in shop-dev") {
		t.Fatalf("abort (%d):\n%s", code, out)
	}
	rest = docker
	for _, want := range []string{"shop-dev compose", "stop db", "shop-prod compose", "start db"} {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("missing %q in order:\n%s", want, docker)
		}
		rest = rest[i+len(want):]
	}
	if strings.Contains(rest, "start db") {
		t.Errorf("the child restored on a partial copy:\n%s", docker)
	}
	if s.current("dev") == "" {
		t.Error("an aborted sync changed current")
	}
	if !fileExists(filepath.Join(s.root, "shop", "dev", remote.SyncIncompleteFile)) {
		t.Error("not marked as a partial copy")
	}

	// Nothing runs on it but a sync: a release and a rollback refuse, a
	// sync from it too.
	next()
	if out, code := s.run(nil, "@dev", "--release"); code == 0 || !strings.Contains(out, "did not complete") {
		t.Errorf("a release on a partial copy (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--rollback"); code == 0 || !strings.Contains(out, "did not complete") {
		t.Errorf("a rollback on a partial copy (%d):\n%s", code, out)
	}
	if status, code := s.run(nil, "@dev", "--status"); code != 0 || !strings.Contains(status, "did not complete") {
		t.Errorf("--status (%d):\n%s", code, status)
	}
	s.logs()

	// An abort that writes nothing leaves it so.
	if out, code := s.run([]string{"FAIL_STOP=shop-prod"}, "@dev", "--sync", "--yes"); code != 1 || !strings.Contains(out, "db stay down") {
		t.Errorf("an abort on a partial copy (%d):\n%s", code, out)
	}
	if docker, _ := s.logs(); strings.Contains(docker, "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" start") {
		t.Errorf("the child restored on a partial copy:\n%s", docker)
	}

	// A sync that completes brings it back.
	out, code = s.run(nil, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 || !strings.Contains(out, "deploy in shop-dev") || !strings.Contains(docker, "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" start db") || fileExists(filepath.Join(s.root, "shop", "dev", remote.SyncIncompleteFile)) {
		t.Fatalf("a complete sync (%d):\n%s\n%s", code, out, docker)
	}
}

// A copy that completed, after which the parent cannot start its stopped
// services again: the child goes on (its data is complete), but the
// command fails, since the parent's database is left down.
func TestSyncParentRestoreFails(t *testing.T) {
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()
	out, code := s.run([]string{"FAIL_START=shop-prod"}, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if !strings.Contains(docker, "shop-prod compose -f "+filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml")+" start db") || !strings.Contains(out, "the copy had completed: going on with it") {
		t.Fatalf("the parent's start not tried, or the child did not go on (%d):\n%s\n%s", code, out, docker)
	}
	if code == 0 {
		t.Errorf("the parent's db left down, and the sync exits 0:\n%s", out)
	}
}

// Stream services: the parent's export spooled into the child's
// directory, the child's import reading it once the export exited 0, in
// depends_on order; the hooks on both sides with their side and lists;
// the child's commands run as the child, without the parent's environment.
func TestSyncCapability(t *testing.T) {
	s := newSyncSetup(t)
	caps := filepath.Join(s.dot, "capabilities", "sync")
	write(t, filepath.Join(caps, "prepare"), "#!/bin/sh\necho \"prepare $1 in $COMPOSE_PROJECT_NAME rsync=$DEVOPSY_SYNC_RSYNC stream=$DEVOPSY_SYNC_STREAM stop=$DEVOPSY_SYNC_STOP from=$DEVOPSY_SYNC_FROM_TARGET to=$DEVOPSY_SYNC_TO_TARGET args=$DEVOPSY_SYNC_ARGS\"\n[ \"${FAIL_PREPARE:-}\" != \"$1\" ]\n", 0o755)
	write(t, filepath.Join(caps, "finish"), "#!/bin/sh\necho \"finish $1 $2 in $COMPOSE_PROJECT_NAME\"\n", 0o755)
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho \"dump of $1 from $COMPOSE_PROJECT_NAME pw=$DB_PASSWORD\"\nif [ \"${FAIL_EXPORT:-}\" = \"$1\" ]; then exit 3; fi\n", 0o755)
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\necho \"import $1 into $COMPOSE_PROJECT_NAME pw=$DB_PASSWORD from=$DEVOPSY_SYNC_FROM_TARGET args=$DEVOPSY_SYNC_ARGS: $(cat)\"\n", 0o755)
	s.releaseProd(nil)
	write(t, filepath.Join(s.root, "shop", "prod", "shared", ".env"), "DB_PASSWORD=parent-secret\n", 0o644)
	write(t, filepath.Join(s.root, "shop", "dev", "shared", ".env"), "DB_PASSWORD=child-secret\n", 0o644)
	spool := filepath.Join(s.root, "shop", "dev", ".devopsy-sync.stream.db")
	inOrder := func(name, out string, code int, want ...string) {
		t.Helper()
		rest := out
		for _, w := range want {
			i := strings.Index(rest, w)
			if i < 0 {
				t.Fatalf("%s: missing %q in order (%d):\n%s", name, w, code, out)
			}
			rest = rest[i+len(w):]
		}
	}

	out, code := s.run(nil, "@dev", "--release", "--", "x")
	if code == 0 {
		t.Fatalf("--release with arguments (%d):\n%s", code, out)
	}
	// db is a stream: rsync for app's storage, then db exported into the
	// child's spool and imported there, with each side's own secrets; the
	// child's db brought up before its import. Hooks on both sides.
	s.labels("FAKE_DB=stream")
	out, code = s.run(nil, "@dev", "--release")
	docker, _ := s.logs()
	if code != 0 {
		t.Fatalf("stream (%d):\n%s", code, out)
	}
	inOrder("stream", out, code, "prepare child in shop-dev rsync=app stream=db stop= from=devopsy@one:prod to=devopsy@one:dev args=", "devopsy: copying",
		"prepare parent in shop-prod rsync=app stream=db stop= from=devopsy@one:prod to=devopsy@one:dev", "rsync mnt/storage", "devopsy: exporting db",
		"import db into shop-dev pw=child-secret from=devopsy@one:prod args=: dump of db from shop-prod pw=parent-secret", "finish parent go in shop-prod", "finish child go in shop-dev", "deploy in shop-dev")
	if strings.Contains(docker, "stop db") || strings.Contains(docker, "/src/db/") || !strings.Contains(docker, "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" up -d --wait db") {
		t.Errorf("stream docker:\n%s", docker)
	}
	if _, err := os.Stat(spool); err == nil {
		t.Errorf("the spool stays")
	}
	// --log shows the copy in its place: the server's own log starts
	// after the sync, at the run step.
	out, code = s.run(nil, "@dev", "--log")
	inOrder("--log", out, code, "prepare child in shop-dev", "devopsy: copying", "rsync mnt/storage", "finish child go in shop-dev", "deploy in shop-dev")
	// --sync's arguments reach the hooks and the import.
	out, code = s.run(nil, "@dev", "--sync", "--yes", "--", "--exclude=css")
	s.logs()
	if code != 0 || !strings.Contains(out, "prepare child in shop-dev rsync=app stream=db stop= from=devopsy@one:prod to=devopsy@one:dev args=--exclude=css") || !strings.Contains(out, "args=--exclude=css: dump of db") {
		t.Errorf("--sync -- args (%d):\n%s", code, out)
	}

	// Two streams, one stopped: in depends_on order (app depends on db),
	// the stopped one never brought up for its import.
	s.labels("FAKE_APP=stream\nFAKE_DB=stream+stop")
	out, code = s.run(nil, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 {
		t.Fatalf("two streams (%d):\n%s", code, out)
	}
	inOrder("two streams", out, code, "prepare child in shop-dev rsync= stream=db app stop=db", "devopsy: stopping db", "devopsy: copying", "devopsy: stopping db", "devopsy: exporting db", "import db into shop-dev", "devopsy: exporting app", "import app into shop-dev", "devopsy: starting db", "devopsy: starting db", "deploy in shop-dev")
	if strings.Contains(docker, "--wait db") || strings.Contains(docker, "--wait --no-deps db") || !strings.Contains(docker, "up -d --wait --no-deps app") || strings.Contains(out, "rsync mnt/") {
		t.Errorf("two streams docker:\n%s", docker)
	}

	// An export that fails is never imported: the sync aborts, the
	// spool goes, the parent finishes abort; the rsync before it wrote,
	// so the child finishes partial, until a sync completes.
	s.labels("FAKE_DB=stream")
	out, code = s.run([]string{"FAIL_EXPORT=db"}, "@dev", "--sync", "--yes")
	s.logs()
	if code == 0 || !strings.Contains(out, "export db: exit status 3: nothing imported") || strings.Contains(out, "import db into") || strings.Contains(out, "deploy in shop-dev") ||
		!strings.Contains(out, "finish parent abort in shop-prod") || !strings.Contains(out, "finish child partial in shop-dev") {
		t.Errorf("failing export (%d):\n%s", code, out)
	}
	if _, err := os.Stat(spool); err == nil {
		t.Errorf("the spool of a failed export stays")
	}
	if out, code := s.run(nil, "@dev", "--sync", "--yes"); code != 0 || !strings.Contains(out, "finish child go in shop-dev") {
		t.Fatalf("a complete sync after (%d):\n%s", code, out)
	}
	s.logs()

	// A failing prepare aborts before anything stops on its side: on the
	// child, before the parent starts; on the parent, before its own.
	s.labels("")
	out, code = s.run([]string{"FAIL_PREPARE=child"}, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code == 0 || strings.Contains(out, "devopsy: copying") || strings.Contains(docker, "stop db") || !strings.Contains(out, "finish child abort in shop-dev") || strings.Contains(out, "deploy in shop-dev") {
		t.Errorf("failing prepare child (%d):\n%s\n%s", code, out, docker)
	}
	out, code = s.run([]string{"FAIL_PREPARE=parent"}, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code == 0 || strings.Contains(docker, "shop-prod compose -f "+filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml")+" stop db") || !strings.Contains(out, "finish parent abort in shop-prod") ||
		!strings.Contains(docker, "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" start db") {
		t.Errorf("failing prepare parent (%d):\n%s\n%s", code, out, docker)
	}

	// Across servers the export goes into the child's spool through an
	// SSH session of the parent's (cat), then the import there.
	s.labels("FAKE_DB=stream")
	across := []string{"DEVOPSY_SERVER_PROD=devopsy@one", "SSH_AUTH_SOCK=/tmp/agent.sock"}
	out, code = s.run(across, "@devopsy@two:dev", "--sync", "--yes")
	_, ssh := s.logs()
	if code != 0 || !strings.Contains(out, "import db into shop-dev pw=child-secret from=devopsy@one:prod args=: dump of db from shop-prod pw=parent-secret") {
		t.Fatalf("stream across servers (%d):\n%s", code, out)
	}
	childSSH := "-o ForwardAgent=no -o BatchMode=yes -o ConnectTimeout=30 -o StrictHostKeyChecking=accept-new"
	if strings.Count(ssh, "-o ClearAllForwardings=yes "+childSSH+" -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -T -p 22 devopsy@two") != 3 {
		t.Errorf("the child's list, the spool's cat and the import, each a session:\n%s", ssh)
	}
	if _, err := os.Stat(spool); err == nil {
		t.Errorf("the spool stays across servers")
	}
	out, code = s.run(append(across, "FAIL_EXPORT=db"), "@devopsy@two:dev", "--sync", "--yes")
	s.logs()
	if code == 0 || strings.Contains(out, "import db into") || !strings.Contains(out, "finish child partial") {
		t.Errorf("failing export across servers (%d):\n%s", code, out)
	}
	if _, err := os.Stat(spool); err == nil {
		t.Errorf("the spool of a failed export stays across servers: the child's abort removes it")
	}

	// A stream needs an import on the child, checked before anything
	// stops anywhere, and an export on the parent, before anything stops
	// there.
	childImport := filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "capabilities", "sync", "import")
	if err := os.Rename(childImport, childImport+".off"); err != nil {
		t.Fatal(err)
	}
	s.labels("FAKE_DB=stream+stop")
	out, code = s.run(nil, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code == 0 || !strings.Contains(out, "db labeled devopsy.sync=stream: the child's release needs an executable .devopsy/capabilities/sync/import") || strings.Contains(docker, "stop db") || strings.Contains(out, "devopsy: copying") || strings.Contains(out, "prepare child") {
		t.Errorf("no import on the child (%d):\n%s\n%s", code, out, docker)
	}
	if err := os.Rename(childImport+".off", childImport); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "capabilities", "sync", "export")); err != nil {
		t.Fatal(err)
	}
	out, code = s.run(nil, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code == 0 || !strings.Contains(out, "db labeled devopsy.sync=stream: the parent's release needs an executable .devopsy/capabilities/sync/export") || !strings.Contains(out, "aborting the sync") ||
		strings.Contains(docker, "shop-prod compose -f "+filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml")+" stop") || strings.Contains(out, "prepare parent") {
		t.Errorf("no export on the parent (%d):\n%s\n%s", code, out, docker)
	}

	// The hidden flags only run inside a push.
	s.labels("")
	prod := filepath.Join(s.root, "shop", "prod", "current")
	for _, flag := range []string{"--sync-exec", "--sync-push"} {
		cmd := exec.Command(binary, flag, "ps")
		cmd.Dir = prod
		cmd.Env = s.env
		if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "only runs in a sync's push") {
			t.Errorf("%s outside a push: %v\n%s", flag, err, out)
		}
	}
	// An import into a service the child does not label stream refuses.
	cmd := exec.Command(binary, "--sync-services", "import", "app")
	cmd.Dir = prod
	cmd.Env = append(append([]string{}, s.env...), "PWD="+prod)
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "the child's app is not labeled devopsy.sync=stream") {
		t.Errorf("import of an rsync service: %v\n%s", err, out)
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
	if out, code := s.run(nil, "@solo", "--sync", "--yes"); code == 0 || !strings.Contains(out, "@devopsy@one:solo is not a child") {
		t.Errorf("not a child (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@solo", "--release-sync"); code == 0 || !strings.Contains(out, "@devopsy@one:solo is not a child") {
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
	if out, code := s.run(nil, "@dev", "--sync", "--yes"); code == 0 || !strings.Contains(out, "has no current release: run 'devopsy @devopsy@one:dev --release' first") {
		t.Errorf("child not released (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--sync", "--from", "dev", "--yes"); code == 0 || !strings.Contains(out, "and its parent are the same environment") {
		t.Errorf("--from itself (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@bare", "--sync", "--yes"); code == 0 || !strings.Contains(out, "environment bare has no release steps") {
		t.Errorf("no run step (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--sync", "--bogus"); code == 0 || !strings.Contains(out, "usage: devopsy @devopsy@one:dev --sync") {
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
		"devopsy: inherited SHARED_KEY, INST_KEY, PROJ_KEY from @devopsy@one:a/prod into @devopsy@one:a/dev's shared/inherit.env\n",
		"@devopsy@one:a/prod does not set MISSING_KEY",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if i, j := strings.Index(out, "devopsy: inherited SHARED_KEY"), strings.Index(out, "devopsy: releasing "); i < 0 || j < i {
		t.Errorf("inherited after the upload:\n%s", out)
	}
	if got := readString(childInherit); got != "SHARED_KEY='s1'\nINST_KEY='i 1'\nPROJ_KEY='p1'\n" {
		t.Errorf("child's inherit.env: %q", got)
	}
	// The child's own shared/.env is untouched and wins; a key set there
	// too is named.
	write(t, childEnv, "OWN=1\nPROJ_KEY=child\n", 0o600)
	if out, code := s.run(nil, "@a/dev", "--inherit"); code != 0 || !strings.Contains(out, "devopsy: warning: @devopsy@one:a/dev's own shared .env also sets PROJ_KEY, which wins there\ndevopsy: | to inherit it:\ndevopsy: | $ devopsy @devopsy@one:a/dev --vars unset PROJ_KEY\n") {
		t.Errorf("shadowed key (%d):\n%s", code, out)
	}
	if got := readString(childEnv); got != "OWN=1\nPROJ_KEY=child\n" {
		t.Errorf("child's shared/.env changed: %q", got)
	}
	if strings.Contains(out, "s1") || strings.Contains(readString(releaseFile(t, filepath.Join(s.root, "shop", "a", "dev", "current"), remote.LogFile)), "s1") {
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
	if out, code := s.run(nil, "@a/dev", "--release-no-sync"); code != 0 || strings.Contains(out, "devopsy: inherited") || !strings.Contains(readString(childInherit), "SHARED_KEY='s1'") {
		t.Fatalf("--release-no-sync (%d): %q\n%s", code, readString(childInherit), out)
	}

	// --inherit resyncs the variables alone: no copy, no run step.
	s.logs()
	out, code = s.run(nil, "@a/dev", "--inherit")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(out, "devopsy: inherited SHARED_KEY, INST_KEY, PROJ_KEY from") || strings.Contains(out, "deploy in") || strings.Contains(docker, "pause") || !strings.Contains(readString(childInherit), "SHARED_KEY='s2'") {
		t.Fatalf("--inherit (%d): %q\n%s\n%s", code, readString(childInherit), out, docker)
	}
	if out, code := s.run(nil, "@a/prod", "--inherit"); code == 0 || !strings.Contains(out, "is not a child") {
		t.Errorf("--inherit on prod (%d):\n%s", code, out)
	}

	// Across servers a re-sync inherits before the holder, into the
	// pending file the sync reads, moved into place once the copy is in:
	// a failed copy leaves the child's file as it was.
	write(t, filepath.Join(shop, "a", "prod", "shared", ".env"), "SHARED_KEY=s3\n", 0o600)
	env := []string{"DEVOPSY_SERVER_PROD=devopsy@one", "SSH_AUTH_SOCK=/tmp/agent.sock"}
	s.logs()
	out, code = s.run(env, "@devopsy@two:a/dev", "--sync", "--yes")
	if code != 0 {
		t.Fatalf("--sync (%d):\n%s", code, out)
	}
	if i, j := strings.Index(out, "devopsy: inherited SHARED_KEY, INST_KEY, PROJ_KEY from @devopsy@one:a/prod for @devopsy@two:a/dev's sync (its shared/inherit.env once the copy is in)\n"), strings.Index(out, "syncing into"); i < 0 || j < i {
		t.Errorf("not inherited before the holder:\n%s", out)
	}
	// The write takes the child's lock before the holder does.
	if docker, _ := s.logs(); strings.Count(docker, "flock -n 9\n") != 2 {
		t.Errorf("the child's lock during a re-sync:\n%s", docker)
	}
	pending := filepath.Join(shop, "a", "dev", ".devopsy-sync.inherit.env")
	if _, err := os.Stat(pending); err == nil {
		t.Errorf("the pending file stays after a sync")
	}
	if got := readString(childInherit); got != "SHARED_KEY='s3'\nINST_KEY='i 1'\nPROJ_KEY='p1'\n" {
		t.Errorf("child's inherit.env: %q", got)
	}
	write(t, filepath.Join(shop, "a", "prod", "shared", "mnt", "storage", "f"), "x\n", 0o644)
	write(t, filepath.Join(shop, "a", "prod", "shared", ".env"), "SHARED_KEY=s4\n", 0o600)
	if out, code := s.run(append(env, "FAIL_RSYNC=23"), "@devopsy@two:a/dev", "--sync", "--yes"); code == 0 || !strings.Contains(readString(childInherit), "SHARED_KEY='s3'") {
		t.Errorf("failed re-sync (%d): %q\n%s", code, readString(childInherit), out)
	}
	if _, err := os.Stat(pending); err == nil {
		t.Errorf("the pending file stays after a failed sync: its abort removes it")
	}
}

// --sync reruns the run step of what is deployed on the child: the
// release's own config on the server decides, as for a rollback or a
// restart, never the checkout, which may be another branch.
func TestSyncRunStepDeployed(t *testing.T) {
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	// The checkout moves on: dev's run step is another command now, which
	// the deployed release does not have.
	write(t, filepath.Join(s.dot, "config.yaml"), "project: shop\ndefaults:\n  mode: image\n  release: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod, release: other}\n", 0o644)
	write(t, filepath.Join(s.dot, "commands", "other"), "#!/bin/sh\necho \"other in $COMPOSE_PROJECT_NAME\"\n", 0o755)
	s.logs()
	out, code := s.run(nil, "@dev", "--sync", "--yes")
	if code != 0 || !strings.Contains(out, "deploy in shop-dev") || strings.Contains(out, "'devopsy other'") {
		t.Errorf("the run step after a sync (%d), not the deployed release's:\n%s", code, out)
	}
}

// Interrupted during an export, devopsy ends the push session first, then
// aborts: the export is ended, its spool removed, both sides' stopped
// services come back, nothing is imported and the run step does not run.
func TestSyncInterrupt(t *testing.T) {
	s := newSyncSetup(t)
	started := filepath.Join(t.TempDir(), "started")
	caps := filepath.Join(s.dot, "capabilities", "sync")
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho partial\ntouch \"$FAKE_STARTED\"\nexec sleep 20\n", 0o755)
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\necho \"IMPORT RAN $(cat)\"\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()
	s.labels("FAKE_DB=stream+stop")

	// Output to a file: the export's sleep, orphaned, keeps a pipe open.
	outFile, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "@dev", "--sync", "--yes")
	cmd.Dir = s.project
	cmd.Env = append(append([]string{}, s.env...), "FAKE_STARTED="+started)
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
			t.Fatalf("the export never started:\n%s", readString(outFile.Name()))
		}
		time.Sleep(50 * time.Millisecond)
	}
	start := time.Now()
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	out := readString(outFile.Name())
	if cmd.ProcessState.ExitCode() == 0 || time.Since(start) > 15*time.Second || !strings.Contains(out, "stopping the copy from @devopsy@one:prod, then aborting") || strings.Contains(out, "deploy in shop-dev") || strings.Contains(out, "IMPORT RAN") || !strings.Contains(out, "export db: interrupted: nothing imported") {
		t.Fatalf("interrupt (%d, %s):\n%s", cmd.ProcessState.ExitCode(), time.Since(start), out)
	}
	// The parent comes back; the child, whose rsync had gone, holds a
	// partial copy and stays down.
	restored := "shop-prod compose -f " + filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml") + " start db"
	for !strings.Contains(readString(s.log), restored) || !fileExists(filepath.Join(s.root, "shop", "dev", remote.SyncIncompleteFile)) {
		if time.Since(start) > 10*time.Second {
			t.Fatalf("prod not restored, or dev not marked partial:\n%s\n%s", readString(s.log), readString(outFile.Name()))
		}
		time.Sleep(100 * time.Millisecond)
	}
	if strings.Contains(readString(s.log), "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" start db") {
		t.Errorf("the child restored on a partial copy:\n%s", readString(s.log))
	}
	if _, err := os.Stat(filepath.Join(s.root, "shop", "dev", ".devopsy-sync.stream.db")); err == nil {
		t.Errorf("the spool of an interrupted export stays")
	}
}

// Labels opt in: without any, nothing is copied or touched; only the
// services labeled devopsy.sync.stop=true are stopped, and restored from
// what was stopped.
func TestSyncLabels(t *testing.T) {
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()

	out, code := s.run([]string{"FAKE_APP=none", "FAKE_DB=none"}, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(out, "nothing to sync: no service labeled devopsy.sync") || !strings.Contains(out, "deploy in shop-dev") ||
		strings.Contains(docker, " stop ") || strings.Contains(docker, "rsync") {
		t.Fatalf("no labels (%d):\n%s\n%s", code, out, docker)
	}

	out, code = s.run(nil, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	rest := out
	for _, want := range []string{"devopsy: stopping db", "devopsy: copying", "devopsy: stopping db", "rsync mnt/db", "rsync mnt/storage", "devopsy: starting db", "devopsy: starting db", "deploy in shop-dev"} {
		i := strings.Index(rest, want)
		if code != 0 || i < 0 {
			t.Fatalf("missing %q in order (%d):\n%s", want, code, out)
		}
		rest = rest[i+len(want):]
	}
	if strings.Contains(docker, "stop app") || strings.Contains(docker, "pause") {
		t.Errorf("app, rsync without stop, touched:\n%s", docker)
	}
	// Both stopped: one compose stop, Compose orders them.
	out, code = s.run([]string{"FAKE_APP=rsync+stop"}, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 || strings.Count(out, "devopsy: stopping app db") != 2 || strings.Count(docker, " stop app db\n") != 2 {
		t.Errorf("both stopped (%d):\n%s\n%s", code, out, docker)
	}

	// A db down before the sync (a maintenance stop) stays down: restoring
	// brings back what was recorded, and the record goes afterwards.
	out, code = s.run([]string{"FAKE_DB_DOWN=1"}, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 || strings.Contains(out, "stopping db") || strings.Contains(out, "starting db") || strings.Contains(docker, "start db") {
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
	// finish alone, in a current release: with a record it restores what
	// the record says; without one (a lost file) every service labeled
	// devopsy.sync.stop=true. On abort, the streams' spools go.
	record := filepath.Join(s.root, "shop", "dev", ".devopsy-sync.services")
	finish := func(answer string) (string, string) {
		t.Helper()
		cmd := exec.Command(binary, "--sync-services", "finish", answer)
		cmd.Dir = filepath.Join(s.root, "shop", "dev", "current")
		// PWD as a shell's cd leaves it: the record lives next to current.
		// db is down: what a restore starts, or not, by the record.
		cmd.Env = append(append([]string{}, s.env...), "COMPOSE_PROJECT_NAME=shop-dev", "PWD="+cmd.Dir, "FAKE_DB_DOWN=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("finish: %v\n%s", err, out)
		}
		docker, _ := s.logs()
		return string(out), docker
	}
	// A one-off of db still running (an import's run --rm) is not db:
	// db, stopped by the sync, starts.
	write(t, record, "stop db\n", 0o644)
	write(t, s.log+".shop-dev", "FAKE_ONEOFF=db\n", 0o644)
	if out, docker := finish("go"); !strings.Contains(docker, "start db") {
		t.Errorf("finish beside a one-off:\n%s\n%s", out, docker)
	}
	write(t, s.log+".shop-dev", "", 0o644)
	write(t, record, "stop other\n", 0o644)
	if out, docker := finish("go"); strings.Contains(docker, "start db") {
		t.Errorf("finish with a record:\n%s\n%s", out, docker)
	}
	if _, err := os.Stat(record); err == nil {
		t.Errorf("the record stays after finish")
	}
	// Spools are one per sync (its id in the name): one left at finish is
	// stale, an earlier sync's, and goes whatever the word.
	spool := filepath.Join(s.root, "shop", "dev", ".devopsy-sync.stream.db.1.2")
	write(t, spool, "partial\n", 0o600)
	if out, docker := finish("go"); !strings.Contains(docker, "start db") {
		t.Errorf("finish without a record:\n%s\n%s", out, docker)
	}
	if _, err := os.Stat(spool); err == nil {
		t.Errorf("finish go left a stale spool")
	}
	write(t, spool, "partial\n", 0o600)
	finish("abort")
	if _, err := os.Stat(spool); err == nil {
		t.Errorf("finish abort left a spool")
	}
	cmd := exec.Command(binary, "--sync-services", "finish")
	cmd.Dir = filepath.Join(s.root, "shop", "dev", "current")
	cmd.Env = s.env
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "usage: devopsy --sync-services prepare [first] | finish go|abort") {
		t.Errorf("finish without an answer: %v\n%s", err, out)
	}

	// An app mounting mnt whole copies the unlabeled db's entry too: a
	// notice.
	out, code = s.run([]string{"FAKE_APP_MOUNT=", "FAKE_DB=none"}, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 || !strings.Contains(out, "devopsy: warning: app copies mnt whole, db (skip: db) included: mount mnt's entries (./mnt/db) on app instead of ./mnt, so each copies by its own label") || !strings.Contains(docker, "rsync -a --delete --numeric-ids /src/./ /dst/./") {
		t.Errorf("whole mnt notice (%d):\n%s\n%s", code, out, docker)
	}
	out, code = s.run([]string{"FAKE_APP_MOUNT="}, "@dev", "--sync", "--yes")
	s.logs()
	if code != 0 || strings.Contains(out, "copies mnt whole") {
		t.Errorf("notice for an rsync db under mnt whole (%d):\n%s", code, out)
	}
	// An old label is an error, before anything stops.
	out, code = s.run([]string{"FAKE_APP=pause"}, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code == 0 || !strings.Contains(out, "service app: devopsy.sync=pause: rsync or stream") || strings.Contains(docker, " stop ") {
		t.Errorf("old label (%d):\n%s\n%s", code, out, docker)
	}
}

// What a sync stopped comes back when a later sync completes, as its
// record says, whatever the labels say by then (a label from an inherited
// variable can change in that sync): db, stopped by a sync left
// partial, starts once a sync completes, labeled stop=true or not.
func TestSyncRestoresRecordedStops(t *testing.T) {
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	if out, code := s.run([]string{"FAIL_RSYNC=23"}, "@dev", "--sync", "--yes"); code == 0 || !strings.Contains(out, "db stay down") {
		t.Fatalf("a cut copy (%d):\n%s", code, out)
	}
	s.labels("FAKE_DB=rsync")
	defer s.labels("")
	s.logs()
	out, code := s.run(nil, "@dev", "--sync", "--yes")
	if code != 0 || fileExists(filepath.Join(s.root, "shop", "dev", remote.SyncIncompleteFile)) {
		t.Fatalf("the sync that completes it (%d):\n%s", code, out)
	}
	if stopped := readString(s.log + ".stopped.shop-dev"); slices.Contains(strings.Fields(stopped), "db") {
		t.Errorf("db, stopped by the cut sync, stays down after a complete one:\n%s", out)
	}
}

// A service the parent rsyncs that the child leaves running (stream or no
// label there) fails the push before the parent stops anything: the child
// restores its services; rsync on the child says running is fine.
func TestSyncMismatch(t *testing.T) {
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "capabilities", "sync", "import"), "#!/bin/sh\ncat >/dev/null\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()
	for _, label := range []string{"stream", "none"} {
		write(t, s.log+".shop-dev", "FAKE_DB="+label+"\nFAKE_APP=rsync+stop\n", 0o644)
		out, code := s.run(nil, "@dev", "--sync", "--yes")
		docker, _ := s.logs()
		want := "labeled devopsy.sync=stream"
		if label == "none" {
			want = "without a devopsy.sync label"
		}
		if code == 0 || !strings.Contains(out, "devopsy: error: the child's sync labels do not match the parent's: nothing changed\ndevopsy: | the child's db is running, "+want+", and mnt/db would change under it: the parent copies it for db (devopsy.sync=rsync)\n") ||
			strings.Contains(out, "deploy in shop-dev") || strings.Count(docker, "shop-prod compose") != 1 || !strings.Contains(docker, "shop-prod compose -f "+filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml")+" --profile * config") ||
			strings.Contains(docker, "rsync") || !strings.Contains(docker, "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" start app") {
			t.Errorf("child db %s (%d):\n%s\n%s", label, code, out, docker)
		}
	}
	write(t, s.log+".shop-dev", "FAKE_DB=rsync\n", 0o644)
	out, code := s.run(nil, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(out, "deploy in shop-dev") || !strings.Contains(docker, "/src/db/ /dst/db/") {
		t.Errorf("child db rsync (%d):\n%s\n%s", code, out, docker)
	}
	// A stream on one side only is refused before the parent stops
	// anything, either way round, with the way out: an inherited label
	// needs --inherit before the sync, which only writes it after the copy.
	write(t, filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "capabilities", "sync", "export"), "#!/bin/sh\necho dump\n", 0o755)
	inherit := "label it alike on both sides (a variable the parent's inherit: names keeps them so; the child's own shared/.env wins over it)"
	for _, c := range []struct{ prod, dev, want string }{
		{"FAKE_DB=stream", "FAKE_DB=rsync+stop", "devopsy: | the parent streams db (devopsy.sync=stream) and the child labels it devopsy.sync=rsync: its import would refuse it\n"},
		{"FAKE_DB=rsync+stop", "FAKE_DB=stream+stop", "devopsy: | the child streams db (devopsy.sync=stream) and the parent rsyncs it (devopsy.sync=rsync): its import would never run\n"},
	} {
		write(t, s.log+".shop-prod", c.prod+"\n", 0o644)
		write(t, s.log+".shop-dev", c.dev+"\n", 0o644)
		out, code = s.run(nil, "@dev", "--sync", "--yes")
		docker, _ = s.logs()
		if code == 0 || !strings.Contains(out, c.want) || !strings.Contains(out, inherit) || strings.Contains(out, "deploy in shop-dev") ||
			strings.Contains(docker, "shop-prod compose -f "+filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml")+" stop") || strings.Contains(docker, "rsync -a") ||
			!strings.Contains(docker, "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" start db") {
			t.Errorf("prod %s, dev %s (%d):\n%s\n%s", c.prod, c.dev, code, out, docker)
		}
	}
	s.labels("")
}

// The child's list, from this devopsy (name, label with +stop, state,
// entries) or an older one (name, policy, entries, or with a state), and
// the guard on it: only a running service not labeled rsync conflicts.
func TestChildServicesParse(t *testing.T) {
	got := parseChildServices("app\trsync+stop\texited\tstorage\ndb\tstream\trunning\tdb\nold\tlive\tdb\nolder\tstop\texited\tdb\nlive4\tlive\trunning\tdb\nbad\n")
	want := []childService{
		{Name: "app", Transfer: "rsync", Stop: true, State: "exited", Dirs: []string{"storage"}},
		{Name: "db", Transfer: "stream", State: "running", Dirs: []string{"db"}},
		{Name: "old", Transfer: "live", Dirs: []string{"db"}},
		{Name: "older", Transfer: "stop", State: "exited", Dirs: []string{"db"}},
		{Name: "live4", Transfer: "live", State: "running", Dirs: []string{"db"}},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed: %+v", got)
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].Transfer != want[i].Transfer || got[i].Stop != want[i].Stop || got[i].State != want[i].State || strings.Join(got[i].Dirs, " ") != strings.Join(want[i].Dirs, " ") {
			t.Errorf("%d: %+v, want %+v", i, got[i], want[i])
		}
	}
	parent := []cli.SyncService{{Name: "db", Transfer: cli.SyncRsync, Dirs: []string{"db"}, Labeled: true}}
	conflicts := syncConflicts(parent, got)
	// db (stream, running) and live4 (an old label, running) conflict; the
	// stopped ones and the stateless old line do not.
	if len(conflicts) != 2 || !strings.Contains(conflicts[0], "the child's db is running, labeled devopsy.sync=stream") || !strings.Contains(conflicts[1], "the child's live4 is running, labeled devopsy.sync=live") {
		t.Errorf("conflicts: %q", conflicts)
	}
	if m := streamMismatches(parent, nil, got); len(m) != 1 || !strings.Contains(m[0], "the child streams db") {
		t.Errorf("mismatches: %q", m)
	}
	if m := streamMismatches([]cli.SyncService{{Name: "db", Transfer: cli.SyncStream}}, []string{"db", "gone"}, got); len(m) != 1 || !strings.Contains(m[0], "the child has no service gone") {
		t.Errorf("stream mismatches: %q", m)
	}
}

// Interrupted during the copy, the parent's push stops at once, though
// its session outlives devopsy's ssh: the closed stdin tells it. It
// removes the rsync container, restores its services, and never runs the
// export of the stream services.
func TestSyncInterruptCopy(t *testing.T) {
	s := newSyncSetup(t)
	tmp := t.TempDir()
	started, pushed := filepath.Join(tmp, "started"), filepath.Join(tmp, "pushed")
	write(t, filepath.Join(s.dot, "capabilities", "sync", "export"), "#!/bin/sh\ntouch \"$FAKE_PUSHED\"\n", 0o755)
	write(t, filepath.Join(s.dot, "capabilities", "sync", "import"), "#!/bin/sh\ncat >/dev/null\n", 0o755)
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
	s.labels("FAKE_APP=rsync+stop\nFAKE_DB=stream")
	cmd.Env = append(append([]string{}, s.env...), "FAKE_SLOW_RSYNC="+started, "FAKE_PUSHED="+pushed, "FAKE_SSH_DETACH=1")
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
	restored := "shop-prod compose -f " + filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml") + " start app"
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
		t.Errorf("the capability's export ran after the interrupt")
	}
	// Said where the parent's words go once its session is cut: its log.
	if log := readString(filepath.Join(s.root, "shop", "prod", ".devopsy-sync.log")); !strings.Contains(log, "devopsy: mnt/storage is half copied: a database copied raw cannot start on it until the next sync completes it") {
		t.Errorf("no word on the entry cut:\n%s", log)
	}
	// What the parent did once nobody read its session is in its log.
	if log := readString(filepath.Join(s.root, "shop", "prod", ".devopsy-sync.log")); !strings.Contains(log, ": the sync's session is gone: restoring what was stopped") || !strings.Contains(log, "devopsy: starting app") {
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
	if code == 0 || !strings.Contains(out, "ended during the copy (255): stopping the copy from @devopsy@one:prod") || !strings.Contains(out, "may hold a partial copy now\ndevopsy: | devopsy @devopsy@one:dev --status") || strings.Contains(out, "deploy in shop-dev") {
		t.Fatalf("holder gone (%d):\n%s\n%s\n%s", code, out, ssh, docker)
	}
	// The push outlives devopsy's session: its restore may still be
	// under way when devopsy has exited.
	restored := "shop-prod compose -f " + filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml") + " start db"
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
	out, code := s.run([]string{"FAKE_DB_DOWN=1"}, "--status")
	if code != 0 || !strings.Contains(out, "containers\n") || !strings.Contains(out, "  app ") || !strings.Contains(out, "  db ") || !strings.Contains(out, "exited") || !strings.Contains(out, "url https://shop.example\n") {
		t.Fatalf("local status (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@prod", "--status"); code != 0 || !strings.Contains(out, "@devopsy@one:prod (devopsy@one:shop/prod)") || !strings.Contains(out, "release     none: not released yet") {
		t.Fatalf("unreleased status (%d):\n%s", code, out)
	}
	s.releaseProd(nil)
	write(t, filepath.Join(s.root, "shop", "prod", "releases", "19990101000000", remote.FailedFile), "", 0o644)
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

// runIO runs devopsy with stdin, stdout and stderr apart.
func (s *syncSetup) runIO(extra []string, stdin []byte, args ...string) (stdout []byte, stderr string, code int) {
	s.t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir = s.project
	cmd.Env = append(append([]string{}, s.env...), extra...)
	cmd.Stdin = bytes.NewReader(stdin)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	// Its own session: no terminal to ask on, whatever runs the tests.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	_ = cmd.Run()
	return o.Bytes(), e.String(), cmd.ProcessState.ExitCode()
}

// tarOf is a tar of files (path: content), directories implied.
func tarOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	var names []string
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(files[n])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(files[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// --export and --import, locally and on a target: a stream through the
// sync capability's scripts, an rsync service as a tar of its entries,
// a stopped service stopped around it; stdin spooled first, and nothing
// changed unless it is complete.
func TestExportImport(t *testing.T) {
	s := newSyncSetup(t)
	imported := filepath.Join(t.TempDir(), "imported")
	caps := filepath.Join(s.dot, "capabilities", "sync")
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho \"dump of $1 from $COMPOSE_PROJECT_NAME stream=$DEVOPSY_SYNC_STREAM stop=$DEVOPSY_SYNC_STOP\"\n", 0o755)
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\n{ echo \"import $1 into $COMPOSE_PROJECT_NAME\"; cat; } > \"$FAKE_IMPORTED\"\n", 0o755)
	env := []string{"FAKE_IMPORTED=" + imported}
	spools := func(dir string) []string {
		m, _ := filepath.Glob(filepath.Join(dir, ".devopsy-sync.stream.*"))
		return m
	}

	// A stream, stopped: stopped around the export, its stdout the data.
	stopped := append(env, "FAKE_DB=stream+stop")
	out, errs, code := s.runIO(stopped, nil, "--export", "db")
	docker, _ := s.logs()
	if code != 0 || string(out) != "dump of db from shop stream=db stop=db\n" || !strings.Contains(errs, "devopsy: stopping db") || !strings.Contains(errs, "devopsy: starting db") {
		t.Fatalf("--export stream (%d): %q\n%s", code, out, errs)
	}
	if i, j := strings.Index(docker, " stop db"), strings.Index(docker, " start db"); i < 0 || j < i {
		t.Errorf("stopped around the export:\n%s", docker)
	}
	// Into a stream: spooled, then the import with it as stdin; a stopped
	// service is never brought up, a running one is.
	if _, errs, code := s.runIO(stopped, []byte("rows\n"), "--import", "db"); code != 0 || readString(imported) != "import db into shop\nrows\n" {
		t.Fatalf("--import stream (%d): %q\n%s", code, readString(imported), errs)
	}
	if docker, _ := s.logs(); strings.Contains(docker, "up -d --wait db") || !strings.Contains(docker, " stop db") {
		t.Errorf("stopped stream import:\n%s", docker)
	}
	if m := spools(s.dot); len(m) > 0 {
		t.Errorf("spools left: %v", m)
	}
	os.Remove(imported)
	if _, errs, code := s.runIO(append(env, "FAKE_DB=stream"), []byte("rows\n"), "--import", "db", "--yes"); code != 0 || !strings.Contains(readString(imported), "rows") {
		t.Fatalf("--import running stream (%d):\n%s", code, errs)
	}
	if docker, _ := s.logs(); !strings.Contains(docker, "up -d --wait db") || strings.Contains(docker, " stop db") {
		t.Errorf("running stream import:\n%s", docker)
	}
	// Nothing on stdin imports nothing.
	os.Remove(imported)
	if _, errs, code := s.runIO(stopped, nil, "--import", "db"); code == 0 || !strings.Contains(errs, "nothing on stdin: nothing imported") || readString(imported) != "" {
		t.Errorf("empty stdin (%d):\n%s", code, errs)
	}
	if docker, _ := s.logs(); strings.Contains(docker, " stop db") {
		t.Errorf("empty stdin stopped db:\n%s", docker)
	}

	// rsync: a tar of the service's entries of mnt, made in the image.
	write(t, filepath.Join(s.dot, "mnt", "storage", "old.txt"), "local\n", 0o644)
	out, errs, code = s.runIO(nil, nil, "--export", "app")
	docker, _ = s.logs()
	mnt := filepath.Join(s.dot, "mnt")
	if code != 0 || !strings.Contains(docker, "run --rm --name devopsy-export-") || !strings.Contains(docker, "-v "+mnt+":/mnt:ro devopsy-util:1 tar -C /mnt -cf - --numeric-owner storage\n") {
		t.Fatalf("--export rsync (%d):\n%s\n%s", code, errs, docker)
	}
	var names []string
	for tr := tar.NewReader(bytes.NewReader(out)); ; {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, strings.TrimSuffix(h.Name, "/"))
	}
	if !slices.Contains(names, "storage/old.txt") {
		t.Errorf("tar members: %v", names)
	}
	// Its import empties the entries the tar carries, then extracts.
	if _, errs, code := s.runIO(nil, tarOf(t, map[string]string{"storage/new.txt": "new\n"}), "--import", "app"); code != 0 || readString(filepath.Join(mnt, "storage", "new.txt")) != "new\n" {
		t.Fatalf("--import rsync (%d):\n%s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(mnt, "storage", "old.txt")); err == nil {
		t.Errorf("the entry was not emptied first")
	}
	if docker, _ := s.logs(); !strings.Contains(docker, "-i -v "+mnt+":/mnt devopsy-util:1 sh -c ") || !strings.Contains(docker, "tar -C /mnt -xf - --numeric-owner --overwrite\n sh storage\n") {
		t.Errorf("rsync import:\n%s", docker)
	}
	// A truncated tar, or one carrying what the service does not mount,
	// changes nothing.
	whole := tarOf(t, map[string]string{"storage/other.txt": strings.Repeat("x", 2000)})
	if _, errs, code := s.runIO(nil, whole[:700], "--import", "app"); code == 0 || !strings.Contains(errs, "stdin is not a complete tar") || readString(filepath.Join(mnt, "storage", "new.txt")) != "new\n" {
		t.Errorf("truncated tar (%d):\n%s", code, errs)
	}
	if _, errs, code := s.runIO(nil, tarOf(t, map[string]string{"db/x": "x"}), "--import", "app"); code == 0 || !strings.Contains(errs, "the tar carries mnt/db, which app does not bind-mount (storage): nothing imported") {
		t.Errorf("foreign entry (%d):\n%s", code, errs)
	}
	if docker, _ := s.logs(); strings.Contains(docker, "devopsy-import-") && strings.Contains(docker, "sh -c") {
		t.Errorf("extracted a refused tar:\n%s", docker)
	}
	if m := spools(s.dot); len(m) > 0 {
		t.Errorf("spools left: %v", m)
	}
	// Unlabeled, unknown: refused.
	if _, errs, code := s.runIO([]string{"FAKE_APP=none"}, nil, "--export", "app"); code == 0 || !strings.Contains(errs, "app has no devopsy.sync label") {
		t.Errorf("unlabeled (%d):\n%s", code, errs)
	}
	if _, errs, code := s.runIO(nil, nil, "--export", "nope"); code == 0 || !strings.Contains(errs, "no service nope in the project (labeled devopsy.sync: app, db)") {
		t.Errorf("unknown (%d):\n%s", code, errs)
	}
	if out, _, code := s.runIO(nil, nil, "--export", "--help"); code != 0 || !strings.Contains(string(out), "Usage: devopsy [@<target>] --export <service>") {
		t.Errorf("--export --help (%d):\n%s", code, out)
	}
	if out, _, code := s.runIO(nil, nil, "@prod", "--import", "--help"); code != 0 || !strings.Contains(string(out), "Usage: devopsy [@<target>] --import <service> [--yes]") {
		t.Errorf("@prod --import --help (%d):\n%s", code, out)
	}
	s.logs()

	// An export interrupted: its process group ended, the service back.
	exportStarted, exportStopped := filepath.Join(t.TempDir(), "started"), filepath.Join(t.TempDir(), "stopped")
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\ntrap 'touch \"$FAKE_STOPPED\"; exit 1' TERM\necho partial\ntouch \"$FAKE_STARTED\"\nsleep 20 &\nwait\n", 0o755)
	cmd := exec.Command(binary, "--export", "db")
	cmd.Dir = s.project
	cmd.Env = append(append([]string{}, s.env...), "FAKE_DB=stream+stop", "FAKE_STARTED="+exportStarted, "FAKE_STOPPED="+exportStopped)
	var eout, eerr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &eout, &eerr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFile(t, exportStarted, "the export", func() string { return eerr.String() })
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	waitFile(t, exportStopped, "the export's end", func() string { return eerr.String() })
	if docker, _ := s.logs(); cmd.ProcessState.ExitCode() == 0 || !strings.Contains(eerr.String(), "export db: interrupted: what was written is incomplete") || !strings.Contains(docker, " start db") {
		t.Errorf("interrupted export (%d):\n%s\n%s", cmd.ProcessState.ExitCode(), eerr.String(), docker)
	}
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho \"dump of $1 from $COMPOSE_PROJECT_NAME stream=$DEVOPSY_SYNC_STREAM stop=$DEVOPSY_SYNC_STOP\"\n", 0o755)

	// A failing import: the service back, the spool gone.
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\ncat >/dev/null\n[ -z \"${FAKE_FAIL_IMPORT:-}\" ] || exit 4\n{ echo \"import $1 into $COMPOSE_PROJECT_NAME\"; cat; } > \"$FAKE_IMPORTED\"\n", 0o755)
	if _, errs, code := s.runIO(append(stopped, "FAKE_FAIL_IMPORT=1"), []byte("rows\n"), "--import", "db"); code == 0 || !strings.Contains(errs, "import db: exit status 4") {
		t.Errorf("failing import (%d):\n%s", code, errs)
	}
	if docker, _ := s.logs(); !strings.Contains(docker, " stop db") || !strings.Contains(docker, " start db") {
		t.Errorf("failing import, db not restored:\n%s", docker)
	}
	if m := spools(s.dot); len(m) > 0 {
		t.Errorf("spools left after a failing import: %v", m)
	}
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\n{ echo \"import $1 into $COMPOSE_PROJECT_NAME\"; cat; } > \"$FAKE_IMPORTED\"\n", 0o755)

	// A stream's import that fails, not stopped for it: what it started
	// in the service may load on, so the service is stopped (and started
	// again, it ran before), its one-off removed, and the data said
	// partial.
	write(t, s.log+".shop", "FAKE_ONEOFF=db\n", 0o644)
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\ncat >/dev/null\nexit 4\n", 0o755)
	if _, errs, code := s.runIO(append(env, "FAKE_DB=stream"), []byte("rows\n"), "--import", "db"); code == 0 || !strings.Contains(errs, "the import of db did not complete: its data may be partial") {
		t.Errorf("a failing import of a running stream (%d):\n%s", code, errs)
	}
	if docker, _ := s.logs(); !strings.Contains(docker, "rm -f shop-db-run-1") || !strings.Contains(docker, " stop db") || strings.Index(docker, " start db") < strings.Index(docker, " stop db") {
		t.Errorf("a failing import of a running stream, docker:\n%s", docker)
	}
	write(t, s.log+".shop", "", 0o644)
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\n{ echo \"import $1 into $COMPOSE_PROJECT_NAME\"; cat; } > \"$FAKE_IMPORTED\"\n", 0o755)

	// A stream down before its import is stopped again after it.
	if _, errs, code := s.runIO(append(env, "FAKE_DB=stream", "FAKE_DB_DOWN=1"), []byte("rows\n"), "--import", "db"); code != 0 || !strings.Contains(errs, "devopsy: stopping db again: it was down before") {
		t.Errorf("import of a stream that was down (%d):\n%s", code, errs)
	}
	if docker, _ := s.logs(); !strings.Contains(docker, "up -d --wait db") || strings.Index(docker, " stop db") < strings.Index(docker, "up -d --wait db") {
		t.Errorf("down stream import:\n%s", docker)
	}

	// A tar cut right after a member lists fine, but has no end blocks:
	// refused, nothing emptied.
	two := tarOf(t, map[string]string{"storage/a.txt": "aaaa", "storage/b.txt": "bbbb"})
	if _, errs, code := s.runIO(nil, two[:1024], "--import", "app"); code == 0 || !strings.Contains(errs, "no end-of-archive blocks") || readString(filepath.Join(mnt, "storage", "new.txt")) != "new\n" {
		t.Errorf("tar cut at a member (%d):\n%s", code, errs)
	}
	// The same cut after a member whose data ends in zeros (a sparse file,
	// a database's preallocated pages): its last 1024 bytes are no
	// end-of-archive blocks.
	zeros := tarOf(t, map[string]string{"storage/a.img": strings.Repeat("\x00", 4096), "storage/b.txt": "bbbb"})
	if _, errs, code := s.runIO(nil, zeros[:512+4096], "--import", "app"); code == 0 || readString(filepath.Join(mnt, "storage", "new.txt")) != "new\n" {
		t.Errorf("tar cut after a member ending in zeros (%d):\n%s", code, errs)
	}
	// A tar starting with a pax global header (git archive writes one,
	// with the commit) carries storage alone: imported, as tar extracts
	// it.
	//
	// Bug: tarEntries takes the global header for a member, an entry
	// named pax_global_header that app does not mount, and refuses the
	// tar, which tar -t (it lists no global header) and tar -x take.
	var global bytes.Buffer
	gw := tar.NewWriter(&global)
	if err := gw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": "0123456789abcdef"}}); err != nil {
		t.Fatal(err)
	}
	if err := gw.WriteHeader(&tar.Header{Name: "storage/new.txt", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := gw.Write([]byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, errs, code := s.runIO(nil, global.Bytes(), "--import", "app"); code != 0 || readString(filepath.Join(mnt, "storage", "new.txt")) != "new\n" {
		t.Errorf("a tar with a pax global header (%d):\n%s", code, errs)
	}
	s.logs()

	// On a target: the same, in its current release, under its release
	// lock, shared for an export, its own for an import.
	s.releaseProd(nil)
	write(t, s.log+".shop-prod", "FAKE_DB=stream\n", 0o644)
	out, errs, code = s.runIO(env, nil, "@prod", "--export", "db")
	docker, _ = s.logs()
	if code != 0 || string(out) != "dump of db from shop-prod stream=db stop=\n" || !strings.Contains(docker, "flock -n -s 9\nflock -n 8\n") {
		t.Fatalf("@prod --export (%d): %q\n%s\n%s", code, out, errs, docker)
	}
	// An import asks for the environment's name, on the terminal: without
	// one, --yes.
	os.Remove(imported)
	if _, errs, code := s.runIO(env, []byte("rows\n"), "@prod", "--import", "db"); code == 0 || !strings.Contains(errs, "devopsy: error: nothing imported: no terminal to confirm it on\ndevopsy: | this replaces db's data on @devopsy@one:prod (devopsy@one:shop/prod)") || !strings.Contains(errs, "add --yes to go on without asking") || readString(imported) != "" {
		t.Errorf("@prod --import without --yes (%d):\n%s", code, errs)
	}
	if _, errs, code := s.runIO(env, []byte("rows\n"), "@prod", "--import", "db", "--yes"); code != 0 || readString(imported) != "import db into shop-prod\nrows\n" {
		t.Fatalf("@prod --import (%d): %q\n%s", code, readString(imported), errs)
	}
	if docker, _ := s.logs(); !strings.Contains(docker, "flock -n 9\n") || strings.Contains(docker, "flock -n -s 9") {
		t.Errorf("@prod --import lock:\n%s", docker)
	}
	if m := spools(filepath.Join(s.root, "shop", "prod")); len(m) > 0 {
		t.Errorf("spools left on the server: %v", m)
	}
	// The server's side only imports what ends with the end line: a cut
	// session reaches it as a plain EOF.
	os.Remove(imported)
	prod := filepath.Join(s.root, "shop", "prod", "current")
	for stdin, want := range map[string]string{"rows\n": "the data stopped before its end", "rows\nEND": "", "END": "nothing on stdin"} {
		cmd := exec.Command(binary, "--import", "db", "--yes")
		cmd.Dir = prod
		cmd.Env = append(append([]string{}, s.env...), append(env, "PWD="+prod, "DEVOPSY_INTERNAL_IMPORT_END=END")...)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if want == "" {
			if err != nil || readString(imported) != "import db into shop-prod\nrows\n" {
				t.Errorf("complete, end line taken off: %v %q\n%s", err, readString(imported), out)
			}
		} else if err == nil || !strings.Contains(string(out), want) {
			t.Errorf("%q: %v\n%s", stdin, err, out)
		}
	}
	s.logs()
	// The laptop case: prod's files into the local project.
	out, errs, code = s.runIO(nil, nil, "@prod", "--export", "app")
	if code != 0 {
		t.Fatalf("@prod --export app (%d):\n%s", code, errs)
	}
	if _, errs, code := s.runIO(nil, out, "--import", "app"); code != 0 || readString(filepath.Join(mnt, "storage", "file.txt")) != "from prod\n" {
		t.Errorf("prod's storage locally (%d):\n%s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(mnt, "storage", "new.txt")); err == nil {
		t.Errorf("the local entry was not replaced")
	}
	os.Remove(s.log + ".shop-prod")
}

// waitFile waits for a file to exist, up to 10 seconds.
func waitFile(t *testing.T, path, what string, out func() string) {
	t.Helper()
	for start := time.Now(); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Since(start) > 10*time.Second {
			t.Fatalf("%s never happened:\n%s", what, out())
		}
	}
}

// A prepare that fails before anything stops restores nothing: a db down
// before the sync stays down, whether the hook fails or the import check
// does; and a stop=true service without a container is only created on a
// first release (no containers at all), never in a project that has
// others.
func TestSyncPrepareFailsKeepsDown(t *testing.T) {
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "capabilities", "sync", "prepare"), "#!/bin/sh\n[ \"${FAIL_PREPARE:-}\" != \"$1\" ]\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()
	dev := "shop-dev compose -f " + filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")
	for _, c := range []struct{ name, labels string }{{"prepare hook", ""}, {"no import", "FAKE_DB=stream+stop"}} {
		s.labels(c.labels)
		out, code := s.run([]string{"FAIL_PREPARE=child", "FAKE_DB_DOWN=1"}, "@dev", "--sync", "--yes")
		docker, _ := s.logs()
		if code == 0 || strings.Contains(docker, dev+" start db") || strings.Contains(docker, dev+" stop") {
			t.Errorf("%s (%d):\n%s\n%s", c.name, code, out, docker)
		}
	}
	s.labels("")
	// finish with a record, db without a container, app with one: db is
	// not created.
	write(t, filepath.Join(s.root, "shop", "dev", ".devopsy-sync.services"), "", 0o644)
	cmd := exec.Command(binary, "--sync-services", "finish", "go")
	cmd.Dir = filepath.Join(s.root, "shop", "dev", "current")
	cmd.Env = append(append([]string{}, s.env...), "COMPOSE_PROJECT_NAME=shop-dev", "PWD="+cmd.Dir, "FAKE_ABSENT=db")
	if out, err := cmd.CombinedOutput(); err != nil || strings.Contains(string(out), "creating") {
		t.Errorf("finish, db absent among others: %v\n%s", err, out)
	}
	if docker, _ := s.logs(); strings.Contains(docker, "up -d --wait db") {
		t.Errorf("a removed db created:\n%s", docker)
	}
}

// A record naming a service the project no longer has (stopped by a sync
// that could not bring it back, then dropped from compose.yaml, its
// container left exited) fails nothing: what a sync stopped comes back
// when the project still has it.
//
// Bug: restoreServices starts every service the record names, and
// Compose refuses one the project does not have (no such service): the
// sync fails, the record stays, and every later sync fails the same way.
// It should leave out what the project no longer has.
func TestSyncRecordedStopGone(t *testing.T) {
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	record := filepath.Join(s.root, "shop", "dev", syncState)
	write(t, record, "stop worker\n", 0o644)
	write(t, s.log+".shop-dev", "FAKE_ORPHANS=worker\n", 0o644)
	defer os.Remove(s.log + ".shop-dev")
	s.logs()
	out, code := s.run(nil, "@dev", "--sync", "--yes")
	if code != 0 || fileExists(record) {
		t.Errorf("a sync whose record names a service gone (%d), record left %v:\n%s", code, fileExists(record), out)
	}
}

// A service labeled devopsy.sync.stop=true stays down on the child for the
// whole sync, and after a copy that did not complete: a stream's import
// brings its service up without what it depends on, which Compose would
// start too (here db, stopped for the sync). The same on a first release
// that finds containers (a first one that failed left them running): db,
// stopped by its sync, stays down.
//
// Bug: on a first release the import brings app up with what it depends
// on (no --no-deps: a first release is taken for one with nothing
// stopped), so Compose starts db, which this sync stopped, and it runs
// on the partial copy. It should stay down, as on a later sync.
func TestSyncImportUpKeepsStoppedDown(t *testing.T) {
	for _, first := range []bool{false, true} {
		s := newSyncSetup(t)
		caps := filepath.Join(s.dot, "capabilities", "sync")
		write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho dump\n", 0o755)
		write(t, filepath.Join(caps, "import"), "#!/bin/sh\ncat >/dev/null\n[ ! -e \"$DOCKER_LOG.failimport.$1\" ]\n", 0o755)
		s.releaseProd(nil)
		args := []string{"@dev", "--release"}
		if !first {
			if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
				t.Fatalf("release (%d):\n%s", code, out)
			}
			args = []string{"@dev", "--sync", "--yes"}
		}
		s.labels("FAKE_APP=stream\nFAKE_DB=stream+stop\nFAKE_UP_DEPS=1")
		write(t, s.log+".failimport.app", "", 0o644)
		s.logs()
		out, code := s.run(nil, args...)
		if code == 0 || !strings.Contains(out, "db stay down") || !fileExists(filepath.Join(s.root, "shop", "dev", remote.SyncIncompleteFile)) {
			t.Fatalf("first release %v: app's import fails after db's (%d):\n%s", first, code, out)
		}
		if stopped := readString(s.log + ".stopped.shop-dev"); !slices.Contains(strings.Fields(stopped), "db") {
			t.Errorf("first release %v: db, stopped for the sync, is up on a partial copy:\n%s", first, out)
		}
		s.labels("")
	}
}

// Across servers, a sync cancelled during a stream's import stops the
// import on the child, though its session outlives devopsy's ssh: the
// pipe the push keeps open as its stdin closes, and the import ends its
// hook and removes the spool.
func TestSyncImportCancelled(t *testing.T) {
	s := newSyncSetup(t)
	tmp := t.TempDir()
	started, stopped := filepath.Join(tmp, "started"), filepath.Join(tmp, "stopped")
	caps := filepath.Join(s.dot, "capabilities", "sync")
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho dump\n", 0o755)
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\ntrap 'touch \"$FAKE_STOPPED\"; exit 1' TERM\ntouch \"$FAKE_STARTED\"\nsleep 20 &\nwait\n", 0o755)
	s.releaseProd(nil)
	env := []string{"DEVOPSY_SERVER_PROD=devopsy@one", "SSH_AUTH_SOCK=/tmp/agent.sock"}
	if out, code := s.run(env, "@devopsy@two:dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()
	s.labels("FAKE_DB=stream")
	defer s.labels("")

	outFile, err := os.CreateTemp(tmp, "out")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "@devopsy@two:dev", "--sync", "--yes")
	cmd.Dir = s.project
	cmd.Env = append(append(append([]string{}, s.env...), env...), "FAKE_STARTED="+started, "FAKE_STOPPED="+stopped, "FAKE_SSH_DETACH=1", "FAKE_IMPORT_DETACH=1")
	cmd.Stdin = strings.NewReader("")
	cmd.Stdout, cmd.Stderr = outFile, outFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	out := func() string { return readString(outFile.Name()) + readString(s.log) }
	waitFile(t, started, "the import", out)
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	waitFile(t, stopped, "the import's end", out)
	for start := time.Now(); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(s.root, "shop", "dev", ".devopsy-sync.stream.db")); err != nil {
			break
		}
		if time.Since(start) > 10*time.Second {
			t.Fatalf("the spool stays:\n%s", out())
		}
	}
}

// A child's first release with a stream and an rsync service left
// running: the stream's import keeps its new container up, and finish
// creates every labeled service before its hook, which sanitizes through
// them; the record of the first release goes afterwards.
func TestSyncFirstReleaseCreates(t *testing.T) {
	s := newSyncSetup(t)
	caps := filepath.Join(s.dot, "capabilities", "sync")
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho dump\n", 0o755)
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\ncat >/dev/null\n", 0o755)
	write(t, filepath.Join(caps, "finish"), "#!/bin/sh\necho \"finish $1 $2: $(devopsy ps --services --status running | tr '\\n' ' ')\"\n", 0o755)
	s.releaseProd(nil)
	s.labels("FAKE_DB=stream")
	defer s.labels("")
	write(t, s.log+".shop-dev", "FAKE_DB=stream\nFAKE_ABSENT='app db'\n", 0o644)
	out, code := s.run(nil, "@dev", "--release")
	docker, _ := s.logs()
	dev := "shop-dev compose -f " + filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")
	if code != 0 {
		t.Fatalf("first release (%d):\n%s", code, out)
	}
	rest := docker
	for _, want := range []string{dev + " up -d --wait db\n", dev + " up -d --wait app db\n", dev + " ps --services --status running"} {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("missing %q in order:\n%s\n%s", want, out, docker)
		}
		rest = rest[i+len(want):]
	}
	if strings.Contains(docker, dev+" stop") || !strings.Contains(out, "devopsy: creating app db") || strings.Index(out, "devopsy: creating app db") > strings.Index(out, "finish child go") {
		t.Errorf("first release:\n%s\n%s", out, docker)
	}
	if _, err := os.Stat(filepath.Join(s.root, "shop", "dev", ".devopsy-sync.services")); err == nil {
		t.Errorf("the record stays")
	}
}

// A child's first release whose stream import failed leaves the stream's
// container up (kept for the first release), and nothing current: the
// next release is a first one again and must create every labeled service
// before finish child go, which sanitizes through them, though a
// container is there already.
func TestSyncFirstReleaseRetryCreates(t *testing.T) {
	s := newSyncSetup(t)
	caps := filepath.Join(s.dot, "capabilities", "sync")
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho dump\n", 0o755)
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\ncat >/dev/null\n[ ! -e \"$DOCKER_LOG.failimport\" ]\n", 0o755)
	write(t, filepath.Join(caps, "finish"), "#!/bin/sh\necho \"finish $1 $2: $(devopsy ps --services --status running | tr '\\n' ' ')\"\n", 0o755)
	s.releaseProd(nil)
	s.labels("FAKE_DB=stream")
	defer s.labels("")
	write(t, s.log+".shop-dev", "FAKE_DB=stream\nFAKE_ABSENT='app db'\n", 0o644)
	write(t, s.log+".failimport", "", 0o644)
	if out, code := s.run(nil, "@dev", "--release"); code == 0 || s.current("dev") != "" || !strings.Contains(out, "no previous release to go back to") {
		t.Fatalf("a first release whose import fails (%d):\n%s", code, out)
	}
	os.Remove(s.log + ".failimport")
	s.logs()
	next()
	out, code := s.run(nil, "@dev", "--release")
	if code != 0 || s.current("dev") == "" {
		t.Fatalf("the first release again (%d):\n%s", code, out)
	}
	if !strings.Contains(out, "finish child go: app db \n") {
		t.Errorf("finish child go ran without every labeled service up:\n%s", out)
	}
}

// A label from an inherited variable follows the parent's in the same
// sync: the re-sync writes the parent's variables into the child's
// pending file before its holder reads the labels, and moves it into
// shared/inherit.env on go. A failed copy leaves the child's file; a
// stale pending file, from a sync that wrote none this time, goes.
func TestSyncInheritedLabel(t *testing.T) {
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "config.yaml"), "project: shop\ndefaults:\n  mode: image\n  release: deploy\nenvironments:\n  prod: {inherit: [FAKE_DB]}\n  dev: {parent: prod}\n  solo: {}\n", 0o644)
	caps := filepath.Join(s.dot, "capabilities", "sync")
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho dump\n", 0o755)
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\necho \"import $1: $(cat)\"\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	childInherit := filepath.Join(s.root, "shop", "dev", "shared", "inherit.env")
	pending := filepath.Join(s.root, "shop", "dev", ".devopsy-sync.inherit.env")

	// The parent flips its database to stream: one --sync, no --inherit.
	write(t, filepath.Join(s.root, "shop", "prod", "shared", ".env"), "FAKE_DB=stream\n", 0o600)
	s.logs()
	out, code := s.run(nil, "@dev", "--sync", "--yes")
	if code != 0 || !strings.Contains(out, "import db: dump") || readString(childInherit) != "FAKE_DB='stream'\n" {
		t.Fatalf("flipped label (%d): %q\n%s", code, readString(childInherit), out)
	}
	if _, err := os.Stat(pending); err == nil {
		t.Errorf("the pending file stays")
	}

	// Back to rsync, and the copy fails: the child keeps stream.
	write(t, filepath.Join(s.root, "shop", "prod", "shared", ".env"), "FAKE_DB=rsync+stop\n", 0o600)
	if out, code := s.run([]string{"FAIL_RSYNC=23"}, "@dev", "--sync", "--yes"); code == 0 || readString(childInherit) != "FAKE_DB='stream'\n" {
		t.Errorf("failed copy (%d): %q\n%s", code, readString(childInherit), out)
	}
	// It left a partial copy, which a sync that completes clears.
	if out, code := s.run(nil, "@dev", "--sync", "--yes"); code != 0 {
		t.Fatalf("a complete sync (%d):\n%s", code, out)
	}

	// From solo, which inherits nothing: a stale pending file goes before
	// the child reads its labels (it would make db a stream here).
	if out, code := s.run(nil, "@solo", "--release"); code != 0 {
		t.Fatalf("solo (%d):\n%s", code, out)
	}
	write(t, filepath.Join(s.root, "shop", "dev", "shared", ".env"), "FAKE_DB=rsync+stop\n", 0o600)
	write(t, pending, "FAKE_DB='garbage'\n", 0o600)
	if out, code := s.run(nil, "@dev", "--sync", "--from", "solo", "--yes"); code != 0 || strings.Contains(out, "garbage") {
		t.Errorf("stale pending (%d):\n%s", code, out)
	}
	if _, err := os.Stat(pending); err == nil {
		t.Errorf("the stale pending file stays")
	}

	// --sync-exec gives the child's devopsy the pending file, on both
	// topologies, from the child's directory the push knows.
	show := filepath.Join(s.dot, "commands", "inheritfile")
	write(t, show, "#!/bin/sh\necho \"file=$DEVOPSY_INTERNAL_INHERIT_FILE\"\n", 0o755)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev with the command (%d):\n%s", code, out)
	}
	base := filepath.Join(s.root, "shop", "dev")
	push := []string{"DEVOPSY_SYNC_TO_DIR=" + filepath.Join(base, "current", ".devopsy"), "DEVOPSY_SYNC_TO_PROJECT=shop-dev", "DEVOPSY_SYNC_TO_BASE=" + base}
	across := append([]string{"DEVOPSY_SYNC_TO=devopsy@two", "DEVOPSY_SYNC_SSH_PORT=22", "SSH_AUTH_SOCK=/tmp/agent.sock"}, push...)
	for name, env := range map[string][]string{"same server": push, "across servers": across} {
		if out, code := s.run(env, "--sync-exec", "inheritfile"); code != 0 || !strings.Contains(out, "file="+pending+"\n") {
			t.Errorf("--sync-exec, %s (%d):\n%s", name, code, out)
		}
	}
}

// A sync that finds containers drops a first release's mark an earlier,
// failed sync left; its stops stay.
func TestSyncRecordStaleFirst(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "current")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(filepath.Dir(cwd), syncState), "first\nstop db\n", 0o644)
	if err := startSyncState(cwd, false); err != nil {
		t.Fatal(err)
	}
	rec, ok := readSyncRecord(cwd)
	if !ok || rec.first || len(rec.stops) != 1 || rec.stops[0] != "db" {
		t.Errorf("record: %+v %v", rec, ok)
	}
}

// Without a terminal devopsy takes Ctrl-C (here SIGTERM) itself: before
// the switch it ends the watched session, which cancels the release;
// during the run step it says the step goes on, and the release finishes.
func TestReleaseInterrupt(t *testing.T) {
	s := newSyncSetup(t)
	interrupt := func(env string, args ...string) (string, int, time.Duration) {
		t.Helper()
		started := filepath.Join(t.TempDir(), "started")
		outFile, err := os.CreateTemp(t.TempDir(), "out")
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, args...)
		cmd.Dir = s.project
		cmd.Env = append(append([]string{}, s.env...), env+"="+started)
		cmd.Stdin = strings.NewReader("")
		cmd.Stdout, cmd.Stderr = outFile, outFile
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		for i := 0; ; i++ {
			if _, err := os.Stat(started); err == nil {
				break
			}
			if i > 300 {
				_ = cmd.Process.Kill()
				t.Fatalf("never started:\n%s", readString(outFile.Name()))
			}
			time.Sleep(50 * time.Millisecond)
		}
		start := time.Now()
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
		return readString(outFile.Name()), cmd.ProcessState.ExitCode(), time.Since(start)
	}
	out, code, took := interrupt("FAKE_SLOW_PREPARE", "@slow", "--release", "--yes")
	if code != 130 || took > 15*time.Second || !strings.Contains(out, "interrupted: cancelling the release on @devopsy@one:slow") || s.current("slow") != "" {
		t.Errorf("before the switch (%d, %s), current %q:\n%s", code, took, s.current("slow"), out)
	}
	// The switch is known from the server's log marker, which needs GNU
	// tail, as on every server (macOS here has none).
	if exec.Command("/bin/sh", "-c", "PATH=/usr/bin:/bin tail --version").Run() != nil {
		return
	}
	next()
	out, code, _ = interrupt("FAKE_SLOW_RUN", "@slow", "--release", "--yes")
	if code != 0 || !strings.Contains(out, "the release goes on on @devopsy@one:slow") || !strings.Contains(out, "deployed slowly") || s.current("slow") == "" {
		t.Errorf("during the run step (%d), current %q:\n%s", code, s.current("slow"), out)
	}
}

// A sync whose holder never decided (killed) leaves its files: whatever
// takes the lock next counts the data as a partial copy, which only a
// sync completes. A partial copy is never a parent.
func TestSyncSettle(t *testing.T) {
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "capabilities", "sync", "finish"), "#!/bin/sh\n[ \"${FAIL_FINISH:-}\" != \"$1 $2\" ]\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	dev, prod := filepath.Join(s.root, "shop", "dev"), filepath.Join(s.root, "shop", "prod")
	write(t, filepath.Join(dev, remote.SyncStartedFile), "", 0o644)
	write(t, filepath.Join(dev, remote.SyncOpenFile), "devopsy@one:prod 1.2\n", 0o644)
	next()
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code == 0 || !strings.Contains(out, "did not complete") {
		t.Errorf("a release after an undecided sync (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--rollback"); code == 0 || !strings.Contains(out, "left its files") || !fileExists(filepath.Join(dev, remote.SyncIncompleteFile)) || fileExists(filepath.Join(dev, remote.SyncStartedFile)) || fileExists(filepath.Join(dev, remote.SyncOpenFile)) {
		t.Errorf("a rollback after an undecided sync (%d):\n%s", code, out)
	}
	if _, _, code, _ := remote.ParseIncomplete(readString(filepath.Join(dev, remote.SyncIncompleteFile))); code != remote.IncompleteSettled {
		t.Errorf("the mark of an undecided sync: %q", readString(filepath.Join(dev, remote.SyncIncompleteFile)))
	}

	// A partial copy is no parent.
	write(t, filepath.Join(prod, remote.SyncIncompleteFile), remote.IncompleteLine("2026-10-18 00:00:00", "x", remote.IncompleteCopy, "cut"), 0o644)
	if out, code := s.run(nil, "@dev", "--sync", "--yes"); code == 0 || !strings.Contains(out, "did not complete") {
		t.Errorf("a sync from a partial copy (%d):\n%s", code, out)
	}
	os.Remove(filepath.Join(prod, remote.SyncIncompleteFile))

	// A sync completes with its finish: one the project's finish child go
	// refuses marks the child, saying so, and nothing runs on it.
	if out, code := s.run([]string{"FAIL_FINISH=child go"}, "@dev", "--sync", "--yes"); code == 0 || !fileExists(filepath.Join(dev, remote.SyncIncompleteFile)) {
		t.Errorf("a failing finish child go (%d):\n%s", code, out)
	}
	if _, from, code, why := remote.ParseIncomplete(readString(filepath.Join(dev, remote.SyncIncompleteFile))); code != remote.IncompleteFinish || from != "devopsy@one:prod" || !strings.Contains(why, "finish child go failed") {
		t.Errorf("the mark of a refused copy: %q", readString(filepath.Join(dev, remote.SyncIncompleteFile)))
	}
	next()
	if out, code := s.run(nil, "@dev", "--rollback"); code == 0 || !strings.Contains(out, "the copy completed, but the project's sync capability refused it: finish child go failed") {
		t.Errorf("a rollback after a refused copy (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--status"); code != 0 || !strings.Contains(out, "from a sync that did not complete: the copy completed, but the project's sync capability refused it") || !strings.Contains(out, "from @devopsy@one:prod)") {
		t.Errorf("--status after a refused copy (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--sync", "--yes"); code != 0 || fileExists(filepath.Join(dev, remote.SyncIncompleteFile)) {
		t.Fatalf("a sync completes it (%d):\n%s", code, out)
	}
	next()
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Errorf("a release after (%d):\n%s", code, out)
	}
}

// The sync log, appended to by every sync whose session went, keeps its
// last lines once too big.
func TestTrimSyncLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), syncLog)
	line := strings.Repeat("x", 99) + "\n"
	write(t, path, strings.Repeat(line, remote.SyncLogMax/len(line)+10)+"last\n", 0o644)
	trimSyncLog(path)
	data := readString(path)
	if n := strings.Count(data, "\n"); n != remote.SyncLogKeep || !strings.HasSuffix(data, "last\n") {
		t.Errorf("%d lines, ends %q", n, data[len(data)-10:])
	}
	write(t, path, "small\n", 0o644)
	if trimSyncLog(path); readString(path) != "small\n" {
		t.Errorf("a small log changed: %q", readString(path))
	}
}

// A write into the child claims the holder's gate first: once the holder
// closed it (deciding), nothing is written into what it restored.
func TestClaimWrite(t *testing.T) {
	base := t.TempDir()
	t.Setenv(remote.SyncIDEnv, "100.7")
	if err := claimWrite(base); err == nil {
		t.Error("claimed with the gate closed")
	}
	// Another sync's gate (a write left over from an earlier one).
	write(t, filepath.Join(base, remote.SyncOpenFile), "vm1:prod 200.8\n", 0o644)
	if err := claimWrite(base); err == nil || fileExists(filepath.Join(base, remote.SyncStartedFile)) {
		t.Errorf("claimed another sync's gate: %v", err)
	}
	write(t, filepath.Join(base, remote.SyncOpenFile), "vm1:prod 100.7\n", 0o644)
	if err := claimWrite(base); err != nil || fileExists(filepath.Join(base, remote.SyncOpenFile)) || readString(filepath.Join(base, remote.SyncStartedFile)) != "vm1:prod 100.7\n" {
		t.Errorf("first claim: %v", err)
	}
	// Later writes of the same sync.
	if err := claimWrite(base); err != nil {
		t.Errorf("second claim: %v", err)
	}
	// Writes record themselves in it; once the holder took it, a late one
	// is refused and leaves nothing.
	if err := recordWrite(base, remote.DoneLine("100.7")); err != nil || !strings.HasSuffix(readString(filepath.Join(base, remote.SyncStartedFile)), "\ndone 100.7\n") {
		t.Errorf("record: %v %q", err, readString(filepath.Join(base, remote.SyncStartedFile)))
	}
	if err := os.Rename(filepath.Join(base, remote.SyncStartedFile), filepath.Join(base, remote.SyncTakenFile)); err != nil {
		t.Fatal(err)
	}
	if err := recordWrite(base, remote.DoneLine("100.7")); !errors.Is(err, errDecided) || fileExists(filepath.Join(base, remote.SyncStartedFile)) {
		t.Errorf("a write after the take: %v", err)
	}
	if err := os.Rename(filepath.Join(base, remote.SyncTakenFile), filepath.Join(base, remote.SyncStartedFile)); err != nil {
		t.Fatal(err)
	}
	// The mark names the parent that wrote, and why; a later one keeps
	// its time and says its own why, unless told to keep it.
	mark := filepath.Join(base, remote.SyncIncompleteFile)
	write(t, mark, remote.IncompleteLine("2026-10-01 00:00:00", "x", remote.IncompleteCopy, "cut"), 0o644)
	if err := markIncomplete(base, remote.IncompleteFinish, "refused", true); err != nil || readString(mark) != "2026-10-01 00:00:00\tx\tcopy\tcut\n" {
		t.Errorf("kept mark %q (%v)", readString(mark), err)
	}
	if err := markIncomplete(base, remote.IncompleteFinish, "refused", false); err != nil || readString(mark) != "2026-10-01 00:00:00\tvm1:prod\tfinish\trefused\n" {
		t.Errorf("mark %q (%v)", readString(mark), err)
	}
}

// A push whose child decided already (its gate closed: the holder gone
// on, a push that outlived its connection) writes nothing into it.
func TestSyncPushGateClosed(t *testing.T) {
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()
	base := filepath.Join(s.root, "shop", "dev")
	// The push runs in the parent's project: here, the checkout's.
	for _, d := range []string{"db", "storage"} {
		if err := os.MkdirAll(filepath.Join(s.dot, "mnt", d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"DEVOPSY_SYNC_TO_DIR=" + filepath.Join(base, "current", ".devopsy"), "DEVOPSY_SYNC_TO_PROJECT=shop-dev", "DEVOPSY_SYNC_TO_BASE=" + base, "DEVOPSY_SYNC_FROM_TARGET=devopsy@one:prod"}
	out, code := s.run(env, "--sync-push")
	docker, _ := s.logs()
	if code == 0 || !strings.Contains(out, "the child's side ended already") || strings.Contains(docker, "rsync -a") || fileExists(filepath.Join(base, remote.SyncStartedFile)) {
		t.Errorf("a push into a decided child (%d):\n%s\n%s", code, out, docker)
	}
}

// What every server-side reader of a session shares: through until its
// end, or a silence.
func TestReadQuiet(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var got []byte
	go func() {
		_, _ = w.Write([]byte("a"))
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte("b"))
	}()
	start := time.Now()
	silent, err := readQuiet(r, 300*time.Millisecond, func(b []byte) error { got = append(got, b...); return nil })
	if !silent || err != nil || string(got) != "ab" || time.Since(start) < 350*time.Millisecond {
		t.Errorf("silence: %v %v %q after %s", silent, err, got, time.Since(start))
	}
	w.Close()
	silent, err = readQuiet(strings.NewReader("c"), 0, func([]byte) error { return nil })
	if silent || err != nil {
		t.Errorf("an end: %v %v", silent, err)
	}
}

// A first release whose sync aborted before writing anything does not go
// live: its services are not created on the child's empty data.
func TestSyncFirstAbortCreatesNothing(t *testing.T) {
	s := newSyncSetup(t)
	s.releaseProd(nil)
	write(t, s.log+".shop-dev", "FAKE_ABSENT='app db'\n", 0o644)
	defer os.Remove(s.log + ".shop-dev")
	s.logs()
	out, code := s.run([]string{"FAIL_STOP=shop-prod"}, "@dev", "--release")
	docker, _ := s.logs()
	if code == 0 || s.current("dev") != "" || !strings.Contains(out, "before writing anything") {
		t.Fatalf("an aborted first sync (%d), current %q:\n%s", code, s.current("dev"), out)
	}
	if strings.Contains(out, "devopsy: creating") || strings.Contains(docker, "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" up -d --wait") {
		t.Errorf("services created on an aborted first release:\n%s\n%s", out, docker)
	}
}

// A partial copy ends whatever may still write into the child before
// anything else is decided on its data: the record the holder took names
// an import still running, whose devopsy is signalled (Linux's /proc says
// it is one), whose service is stopped (what docker exec started in it
// dies with it) and recorded, since it ran before the sync, and whose
// one-off is removed; and the sync's own containers go, never another
// sync's.
func TestSyncEndWriters(t *testing.T) {
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	dev := filepath.Join(s.root, "shop", "dev")
	// An import's devopsy, still running: what isSyncImport looks for is
	// in its arguments (": " keeps sh from exec'ing sleep).
	imp := exec.Command("sh", "-c", "sleep 30; :", "devopsy", "--sync-services", "import", "db")
	if err := imp.Start(); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { ended <- imp.Wait() }()
	defer func() { _ = imp.Process.Kill() }()
	write(t, filepath.Join(dev, remote.SyncTakenFile), "devopsy@one:prod 1.2\n"+remote.WriterLine("db", imp.Process.Pid, "running")+"\n", 0o644)
	write(t, s.log+".shop-dev", "FAKE_ONEOFF=db\nFAKE_SYNC_CONTAINERS='devopsy-sync-1.2-receive devopsy-sync-9.9-rsync-0'\n", 0o644)
	s.logs()
	cmd := exec.Command(binary, "--sync-services", "finish", "partial")
	cmd.Dir = filepath.Join(dev, "current")
	cmd.Env = append(append([]string{}, s.env...), "COMPOSE_PROJECT_NAME=shop-dev", "PWD="+cmd.Dir)
	out, _ := cmd.CombinedOutput()
	docker, _ := s.logs()
	for _, want := range []string{"rm -f devopsy-sync-1.2-receive", "rm -f shop-dev-db-run-1", " stop db"} {
		if !strings.Contains(docker, want) {
			t.Errorf("missing %q:\n%s\n%s", want, out, docker)
		}
	}
	if strings.Contains(docker, "devopsy-sync-9.9") {
		t.Errorf("another sync's container removed:\n%s", docker)
	}
	if rec := readString(filepath.Join(dev, ".devopsy-sync.services")); !strings.Contains(rec, "stop db") {
		t.Errorf("the stopped db not recorded: %q", rec)
	}
	if _, err := os.Stat("/proc/self/cmdline"); err == nil {
		select {
		case <-ended:
		case <-time.After(20 * time.Second):
			t.Errorf("the import still runs:\n%s", out)
		}
	}
}

// A sync hook runs in the foreground: what it leaves running in the
// background ends when it returns, said, and the hook still succeeds.
func TestSyncHookLeftovers(t *testing.T) {
	s := newSyncSetup(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	write(t, filepath.Join(s.dot, "capabilities", "sync", "finish"), "#!/bin/sh\n[ \"$1 $2\" = \"child go\" ] || exit 0\nsleep 30 >/dev/null 2>&1 &\necho $! >"+pidFile+"\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--sync", "--yes"); code != 0 || !strings.Contains(out, "left processes running: ending them") {
		t.Fatalf("a hook leaving a process (%d):\n%s", code, out)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(readString(pidFile)))
	if err != nil {
		t.Fatalf("pid: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the hook's background process %d still runs", pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
