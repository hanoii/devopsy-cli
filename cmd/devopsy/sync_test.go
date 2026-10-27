package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// syncFakeDocker logs every call, with its compose project, to
// $DOCKER_LOG, answers compose config and ps for a project whose app
// has data copied as volumes (devopsy.data=true) and db too, stopped own
// (devopsy.data.stop=own), app depending on db (FAKE_APP and FAKE_DB
// change them: mnt for volumes, stream for custom, keep for a copy of
// none, none for false, each with +<stop word>, - for no devopsy.data
// label, or none for no label at all;
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
// FAKE_APP_MOUNT2 is a second entry app mounts (/cache). FAKE_APP_OWNER
// is app's devopsy.mnt.owner, FAKE_DB_OWNER db's. devopsy-util is there
// and fresh unless FAKE_UTIL_* say otherwise (below).
const syncFakeDocker = `#!/bin/sh
# devopsy's own compose file (images.ComposeFile) is left out of the log:
# the tests here match the project's compose commands.
echo "${COMPOSE_PROJECT_NAME:-} $*" | sed 's# -f [^ ]*/devopsy-compose\.yaml##' >> "$DOCKER_LOG"
[ ! -f "$DOCKER_LOG.${COMPOSE_PROJECT_NAME:-}" ] || . "$DOCKER_LOG.${COMPOSE_PROJECT_NAME:-}"
if [ "$1" = compose ] && [ "$2" = -f ]; then dir=$(dirname "$3"); fi
# The compose calls made holding the release lock (fd 9, which the
# scripts open): what a devopsy killed outright leaves running holds it,
# so the next lock-taker waits for it.
[ "$1" != compose ] || [ ! -e /dev/fd/9 ] || echo "${COMPOSE_PROJECT_NAME:-} $*" | sed 's# -f [^ ]*##g' >> "$DOCKER_LOG.locked"
# $DOCKER_LOG.current.<compose project> sets them for the current
# release's code alone: a restore reads the restored release's too.
case ${dir:-} in */current/.devopsy) [ ! -f "$DOCKER_LOG.current.${COMPOSE_PROJECT_NAME:-}" ] || . "$DOCKER_LOG.current.${COMPOSE_PROJECT_NAME:-}" ;; esac
for a; do last2=${last:-}; last=$a; done
# FAKE_ABSENT names services without a container (a first release),
# until an up creates them and a down removes them again.
absent() {
  ! grep -qx "$1" "$DOCKER_LOG.up.${COMPOSE_PROJECT_NAME:-}" 2>/dev/null || return 1
  ! grep -qx "$1" "$DOCKER_LOG.down.${COMPOSE_PROJECT_NAME:-}" 2>/dev/null || return 0
  case " ${FAKE_ABSENT:-} " in *" $1 "*) return 0 ;; esac; return 1
}
# lab: <data>[+<stop>] as labels, - for no devopsy.data, none for no
# label at all. dl: a data word's: mnt (devopsy.data=true, its copy
# volumes), stream (true, custom), keep (true, none: no copy carries
# it), none (false); any other is devopsy.data's value as it is.
dl() {
  case $1 in
    mnt) echo '"devopsy.data":"true"' ;;
    stream) echo '"devopsy.data":"true","devopsy.data.copy":"custom"' ;;
    keep) echo '"devopsy.data":"true","devopsy.data.copy":"none"' ;;
    none) echo '"devopsy.data":"false"' ;;
    *) echo "\"devopsy.data\":\"$1\"" ;;
  esac
}
lab() {
  case $1 in
    none) echo '{}' ;;
    old) echo '{"devopsy.sync":"rsync"}' ;;
    -+*) echo "{\"devopsy.data.stop\":\"${1#-+}\"}" ;;
    *+*) echo "{$(dl "${1%%+*}"),\"devopsy.data.stop\":\"${1#*+}\"}" ;;
    *) echo "{$(dl "$1")}" ;;
  esac
}
stopped="$DOCKER_LOG.stopped.${COMPOSE_PROJECT_NAME:-}"
# FAKE_WORKER: a third service, worker, depending on db, labeled so
# (none for no label); FAKE_WORKER_PROFILE puts it in that profile.
svcs="app db${FAKE_WORKER:+ worker}"
worker=
wprofile=
[ -z "${FAKE_WORKER_PROFILE:-}" ] || wprofile=$(printf ',"profiles":["%s"]' "$FAKE_WORKER_PROFILE")
[ -z "${FAKE_WORKER:-}" ] || worker=$(printf ',"worker":{"labels":%s%s,"depends_on":{"db":{"condition":"service_started"}}}' "$(lab "$FAKE_WORKER")" "$wprofile")
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
# FAKE_APP_OWNER: app's devopsy.mnt.owner.
applab=$(lab "${FAKE_APP:-mnt}")
dblab=$(lab "${FAKE_DB:-mnt+own}")
# FAKE_DB_OWNER: db's.
[ -z "${FAKE_DB_OWNER:-}" ] || dblab="${dblab%\}},\"devopsy.mnt.owner\":\"$FAKE_DB_OWNER\"}"
if [ -n "${FAKE_APP_OWNER:-}" ]; then
  case $applab in '{}') applab="{\"devopsy.mnt.owner\":\"$FAKE_APP_OWNER\"}" ;; *) applab="${applab%\}},\"devopsy.mnt.owner\":\"$FAKE_APP_OWNER\"}" ;; esac
fi
# FAKE_APP_REFLINK: app's devopsy.data.copy.reflink.
if [ -n "${FAKE_APP_REFLINK:-}" ]; then
  case $applab in '{}') applab="{\"devopsy.data.copy.reflink\":\"$FAKE_APP_REFLINK\"}" ;; *) applab="${applab%\}},\"devopsy.data.copy.reflink\":\"$FAKE_APP_REFLINK\"}" ;; esac
fi
app2=
[ -z "${FAKE_APP_MOUNT2:-}" ] || app2=$(printf ',{"type":"bind","source":"%s/mnt%s"}' "${dir:-}" "$FAKE_APP_MOUNT2")
# FAKE_DB_VOLUME: a named volume db mounts, by key, the environment's own
# (<project>_<key>); FAKE_APP_VOLUME one app mounts. Volumes are
# directories of $DOCKER_LOG.vol, by name.
dbvol= appvol= topvols=
[ -z "${FAKE_DB_VOLUME:-}" ] || { dbvol=$(printf ',{"type":"volume","source":"%s"}' "$FAKE_DB_VOLUME"); topvols=$(printf '"%s":{"name":"%s_%s"}' "$FAKE_DB_VOLUME" "${COMPOSE_PROJECT_NAME:-}" "$FAKE_DB_VOLUME"); }
# FAKE_APP_BUILD: app has a build: section, its context the release's
# code, its argument APP_UID FAKE_APP_UID (10001 by default).
appbuild=
[ -z "${FAKE_APP_BUILD:-}" ] || appbuild=$(printf '"build":{"context":"%s/..","dockerfile":".devopsy/Dockerfile","args":{"APP_UID":"%s"}},' "${dir:-}" "${FAKE_APP_UID:-10001}")
# FAKE_DB_IMAGE: db's image: (none by default: compose names a build).
dbimg=
[ -z "${FAKE_DB_IMAGE:-}" ] || dbimg=$(printf '"image":"%s",' "$FAKE_DB_IMAGE")
# FAKE_DB_NO_BIND: db mounts no entry of mnt (its data inside its
# container: an anonymous volume its image declares, say).
dbmounts=$(printf '{"type":"bind","source":"%s/mnt/db"}%s' "${dir:-}" "$dbvol")
[ -z "${FAKE_DB_NO_BIND:-}" ] || dbmounts=${dbvol#,}
[ -z "${FAKE_APP_VOLUME:-}" ] || { appvol=$(printf ',{"type":"volume","source":"%s"}' "$FAKE_APP_VOLUME"); [ "$FAKE_APP_VOLUME" = "${FAKE_DB_VOLUME:-}" ] || topvols="${topvols:+$topvols,}$(printf '"%s":{"name":"%s_%s"}' "$FAKE_APP_VOLUME" "${COMPOSE_PROJECT_NAME:-}" "$FAKE_APP_VOLUME")"; }
vols="$DOCKER_LOG.vol"
# FAKE_RUN_WAIT: a docker run whose arguments hold $FAKE_RUN_WAIT_ON (a
# long check, copy or tar) takes until $FAKE_RUN_WAIT.go exists, or its
# client is killed.
if [ -n "${FAKE_RUN_WAIT:-}" ]; then case "$*" in "run "*"$FAKE_RUN_WAIT_ON"*) : >"$FAKE_RUN_WAIT"; n=0; while [ ! -e "$FAKE_RUN_WAIT.go" ] && [ $n -lt 200 ]; do sleep 0.1; n=$((n + 1)); done ;; esac; fi
case "$*" in
  *" config --format json")
    printf '{"name":"%s","volumes":{%s},"services":{"app":{%s"labels":%s,"depends_on":{"db":{"condition":"service_started"}},"volumes":[{"type":"bind","source":"%s/mnt%s"}%s%s]},"db":{%s"labels":%s,"restart":"%s","volumes":[%s]}%s},"networks":{"default":{"name":"%s_default"},"traefik":{"name":"traefik","external":true}}}\n' "${COMPOSE_PROJECT_NAME:-}" "$topvols" "$appbuild" "$applab" "$dir" "${FAKE_APP_MOUNT-/storage}" "$app2" "$appvol" "$dbimg" "$dblab" "${FAKE_DB_RESTART:-unless-stopped}" "$dbmounts" "$worker" "${COMPOSE_PROJECT_NAME:-}" ;;
  "volume inspect --format {{.Name}} "*)
    [ -d "$vols/$5" ] || { echo "Error: No such volume: $5" >&2; exit 1; }
    echo "$5" ;;
  "volume create "*) mkdir -p "$vols/$last" ;;
  # FAKE_NETWORK_OWNER: every network exists and is that project's.
  "network inspect "*) [ -n "${FAKE_NETWORK_OWNER:-}" ] || exit 1; echo "$FAKE_NETWORK_OWNER" ;;
  # down removes every container, until an up creates them again.
  *" down "*)
    rm -f "$DOCKER_LOG.up.${COMPOSE_PROJECT_NAME:-}"; for s in $svcs; do echo "$s"; done > "$DOCKER_LOG.down.${COMPOSE_PROJECT_NAME:-}"
    case "$* " in *" --volumes "*) rm -rf "$vols/${COMPOSE_PROJECT_NAME:-}"_* ;; esac ;;
  *" stop "*)
    # FAKE_STOP_WAIT: a stop that writes its caller's pid there and takes
    # until $FAKE_STOP_WAIT.go exists.
    # FAKE_STOPPED_WAIT: the same once the services are stopped (a
    # devopsy killed then: its orphaned stop never races the settle).
    if [ -n "${FAKE_STOPPED_WAIT:-}" ]; then after stop "$@" >> "$stopped"; echo $PPID >"$FAKE_STOPPED_WAIT"; n=0; while [ ! -e "$FAKE_STOPPED_WAIT.go" ] && [ $n -lt 200 ]; do sleep 0.1; n=$((n + 1)); done; exit 0; fi
    if [ -n "${FAKE_STOP_WAIT:-}" ]; then echo $PPID >"$FAKE_STOP_WAIT"; n=0; while [ ! -e "$FAKE_STOP_WAIT.go" ] && [ $n -lt 200 ]; do sleep 0.1; n=$((n + 1)); done; fi
    after stop "$@" >> "$stopped"
    # FAIL_STOP: a stop that fails partway, its services stopped.
    [ "${FAIL_STOP:-}" != "${COMPOSE_PROJECT_NAME:-}" ] || exit 1 ;;
  *" start "*)
    # FAKE_START_WAIT: a start that takes until $FAKE_START_WAIT.go exists.
    if [ -n "${FAKE_START_WAIT:-}" ]; then : >"$FAKE_START_WAIT"; n=0; while [ ! -e "$FAKE_START_WAIT.go" ] && [ $n -lt 200 ]; do sleep 0.1; n=$((n + 1)); done; fi
    # FAKE_KILL_START: the devopsy starting them killed outright (SIGKILL,
    # the OOM killer) as they start.
    [ -z "${FAKE_KILL_START:-}" ] || { kill -KILL "$PPID"; exit 137; }
    # FAIL_START: a start that fails, its services left stopped.
    [ "${FAIL_START:-}" != "${COMPOSE_PROJECT_NAME:-}" ] || exit 1
    for s in $(after start "$@"); do case " $svcs " in *" $s "*) ;; *) echo "no such service: $s" >&2; exit 1 ;; esac; done
    for s in $(after start "$@"); do grep -vx "$s" "$stopped" > "$stopped.new" 2>/dev/null; mv "$stopped.new" "$stopped"; done ;;
  *" ps --services --status running") for s in $svcs; do [ "$(st $s)" != running ] || echo $s; done ;;
  *" ps --all --format json "*)
    # What the fake stopped is exited, the rest running and healthy.
    for s in $(after json "$@"); do
      if grep -qx "$s" "$stopped" 2>/dev/null; then
        printf '{"Service":"%s","State":"exited"}\n' "$s"
      else
        printf '{"Service":"%s","State":"running","Health":"healthy"}\n' "$s"
      fi
      case " ${FAKE_ONEOFF:-} " in *" $s "*) printf '{"Name":"%s-%s-run-1","Service":"%s","State":"running","Labels":"com.docker.compose.oneoff=True"}\n' "${COMPOSE_PROJECT_NAME:-}" "$s" "$s" ;; esac
    done ;;
  # FAKE_STALE_DAEMON: an rsync daemon an earlier push from this parent
  # left, listed by its label.
  "ps -aq --filter label=devopsy.data.parent="*) echo "${FAKE_STALE_DAEMON:-}" ;;
  # FAKE_IMPORT_CONTAINERS: containers an --import left
  # (devopsy-import-<compose project>-<pid>-extract|owners-<n>), and
  # FAKE_OWNERS_CONTAINERS an owners' step's
  # (devopsy-owners-<compose project>-<pid>-<n>), listed by a name filter,
  # a regular expression, as Docker's. FAKE_SAME_NAME: a filter naming
  # one container exactly finds it, as one a killed devopsy whose pid
  # this one reuses left.
  "ps -aq --filter name=^/?devopsy-"*)
    f=${4#name=}
    case $f in
      *'[0-9]'*) for c in ${FAKE_IMPORT_CONTAINERS:-} ${FAKE_OWNERS_CONTAINERS:-}; do ! printf '%s\n' "$c" | grep -Eq -- "$f" || echo "$c"; done ;;
      *) [ -z "${FAKE_SAME_NAME:-}" ] || { f=${f#^/?}; echo "${f%\$}"; } ;;
    esac ;;
  # FAKE_SYNC_CONTAINERS: containers a sync left (devopsy-sync-<id>-...),
  # listed by a name filter.
  "ps -aq --filter name="*)
    [ ! -e "$DOCKER_LOG.rm-done" ] || exit 0
    for c in ${FAKE_SYNC_CONTAINERS:-}; do case $c in "${4#name=}"*) echo "$c" ;; esac; done ;;
  # FAKE_RM_IN_PROGRESS: a sync's containers removed meanwhile by
  # another (the push's own removal), which Docker refuses as in
  # progress; they are gone right after.
  "rm -f devopsy-sync-"*)
    [ -n "${FAKE_RM_IN_PROGRESS:-}" ] || exit 0
    : >"$DOCKER_LOG.rm-done"
    echo "Error response from daemon: removal of container $3 is already in progress" >&2
    exit 1 ;;
  *" ps --all --format json")
    for s in $svcs; do [ "$(st $s)" = none ] || printf '{"Service":"%s","State":"%s"}\n' "$s" "$(st $s)"; done
    for s in ${FAKE_ORPHANS:-}; do printf '{"Service":"%s","State":"exited"}\n' "$s"; done
    for s in ${FAKE_ONEOFF:-}; do printf '{"Name":"%s-%s-run-1","Service":"%s","State":"running","Labels":"com.docker.compose.project=%s,com.docker.compose.oneoff=True"}\n' "${COMPOSE_PROJECT_NAME:-}" "$s" "$s" "${COMPOSE_PROJECT_NAME:-}"; done ;;
  # up -d --wait [--no-deps] <svc>... (upFor, a first release's create)
  # and up -d --no-deps --wait <svc>... (a recreate on the image the data
  # now needs).
  *" up -d --wait "*|*" up -d --no-deps --wait "*)
    [ "${FAIL_UP:-}" != "${COMPOSE_PROJECT_NAME:-}" ] || exit 1
    # FAKE_UP_WAIT: an up that writes its caller's pid there and takes
    # until $FAKE_UP_WAIT.go exists.
    # Its caller killed meanwhile, it changes nothing: the script's settle
    # then starts what was stopped, never raced by this one's state write.
    if [ -n "${FAKE_UP_WAIT:-}" ]; then echo $PPID >"$FAKE_UP_WAIT"; n=0; while [ ! -e "$FAKE_UP_WAIT.go" ] && [ $n -lt 200 ]; do sleep 0.1; n=$((n + 1)); done; kill -0 "$PPID" 2>/dev/null || exit 1; fi
    # FAKE_UP_MKDIR: an up that makes that directory (where a file
    # devopsy writes next goes, so the write fails).
    [ -z "${FAKE_UP_MKDIR:-}" ] || mkdir -p "$FAKE_UP_MKDIR"
    # FAKE_UP_DEPS: as Compose does, an up of app starts db, which it
    # depends on, too, unless --no-deps.
    ups=$(after --wait "$@" | grep -v '^--')
    case "$* " in
      *" --no-deps "*) ;;
      *) [ -z "${FAKE_UP_DEPS:-}" ] || case " $ups " in *" app "*) ups="db $ups" ;; esac ;;
    esac
    # A container of $DOCKER_LOG.containers.<project> is made again on
    # what the release's devopsy-compose.yaml gives its service: its
    # devopsy.digest label, and that digest as its image.
    cf=$(cd -P "$dir/.." 2>/dev/null && cd .. && pwd)/devopsy-compose.yaml
    cfile="$DOCKER_LOG.containers.${COMPOSE_PROJECT_NAME:-}"
    for s in $ups; do
      echo "$s" >> "$DOCKER_LOG.up.${COMPOSE_PROJECT_NAME:-}"
      grep -vx "$s" "$stopped" > "$stopped.new" 2>/dev/null; mv "$stopped.new" "$stopped"
      if [ -f "$cfile" ] && [ -f "$cf" ] && grep -q "^$s " "$cfile"; then
        l=$(awk -v svc="  \"$s\":" '$0 == svc { in_s = 1; next } /^  [^ ]/ { in_s = 0 } in_s && /"devopsy.digest":/ { sub(/.*"devopsy.digest": "/, ""); sub(/"$/, ""); print; exit }' "$cf")
        [ -z "$l" ] || { grep -v "^$s " "$cfile" > "$cfile.new"; echo "$s $l ${l#*@}" >> "$cfile.new"; mv "$cfile.new" "$cfile"; }
      fi
    done ;;
  "run --rm -d "*)
    : >"$DOCKER_LOG.daemonvols"
    for a; do case $a in *:/mnt:ro) echo "${a%:/mnt:ro}" > "$DOCKER_LOG.daemon" ;; *:/vol/*:ro) v=${a%:ro}; echo "${v##*/} ${a%%:*}" >> "$DOCKER_LOG.daemonvols" ;; RSYNCD_CONF=*) printf '%s\n' "${a#RSYNCD_CONF=}" > "$DOCKER_LOG.rsyncd" ;; esac; done
    echo cid123 ;;
  # Images as devopsy's images rules inspect them (images.CLI): FAKE_IMAGES,
  # space separated <id>=<registry digest>, found by id or by digest.
  "image inspect --format {{json .}} "*)
    for i in ${FAKE_IMAGES:-}; do
      case $5 in "${i%%=*}"|*"@${i#*@}") printf '{"Id":"%s","RepoDigests":["%s"]}\n' "${i%%=*}" "${i#*=}"; exit 0 ;; esac
    done
    echo "Error: No such image: $5" >&2; exit 1 ;;
  # FAKE_NAMES: space separated <name>=<id>, the image a name points to
  # (images.CLI's ID); any other name answers nothing.
  "image inspect --format {{.Id}} "*)
    for n in ${FAKE_NAMES:-}; do [ "${n%%=*}" != "$5" ] || { echo "${n#*=}"; exit 0; }; done ;;
  # FAKE_USERS: the compose projects whose containers use any image
  # (images.CLI's Users: a container's id, then its project), none by
  # default.
  "ps -a --filter ancestor="*) for u in ${FAKE_USERS:-}; do echo "c1 $u"; done ;;
  # Containers as devopsy's images rules read them (images.CLI): one per
  # line "<service> <devopsy.digest label> [<image id>]" of
  # $DOCKER_LOG.containers.<project>, exited, its id ctr:<project>:<service>,
  # its image sha256:<service> unless the line names one.
  "ps -a -q --no-trunc --filter label=com.docker.compose.project="*)
    p=${6#label=com.docker.compose.project=}
    [ ! -f "$DOCKER_LOG.containers.$p" ] || while read -r cs cl; do echo "ctr:$p:$cs"; done <"$DOCKER_LOG.containers.$p" ;;
  "inspect --format {{json .}} ctr:"*)
    shift 3
    for c; do
      p=${c#ctr:}; cs=${p#*:}; p=${p%%:*}
      cl=$(grep "^$cs " "$DOCKER_LOG.containers.$p" | cut -d' ' -f2)
      ci=$(grep "^$cs " "$DOCKER_LOG.containers.$p" | cut -d' ' -f3)
      printf '{"Name":"/%s-%s-1","Image":"%s","State":{"Status":"exited"},"Config":{"Image":"y","Labels":{"com.docker.compose.project":"%s","com.docker.compose.service":"%s","devopsy.digest":"%s"}}}\n' "$p" "$cs" "${ci:-sha256:$cs}" "$p" "$cs" "$cl"
    done ;;
  "inspect "*) echo 172.17.0.9 ;;
  # devopsy-util: sha256:old built at FAKE_UTIL_CREATED (now by default),
  # none with FAKE_UTIL_MISSING; a build fails with FAIL_UTIL_BUILD, else
  # keeps its Dockerfile and makes sha256:new, built now.
  "image inspect -f "*)
    [ -z "${FAKE_UTIL_MISSING:-}" ] || exit 1
    id=sha256:old; c=${FAKE_UTIL_CREATED:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
    [ ! -e "$DOCKER_LOG.util" ] || { id=sha256:new; c=$(date -u +%Y-%m-%dT%H:%M:%SZ); }
    case $4 in *Created*) echo "$id $c" ;; *) echo "$id" ;; esac ;;
  "build "*)
    cat >"$DOCKER_LOG.dockerfile"
    [ -z "${FAIL_UTIL_BUILD:-}" ] || { echo "pull failed" >&2; exit 1; }
    : >"$DOCKER_LOG.util" ;;
  # The owners' containers, one per entry (devopsy made it).
  # FAKE_OWNERS_WAIT: each takes until $FAKE_OWNERS_WAIT.go exists.
  "run --rm --name devopsy-owners-"*|"run --rm --name devopsy-sync-"*"-owners-"*|"run --rm --name devopsy-import-"*"-owners-"*)
    if [ -n "${FAKE_OWNERS_WAIT:-}" ]; then : >"$FAKE_OWNERS_WAIT"; n=0; while [ ! -e "$FAKE_OWNERS_WAIT.go" ] && [ $n -lt 200 ]; do sleep 0.1; n=$((n + 1)); done; fi
    [ -z "${FAIL_OWNERS:-}" ] || exit "$FAIL_OWNERS" ;;
  "run --rm --name devopsy-sync-"*"-wipe "*)
    for a; do case $a in *:/mnt) dst=${a%:/mnt} ;; esac; done
    find "$dst" -mindepth 1 -delete ;;
  "run --rm --name devopsy-sync-"*"-receive --network host "*" rsync://127.0.0.1:"*"/vol/"*)
    # A volume, from the daemon's mount of it.
    [ -z "${FAIL_RSYNC:-}" ] || exit "$FAIL_RSYNC"
    m=${last2#rsync://127.0.0.1:*/}; m=${m%/}
    grep -qx "\[$m\]" "$DOCKER_LOG.rsyncd" || { echo "@ERROR: Unknown module '$m'" >&2; exit 5; }
    from=$(grep "^$m " "$DOCKER_LOG.daemonvols" | cut -d' ' -f2)
    for a; do case $a in *:/vol/*) to=${a%%:*} ;; esac; done
    case $from in /*) ;; *) from="$vols/$from" ;; esac
    mkdir -p "$vols/$to" && cp -R "$from/." "$vols/$to" ;;
  "run --rm --name devopsy-sync-"*"-receive --network host "*)
    [ -z "${FAIL_RSYNC:-}" ] || exit "$FAIL_RSYNC"
    for a; do case $a in *:/mnt) dst=${a%:/mnt} ;; esac; done
    src=$(cat "$DOCKER_LOG.daemon"); d=${last#/mnt/}
    # Only a module the daemon was given (its name, or mnt for all of it).
    m=${last2#rsync://127.0.0.1:*/}; m=${m%/}; f=${m#*/}; m=${m%%/*}
    grep -qx "\[$m\]" "$DOCKER_LOG.rsyncd" || { echo "@ERROR: Unknown module '$m'" >&2; exit 5; }
    if [ "$f" != "$m" ]; then cp "$src/$f" "$dst/"; else mkdir -p "$dst/$d" && cp -R "$src/$d." "$dst/$d"; fi ;;
  # A snapshot's: its mirror's old entries removed, its tars (plain, not
  # compressed) from the mirror to stdout, the data measured (du -sk:
  # FAKE_DU kilobytes each, 4 by default).
  # A snapshot's tar, decompressed (the fake's are plain tars) into the
  # FIFO of -v <dir>:/o.
  "run --rm --name devopsy-snapshot-"*"-check "*)
    # FAKE_STALE_CHECK: a check's container a devopsy killed outright
    # left, blocked on opening its FIFO: Docker refuses the name until
    # that container is removed (rm -f), as for any name in use.
    [ -z "${FAKE_STALE_CHECK:-}" ] || grep -q " rm -f .*-check$" "$DOCKER_LOG" || { echo "docker: Error response from daemon: Conflict. The container name \"/$4\" is already in use by container \"stale\"." >&2; exit 125; }
    for a; do case $a in *:/s:ro) sd=${a%:/s:ro} ;; *:/o) o=${a%:/o} ;; esac; done
    exec cat "$sd/$last" >"$o/tar" ;;
  # A restore's space: per path its filesystem (fakefs), the free
  # kilobytes there (FAKE_FREE_KB, plenty by default) and its own use
  # (FAKE_DU, 4 by default).
  "run --rm --name devopsy-snapshot-"*"-space "*)
    # The paths: what follows sh -c <script> sh.
    w=0; for a; do case $w in 3) printf '%s\tfakefs\t%s\t%s\n' "$a" "${FAKE_FREE_KB:-1000000000}" "${FAKE_DU:-4}" ;; 0) [ "$a" != -c ] || w=1 ;; *) w=$((w + 1)) ;; esac; done ;;
  "run --rm --name devopsy-snapshot-"*"-stale "*)
    for a; do case $a in *:/m) m=${a%:/m} ;; esac; done
    for a; do case $a in /m/*) rm -rf "$m/${a#/m/}" ;; esac; done ;;
  # FAKE_REFLINK: the data clones by reflinks (it does not by default),
  # for a snapshot's take and a sync's or an export's copy.
  "run --rm --name devopsy-snapshot-"*"-reflink "*|"run --rm --name devopsy-sync-"*"-reflink "*)
    [ -n "${FAKE_REFLINK:-}" ] || exit 1 ;;
  # A reflink clone: -v <mnt>:/m/mnt:ro, -v <volume>:/m/volumes/<key>:ro,
  # -v <clone>:/c; copied as cp -a would.
  "run --rm --name devopsy-snapshot-"*"-clone "*|"run --rm --name devopsy-sync-"*"-clone "*)
    # FAIL_CLONE: part of the data does not clone (another filesystem
    # mounted inside an entry, a btrfs nodatacow file), whatever the check
    # found of the one file it tried.
    [ -z "${FAIL_CLONE:-}" ] || { echo "cp: failed to clone '/c/mnt/db/ibdata': Operation not supported" >&2; exit 1; }
    m= c=
    for a; do case $a in *:/m/mnt:ro) m=${a%:/m/mnt:ro} ;; *:/c) c=${a%:/c} ;; esac; done
    mkdir -p "$c/mnt" "$c/volumes"
    for e in $(printf '%s\n' "$last" | sed -n "s|^cp -a --reflink=always '/m/mnt/\\([^']*\\)' '/c/mnt/' .*|\\1|p"); do cp -Rp "$m/$e" "$c/mnt/"; done
    for a; do case $a in *:/m/volumes/*:ro) vn=${a%%:*}; k=${a#*:/m/volumes/}; k=${k%:ro}; mkdir -p "$c/volumes/$k"; cp -Rp "$vols/$vn/." "$c/volumes/$k/" ;; esac; done ;;
  "run --rm --name devopsy-snapshot-"*"-unclone "*|"run --rm --name devopsy-sync-"*"-unclone "*|"run --rm --name devopsy-destroy-"*"-clone "*)
    for a; do case $a in *:/s) sd=${a%:/s} ;; esac; done
    rm -rf "$sd/${last#/s/}" ;;
  "run --rm --name devopsy-snapshot-"*"-unmirror "*)
    for a; do case $a in *:/s) sd=${a%:/s} ;; esac; done
    rm -rf "$sd/.mirror" ;;
  "run --rm --name devopsy-snapshot-"*"-tar "*)
    [ -z "${FAIL_SNAPSHOT_TAR:-}" ] || exit "$FAIL_SNAPSHOT_TAR"
    # FAKE_KILL_SNAPSHOT_TAR: the devopsy writing the tars killed
    # outright (SIGKILL, the OOM killer).
    [ -z "${FAKE_KILL_SNAPSHOT_TAR:-}" ] || { kill -KILL "$PPID"; exit 137; }
    # FAKE_SLOW_TAR: a tar that takes until $FAKE_SLOW_TAR.go exists.
    if [ -n "${FAKE_SLOW_TAR:-}" ]; then : >"$FAKE_SLOW_TAR"; n=0; while [ ! -e "$FAKE_SLOW_TAR.go" ] && [ $n -lt 200 ]; do sleep 0.1; n=$((n + 1)); done; fi
    # From the mirror (-v <mirror>:/m:ro), or a take without one from the
    # data itself: -v <mnt>:/m/mnt:ro, -v <volume>:/m/volumes/<key>:ro.
    # ... sh <dir> <entry> <file> <uid:gid>, the file in -v <dir>:/o.
    m= vn= o=
    for a; do case $a in *:/m:ro) m=${a%:/m:ro} ;; *:/m/mnt:ro) m=${a%:/m/mnt:ro}/.. ;; *:/m/volumes/*:ro) vn=${a%%:*} ;; *:/o) o=${a%:/o} ;; esac; done
    n=$#; eval "tdir=\${$((n - 3))} tentry=\${$((n - 2))}"; out="$o/$last2"
    # Only files, directories and links, as the script's find.
    ftar() { (cd "$1" && find "$2" \( -type f -o -type d -o -type l \) | COPYFILE_DISABLE=1 tar -cf "$out" --no-recursion $hd -T -); }
    # GNU tar's --hard-dereference; bsdtar (macOS) has none, and its own
    # hard links the test fakes never hold.
    hd=; tar --version 2>/dev/null | grep -q GNU && hd=--hard-dereference
    if [ -n "$vn" ]; then st=$(mktemp -d); cp -R "$vols/$vn" "$st/$tentry"; ftar "$st" "$tentry"; r=$?; rm -rf "$st"; exit $r; fi
    case $m in */..) ftar "${m%/..}" "$tentry"; exit $? ;; esac
    ftar "$m/$tdir" "$tentry" ;;
  "run --rm --network none "*" du -sk "*)
    w=0; for a; do [ "$w" = 1 ] && printf '%s\t%s\n' "${FAKE_DU:-4}" "$a"; [ "$a" = -sk ] && w=1; done ;;
  # A clone into the child's entry or volume (--mount
  # type=bind|volume,source=<path or volume>,target=/s|/d), emptied first:
  # only with FAKE_REFLINK, as reflinks between them; without, the
  # script's exit 4, nothing changed.
  "run --rm --name devopsy-sync-"*"-share-"*)
    [ -n "${FAKE_REFLINK:-}" ] || exit 4
    for a; do case $a in
      type=bind,source=*,target=/s*) sh_src=${a#type=bind,source=}; sh_src=${sh_src%%,target=*} ;;
      type=volume,source=*,target=/s*) sh_src=${a#type=volume,source=}; sh_src="$vols/${sh_src%%,target=*}" ;;
      type=bind,source=*,target=/d) sh_dst=${a#type=bind,source=}; sh_dst=${sh_dst%%,target=*} ;;
      type=volume,source=*,target=/d) sh_dst=${a#type=volume,source=}; sh_dst="$vols/${sh_dst%%,target=*}" ;;
    esac; done
    mkdir -p "$sh_dst" && find "$sh_dst" -mindepth 1 -maxdepth 1 -exec rm -rf {} + && cp -Rp "$sh_src/." "$sh_dst/" ;;
  "run --rm --name devopsy-sync-"*|"run --rm --name devopsy-snapshot-"*)
    [ -z "${FAIL_RSYNC:-}" ] || exit "$FAIL_RSYNC"
    if [ -n "${FAKE_SLOW_RSYNC:-}" ]; then touch "$FAKE_SLOW_RSYNC"; exec sleep 20; fi
    # FAKE_SLOW_STOPPED: a snapshot's stopped pass that takes 20 s.
    case "$*" in *-stopped-*) [ -z "${FAKE_SLOW_STOPPED:-}" ] || { touch "$FAKE_SLOW_STOPPED"; exec sleep 20; } ;; esac
    # $DOCKER_LOG.mirrorhook: what happens on the server while a take's
    # first pass copies (a release switching current), run once.
    case "$*" in *-mirror-0\ *) [ ! -f "$DOCKER_LOG.mirrorhook" ] || { sh "$DOCKER_LOG.mirrorhook"; rm -f "$DOCKER_LOG.mirrorhook"; } ;; esac
    # A volume: -v <name>:/src/<key>:ro and -v <name>:/dst/<key>; from a
    # clone, its directory (a path) as the source.
    vsrc= vdst=
    for a; do case $a in *:/src:ro) src=${a%:/src:ro} ;; *:/dst) dst=${a%:/dst} ;; /*:/src/*:ro) vsrc=${a%%:*} ;; *:/src/*:ro) vsrc="$vols/${a%%:*}" ;; /*:/dst/*) vdst=${a%%:*} ;; *:/dst/*) vdst="$vols/${a%%:*}" ;; esac; done
    if [ -n "$vsrc" ]; then mkdir -p "$vdst" && cp -R "$vsrc/." "$vdst"; exit $?; fi
    d=${last2#/src/}
    if [ -f "$src/$d" ]; then cp "$src/$d" "$dst/"; else mkdir -p "$dst/$d" && cp -R "$src/$d." "$dst/$d"; fi ;;
  "run --rm --name devopsy-destroy-"*"-mirror "*)
    for a; do case $a in *:/s) sd=${a%:/s} ;; esac; done
    rm -rf "$sd/.mirror" ;;
  "run --rm --name devopsy-destroy-"*)
    # FAIL_DESTROY: emptying mnt fails (or its devopsy is killed).
    [ -z "${FAIL_DESTROY:-}" ] || exit "$FAIL_DESTROY"
    # FAKE_KILL_DESTROY: its devopsy (--destroy-data, which runs this)
    # killed outright meanwhile, as SIGKILL or the OOM killer would.
    [ -z "${FAKE_KILL_DESTROY:-}" ] || kill -9 "$PPID"
    for a; do case $a in *:/mnt) dst=${a%:/mnt} ;; esac; done
    find "$dst" -mindepth 1 -delete ;;
  "run --rm --name devopsy-export-"*)
    [ -z "${FAIL_TAR:-}" ] || exit "$FAIL_TAR"
    # FAKE_KILL_EXPORT: the devopsy exporting killed outright (SIGKILL,
    # the OOM killer) as its tar runs.
    [ -z "${FAKE_KILL_EXPORT:-}" ] || { kill -KILL "$PPID"; exit 137; }
    # Each entry (--mount type=bind,source=<path>,target=/x/<entry>) and
    # volume (-v <name>:/x/<key>:ro) copied into a directory of its own,
    # where the script that follows the image runs, /x being it.
    # Hard links kept, as the container's bind mount shows them (GNU
    # cp -a; macOS's cp keeps none).
    x="$DOCKER_LOG.x"; rm -rf "$x"; mkdir -p "$x"
    cpx="cp -Rp"; cp --version 2>/dev/null | grep -q GNU && cpx="cp -a"
    while [ $# -gt 0 ]; do case $1 in
      --mount) m=${2%,readonly}; src=${m#type=bind,source=}; src=${src%%,target=*}; $cpx "$src" "$x/${m##*,target=/x/}"; shift ;;
      *:/x/*:ro) v=${1%:ro}; cp -Rp "$vols/${1%%:*}" "$x/${v##*/}" ;;
      -c) script=$2; shift 2; break ;;
    esac; shift; done
    # bsdtar (macOS) has neither --hard-dereference nor --sparse.
    gnu=; tar --version 2>/dev/null | grep -q GNU || gnu="s| --hard-dereference --sparse||;"
    COPYFILE_DISABLE=1 exec sh -c "$(printf '%s' "$script" | sed "$gnu s|set -o pipefail; ||; s|cd /x|cd '$x'|")" "$@" ;;
  "run --rm --name devopsy-import-"*)
    # Which of the release's lock descriptors the client got.
    for fd in 8 9; do [ ! -e /dev/fd/$fd ] || echo "$fd" >> "$DOCKER_LOG.fds"; done
    [ -z "${FAIL_TAR:-}" ] || exit "$FAIL_TAR"
    # FAKE_KILL_IMPORT: the devopsy that started the extraction killed
    # outright (SIGKILL, the OOM killer) as it writes.
    [ -z "${FAKE_KILL_IMPORT:-}" ] || { kill -KILL "$PPID"; exit 137; }
    # One entry per container (--mount, its source the entry): emptied,
    # then the members named after it extracted from the tar mounted at
    # /in (a snapshot's .tar.zst is a plain tar here), as the util image
    # does.
    vol= in=
    while [ $# -gt 0 ]; do case $1 in --mount) case $2 in *,target=/in/*) in=${2#type=bind,source=}; in=${in%%,target=*} ;; type=volume,*) vol=1; src=${2#type=volume,source=}; src="$vols/${src%%,target=*}" ;; *) src=${2#type=bind,source=}; src=${src%%,target=*} ;; esac ;; -c) shift 3; break ;; esac; shift; done
    e=$2; shift 2
    [ -n "$in" ] || { echo "the tar is no file mounted at /in" >&2; exit 1; }
    if [ -d "$src" ] && [ ! -L "$src" ]; then find "$src" -mindepth 1 -maxdepth 1 -exec rm -rf {} +; fi
    if [ -n "$vol" ]; then
      # A volume (its mount, by name): what the members under its key hold.
      t="$DOCKER_LOG.vtmp"; rm -rf "$t"; mkdir -p "$t" "$src"
      tar -C "$t" -xf "$in" "$@" && cp -R "$t/$e/." "$src/"; exit $?
    fi
    exec tar -C "${src%/"$e"}" -xf "$in" "$@" ;;
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
# Which sessions carried the operator's --yes for the images' checks.
case "$s" in *DEVOPSY_INTERNAL_IMAGES_ACCEPT=*) case "$s" in *"ready for sync in"*) echo holder ;; *) echo other ;; esac >> "$SSH_LOG.yes" ;; esac

# FAKE_SSH_SCRIPTS: each session's script there too, after a line ----.
[ -z "${FAKE_SSH_SCRIPTS:-}" ] || printf -- '----\n%s\n' "$s" >> "$FAKE_SSH_SCRIPTS"
case "$opts" in *ForwardAgent=yes*) ;; *) unset SSH_AUTH_SOCK ;; esac
[ -z "${FAKE_RESTRICT:-}" ] || unset SSH_AUTH_SOCK
# FAKE_SSH_DETACH: the push outlives its ssh, as on a server, where only
# its stdin closing tells it the session is gone. A background job's
# stdin is /dev/null before its own redirections apply (dash), hence fd 3.
exec 3<&0
# FAKE_CHILD_PATH: the parent's sessions to the child (ChildSSH) find its
# devopsy there first.
case "$opts" in *BatchMode=yes*) [ -z "${FAKE_CHILD_PATH:-}" ] || PATH="$FAKE_CHILD_PATH:$PATH" ;; esac
case "$s" in *--sync-push*) if [ -n "${FAKE_SSH_DETACH:-}" ]; then sh -c "$s" <&3 & wait $!; exit $?; fi ;; esac
# FAKE_SSH_LOST_PUSH: the push's connection lost once it ran (ssh's 255),
# its copy complete.
case "$s" in *--sync-push*) if [ -n "${FAKE_SSH_LOST_PUSH:-}" ]; then sh -c "$s" <&3; exit 255; fi ;; esac
# FAKE_SSH_LOST_IMPORT: an --import's session lost on the way (a network
# gone during the upload), as ssh says it.
case "$s" in *"--import "*) [ -z "${FAKE_SSH_LOST_IMPORT:-}" ] || { echo "Read from remote host one: Operation timed out" >&2; exit 255; } ;; esac
# FAKE_SSH_LOST: any session whose script holds this text lost on the
# way, as ssh says it (its 255).
if [ -n "${FAKE_SSH_LOST:-}" ]; then case "$s" in *"$FAKE_SSH_LOST"*) echo "Read from remote host one: Operation timed out" >&2; exit 255 ;; esac; fi
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
# FAKE_SILENT: an activation's session stays open and sends nothing, as
# one whose connection died without a FIN: a fifo opened both ways.
case "$s" in *DEVOPSY_INTERNAL_WATCH=1*) if [ -n "${FAKE_SILENT:-}" ]; then
  mkfifo "$SSH_LOG.silent.$$" && exec 7<>"$SSH_LOG.silent.$$" && rm -f "$SSH_LOG.silent.$$"
  exec sh -c "$s" <&7
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
	// flock logs, and locks nothing unless FAKE_REAL_FLOCK names the real
	// one (Linux: a test of what a held lock shows).
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\necho \"flock $*\" >> \"$DOCKER_LOG\"\n[ -z \"${FAKE_REAL_FLOCK:-}\" ] || exec \"$FAKE_REAL_FLOCK\" \"$@\"\nexit 0\n", 0o755)
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
	write(t, filepath.Join(s.dot, "config.yaml"), "project: shop\ndefaults:\n  mode: image\n  release: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n  solo: {}\n  nosnap: {snapshots: {manual: false}}\n  snap1: {snapshots: {manual: {keep: 1}}}\n  slow: {release: {prepare: [slow], run: deploy}}\n  bare: {parent: prod, release: {before: [local: deploy]}}\n", 0o644)
	write(t, filepath.Join(s.dot, ".env"), "DEVOPSY_SERVER=devopsy@one\n", 0o644)
	write(t, filepath.Join(s.dot, "commands", "deploy"), "#!/bin/sh\necho \"deploy in $COMPOSE_PROJECT_NAME parent=${DEVOPSY_PARENT:-} ${DEVOPSY_PARENT_TARGET:-}\"\n"+
		// FAKE_FAIL_ONCE: the run step fails once (the file made), so a
		// FAKE_KILL_RUN beside it kills the restart that follows.
		"if [ -n \"${FAKE_FAIL_ONCE:-}\" ] && [ ! -e \"$FAKE_FAIL_ONCE\" ]; then : >\"$FAKE_FAIL_ONCE\"; exit 3; fi\n"+
		// FAKE_KILL_RUN: the activation's shell killed outright (SIGKILL,
		// the OOM killer) during its run step, once (the file made), on
		// Linux (/proc): the topmost ancestor running the script itself
		// (its first line its needs; its subshells have the same command
		// line, and above it is ssh's sh -c of it, a connection).
		"if [ -n \"${FAKE_KILL_RUN:-}\" ] && [ ! -e \"$FAKE_KILL_RUN\" ]; then : >\"$FAKE_KILL_RUN\"; p=$PPID; k=; while [ \"$p\" -gt 1 ]; do [ \"$(tr '\\0' '\\n' </proc/$p/cmdline | sed -n 3p | cut -c1-14)\" != devopsy_needs= ] || k=$p; p=$(cut -d' ' -f4 /proc/$p/stat); done; [ -z \"$k\" ] || kill -KILL $k; exit 0; fi\n"+
		"if [ -n \"${FAKE_SLOW_RUN:-}\" ]; then touch \"$FAKE_SLOW_RUN\"; sleep 3; echo \"deployed slowly\"; fi\n[ -z \"${FAKE_FAIL_DEPLOY:-}\" ] || exit 3\n", 0o755)
	write(t, filepath.Join(s.dot, "commands", "slow"), "#!/bin/sh\n## Description: a slow prepare step\nif [ -n \"${FAKE_SLOW_PREPARE:-}\" ]; then touch \"$FAKE_SLOW_PREPARE\"; exec sleep 20; fi\n", 0o755)
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

// A child's first release syncs its parent on the same server: the
// services labeled devopsy.data.stop=own stopped on both sides, one
// container copying, then deploy on the parent's data. Later releases do not, unless --sync; --sync
// resyncs.
// A watcher about to disarm waits for its session's next byte, which
// devopsy sends at once on the script's BeatMarker: a release goes live
// and a sync's holder goes on past its lock in a round trip, not at the
// next beat (30 seconds here), and the marker never shows. A session
// silent by then (its connection died just before the switch, the watcher
// not counting its quiet time yet) never sees its release go live.
func TestBeatMarkerAnswered(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	start := time.Now()
	out, code := s.run([]string{remote.HeartbeatEveryEnv + "=1", "FAKE_SILENT=1"}, "@prod", "--release")
	logs, _ := filepath.Glob(filepath.Join(s.root, "shop", "prod", "releases", "*", remote.ServerLogFile))
	server := ""
	if len(logs) == 1 {
		server = readString(logs[0])
	}
	if took := time.Since(start); code != 130 || s.current("prod") != "" || took < 5*time.Second || !strings.Contains(server, "the session is gone: undoing the fetch") {
		t.Fatalf("a silent session's release (%d) in %s, current %q:\n%s\nserver's log:\n%s", code, took, s.current("prod"), out, server)
	}
	beat := []string{remote.HeartbeatEveryEnv + "=30"}
	for _, args := range [][]string{{"@prod", "--release"}, {"@dev", "--release-no-sync"}, {"@dev", "--sync", "--yes"}} {
		start := time.Now()
		out, code := s.run(beat, args...)
		if took := time.Since(start); code != 0 || took > 20*time.Second || strings.Contains(out, remote.BeatMarker) {
			t.Fatalf("%v (%d) in %s:\n%s", args, code, took, out)
		}
	}
}

func TestSyncSameHost(t *testing.T) {
	t.Parallel()
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
		"run --rm --name devopsy-sync-", " -v " + s.mnt("prod") + ":/src:ro -v " + s.mnt("dev") + ":/dst devopsy-util:3 rsync -a --no-D --delete --numeric-ids --modify-window=-1 --sparse /src/db/ /dst/db/",
		"run --rm --name devopsy-sync-", " -v " + s.mnt("prod") + ":/src:ro -v " + s.mnt("dev") + ":/dst devopsy-util:3 rsync -a --no-D --delete --numeric-ids --modify-window=-1 --sparse /src/storage/ /dst/storage/",
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
	out, code = s.run(nil, "@dev", "--release")
	docker, _ = s.logs()
	if code != 0 || strings.Contains(out, "devopsy: syncing") || strings.Contains(docker, "stop db") {
		t.Fatalf("second release (%d):\n%s\n%s", code, out, docker)
	}

	// --sync asks; --yes; the log goes with the current release.
	write(t, filepath.Join(s.mnt("prod"), "storage", "file.txt"), "newer\n", 0o644)
	if out, code := s.run(nil, "@dev", "--sync"); code == 0 || !strings.Contains(out, "add --yes to go on without asking") {
		t.Fatalf("--sync without a terminal (%d):\n%s", code, out)
	}
	s.logs()
	out, code = s.run(nil, "@dev", "--sync", "--yes")
	docker, ssh = s.logs()
	if code != 0 || !strings.Contains(out, "syncing into") || !strings.Contains(out, "deploy in shop-dev") || !strings.Contains(docker, "--numeric-ids --modify-window=-1 --sparse /src/storage/") {
		t.Fatalf("--sync (%d):\n%s\n%s", code, out, docker)
	}
	// Four sessions, as before snapshots: the parent's check, the holder,
	// the push and the log's save. A child no restore marked needs no
	// look at its mark first.
	if n := strings.Count(ssh, "\n"); n != 4 {
		t.Errorf("--sync opened %d sessions, want 4:\n%s", n, ssh)
	}
	if got := readString(filepath.Join(s.mnt("dev"), "storage", "file.txt")); got != "newer\n" {
		t.Errorf("not resynced: %q", got)
	}
	if out, code := s.run(nil, "@dev", "--log"); code != 0 || !strings.Contains(out, "release exited 0") || !strings.Contains(out, "sync of devopsy@one:dev") || !strings.Contains(out, "sync exited 0") {
		t.Errorf("--log (%d):\n%s", code, out)
	}

	// --release-sync syncs on any release, asking first on one that
	// has data (--yes, or a terminal); --release-no-sync never.
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
	if out, code := s.run(nil, "@dev", "--sync", "--help"); code != 0 || !strings.Contains(out, "Usage: devopsy @<target> --sync [--from @<target>] [--accept-image <service>,...] [--yes]") || !strings.Contains(out, "devopsy.data.copy=custom") {
		t.Errorf("--sync --help (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--release", "--help"); code != 0 || !strings.Contains(out, "--release-sync [--from @<target>] [--accept-image <service>,...] [--yes]") {
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
	t.Parallel()
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
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code == 0 || !strings.Contains(out, "did not complete") || !strings.Contains(out, "release it: a first release syncs\ndevopsy: | $ devopsy @devopsy@one:dev --release\n") || !strings.Contains(out, ": nothing changed\n") {
		t.Fatalf("--release-no-sync on a partial copy (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--release"); code != 0 || !strings.Contains(out, "deploy in shop-dev") || s.current("dev") == "" || fileExists(filepath.Join(s.root, "shop", "dev", remote.SyncIncompleteFile)) {
		t.Fatalf("a first release again, syncing (%d):\n%s", code, out)
	}
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
	t.Parallel()
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

// Where the parent's data clones by reflinks, the services a sync stops
// for its copy are down for the clone only: started again before the
// rsync, which copies from the clone, removed after. A custom service
// stopped by its labels is exported before they start, one running
// after; across servers the daemon serves the clone, and an export on a
// target tars from one. Nothing of what the rsync copies stopped: no
// clone. A clone that fails copies from the stopped data, as before; one
// a push killed outright left goes with the next push and with
// --destroy.
func TestSyncReflinkClone(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	prod := filepath.Join(s.root, "shop", "prod")
	clone := filepath.Join(prod, remote.SyncClonePrefix)
	reflink := []string{"FAKE_REFLINK=1"}
	inOrder := func(name, all string, want ...string) {
		t.Helper()
		rest := all
		for _, w := range want {
			i := strings.Index(rest, w)
			if i < 0 {
				t.Fatalf("%s: missing %q in order, after:\n%s\nall:\n%s", name, w, rest, all)
			}
			rest = rest[i+len(w):]
		}
	}
	noClone := func(name string) {
		t.Helper()
		if left, _ := filepath.Glob(clone + "*"); len(left) > 0 {
			t.Errorf("%s: the clone left: %v", name, left)
		}
	}

	out, code := s.run(reflink, "@dev", "--release")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(out, "the data clones by reflinks") || readString(filepath.Join(s.mnt("dev"), "db", "ibdata")) != "rows\n" {
		t.Fatalf("first release (%d):\n%s", code, out)
	}
	inOrder("first release", docker, "shop-prod compose", "-reflink ", "stop db", "-clone ", "shop-prod compose", "start db",
		" -v "+clone, "/mnt:/src:ro -v "+s.mnt("dev")+":/dst devopsy-util:3 rsync", "/src/db/ /dst/db/", "-unclone ")
	noClone("first release")
	// The clone's containers are the parent's, never named as the sync's
	// own, which the child's holder removes as writers into the child.
	if !regexp.MustCompile(`run --rm --name devopsy-sync-parent-[0-9.a-f]+-clone `).MatchString(docker) {
		t.Errorf("the clone's container name:\n%s", docker)
	}

	// db not stopped (no stop label): the rsync reads the live data.
	s.labels("FAKE_DB=mnt")
	out, code = s.run(reflink, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 || strings.Contains(docker, "-clone ") || !strings.Contains(docker, " -v "+s.mnt("prod")+":/src:ro") {
		t.Errorf("nothing stopped (%d):\n%s\n%s", code, out, docker)
	}
	s.labels("")

	// A clone that fails though the check passed: from the stopped data.
	out, code = s.run(append(reflink, "FAIL_CLONE=1"), "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 || !strings.Contains(out, "copying from the stopped data instead") {
		t.Fatalf("a clone that fails (%d):\n%s", code, out)
	}
	inOrder("a clone that fails", docker, "stop db", "-clone ", "-unclone ", " -v "+s.mnt("prod")+":/src:ro", "/src/db/", "shop-prod compose", "start db")
	noClone("a clone that fails")

	// Streams: one stopped by its labels exported before the start, one
	// running after it; the rsync from the clone last.
	caps := filepath.Join(s.dot, "capabilities", "data")
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho \"dump of $1\"\n", 0o755)
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\ncat >/dev/null\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev with the scripts (%d):\n%s", code, out)
	}
	s.logs()
	s.labels("FAKE_APP=mnt+own\nFAKE_DB=stream+own")
	out, code = s.run(reflink, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 {
		t.Fatalf("a stopped stream (%d):\n%s", code, out)
	}
	inOrder("a stopped stream", out, "the data clones by reflinks", "exporting db", "starting db app", "rsync mnt/storage")
	inOrder("a stopped stream", docker, "-clone ", "start db app", " -v "+clone, "/src/storage/")
	s.labels("FAKE_APP=mnt+own\nFAKE_DB=stream")
	out, code = s.run(reflink, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 {
		t.Fatalf("a running stream (%d):\n%s", code, out)
	}
	inOrder("a running stream", out, "exporting db", "rsync mnt/storage")
	inOrder("a running stream", docker, "stop app", "-clone ", "start app", " -v "+clone, "/src/storage/")
	if i, j := strings.Index(out, "starting app"), strings.Index(out, "exporting db"); i < 0 || j < i {
		t.Errorf("a running stream exported before the start:\n%s", out)
	}
	s.labels("")

	// Across servers the daemon serves the clone.
	across := append([]string{"DEVOPSY_SERVER_PROD=devopsy@one", "SSH_AUTH_SOCK=/tmp/agent.sock"}, reflink...)
	out, code = s.run(across, "@devopsy@two:dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 || readString(filepath.Join(s.mnt("dev"), "db", "ibdata")) != "rows\n" {
		t.Fatalf("across servers (%d):\n%s", code, out)
	}
	inOrder("across servers", docker, "stop db", "-clone ", "start db", "run --rm -d --name devopsy-sync-", " -v "+clone, "/mnt:/mnt:ro", "-unclone ")
	noClone("across servers")

	// An export on a target tars from a clone, db started first.
	cmd := exec.Command(binary, "@prod", "--export", "db")
	cmd.Dir, cmd.Env = s.project, append(append([]string{}, s.env...), reflink...)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = io.Discard, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("export: %v\n%s", err, stderr.String())
	}
	docker, _ = s.logs()
	inOrder("export", docker, "stop db", "-clone ", "start db", "run --rm --name devopsy-export-", "source="+clone+"export.", "-unclone ")
	noClone("export")

	// Interrupted during the rsync from the clone: the clone goes too.
	started := filepath.Join(t.TempDir(), "started")
	cut := exec.Command(binary, "@dev", "--sync", "--yes")
	cut.Dir, cut.Env, cut.Stdin = s.project, append(append([]string{}, s.env...), append(reflink, "FAKE_SLOW_RSYNC="+started, "FAKE_SSH_DETACH=1")...), strings.NewReader("")
	if err := cut.Start(); err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); !fileExists(started); time.Sleep(50 * time.Millisecond) {
		if time.Since(start) > startWait {
			_ = cut.Process.Kill()
			t.Fatalf("the copy never started:\n%s", readString(s.log))
		}
	}
	_ = cut.Process.Signal(syscall.SIGTERM)
	_ = cut.Wait()
	for start := time.Now(); !strings.Contains(readString(s.log), "-unclone "); time.Sleep(100 * time.Millisecond) {
		if time.Since(start) > 10*time.Second {
			t.Fatalf("the clone not removed after the interrupt:\n%s", readString(s.log))
		}
	}
	noClone("interrupted")
	s.logs()
	// The interrupted copy left the child partial: a sync completes it.
	if out, code := s.run(nil, "@dev", "--sync", "--yes"); code != 0 {
		t.Fatalf("the sync after the interrupt (%d):\n%s", code, out)
	}

	// What a push killed outright left: removed by the next push, one
	// that copies nothing as files too, then by --destroy, its containers
	// first.
	write(t, filepath.Join(clone+"old", "mnt", "db", "ibdata"), "old\n", 0o644)
	s.labels("FAKE_APP=keep\nFAKE_DB=stream")
	if out, code := s.run(nil, "@dev", "--sync", "--yes"); code != 0 || !strings.Contains(out, "a clone an earlier copy left") {
		t.Fatalf("a leftover clone (%d):\n%s", code, out)
	}
	s.labels("")
	noClone("a leftover clone")
	write(t, filepath.Join(clone+"old", "mnt", "db", "ibdata"), "old\n", 0o644)
	write(t, s.log+".shop-prod", "FAKE_STALE_DAEMON=old-clone\n", 0o644)
	s.logs()
	if out, code := s.run(nil, "@prod", "--destroy", "--yes"); code != 0 {
		t.Fatalf("--destroy (%d):\n%s", code, out)
	}
	if docker, _ := s.logs(); !strings.Contains(docker, "rm -f old-clone") || !strings.Contains(docker, "/s/"+remote.SyncClonePrefix+"old") || strings.Index(docker, "rm -f old-clone") > strings.Index(docker, "/s/"+remote.SyncClonePrefix+"old") {
		t.Errorf("--destroy did not end the clone's containers, then remove it as root:\n%s", docker)
	}
}

// A child's service labeled devopsy.data.copy.reflink=true gets its
// entries cloned from the parent's by reflinks on the same server: the
// child's entry emptied, never removed, then cloned, its owner given;
// the rest rsynced. Where nothing clones, and across servers, rsync. A
// reset wipes, then clones.
func TestSyncReflinkShare(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	s.logs()
	write(t, s.log+".shop-dev", "FAKE_APP_REFLINK=true\nFAKE_APP_OWNER=10001:10001\n", 0o644)
	reflink := []string{"FAKE_REFLINK=1"}
	stale := filepath.Join(s.mnt("dev"), "storage", "stale.txt")

	write(t, stale, "dev's own\n", 0o644)
	out, code := s.run(reflink, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(out, "mnt/storage shared with the parent by reflinks (devopsy.data.copy.reflink)") || readString(filepath.Join(s.mnt("dev"), "storage", "file.txt")) != "from prod\n" || fileExists(stale) {
		t.Fatalf("shared (%d):\n%s\n%s", code, out, docker)
	}
	// db rsynced, storage cloned, then given to its owner; never rsynced.
	if !strings.Contains(docker, "/src/db/ /dst/db/") || strings.Contains(docker, "/src/storage/") || !regexp.MustCompile(`run --rm --name devopsy-sync-[0-9.a-f]+-share-1 --network none --mount type=bind,source=[^ ]*,target=/s,readonly --mount type=bind,source=`+regexp.QuoteMeta(filepath.Join(s.mnt("dev"), "storage"))+`,target=/d devopsy-util:3`).MatchString(docker) {
		t.Errorf("shared docker:\n%s", docker)
	}
	if i, j := strings.Index(docker, "-share-1 "), strings.Index(docker, "-owners-0 "); i < 0 || j < i {
		t.Errorf("the owner not given after the clone:\n%s", docker)
	}

	// No reflinks between them: rsync, said (its --delete removes what the
	// child has of its own; the fake's copy does not).
	out, code = s.run(nil, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 || !strings.Contains(out, "no reflinks between the parent's data and the child's: devopsy.data.copy.reflink rsynced as any") || !strings.Contains(docker, "--chown=10001:10001 /src/storage/ /dst/storage/") {
		t.Errorf("no reflinks (%d):\n%s\n%s", code, out, docker)
	}

	// A reset wipes at the claim, then clones.
	write(t, stale, "dev's own\n", 0o644)
	out, code = s.run(reflink, "@dev", "--reset", "--yes")
	docker, _ = s.logs()
	if code != 0 || readString(filepath.Join(s.mnt("dev"), "storage", "file.txt")) != "from prod\n" || fileExists(stale) {
		t.Fatalf("reset (%d):\n%s\n%s", code, out, docker)
	}
	if i, j := strings.Index(docker, "-wipe "), strings.Index(docker, "-share-1 "); i < 0 || j < i {
		t.Errorf("reset: the clone not after the wipe:\n%s", docker)
	}

	// Across servers: rsync.
	across := append([]string{"DEVOPSY_SERVER_PROD=devopsy@one", "SSH_AUTH_SOCK=/tmp/agent.sock"}, reflink...)
	out, code = s.run(across, "@devopsy@two:dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 || strings.Contains(docker, "-share-") || !strings.Contains(docker, "rsync://127.0.0.1:") {
		t.Errorf("across servers (%d):\n%s\n%s", code, out, docker)
	}
}

// A service bind-mounting a file of mnt: copied as a file, on both paths.
func TestSyncFileEntry(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	write(t, filepath.Join(s.mnt("prod"), "app.ini"), "[prod]\n", 0o644)
	env := []string{"FAKE_APP_MOUNT=/app.ini"}

	out, code := s.run(env, "@dev", "--release")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(docker, "devopsy-util:3 rsync -a --no-D --delete --numeric-ids --modify-window=-1 --sparse --inplace /src/app.ini /dst/\n") || readString(filepath.Join(s.mnt("dev"), "app.ini")) != "[prod]\n" {
		t.Fatalf("same server (%d):\n%s\n%s", code, out, docker)
	}

	env = append(env, "DEVOPSY_SERVER_PROD=devopsy@one", "SSH_AUTH_SOCK=/tmp/agent.sock")
	out, code = s.run(env, "@devopsy@two:dev", "--sync", "--yes")
	docker, _ = s.logs()
	for _, want := range []string{"[app.ini]\npath = /mnt\ninclude = /app.ini\nexclude = *\n", "--numeric-ids --modify-window=-1 --sparse --inplace rsync://127.0.0.1:"} {
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
	t.Parallel()
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

	// A daemon an earlier push from prod left (its devopsy killed
	// outright) goes before this push starts its own.
	write(t, s.log+".shop-prod", "FAKE_STALE_DAEMON=old-daemon\n", 0o644)
	out, code := s.run(agent, "@devopsy@two:dev", "--release")
	docker, ssh := s.logs()
	if rm, run := strings.Index(docker, "rm -f old-daemon"), strings.Index(docker, "run --rm -d --name devopsy-sync-"); rm < 0 || run < rm {
		t.Errorf("the stale daemon not removed before the push's own:\n%s", docker)
	}
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
	// holder, the push, the push's look at the child's services and its
	// util image (--sync-exec), the receive, and the log's save.
	keep := "-o ServerAliveInterval=15 -o ServerAliveCountMax=3"
	if strings.Count(ssh, keep) != 7 || !strings.Contains(ssh, "-o ConnectTimeout=30 "+keep+" -T devopsy@two\n") || !strings.Contains(ssh, keep+" -T devopsy@two\n") || !strings.Contains(ssh, childSSH+" "+keep+" -T -p 22 devopsy@two\n") {
		t.Errorf("keepalive on upload, holder, push, exec, receive and the log's save:\n%s", ssh)
	}
	for _, want := range []string{"-daemon --label devopsy.data.parent=" + s.mnt("prod") + " -v " + s.mnt("prod") + ":/mnt:ro", "RSYNCD_CONF=uid = 0\ngid = 0\nuse chroot = yes\nread only = yes\n[db]\npath = /mnt/db\n[storage]\npath = /mnt/storage\n", "inspect -f", " --network host -v " + s.mnt("dev") + ":/mnt devopsy-util:3 rsync -a --no-D --delete --numeric-ids --modify-window=-1 --sparse rsync://127.0.0.1:", "/db/ /mnt/db/", "/storage/ /mnt/storage/", " rm -f cid123"} {
		if !strings.Contains(docker, want) {
			t.Errorf("missing %q in:\n%s", want, docker)
		}
	}
	// Both servers get the util image ready before using it: the push
	// in Go, the child's through devopsy --util-image before the parent
	// stops anything (a weekly rebuild would lengthen its downtime), and
	// the receive script's again.
	check := "image inspect -f {{.Id}} {{.Created}} devopsy-util:3\n"
	if n := strings.Count(docker, check); n != 3 {
		t.Errorf("the util image checked %d times, want 3 (push, the child's before the stop, receive):\n%s", n, docker)
	}
	first := strings.Index(docker, check)
	second := strings.Index(docker[first+len(check):], check) + first + len(check)
	if stop := strings.Index(docker, "shop-prod compose -f "+filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml")+" stop"); stop < 0 || second > stop {
		t.Errorf("the child's util image checked after the parent stopped:\n%s", docker)
	}
	// Vanished files across servers: the receive script goes on too.
	s.logs()
	if out, code := s.run(append(agent, "FAIL_RSYNC=24"), "@devopsy@two:dev", "--sync", "--yes"); code != 0 || strings.Count(out, "some files vanished during the copy") != 2 || !strings.Contains(out, "deploy in shop-dev") {
		t.Errorf("vanished files across servers (%d):\n%s", code, out)
	}
	s.logs()
	// An entry the child's labels own (its list says so) lands as its
	// owner's (--chown), whatever the parent's files are; the rest keep
	// theirs.
	write(t, s.log+".shop-dev", "FAKE_APP_OWNER=10001:10001\n", 0o644)
	out, code = s.run(agent, "@devopsy@two:dev", "--sync", "--yes")
	docker, _ = s.logs()
	os.Remove(s.log + ".shop-dev")
	if code != 0 || !regexp.MustCompile(`--numeric-ids --modify-window=-1 --sparse --chown=10001:10001 rsync://127\.0\.0\.1:[0-9]+/storage/ /mnt/storage/`).MatchString(docker) || !regexp.MustCompile(`--numeric-ids --modify-window=-1 --sparse rsync://127\.0\.0\.1:[0-9]+/db/ /mnt/db/`).MatchString(docker) {
		t.Errorf("an owned entry across servers (%d):\n%s\n%s", code, out, docker)
	}

	// A child's devopsy older than the parent's (lacking --say, which
	// only the receive calls) refuses on the push's first session there,
	// the look at its services, before the parent stops or copies
	// anything.
	lacks := filepath.Join(t.TempDir(), "lacks")
	write(t, filepath.Join(lacks, "devopsy"), "#!/bin/sh\nif [ \"$1\" = --release-settings ]; then "+binary+" --release-settings | sed 's/ say / /'; exit; fi\nexec "+binary+" \"$@\"\n", 0o755)
	out, code = s.run(append(agent, "FAKE_CHILD_PATH="+lacks), "@devopsy@two:dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code == 0 || !strings.Contains(out, "the server's devopsy lacks what this needs (say): upgrade it (devopsy @devopsy@two: --upgrade)") || !strings.Contains(out, "listing the child's services") || strings.Contains(docker, filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml")+" stop") || strings.Contains(docker, "rsync://") {
		t.Fatalf("a child lacking what the push needs (%d):\n%s\n%s", code, out, docker)
	}

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
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()
	// The parent's stop fails, before anything is written: what it
	// stopped comes back, and so does the child. --yes is only the
	// prompt's: no session gets the images' acceptance.
	os.Remove(s.sshLog + ".yes")
	out, code := s.run([]string{"FAIL_STOP=shop-prod"}, "@dev", "--sync", "--yes")
	if yes, _ := os.ReadFile(s.sshLog + ".yes"); len(yes) != 0 {
		t.Errorf("sessions with the images' acceptance from --yes: %q", yes)
	}
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
	// services stay down (app with db, which it depends on), the parent's
	// come back.
	out, code = s.run([]string{"FAIL_RSYNC=23"}, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 1 || !strings.Contains(out, "aborting the sync") || !strings.Contains(out, "the copy was cut") || !strings.Contains(out, "db app stay down") || strings.Contains(out, "deploy in shop-dev") {
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
	if out, code := s.run([]string{"FAIL_STOP=shop-prod"}, "@dev", "--sync", "--yes"); code != 1 || !strings.Contains(out, "db app stay down") {
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
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()
	write(t, filepath.Join(s.dot, "capabilities", "data", "finish"), "#!/bin/sh\necho \"finish $1 $2\"\n", 0o755)
	if out, code := s.run(nil, "@prod", "--release"); code != 0 {
		t.Fatalf("prod with the hook (%d):\n%s", code, out)
	}
	s.logs()
	out, code := s.run([]string{"FAIL_START=shop-prod"}, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	// Both sides hear what the copy did: it completed.
	if !strings.Contains(out, "finish parent go") || strings.Contains(out, "finish parent abort") {
		t.Errorf("the parent's finish word, the copy complete:\n%s", out)
	}
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
// The parent's services start again once every stream is exported,
// before the child's imports: an import that then fails leaves the child
// partial, marked, its stopped services down, and the parent running,
// never stopped again for it.
func TestSyncImportFailsAfterParentStarts(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	caps := filepath.Join(s.dot, "capabilities", "data")
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho \"dump of $1\"\n", 0o755)
	// The child's hooks get a clean environment, HOME kept.
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\ncat >/dev/null\n[ ! -e \"$HOME/fail-import-$1\" ] || exit 3\necho \"import $1 into $COMPOSE_PROJECT_NAME\"\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	s.logs()
	s.labels("FAKE_APP=stream\nFAKE_DB=stream+own")
	home := filepath.Dir(s.sshLog)
	write(t, filepath.Join(home, "fail-import-app"), "", 0o644)
	out, code := s.run(nil, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code == 0 || !fileExists(filepath.Join(s.root, "shop", "dev", remote.SyncIncompleteFile)) {
		t.Fatalf("an import that fails (%d), marked %v:\n%s", code, fileExists(filepath.Join(s.root, "shop", "dev", remote.SyncIncompleteFile)), out)
	}
	rest := out
	for _, want := range []string{"devopsy: exporting db", "devopsy: exporting app", "devopsy: starting db app", "import db into shop-dev", "import app: exit status 3"} {
		i := strings.Index(rest, want)
		if i < 0 {
			t.Fatalf("missing %q in order:\n%s", want, out)
		}
		rest = rest[i+len(want):]
	}
	// The parent ran on from its start: nothing of it stopped since.
	start := strings.LastIndex(docker, "shop-prod compose -f "+filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml")+" start db app")
	if start < 0 || strings.Contains(docker[start:], "shop-prod compose -f "+filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml")+" stop") || strings.TrimSpace(readString(s.log+".stopped.shop-prod")) != "" {
		t.Errorf("the parent not running after the child's import failed:\n%s", docker)
	}
}

func TestSyncCapability(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	caps := filepath.Join(s.dot, "capabilities", "data")
	write(t, filepath.Join(caps, "prepare"), "#!/bin/sh\necho \"prepare $1 in $COMPOSE_PROJECT_NAME volumes=$DEVOPSY_DATA_VOLUMES custom=$DEVOPSY_DATA_CUSTOM stop=$DEVOPSY_DATA_STOP from=$DEVOPSY_SYNC_FROM_TARGET to=$DEVOPSY_SYNC_TO_TARGET\"\n[ \"${FAIL_PREPARE:-}\" != \"$1\" ]\n", 0o755)
	write(t, filepath.Join(caps, "finish"), "#!/bin/sh\necho \"finish $1 $2 in $COMPOSE_PROJECT_NAME\"\n", 0o755)
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho \"dump of $1 from $COMPOSE_PROJECT_NAME pw=$DB_PASSWORD\"\nif [ \"${FAIL_EXPORT:-}\" = \"$1\" ]; then exit 3; fi\n", 0o755)
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\necho \"import $1 into $COMPOSE_PROJECT_NAME pw=$DB_PASSWORD from=$DEVOPSY_SYNC_FROM_TARGET: $(cat)\"\n", 0o755)
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
	inOrder("stream", out, code, "prepare child in shop-dev volumes=app custom=db stop= from=devopsy@one:prod to=devopsy@one:dev", "devopsy: copying",
		"prepare parent in shop-prod volumes=app custom=db stop= from=devopsy@one:prod to=devopsy@one:dev", "rsync mnt/storage", "devopsy: exporting db",
		"import db into shop-dev pw=child-secret from=devopsy@one:prod: dump of db from shop-prod pw=parent-secret", "finish parent go in shop-prod", "finish child go in shop-dev", "deploy in shop-dev")
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

	// Two streams, db stopped (own) and app with it (it depends on db):
	// in depends_on order, both exported with the parent's stopped, the
	// parent started, then both imported, neither brought up for its
	// import (no exception for streams): the parent is never down
	// through the child's load.
	s.labels("FAKE_APP=stream\nFAKE_DB=stream+own")
	out, code = s.run(nil, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 {
		t.Fatalf("two streams (%d):\n%s", code, out)
	}
	inOrder("two streams", out, code, "prepare child in shop-dev volumes= custom=db app stop=db app", "devopsy: stopping db (own), app (depends on db)", "devopsy: copying", "devopsy: stopping db (own), app (depends on db)", "devopsy: exporting db", "devopsy: exporting app", "devopsy: starting db app", "import db into shop-dev", "import app into shop-dev", "devopsy: starting db app", "deploy in shop-dev")
	if strings.Contains(docker, "up -d --wait") || strings.Contains(out, "rsync mnt/") {
		t.Errorf("two streams docker:\n%s", docker)
	}
	// app import, db a stream that runs: app down on the child for the
	// import only, db brought up for its own without its dependencies.
	s.labels("FAKE_APP=stream+import\nFAKE_DB=stream")
	out, code = s.run(nil, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 {
		t.Fatalf("an import app (%d):\n%s", code, out)
	}
	inOrder("an import app", out, code, "prepare child in shop-dev volumes= custom=db app stop=app", "devopsy: stopping app (import)", "devopsy: copying", "prepare parent in shop-prod volumes= custom=db app stop= ", "import db into shop-dev", "import app into shop-dev", "devopsy: starting app", "deploy in shop-dev")
	if strings.Count(out, "devopsy: stopping") != 1 || !strings.Contains(docker, "up -d --wait --no-deps db") || strings.Contains(docker, "--wait --no-deps app") {
		t.Errorf("an import app docker:\n%s\n%s", out, docker)
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
	if code != 0 || !strings.Contains(out, "import db into shop-dev pw=child-secret from=devopsy@one:prod: dump of db from shop-prod pw=parent-secret") {
		t.Fatalf("stream across servers (%d):\n%s", code, out)
	}
	childSSH := "-o ForwardAgent=no -o BatchMode=yes -o ConnectTimeout=30 -o StrictHostKeyChecking=accept-new"
	if strings.Count(ssh, "-o ClearAllForwardings=yes "+childSSH+" -o ServerAliveInterval=15 -o ServerAliveCountMax=3 -T -p 22 devopsy@two") != 4 {
		t.Errorf("the child's list, its util image (app's storage is rsynced), the spool's cat and the import, each a session:\n%s", ssh)
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
	childImport := filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "capabilities", "data", "import")
	if err := os.Rename(childImport, childImport+".off"); err != nil {
		t.Fatal(err)
	}
	s.labels("FAKE_DB=stream+own")
	out, code = s.run(nil, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code == 0 || !strings.Contains(out, "db labeled devopsy.data.copy=custom: the child's release needs an executable .devopsy/capabilities/data/import") || strings.Contains(docker, "stop db") || strings.Contains(out, "devopsy: copying") || strings.Contains(out, "prepare child") {
		t.Errorf("no import on the child (%d):\n%s\n%s", code, out, docker)
	}
	if err := os.Rename(childImport+".off", childImport); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "capabilities", "data", "export")); err != nil {
		t.Fatal(err)
	}
	out, code = s.run(nil, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code == 0 || !strings.Contains(out, "db labeled devopsy.data.copy=custom: the parent's release needs an executable .devopsy/capabilities/data/export") || !strings.Contains(out, "aborting the sync") ||
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
	// An import into a service the child does not copy as custom refuses.
	cmd := exec.Command(binary, "--sync-services", "import", "app")
	cmd.Dir = prod
	cmd.Env = append(append([]string{}, s.env...), "PWD="+prod)
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "the child's app is not labeled devopsy.data.copy=custom") {
		t.Errorf("import of an rsync service: %v\n%s", err, out)
	}
}

// What --sync refuses before anything starts.
func TestSyncGuards(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

	// --release-no-sync inherits nothing.
	write(t, filepath.Join(shop, "a", "prod", "shared", ".env"), "SHARED_KEY=s2\n", 0o600)
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

	// What the parent's current release names decides, never the
	// checkout's: one naming another key of the parent's takes nothing
	// more, and one naming none still inherits the deployed keys.
	config := readString(filepath.Join(s.dot, "config.yaml"))
	write(t, filepath.Join(shop, "a", "prod", "shared", ".env"), "SHARED_KEY=s2\nOTHER_KEY=secret\n", 0o600)
	for _, inherit := range []string{"{inherit: [OTHER_KEY]}", "{}"} {
		write(t, filepath.Join(s.dot, "config.yaml"), strings.Replace(config, "{inherit: [SHARED_KEY, INST_KEY, PROJ_KEY, MISSING_KEY]}", inherit, 1), 0o644)
		if out, code := s.run(nil, "@a/dev", "--inherit"); code != 0 || !strings.Contains(out, "devopsy: inherited SHARED_KEY, INST_KEY, PROJ_KEY from") || readString(childInherit) != "SHARED_KEY='s2'\nINST_KEY='i 1'\nPROJ_KEY='p1'\n" {
			t.Errorf("--inherit, the checkout's prod %s (%d): %q\n%s", inherit, code, readString(childInherit), out)
		}
	}
	write(t, filepath.Join(s.dot, "config.yaml"), config, 0o644)

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
	pending := filepath.Join(shop, "a", "dev", ".devopsy-sync.inherit.*")
	if left, _ := filepath.Glob(pending); len(left) > 0 {
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
	if left, _ := filepath.Glob(pending); len(left) > 0 {
		t.Errorf("the pending file stays after a failed sync: its abort removes it")
	}
}

// --sync reruns the run step of what is deployed on the child: the
// release's own config on the server decides, as for a rollback or a
// restart, never the checkout, which may be another branch.
func TestSyncRunStepDeployed(t *testing.T) {
	t.Parallel()
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

// --sync --accept-image, the fix SyncCheck's refusal names, reaches the
// holder's img sync-check with the services named, each once.
func TestSyncAcceptReachesHolder(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	scripts := filepath.Join(t.TempDir(), "scripts")
	if out, code := s.run([]string{"FAKE_SSH_SCRIPTS=" + scripts}, "@dev", "--sync", "--yes", "--accept-image", "db,app", "--accept-image", "db"); code != 0 {
		t.Fatalf("--sync --accept-image (%d):\n%s", code, out)
	}
	holder := ""
	for _, sc := range strings.Split(readString(scripts), "----\n") {
		if strings.Contains(sc, "img sync-check") {
			holder = sc
		}
	}
	if holder == "" {
		t.Fatalf("no holder session:\n%s", readString(scripts))
	}
	// Inside sh -c's quotes: 'db app' quoted again.
	if !strings.Contains(holder, remote.ImagesAcceptEnv+`='\''db app'\''`) {
		t.Errorf("the holder's img sync-check does not get --accept-image's services (%s):\n%s", remote.ImagesAcceptEnv, holder[:min(600, len(holder))])
	}
}

// Interrupted during an export, devopsy ends the push session first, then
// aborts: the export is ended, its spool removed, both sides' stopped
// services come back, nothing is imported and the run step does not run.
func TestSyncInterrupt(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	started := filepath.Join(t.TempDir(), "started")
	caps := filepath.Join(s.dot, "capabilities", "data")
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho partial\ntouch \"$FAKE_STARTED\"\nexec sleep 20\n", 0o755)
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\necho \"IMPORT RAN $(cat)\"\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()
	s.labels("FAKE_DB=stream+own")

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
	for start := time.Now(); ; {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Since(start) > startWait {
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

// Interrupted before the copy (here in the child's prepare), devopsy
// answers abort and waits, as for a release: the holder finishes in the
// session, which shows it, nothing is copied and the child is no partial
// copy.
func TestSyncInterruptPrepare(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	started := filepath.Join(t.TempDir(), "started")
	caps := filepath.Join(s.dot, "capabilities", "data")
	write(t, filepath.Join(caps, "prepare"), "#!/bin/sh\nif [ \"$1\" = child ] && [ -n \"${FAKE_STARTED:-}\" ]; then touch \"$FAKE_STARTED\"; sleep 2; fi\n", 0o755)
	write(t, filepath.Join(caps, "finish"), "#!/bin/sh\necho \"finished $1 $2\"\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()

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
	for start := time.Now(); ; {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Since(start) > startWait {
			_ = cmd.Process.Kill()
			t.Fatalf("the prepare never started:\n%s", readString(outFile.Name()))
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	out := readString(outFile.Name())
	dev := filepath.Join(s.root, "shop", "dev")
	if cmd.ProcessState.ExitCode() != 130 || !strings.Contains(out, "aborting the sync into @devopsy@one:dev") || !strings.Contains(out, "finished child abort") || strings.Contains(out, "copying @") || strings.Contains(out, "deploy in shop-dev") {
		t.Fatalf("interrupt (%d):\n%s", cmd.ProcessState.ExitCode(), out)
	}
	if strings.Contains(readString(filepath.Join(dev, ".devopsy-sync.log")), "no answer") || fileExists(filepath.Join(dev, remote.SyncIncompleteFile)) {
		t.Errorf("the holder lost its session, or marked dev:\n%s", readString(filepath.Join(dev, ".devopsy-sync.log")))
	}
}

// Labels opt in: without any, nothing is copied or touched; a sync stops
// on each side its own services (devopsy.data.stop=own) and what depends
// on them, the child its import ones too, and restores what was stopped.
func TestSyncLabels(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()

	out, code := s.run([]string{"FAKE_APP=none", "FAKE_DB=none"}, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(out, "nothing to sync: no service has data a copy carries (devopsy.data=true, devopsy.data.copy not none)") || !strings.Contains(out, "deploy in shop-dev") ||
		strings.Contains(docker, " stop ") || strings.Contains(docker, "rsync") {
		t.Fatalf("no labels (%d):\n%s\n%s", code, out, docker)
	}

	// db own, app depending on it: both down on both sides, said with
	// why, in one compose stop.
	out, code = s.run(nil, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	rest := out
	for _, want := range []string{"devopsy: stopping db (own), app (depends on db)", "devopsy: copying", "devopsy: stopping db (own), app (depends on db)", "rsync mnt/db", "rsync mnt/storage", "devopsy: starting db app", "devopsy: starting db app", "deploy in shop-dev"} {
		i := strings.Index(rest, want)
		if code != 0 || i < 0 {
			t.Fatalf("missing %q in order (%d):\n%s", want, code, out)
		}
		rest = rest[i+len(want):]
	}
	if strings.Count(docker, " stop db app\n") != 2 || strings.Contains(docker, "pause") {
		t.Errorf("db and its app stopped:\n%s", docker)
	}
	// Both own: the union, one compose stop, Compose orders them.
	out, code = s.run([]string{"FAKE_APP=mnt+own"}, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 || strings.Count(out, "devopsy: stopping db (own), app (own)") != 2 || strings.Count(docker, " stop db app\n") != 2 {
		t.Errorf("both stopped (%d):\n%s\n%s", code, out, docker)
	}
	// An app labeled import: down on the child, where data is imported,
	// running on the parent, which only exports.
	out, code = s.run([]string{"FAKE_APP=mnt+import", "FAKE_DB=mnt"}, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 || strings.Count(out, "devopsy: stopping app (import)") != 1 || !strings.Contains(docker, "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" stop app\n") || strings.Contains(docker, "shop-prod compose -f "+filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml")+" stop") {
		t.Errorf("an import app (%d):\n%s\n%s", code, out, docker)
	}

	// A db down before the sync (a maintenance stop) stays down: restoring
	// brings back what was recorded, and the record goes afterwards. Its
	// running app still stops with it.
	out, code = s.run([]string{"FAKE_DB_DOWN=1"}, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code != 0 || strings.Contains(out, "stopping db") || !strings.Contains(out, "stopping app (depends on db)") || strings.Contains(out, "starting db") || strings.Contains(docker, "start db") {
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
	// the record says; without one (a lost file) the child side's set.
	// On abort, the streams' spools go.
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

	// An app with data mounting mnt whole is refused where labels are
	// read, before anything stops: mnt whole is no entry.
	out, code = s.run([]string{"FAKE_APP_MOUNT=", "FAKE_DB=none"}, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code == 0 || !strings.Contains(out, "service app: devopsy.data=true mounts ./mnt whole: mount its entries (./mnt/<name>)") || strings.Contains(docker, " stop ") || strings.Contains(docker, "rsync") {
		t.Errorf("mnt whole (%d):\n%s\n%s", code, out, docker)
	}
	// A value devopsy does not know is an error, before anything stops;
	// so is a label from before devopsy.data on the child alone: its
	// prepare refuses, naming what replaces it.
	out, code = s.run([]string{"FAKE_APP=pause"}, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code == 0 || !strings.Contains(out, "service app: devopsy.data=pause: true or false") || strings.Contains(docker, " stop ") {
		t.Errorf("unknown value (%d):\n%s\n%s", code, out, docker)
	}
	write(t, s.log+".shop-dev", "FAKE_APP=old\n", 0o644)
	out, code = s.run(nil, "@dev", "--sync", "--yes")
	docker, _ = s.logs()
	if code == 0 || !strings.Contains(out, "service app: devopsy.sync=rsync: that label is now devopsy.data=true (rsync is the default devopsy.data.copy=volumes)") || strings.Contains(docker, " stop ") || parentChanged(docker) {
		t.Errorf("an old label on the child (%d):\n%s\n%s", code, out, docker)
	}
	os.Remove(s.log + ".shop-dev")
}

// What a sync stopped comes back when a later sync completes, as its
// record says, whatever the labels say by then (a label from an inherited
// variable can change in that sync): db, stopped by a sync left
// partial, starts once a sync completes, labeled own or not.
func TestSyncRestoresRecordedStops(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	if out, code := s.run([]string{"FAIL_RSYNC=23"}, "@dev", "--sync", "--yes"); code == 0 || !strings.Contains(out, "db app stay down") {
		t.Fatalf("a cut copy (%d):\n%s", code, out)
	}
	s.labels("FAKE_DB=mnt")
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

// A service the parent rsyncs that the child leaves running (custom, no
// copy or no label there) fails the push before the parent stops
// anything: the child restores its services; volumes on the child says
// running is fine.
func TestSyncMismatch(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "capabilities", "data", "import"), "#!/bin/sh\ncat >/dev/null\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()
	for _, label := range []string{"stream", "none", "keep"} {
		write(t, s.log+".shop-dev", "FAKE_DB="+label+"\nFAKE_APP=mnt+own\n", 0o644)
		out, code := s.run(nil, "@dev", "--sync", "--yes")
		docker, _ := s.logs()
		want := "copied as devopsy.data.copy=custom"
		if label != "stream" {
			want = "copied as nothing (no data, or devopsy.data.copy=none)"
		}
		if code == 0 || !strings.Contains(out, "devopsy: error: the child's data labels do not match the parent's: nothing changed\ndevopsy: | the child's db is running, "+want+", and mnt/db would change under it: the parent copies it for db (devopsy.data.copy=volumes)\n") ||
			strings.Contains(out, "deploy in shop-dev") || parentChanged(docker) || !strings.Contains(docker, "shop-prod compose -f "+filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "compose.yaml")+" --profile * config") ||
			strings.Contains(docker, "rsync") || !strings.Contains(docker, "shop-dev compose -f "+filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")+" start app") {
			t.Errorf("child db %s (%d):\n%s\n%s", label, code, out, docker)
		}
	}
	write(t, s.log+".shop-dev", "FAKE_DB=mnt\n", 0o644)
	out, code := s.run(nil, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(out, "deploy in shop-dev") || !strings.Contains(docker, "/src/db/ /dst/db/") {
		t.Errorf("child db rsync (%d):\n%s\n%s", code, out, docker)
	}
	// A stream on one side only is refused before the parent stops
	// anything, either way round, with the way out: an inherited label
	// needs --inherit before the sync, which only writes it after the copy.
	write(t, filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "capabilities", "data", "export"), "#!/bin/sh\necho dump\n", 0o755)
	inherit := "label it alike on both sides (a variable the parent's inherit: names keeps them so; the child's own shared/.env wins over it)"
	for _, c := range []struct{ prod, dev, want string }{
		{"FAKE_DB=stream", "FAKE_DB=mnt+own", "devopsy: | the parent streams db (devopsy.data.copy=custom) and the child copies it as devopsy.data.copy=volumes: its import would refuse it\n"},
		{"FAKE_DB=mnt+own", "FAKE_DB=stream+own", "devopsy: | the child streams db (devopsy.data.copy=custom) and the parent copies it as files (devopsy.data.copy=volumes): its import would never run\n"},
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

// The child's list (name, label as <copy>+<stop word>, state, entries,
// and the import mark's service), and the guard on it: only a running
// service not copied as volumes conflicts; a stop word differing between
// the sides is named.
func TestChildServicesParse(t *testing.T) {
	t.Parallel()
	// The last service mounts nothing: its line ends in a tab.
	got, mark, restore := parseChildServices("app\tvolumes+import\texited\tstorage\ndb\tcustom+never\trunning\tdb\nworker\tnone+never\trunning\tdb\n!import\tdb\nbad\ncache\tnone+never\tnone\t\n!restore\tdb\tdb\tdbdata\n")
	if len(restore) != 1 || restore[0].Name != "db" || strings.Join(restore[0].Dirs, " ") != "db" || strings.Join(restore[0].Volumes, " ") != "dbdata" {
		t.Errorf("a restore's mark: %+v", restore)
	}
	want := []childService{
		{Name: "app", Copy: "volumes", Stop: "import", State: "exited", Dirs: []string{"storage"}},
		{Name: "db", Copy: "custom", Stop: "never", State: "running", Dirs: []string{"db"}},
		{Name: "worker", Copy: "none", Stop: "never", State: "running", Dirs: []string{"db"}},
		{Name: "cache", Copy: "none", Stop: "never", State: "none"},
	}
	if len(got) != len(want) || mark != "db" {
		t.Fatalf("parsed: %+v %q", got, mark)
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].Copy != want[i].Copy || got[i].Stop != want[i].Stop || got[i].State != want[i].State || strings.Join(got[i].Dirs, " ") != strings.Join(want[i].Dirs, " ") {
			t.Errorf("%d: %+v, want %+v", i, got[i], want[i])
		}
	}
	parent := []cli.SyncService{{Name: "db", Copy: cli.CopyVolumes, Stop: cli.StopOwn, Dirs: []string{"db"}, HasData: true}, {Name: "app", Copy: cli.CopyVolumes, Stop: cli.StopNever, HasData: true}}
	conflicts := syncConflicts(parent, got)
	// db (custom, running) and worker (no copy, running) conflict; the
	// stopped app does not.
	if len(conflicts) != 2 || !strings.Contains(conflicts[0], "the child's db is running, copied as devopsy.data.copy=custom") || !strings.Contains(conflicts[1], "the child's worker is running, copied as nothing (no data, or devopsy.data.copy=none)") {
		t.Errorf("conflicts: %q", conflicts)
	}
	if m := streamMismatches(parent, nil, got); len(m) != 1 || !strings.Contains(m[0], "the child streams db (devopsy.data.copy=custom) and the parent copies it as files (devopsy.data.copy=volumes)") {
		t.Errorf("mismatches: %q", m)
	}
	if m := streamMismatches([]cli.SyncService{{Name: "db", Copy: cli.CopyCustom}}, []string{"db", "gone"}, got); len(m) != 1 || !strings.Contains(m[0], "the child has no service gone") {
		t.Errorf("stream mismatches: %q", m)
	}
	if m := stopMismatches(parent, got); len(m) != 2 || m[0] != "app is devopsy.data.stop=never on the parent and import on the child, which stops it by its own" || !strings.HasPrefix(m[1], "db is devopsy.data.stop=own on the parent and never") {
		t.Errorf("stop mismatches: %q", m)
	}
	// The seventh field: a service whose entries and volumes the child
	// shares with the parent by reflinks; a list without it shares none.
	shared, _, _ := parseChildServices("app\tvolumes+never\trunning\tstorage\t\tuploads\treflink\ndb\tvolumes+own\trunning\tdb\t\t\t\n")
	if got := childShares(shared); len(got) != 2 || !got["storage"] || !got["uploads"] {
		t.Errorf("shares: %v", got)
	}
	if got := childShares(want); len(got) != 0 {
		t.Errorf("shares without the field: %v", got)
	}
}

// Interrupted during the copy, the parent's push stops at once, though
// its session outlives devopsy's ssh: the closed stdin tells it. It
// removes the rsync container, restores its services, and never runs the
// export of the stream services.
func TestSyncInterruptCopy(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	tmp := t.TempDir()
	started, pushed := filepath.Join(tmp, "started"), filepath.Join(tmp, "pushed")
	write(t, filepath.Join(s.dot, "capabilities", "data", "export"), "#!/bin/sh\ntouch \"$FAKE_PUSHED\"\n", 0o755)
	write(t, filepath.Join(s.dot, "capabilities", "data", "import"), "#!/bin/sh\ncat >/dev/null\n", 0o755)
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
	s.labels("FAKE_APP=mnt+own\nFAKE_DB=stream")
	cmd.Env = append(append([]string{}, s.env...), "FAKE_SLOW_RSYNC="+started, "FAKE_PUSHED="+pushed, "FAKE_SSH_DETACH=1")
	cmd.Stdin = strings.NewReader("")
	cmd.Stdout, cmd.Stderr = outFile, outFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); ; {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Since(start) > startWait {
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
	t.Parallel()
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
	t.Parallel()
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

// A sync whose push's connection is lost (ssh's 255) after a copy that
// completed: what the copy wrote decides, so the child goes on, and the
// operator is told that, never that the sync aborts. Found by hand
// (2026-10-27): "the copy ... failed (255): aborting the sync", then the
// child's finish and deploy ran. --status says it synced: the child
// records its last completed sync itself (remote.SyncedFile), where the
// operator's log, which said it before, has no copy line for a session
// lost (found by hand, 2026-10-27: "never synced" after a sync that
// completed).
func TestSyncPushLostSaysCopyDecides(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	out, _ := s.run([]string{"FAKE_SSH_LOST_PUSH=1"}, "@dev", "--sync", "--yes")
	if strings.Contains(out, "aborting the sync") || !strings.Contains(out, "what the copy wrote decides how the sync ends") || fileExists(filepath.Join(s.root, "shop", "dev", remote.SyncIncompleteFile)) {
		t.Errorf("a push lost after its copy completed:\n%s", out)
	}
	if out, code := s.run(nil, "@dev", "--status"); code != 0 || !strings.Contains(out, "parent      prod  last synced just now") {
		t.Errorf("--status after a sync whose push was lost once its copy completed (%d):\n%s", code, out)
	}

	// A sync that does not complete (its finish refuses the copy) is no
	// sync to --status.
	os.Remove(filepath.Join(s.root, "shop", "dev", remote.SyncedFile))
	write(t, filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "capabilities", "data", "finish"), "#!/bin/sh\n[ \"$1 $2\" != \"child go\" ] || exit 3\n", 0o755)
	if out, code := s.run(nil, "@dev", "--sync", "--yes"); code == 0 {
		t.Fatalf("a sync whose finish child go refuses (%d):\n%s", code, out)
	}
	if out, _ := s.run(nil, "@dev", "--status"); !strings.Contains(out, "never synced") {
		t.Errorf("--status after a sync whose finish refused it:\n%s", out)
	}
}

// --status: the containers and the status capability's lines, locally;
// with a target, the release's record and the last sync first.
func TestStatus(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "capabilities", "status", "show"), "#!/bin/sh\necho \"url https://$COMPOSE_PROJECT_NAME.example\"\n", 0o755)
	out, code := s.run([]string{"FAKE_DB_DOWN=1"}, "--status")
	if code != 0 || !strings.Contains(out, "containers\n") || !strings.Contains(out, "  app ") || !strings.Contains(out, "  db ") || !strings.Contains(out, "exited") || !strings.Contains(out, "url https://shop.example\n") {
		t.Fatalf("local status (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@prod", "--status"); code != 0 || !strings.Contains(out, "@devopsy@one:prod (devopsy@one:shop/prod)") || !strings.Contains(out, "release     none: not released yet") {
		t.Fatalf("unreleased status (%d):\n%s", code, out)
	}
	// A current that is a directory (a failed release's containers
	// restarting, Docker making their bind mounts under it as root) is
	// none, as before EnvStatus: --status ends there (review).
	if err := os.MkdirAll(filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "mnt", "storage"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, code := s.run(nil, "@prod", "--status"); code != 0 || !strings.HasSuffix(out, "release     none: not released yet\n") {
		t.Fatalf("a current that is a directory (%d):\n%s", code, out)
	}
	os.RemoveAll(filepath.Join(s.root, "shop", "prod", "current"))
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

// A restore under way holds the release lock with its mark there, from
// its first write to its run step: --status says it is restoring, not
// that it did not complete (review).
func TestStatusRestoreRunning(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	base := filepath.Join(s.root, "shop", "prod")
	write(t, filepath.Join(base, remote.SyncIncompleteFile), remote.IncompleteLine("2026-10-27 11:00:00", "20261020000000", remote.IncompleteRestore, "restoring"), 0o644)
	// The release lock held: the probes of it (LockBusy, flock -n -s
	// .lock) fail.
	bin := filepath.Join(t.TempDir(), "bin")
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\ncase \"$*\" in \"-n -s \"*\"/.lock true\") exit 1 ;; esac\nexec "+filepath.Join(filepath.Dir(s.root), "bin", "flock")+" \"$@\"\n", 0o755)
	out, code := s.run([]string{"PATH=" + bin + ":" + filepath.Join(filepath.Dir(s.root), "bin") + ":/usr/bin:/bin"}, "@prod", "--status")
	if code != 0 || strings.Contains(out, "did not complete") || !strings.Contains(out, "restoring snapshot 20261020000000") {
		t.Errorf("--status during a restore (%d):\n%s", code, out)
	}
}

// A release never takes over another project's network: the networks a
// project names are its own, or external.
func TestReleaseNetworkOwned(t *testing.T) {
	t.Parallel()
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
// data capability's scripts, a mnt service as a tar of its entries, what
// its stop label says (and what depends on it) stopped around it; stdin
// spooled first, and nothing changed unless it is complete.
func TestExportImport(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	imported := filepath.Join(t.TempDir(), "imported")
	caps := filepath.Join(s.dot, "capabilities", "data")
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho \"dump of $1 from $COMPOSE_PROJECT_NAME custom=$DEVOPSY_DATA_CUSTOM stop=$DEVOPSY_DATA_STOP\"\n", 0o755)
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\n{ echo \"import $1 into $COMPOSE_PROJECT_NAME\"; cat; } > \"$FAKE_IMPORTED\"\n", 0o755)
	env := []string{"FAKE_IMPORTED=" + imported}
	spools := func(dir string) []string {
		m, _ := filepath.Glob(filepath.Join(dir, ".devopsy-sync.stream.*"))
		return m
	}

	// A stream, own: stopped around the export with what depends on it,
	// said with why, its stdout the data.
	stopped := append(env, "FAKE_DB=stream+own")
	out, errs, code := s.runIO(stopped, nil, "--export", "db")
	docker, _ := s.logs()
	if code != 0 || string(out) != "dump of db from shop custom=db stop=db app\n" || !strings.Contains(errs, "devopsy: stopping db (own), app (depends on db)") || !strings.Contains(errs, "devopsy: starting db app") {
		t.Fatalf("--export stream (%d): %q\n%s", code, out, errs)
	}
	if i, j := strings.Index(docker, " stop db app"), strings.Index(docker, " start db app"); i < 0 || j < i {
		t.Errorf("stopped around the export:\n%s", docker)
	}
	// Into a stream: spooled, then the import with it as stdin; a stopped
	// service is never brought up, a running one is, alone.
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
	if docker, _ := s.logs(); !strings.Contains(docker, "up -d --wait --no-deps db") || strings.Contains(docker, " stop db") {
		t.Errorf("running stream import:\n%s", docker)
	}
	// A stream database imported, its app import: the app down for the
	// load and back after it, the database up alone; a worker depending
	// on it without a stop label is named, left running.
	os.Remove(imported)
	_, errs, code = s.runIO(append(env, "FAKE_DB=stream", "FAKE_APP=mnt+import", "FAKE_WORKER=none"), []byte("rows\n"), "--import", "db")
	docker, _ = s.logs()
	if code != 0 || !strings.Contains(errs, "devopsy: stopping app (import)\n") || !strings.Contains(errs, "devopsy: warning: worker depends on db and keeps running while it is imported: label it devopsy.data.stop=import, or never to keep it running") || readString(imported) != "import db into shop\nrows\n" {
		t.Fatalf("an import app (%d):\n%s", code, errs)
	}
	inOrder := func(what, log string, want ...string) {
		t.Helper()
		rest := log
		for _, w := range want {
			i := strings.Index(rest, w)
			if i < 0 {
				t.Errorf("%s: missing %q in order:\n%s", what, w, log)
				return
			}
			rest = rest[i+len(w):]
		}
	}
	inOrder("an import app", docker, " stop app\n", " up -d --wait --no-deps db\n", " start app\n")
	if strings.Contains(docker, "stop db") || strings.Contains(docker, "stop app worker") {
		t.Errorf("an import app, docker:\n%s", docker)
	}
	// An explicit never silences it.
	if _, errs, _ := s.runIO(append(env, "FAKE_DB=stream", "FAKE_APP=mnt+never", "FAKE_WORKER=-+never"), []byte("rows\n"), "--import", "db"); strings.Contains(errs, "keeps running") {
		t.Errorf("never, still named:\n%s", errs)
	}
	s.logs()
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
	if code != 0 || !strings.Contains(docker, "run --rm --name devopsy-export-") || !strings.Contains(docker, "--mount type=bind,source="+mnt+"/storage,target=/x/storage,readonly devopsy-util:3 sh -c ") || !strings.Contains(docker, " sh storage\n") {
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
	// An entry that is a symlink here is refused both ways, before
	// anything stops: tar carries only the link (an export that succeeds
	// empty), and the image's /mnt never reaches its target.
	real := filepath.Join(mnt, "storage.real")
	if err := os.Rename(filepath.Join(mnt, "storage"), real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("storage.real", filepath.Join(mnt, "storage")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--export", "app"}, {"--import", "app"}} {
		_, errs, code := s.runIO(nil, tarOf(t, map[string]string{"storage/new.txt": "new\n"}), args...)
		if docker, _ := s.logs(); code == 0 || !strings.Contains(errs, "mnt/storage is a symlink") || strings.Contains(docker, "run --rm --name devopsy-") || strings.Contains(docker, " stop ") {
			t.Errorf("%s of a symlinked entry (%d):\n%s\n%s", args[0], code, errs, docker)
		}
	}
	os.Remove(filepath.Join(mnt, "storage"))
	if err := os.Rename(real, filepath.Join(mnt, "storage")); err != nil {
		t.Fatal(err)
	}
	// Tars made member by member. A directory needs its x bit: GNU tar
	// sets a directory's mode once it leaves its members, by name, so
	// ./storage at 0644 refused storage/dir/htpasswd on Linux, where
	// macOS's bsdtar sets modes at the end.
	member := func(tw *tar.Writer, h *tar.Header, data string) {
		h.Mode, h.Size = 0o644, int64(len(data))
		if h.Typeflag == tar.TypeDir {
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	reaching := func(members func(tw *tar.Writer)) []byte {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		members(tw)
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	// Its import empties each entry the tar carries, then extracts its
	// members, in a container that mounts that entry alone, without a
	// network, never all of mnt (review of 2026-10-23, round 3): the
	// service whose entry it is runs meanwhile unless its stop label
	// says otherwise (never, the default, here), and its owner can swap a
	// directory the extraction just made for a link to ../<another
	// entry> (renameat2's RENAME_EXCHANGE, or a link in place of a
	// directory a tar implies without a member of its own, as tarOf's
	// do): GNU tar follows it and writes the tar's files, as root, into
	// that entry. Checked on alpine 3.22's tar with the app as 10001
	// without capabilities, mounting only its entry: auth/htpasswd of
	// another entry replaced with the tar's. Members are selected by the
	// names the archive gives the entry (GNU tar matches ./storage/x only
	// by ./storage), each read from the whole spool.
	write(t, filepath.Join(mnt, "auth", "htpasswd"), "admin:secret\n", 0o644)
	dotted := reaching(func(tw *tar.Writer) {
		member(tw, &tar.Header{Name: "./storage/", Typeflag: tar.TypeDir}, "")
		member(tw, &tar.Header{Name: "./storage/new.txt", Typeflag: tar.TypeReg}, "new\n")
		member(tw, &tar.Header{Name: "storage/dir/htpasswd", Typeflag: tar.TypeReg}, "pwned\n")
	})
	if _, errs, code := s.runIO(nil, dotted, "--import", "app"); code != 0 || readString(filepath.Join(mnt, "storage", "new.txt")) != "new\n" || readString(filepath.Join(mnt, "auth", "htpasswd")) != "admin:secret\n" {
		t.Fatalf("--import rsync (%d):\n%s", code, errs)
	}
	if _, err := os.Stat(filepath.Join(mnt, "storage", "old.txt")); err == nil {
		t.Errorf("the entry was not emptied first")
	}
	// The spool mounted read-only and read there, never on the
	// container's stdin (Docker's stream is slow).
	if docker, _ := s.logs(); !regexp.MustCompile(`run --rm --name devopsy-import-shop-[0-9]+-extract-0 --network none --mount type=bind,source=`+regexp.QuoteMeta(mnt)+`/storage,target=/mnt/storage --mount type=bind,source=[^ ]*/\.devopsy-sync\.stream\.[^ ,]*,target=/in/\.devopsy-sync\.stream\.[^ ,]*,readonly devopsy-util:3 sh -c `).MatchString(docker) || !strings.Contains(docker, `*) exec tar -C /mnt -xf "$src" --numeric-owner --overwrite --no-wildcards -- "$@" ;;`) || !regexp.MustCompile(` sh /in/\.devopsy-sync\.stream\.[^ ]* storage \./storage storage\n`).MatchString(docker) || strings.Contains(docker, "-extract-0 -i ") || strings.Contains(docker, ":/mnt devopsy-util") || strings.Contains(docker, " stop app") || strings.Contains(docker, "-owners-") {
		t.Errorf("rsync import:\n%s", docker)
	}
	// An entry that is a file: made empty when missing, for its
	// container to mount, then written in place; one the tar carries as
	// the other kind is refused before anything changes (the mount point
	// could not be replaced).
	file := []string{"FAKE_APP_MOUNT=/app.ini"}
	if _, errs, code := s.runIO(file, tarOf(t, map[string]string{"app.ini": "[x]\n"}), "--import", "app"); code != 0 || readString(filepath.Join(mnt, "app.ini")) != "[x]\n" {
		t.Errorf("--import of a file entry (%d):\n%s", code, errs)
	}
	if docker, _ := s.logs(); !strings.Contains(docker, " --network none --mount type=bind,source="+mnt+"/app.ini,target=/mnt/app.ini --mount type=bind,source=") || !regexp.MustCompile(` sh /in/[^ ]* app\.ini app\.ini\n`).MatchString(docker) {
		t.Errorf("--import of a file entry:\n%s", docker)
	}
	if _, errs, code := s.runIO(file, tarOf(t, map[string]string{"app.ini/x": "x"}), "--import", "app"); code == 0 || !strings.Contains(errs, "the tar carries mnt/app.ini as a directory, and it is a file here: nothing imported") || readString(filepath.Join(mnt, "app.ini")) != "[x]\n" {
		t.Errorf("--import of a directory over a file entry (%d):\n%s", code, errs)
	}
	s.logs()
	// An entry its labels own goes to its owner once extracted, before
	// the import counts as complete (a tar made anywhere carries its
	// maker's owners), in a container that mounts that entry alone. Not
	// on macOS, as before up (review of 2026-10-23, round 2): Docker
	// Desktop's file sharing may refuse the chown, and any container
	// user writes the Mac's files anyway.
	_, errs, code = s.runIO([]string{"FAKE_APP_OWNER=10001:10001"}, tarOf(t, map[string]string{"storage/new.txt": "new\n"}), "--import", "app")
	docker, _ = s.logs()
	owners := regexp.MustCompile(`run --rm --name devopsy-import-shop-[0-9]+-owners-0 --network none --mount type=bind,source=` + regexp.QuoteMeta(mnt) + `/storage,target=/mnt/storage devopsy-util:3 sh -c set -eu`).MatchString(docker)
	if code != 0 || owners != (runtime.GOOS != "darwin") || strings.Contains(errs, "devopsy: giving mnt/storage to 10001:10001\n") != (runtime.GOOS != "darwin") || strings.Contains(docker, ":/mnt devopsy-util") {
		t.Errorf("--import of an owned entry (%d), the owners' container %v:\n%s\n%s", code, owners, errs, docker)
	}
	// A truncated tar, or one carrying what the service does not mount,
	// changes nothing.
	whole := tarOf(t, map[string]string{"storage/other.txt": strings.Repeat("x", 2000)})
	if _, errs, code := s.runIO(nil, whole[:700], "--import", "app"); code == 0 || !strings.Contains(errs, "stdin is not a complete tar") || readString(filepath.Join(mnt, "storage", "new.txt")) != "new\n" {
		t.Errorf("truncated tar (%d):\n%s", code, errs)
	}
	if _, errs, code := s.runIO(nil, tarOf(t, map[string]string{"db/x": "x"}), "--import", "app"); code == 0 || !strings.Contains(errs, "the tar carries db, which app neither bind-mounts from mnt (storage) nor mounts as a volume (none): nothing imported") {
		t.Errorf("foreign entry (%d):\n%s", code, errs)
	}
	if docker, _ := s.logs(); strings.Contains(docker, "devopsy-import-") && strings.Contains(docker, "sh -c") {
		t.Errorf("extracted a refused tar:\n%s", docker)
	}
	if m := spools(s.dot); len(m) > 0 {
		t.Errorf("spools left: %v", m)
	}
	// A tar whose members reach past the entries it names is refused
	// before anything is emptied, as one naming a foreign entry is. GNU
	// tar (the util image's, checked on alpine 3.22) extracts each of
	// these through to another entry, or refuses it only after the
	// entries were emptied, or empties the container's root:
	//
	// - storage/../db/x: tar refuses the member ("contains '..'") once
	//   mnt/storage is emptied: the data gone, nothing imported.
	// - storage as a symlink to db, then storage/x: tar removes the
	//   emptied mnt/storage (a running container keeps the inode it
	//   bind-mounts), links it to db and writes db/x.
	// - storage/h a hard link to db/secret, then storage/h: --overwrite
	//   writes through the link, into db's file.
	// - ../x for a service mounting mnt whole: tarEntries lists "..",
	//   which nothing checks then, and the emptying step runs on
	//   /mnt/.., the container's root, mnt included: all of it gone.
	//
	// Bug: tarEntries keeps only each member's first path component and
	// never looks at link targets or member types, and with "." nothing
	// is checked at all. FAIL_TAR keeps the fake from touching files: what
	// counts is that the import's container never runs.
	for _, c := range []struct {
		name  string
		mount string
		tar   []byte
	}{
		{"storage/../db/x", "/storage", reaching(func(tw *tar.Writer) {
			member(tw, &tar.Header{Name: "storage/../db/x", Typeflag: tar.TypeReg}, "evil\n")
		})},
		{"storage a symlink to db", "/storage", reaching(func(tw *tar.Writer) {
			member(tw, &tar.Header{Name: "storage", Typeflag: tar.TypeSymlink, Linkname: "db"}, "")
			member(tw, &tar.Header{Name: "storage/x", Typeflag: tar.TypeReg}, "evil\n")
		})},
		{"storage/h a hard link to db/secret", "/storage", reaching(func(tw *tar.Writer) {
			member(tw, &tar.Header{Name: "storage/h", Typeflag: tar.TypeLink, Linkname: "db/secret"}, "")
			member(tw, &tar.Header{Name: "storage/h", Typeflag: tar.TypeReg}, "evil\n")
		})},
		{"../x", "/storage", reaching(func(tw *tar.Writer) {
			member(tw, &tar.Header{Name: "../x", Typeflag: tar.TypeReg}, "evil\n")
		})},
		// mnt itself: its owner and mode are no entry's.
		{"./", "/storage", reaching(func(tw *tar.Writer) {
			member(tw, &tar.Header{Name: "./", Typeflag: tar.TypeDir}, "")
			member(tw, &tar.Header{Name: "./storage/x", Typeflag: tar.TypeReg}, "fine\n")
		})},
		// Device nodes and FIFOs, anywhere (review of 2026-10-23, round
		// 3): GNU tar as root makes them in shared/mnt on the host, where
		// bind mounts are not nodev, and the owners' step then gives one
		// to the entry's owner, the app's UID: a block device of the
		// server's disk (b 8 0), 0666 in the tar, is the raw disk for any
		// user on the host. A tar of an entry never needs one.
		{"storage/disk a block device", "/storage", reaching(func(tw *tar.Writer) {
			member(tw, &tar.Header{Name: "storage/", Typeflag: tar.TypeDir}, "")
			member(tw, &tar.Header{Name: "storage/disk", Typeflag: tar.TypeBlock, Devmajor: 8}, "")
		})},
		{"storage/tty a character device", "/storage", reaching(func(tw *tar.Writer) {
			member(tw, &tar.Header{Name: "storage/tty", Typeflag: tar.TypeChar, Devmajor: 5}, "")
		})},
		{"storage/pipe a FIFO", "/storage", reaching(func(tw *tar.Writer) {
			member(tw, &tar.Header{Name: "storage/pipe", Typeflag: tar.TypeFifo}, "")
		})},
		// A bind source made a link out of mnt through another link of
		// the tar (review of 2026-10-23, round 4): storage/x -> . never
		// descends, so each x/.. pair path.Clean cancels climbs one
		// directory for real. Judged lexically, storage/w's target was
		// storage/etc/shadow, inside its entry; GNU tar extracts both
		// links (checked in devopsy-util:3, one container mounting
		// storage alone), and on the server `realpath
		// shared/mnt/storage/w` was /etc/shadow, which Docker follows for
		// a bind of mnt/storage/w. A link alone is the data's
		// (TestImportAppAbsoluteLink); the ways a tar's links can lead a
		// bind source are linkAtBind's (TestTarLinks).
		{"storage/w, a bind source, a link out of mnt through storage/x", "/storage/w", reaching(func(tw *tar.Writer) {
			member(tw, &tar.Header{Name: "storage/", Typeflag: tar.TypeDir}, "")
			member(tw, &tar.Header{Name: "storage/x", Typeflag: tar.TypeSymlink, Linkname: "."}, "")
			member(tw, &tar.Header{Name: "storage/w", Typeflag: tar.TypeSymlink, Linkname: strings.Repeat("x/", 12) + strings.Repeat("../", 12) + "etc/shadow"}, "")
		})},
		// Names no plain path, which each entry's extraction could not
		// select its members by, and an entry both a file and a directory.
		{"././storage/x", "/storage", reaching(func(tw *tar.Writer) {
			member(tw, &tar.Header{Name: "././storage/x", Typeflag: tar.TypeReg}, "x\n")
		})},
		{"storage//x", "/storage", reaching(func(tw *tar.Writer) {
			member(tw, &tar.Header{Name: "storage//x", Typeflag: tar.TypeReg}, "x\n")
		})},
		{"storage a file and a directory", "/storage", reaching(func(tw *tar.Writer) {
			member(tw, &tar.Header{Name: "storage", Typeflag: tar.TypeReg}, "x\n")
			member(tw, &tar.Header{Name: "storage/x", Typeflag: tar.TypeReg}, "x\n")
		})},
	} {
		_, errs, code := s.runIO([]string{"FAIL_TAR=1", "FAKE_APP_MOUNT=" + c.mount}, c.tar, "--import", "app")
		if docker, _ := s.logs(); code == 0 || strings.Contains(docker, "run --rm --name devopsy-import-") {
			t.Errorf("a tar reaching out of its entries, %s (%d), the import's container ran:\n%s", c.name, code, errs)
		}
		if !strings.Contains(errs, "devopsy: error: the tar has a member ") || !strings.Contains(errs, ": nothing imported") {
			t.Errorf("a tar reaching out of its entries, %s, not said so:\n%s", c.name, errs)
		}
	}
	// Without data, with data no copy carries, unknown: refused.
	if _, errs, code := s.runIO([]string{"FAKE_APP=none"}, nil, "--export", "app"); code == 0 || !strings.Contains(errs, "app has no data: label it devopsy.data=true (its entries of mnt are copied; devopsy.data.copy=custom for the data capability's scripts)") {
		t.Errorf("no data (%d):\n%s", code, errs)
	}
	for _, flag := range []string{"--export", "--import"} {
		if _, errs, code := s.runIO([]string{"FAKE_APP=keep"}, []byte("x"), flag, "app"); code == 0 || !strings.Contains(errs, "app has data no copy carries (devopsy.data.copy=none): --export and --import leave it out") {
			t.Errorf("%s of a copy of none (%d):\n%s", flag, code, errs)
		}
	}
	if _, errs, code := s.runIO([]string{"FAKE_APP=keep"}, nil, "--export", "nope"); code == 0 || !strings.Contains(errs, "no service nope in the project (with data a copy carries: db)") {
		t.Errorf("unknown (%d):\n%s", code, errs)
	}
	if out, _, code := s.runIO(nil, nil, "--export", "--help"); code != 0 || !strings.Contains(string(out), "Usage: devopsy [@<target>] --export <service>") {
		t.Errorf("--export --help (%d):\n%s", code, out)
	}
	if out, _, code := s.runIO(nil, nil, "@prod", "--import", "--help"); code != 0 || !strings.Contains(string(out), "Usage: devopsy [@<target>] --import <service> [--yes]\n       devopsy @<target> --import <service> [--accept-image <service>] [--yes]\n       devopsy @<target> --import <service> --resolved [--yes]") {
		t.Errorf("@prod --import --help (%d):\n%s", code, out)
	}
	s.logs()

	// An export interrupted: its process group ended, the service back.
	exportStarted, exportStopped := filepath.Join(t.TempDir(), "started"), filepath.Join(t.TempDir(), "stopped")
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\ntrap 'touch \"$FAKE_STOPPED\"; exit 1' TERM\necho partial\ntouch \"$FAKE_STARTED\"\nsleep 20 &\nwait\n", 0o755)
	cmd := exec.Command(binary, "--export", "db")
	cmd.Dir = s.project
	cmd.Env = append(append([]string{}, s.env...), "FAKE_DB=stream+own", "FAKE_STARTED="+exportStarted, "FAKE_STOPPED="+exportStopped)
	var eout, eerr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &eout, &eerr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFile(t, exportStarted, "the export", startWait, func() string { return eerr.String() })
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	waitFile(t, exportStopped, "the export's end", 10*time.Second, func() string { return eerr.String() })
	if docker, _ := s.logs(); cmd.ProcessState.ExitCode() == 0 || !strings.Contains(eerr.String(), "export db: interrupted: what was written is incomplete") || !strings.Contains(docker, " start db app") {
		t.Errorf("interrupted export (%d):\n%s\n%s", cmd.ProcessState.ExitCode(), eerr.String(), docker)
	}
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho \"dump of $1 from $COMPOSE_PROJECT_NAME custom=$DEVOPSY_DATA_CUSTOM stop=$DEVOPSY_DATA_STOP\"\n", 0o755)

	// A failing import (once it wrote) keeps what it stopped down, said
	// with the way to start it; the spool goes. Locally no mark.
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\ncat >/dev/null\n[ -z \"${FAKE_FAIL_IMPORT:-}\" ] || exit 4\n{ echo \"import $1 into $COMPOSE_PROJECT_NAME\"; cat; } > \"$FAKE_IMPORTED\"\n", 0o755)
	if _, errs, code := s.runIO(append(stopped, "FAKE_FAIL_IMPORT=1"), []byte("rows\n"), "--import", "db"); code == 0 || !strings.Contains(errs, "import db: exit status 4") || !strings.Contains(errs, "the import of db did not complete: its data may be partial, and db, app stay down") || !strings.Contains(errs, "$ devopsy start db app") {
		t.Errorf("failing import (%d):\n%s", code, errs)
	}
	if docker, _ := s.logs(); !strings.Contains(docker, " stop db app") || strings.Contains(docker, " start ") {
		t.Errorf("failing import, db and app restarted:\n%s", docker)
	}
	if m := spools(s.dot); len(m) > 0 {
		t.Errorf("spools left after a failing import: %v", m)
	}
	if fileExists(filepath.Join(s.project, remote.SyncIncompleteFile)) || fileExists(filepath.Join(s.dot, remote.SyncIncompleteFile)) {
		t.Errorf("a local import left a mark")
	}
	os.Remove(s.log + ".stopped.shop")
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\n{ echo \"import $1 into $COMPOSE_PROJECT_NAME\"; cat; } > \"$FAKE_IMPORTED\"\n", 0o755)

	// A stop that fails partway (FAIL_STOP: db stopped all the same, as
	// when Ctrl-C reaches docker compose stop, whose request the daemon
	// carries out) still brings db back: an export or an import that
	// stopped it restores it, whatever happened.
	//
	// Bug: stopFor (now stopSet) returned its no-op restore with the stop's error, so
	// db stays down after a failed --export or --import.
	for _, args := range [][]string{{"--export", "db"}, {"--import", "db"}} {
		_, errs, code := s.runIO(append(stopped, "FAIL_STOP=shop"), []byte("rows\n"), args...)
		if docker, _ := s.logs(); code == 0 || !strings.Contains(docker, " stop db") || !strings.Contains(docker, " start db") {
			t.Errorf("%s after a stop that failed (%d), db left down:\n%s\n%s", args[0], code, errs, docker)
		}
		// The fake's memory of what is stopped: db running again for what
		// follows.
		os.Remove(s.log + ".stopped.shop")
	}

	// A stream's import that fails, not stopped for it: what it started
	// in the service may load on, so the service and what depends on it
	// are stopped, never started again, its one-off removed, and the data
	// said partial.
	write(t, s.log+".shop", "FAKE_ONEOFF=db\n", 0o644)
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\ncat >/dev/null\nexit 4\n", 0o755)
	if _, errs, code := s.runIO(append(env, "FAKE_DB=stream"), []byte("rows\n"), "--import", "db"); code == 0 || !strings.Contains(errs, "the import of db did not complete: its data may be partial, and db, app stay down") {
		t.Errorf("a failing import of a running stream (%d):\n%s", code, errs)
	}
	if docker, _ := s.logs(); !strings.Contains(docker, "rm -f shop-db-run-1") || !strings.Contains(docker, " stop db app") || strings.Contains(docker, " start ") {
		t.Errorf("a failing import of a running stream, docker:\n%s", docker)
	}
	write(t, s.log+".shop", "", 0o644)
	os.Remove(s.log + ".stopped.shop")
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\n{ echo \"import $1 into $COMPOSE_PROJECT_NAME\"; cat; } > \"$FAKE_IMPORTED\"\n", 0o755)

	// A stream down before its import is stopped again after it.
	if _, errs, code := s.runIO(append(env, "FAKE_DB=stream", "FAKE_DB_DOWN=1"), []byte("rows\n"), "--import", "db"); code != 0 || !strings.Contains(errs, "devopsy: stopping db again: it was down before") {
		t.Errorf("import of a stream that was down (%d):\n%s", code, errs)
	}
	if docker, _ := s.logs(); !strings.Contains(docker, "up -d --wait --no-deps db") || strings.Index(docker, " stop db") < strings.Index(docker, "up -d --wait --no-deps db") {
		t.Errorf("down stream import:\n%s", docker)
	}
	// Its up failing (FAIL_UP: docker compose up -d --wait, whose
	// container Compose started and left running, unhealthy or late)
	// stops it again too: it was down before, and nothing was imported.
	//
	// Bug: upFor returns its no-op down with the up's error, so a
	// service down on purpose is left running after a failed import.
	if _, errs, code := s.runIO(append(env, "FAKE_DB=stream", "FAKE_DB_DOWN=1", "FAIL_UP=shop"), []byte("rows\n"), "--import", "db"); code == 0 {
		t.Errorf("import of a stream that was down, its up failing (%d):\n%s", code, errs)
	}
	if docker, _ := s.logs(); !strings.Contains(docker, "up -d --wait --no-deps db") || strings.Index(docker, " stop db") < strings.Index(docker, "up -d --wait --no-deps db") {
		t.Errorf("a stream down before its import, its up failing, left up:\n%s", docker)
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
	if code != 0 || string(out) != "dump of db from shop-prod custom=db stop=\n" || !strings.Contains(docker, "flock -n -s 9\nflock -n 8\n") {
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
	// The server's side only imports what ends with the end line, found
	// in the stream (a cut session reaches it as a plain EOF), and only
	// once a byte follows it, the session's go for the first write.
	os.Remove(imported)
	prod := filepath.Join(s.root, "shop", "prod", "current")
	for stdin, want := range map[string]string{"rows\n": "the data stopped before its end", "rows\nEND\x00": "", "rows\nEND": "the session ended before the import started: nothing imported", "END": "nothing on stdin"} {
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

// tarEntries refuses the members whose names would not be where they
// land (review of 2026-10-23, round 4), and takes a symbolic link's
// target as the data, wherever it leads; a bind source through the
// tar's links is judged by where it leads as the host resolves it
// (linkAtBind, tarLinks.walkMnt): through mnt only back into its entry,
// however path.Clean reads the way, a loop refused. A pax global
// header is no member, but GNU tar applies its records (linkpath, path,
// size...) to every member after it, where archive/tar, which the check
// reads the tar with, applies none (checked in devopsy-util:3, GNU tar
// 1.35, 2026-10-24: a global linkpath made storage/x -> sub into storage/x
// -> ../../../../etc): a global header carrying anything but the export's
// own record (images.ExportKey), wherever it is, must be refused, or the
// check judges links GNU tar never writes.
func TestTarLinks(t *testing.T) {
	t.Parallel()
	sym := func(name, target string) *tar.Header {
		return &tar.Header{Name: name, Typeflag: tar.TypeSymlink, Linkname: target, Mode: 0o777}
	}
	global := func(records map[string]string) *tar.Header {
		return &tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: records}
	}
	hard := func(name, target string) *tar.Header {
		return &tar.Header{Name: name, Typeflag: tar.TypeLink, Linkname: target, Mode: 0o644}
	}
	file := func(name string) *tar.Header { return &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644} }
	dir := func(name string) *tar.Header { return &tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755} }
	for _, c := range []struct {
		name    string
		members []*tar.Header
		refused string
		// bind, a bind source under mnt, is refused by linkAtBind with
		// atBind (passes when empty).
		bind, atBind string
	}{
		{"links within their entry", []*tar.Header{
			dir("storage/"), dir("storage/sub/"), file("storage/sub/f"),
			sym("storage/a", "sub/f"), sym("storage/sub/b", "../f"), sym("storage/self", "."),
			sym("storage/c", "self/sub/./f"), sym("storage/d", "sub/"), sym("storage/sub/up", ".."),
			sym("storage/e", "sub/up/sub/f"), sym("storage/dangling", "gone/away"),
			hard("storage/h", "storage/sub/f"), hard("./storage/i", "./storage/sub/f"),
		}, "", "", ""},
		{"links leading anywhere, the data's own", []*tar.Header{
			dir("storage/"), sym("storage/abs", "/app/storage/f"), sym("storage/root", "/"), sym("storage/db", "../db/x"),
		}, "", "", ""},
		{"through a link to the entry, path.Clean inside", []*tar.Header{
			dir("storage/"), sym("storage/x", "."), sym("storage/w", strings.Repeat("x/", 3)+strings.Repeat("../", 3)+"etc/shadow"),
		}, "", "storage/w", "leads out of mnt/storage, above mnt"},
		{"through a link to a parent, path.Clean inside", []*tar.Header{
			dir("storage/sub/"), sym("storage/sub/up", ".."), sym("storage/w", "sub/up/../x"),
		}, "", "storage/w", "leads out of mnt/storage, into mnt/x"},
		{"a link in a subdirectory through another", []*tar.Header{
			dir("storage/sub/"), sym("storage/x", "."), sym("storage/sub/up", "../x/.."),
		}, "", "storage/sub/up", "leads out of mnt/storage, to mnt itself"},
		{"out and back through mnt", []*tar.Header{
			file("storage/b"), sym("storage/a", "../storage/b"),
		}, "", "storage/a", ""},
		{"a loop", []*tar.Header{
			sym("storage/a", "b"), sym("storage/b", "a"),
		}, "", "storage/a", "follows more than 40 links"},
		{"a member below a link", []*tar.Header{
			sym("storage/x", "sub"), dir("storage/sub/"), file("storage/x/f"),
		}, "storage/x/f is below the link storage/x", "", ""},
		{"a link and a directory of one name", []*tar.Header{
			sym("storage/x", "sub"), dir("storage/x/"),
		}, "storage/x is a link and another member of the same name", "", ""},
		{"a hard link to a link", []*tar.Header{
			dir("storage/sub/"), file("storage/f"), sym("storage/sub/s", "../f"), hard("storage/h", "storage/sub/s"),
		}, "storage/h is a hard link to the link storage/sub/s", "", ""},
		{"a hard link below a link", []*tar.Header{
			file("storage/f"), sym("storage/x", "."), hard("storage/h", "storage/x/f"),
		}, "storage/h is a hard link below the link storage/x", "", ""},
		{"the export's record alone in a global header", []*tar.Header{
			global(map[string]string{images.ExportKey: "image\tapp\tmariadb:11.8\tsha256:own\tmariadb@sha256:own\n"}),
			dir("storage/"), sym("storage/x", "sub"),
		}, "", "", ""},
		{"a global linkpath beside the export's record", []*tar.Header{
			global(map[string]string{images.ExportKey: "image\tapp\tmariadb:11.8\tsha256:own\tmariadb@sha256:own\n", "linkpath": "../../../../etc"}),
			dir("storage/"), sym("storage/x", "sub"),
		}, "*", "", ""},
		{"a global linkpath after a member", []*tar.Header{
			dir("storage/"), global(map[string]string{"linkpath": "../../../../etc"}), sym("storage/x", "sub"),
		}, "*", "", ""},
	} {
		var b bytes.Buffer
		tw := tar.NewWriter(&b)
		for _, h := range c.members {
			if err := tw.WriteHeader(h); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		spool := filepath.Join(t.TempDir(), "spool")
		write(t, spool, b.String(), 0o644)
		es, err := tarEntries(spool)
		if c.bind != "" && err == nil {
			err = linkAtBind(es[0], []cli.SyncService{{Name: "app", Binds: []string{c.bind}}})
			if c.atBind == "" && err != nil {
				t.Errorf("%s: a bind of %s refused: %v", c.name, c.bind, err)
			} else if c.atBind != "" && (!errors.Is(err, errOutside) || !strings.Contains(err.Error(), c.atBind)) {
				t.Errorf("%s: a bind of %s: %v, want %q", c.name, c.bind, err, c.atBind)
			}
			continue
		}
		switch {
		case c.refused == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		// "*": refused, however it is said.
		case c.refused == "*" && !errors.Is(err, errOutside) && !errors.Is(err, errMember):
			t.Errorf("%s: %v, want refused", c.name, err)
		case c.refused != "" && c.refused != "*" && (!errors.Is(err, errOutside) || !strings.Contains(err.Error(), c.refused)):
			t.Errorf("%s: %v, want %q", c.name, err, c.refused)
		}
	}
}

// waitFile waits for a file to exist, up to within: startWait for what
// the test then acts on, the behavior's own bound for what it checks.
func waitFile(t *testing.T, path, what string, within time.Duration, out func() string) {
	t.Helper()
	for start := time.Now(); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Since(start) > within {
			t.Fatalf("%s never happened:\n%s", what, out())
		}
	}
}

// A prepare that fails before anything stops restores nothing: a db down
// before the sync stays down, whether the hook fails or the import check
// does; and an own service without a container is only created on a
// first release (no containers at all), never in a project that has
// others.
func TestSyncPrepareFailsKeepsDown(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "capabilities", "data", "prepare"), "#!/bin/sh\n[ \"${FAIL_PREPARE:-}\" != \"$1\" ]\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()
	dev := "shop-dev compose -f " + filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml")
	for _, c := range []struct{ name, labels string }{{"prepare hook", ""}, {"no import", "FAKE_DB=stream+own"}} {
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
	t.Parallel()
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

// A service labeled devopsy.data.stop=own stays down on the child for the
// whole sync, and after a copy that did not complete: a stream's import
// never brings up what it depends on, which Compose would start too (here
// db, stopped for the sync; app stops with it, depending on it). The same
// on a first release that finds containers (a first one killed before its
// down left them): db, stopped by its sync, stays down.
//
// Bug: on a first release the import brings app up with what it depends
// on (no --no-deps: a first release is taken for one with nothing
// stopped), so Compose starts db, which this sync stopped, and it runs
// on the partial copy. It should stay down, as on a later sync.
func TestSyncImportUpKeepsStoppedDown(t *testing.T) {
	t.Parallel()
	for _, first := range []bool{false, true} {
		s := newSyncSetup(t)
		caps := filepath.Join(s.dot, "capabilities", "data")
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
		s.labels("FAKE_APP=stream\nFAKE_DB=stream+own\nFAKE_UP_DEPS=1")
		write(t, s.log+".failimport.app", "", 0o644)
		s.logs()
		out, code := s.run(nil, args...)
		if code == 0 || !strings.Contains(out, "db app stay down") || !fileExists(filepath.Join(s.root, "shop", "dev", remote.SyncIncompleteFile)) {
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
	t.Parallel()
	s := newSyncSetup(t)
	tmp := t.TempDir()
	started, stopped := filepath.Join(tmp, "started"), filepath.Join(tmp, "stopped")
	caps := filepath.Join(s.dot, "capabilities", "data")
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
	waitFile(t, started, "the import", startWait, out)
	_ = cmd.Process.Signal(syscall.SIGTERM)
	_ = cmd.Wait()
	waitFile(t, stopped, "the import's end", 10*time.Second, out)
	for start := time.Now(); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(s.root, "shop", "dev", ".devopsy-sync.stream.db")); err != nil {
			break
		}
		if time.Since(start) > 10*time.Second {
			t.Fatalf("the spool stays:\n%s", out())
		}
	}
}

// A sync copies the parent's symbolic links as they are, and the child's
// services start on them. Here mnt/storage/public, a bind source of app
// below an entry nobody owns (as a web server beside it binds), is a link
// to / in the parent's data (its app wrote it, or a restored snapshot's
// data had it): Docker mounts the host's root at the child's next start.
// A release and a rollback refuse such a bind over such a link (the
// mounts check); the data a sync brings must not start without the same
// check.
func TestSyncLinkAtBindSource(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.labels("FAKE_APP_MOUNT2=/storage/public")
	s.releaseProd(nil)
	if err := os.Symlink("/", filepath.Join(s.mnt("prod"), "storage", "public")); err != nil {
		t.Fatal(err)
	}
	out, code := s.run(nil, "@dev", "--release")
	docker, _ := s.logs()
	if target, err := os.Readlink(filepath.Join(s.mnt("dev"), "storage", "public")); code == 0 && err == nil && target == "/" {
		t.Errorf("the child's first release (%d) went live with mnt/storage/public, a bind source of app below its entry, the parent's link to %s: Docker mounts that path of the host:\n%s\n%s", code, target, out, docker)
	}
}

// A child's first release with a stream and an rsync service left
// running: the stream's import keeps its new container up, and finish
// creates every labeled service before its hook, which sanitizes through
// them; the record of the first release goes afterwards.
func TestSyncFirstReleaseCreates(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	caps := filepath.Join(s.dot, "capabilities", "data")
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

// A child's first release whose stream import failed takes down what it
// started (the stream's container), what it was left as into the
// server's log first,
// and leaves nothing current, which --status says: the next release is a
// first one again and must create every labeled service before finish
// child go, which sanitizes through them.
func TestSyncFirstReleaseRetryCreates(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	caps := filepath.Join(s.dot, "capabilities", "data")
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
	compose := "shop-dev compose -f " + filepath.Join(s.root, "shop", "dev", "current", ".devopsy", "compose.yaml") + " --profile * "
	if docker, _ := s.logs(); !strings.Contains(docker, compose+"ps -a --format {{.Name}} {{.Service}} {{.State}} {{.ExitCode}} {{.Health}}\n"+compose+"down --remove-orphans\n") {
		t.Errorf("the failed first release's containers stay:\n%s", docker)
	}
	entries, _ := os.ReadDir(filepath.Join(s.root, "shop", "dev", "releases"))
	if out, code := s.run(nil, "@dev", "--status"); code != 0 || len(entries) != 1 || !strings.Contains(out, "release     none: "+entries[0].Name()+" failed, devopsy @devopsy@one:dev --log shows why") {
		t.Errorf("--status after a failed first release (%d):\n%s", code, out)
	}
	os.Remove(s.log + ".failimport")
	s.logs()
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
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "config.yaml"), "project: shop\ndefaults:\n  mode: image\n  release: deploy\nenvironments:\n  prod: {inherit: [FAKE_DB]}\n  dev: {parent: prod}\n  solo: {}\n", 0o644)
	caps := filepath.Join(s.dot, "capabilities", "data")
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho dump\n", 0o755)
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\necho \"import $1: $(cat)\"\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	childInherit := filepath.Join(s.root, "shop", "dev", "shared", "inherit.env")
	pending := filepath.Join(s.root, "shop", "dev", ".devopsy-sync.inherit.*")

	// The parent flips its database to stream: one --sync, no --inherit.
	write(t, filepath.Join(s.root, "shop", "prod", "shared", ".env"), "FAKE_DB=stream\n", 0o600)
	s.logs()
	out, code := s.run(nil, "@dev", "--sync", "--yes")
	if code != 0 || !strings.Contains(out, "import db: dump") || readString(childInherit) != "FAKE_DB='stream'\n" {
		t.Fatalf("flipped label (%d): %q\n%s", code, readString(childInherit), out)
	}
	if left, _ := filepath.Glob(pending); len(left) > 0 {
		t.Errorf("the pending file stays")
	}

	// Back to rsync, and the copy fails: the child keeps stream.
	write(t, filepath.Join(s.root, "shop", "prod", "shared", ".env"), "FAKE_DB=mnt+own\n", 0o600)
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
	write(t, filepath.Join(s.root, "shop", "dev", "shared", ".env"), "FAKE_DB=mnt+own\n", 0o600)
	// Another sync's (its own id), never read here, and gone once this
	// one finishes.
	stale := filepath.Join(s.root, "shop", "dev", remote.SyncInheritPending("1.2"))
	write(t, stale, "FAKE_DB='garbage'\n", 0o600)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if out, code := s.run(nil, "@dev", "--sync", "--from", "solo", "--yes"); code != 0 || strings.Contains(out, "garbage") {
		t.Errorf("stale pending (%d):\n%s", code, out)
	}
	if fileExists(stale) {
		t.Errorf("the stale pending file stays")
	}
	// Another sync's that is fresh (its holder waiting for the lock) is
	// never read or removed.
	fresh := filepath.Join(s.root, "shop", "dev", remote.SyncInheritPending("3.4"))
	write(t, fresh, "FAKE_DB='garbage'\n", 0o600)
	if out, code := s.run(nil, "@dev", "--sync", "--from", "solo", "--yes"); code != 0 || strings.Contains(out, "garbage") || !fileExists(fresh) {
		t.Errorf("another sync's fresh pending (%d):\n%s", code, out)
	}
	os.Remove(fresh)

	// --sync-exec gives the child's devopsy the pending file, on both
	// topologies, from the child's directory the push knows.
	show := filepath.Join(s.dot, "commands", "inheritfile")
	write(t, show, "#!/bin/sh\necho \"file=$DEVOPSY_INTERNAL_INHERIT_FILE\"\n", 0o755)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev with the command (%d):\n%s", code, out)
	}
	base := filepath.Join(s.root, "shop", "dev")
	push := []string{"DEVOPSY_SYNC_TO_DIR=" + filepath.Join(base, "current", ".devopsy"), "DEVOPSY_SYNC_TO_PROJECT=shop-dev", "DEVOPSY_SYNC_TO_BASE=" + base, remote.SyncIDEnv + "=1.2"}
	across := append([]string{"DEVOPSY_SYNC_TO=devopsy@two", "DEVOPSY_SYNC_SSH_PORT=22", "SSH_AUTH_SOCK=/tmp/agent.sock"}, push...)
	for name, env := range map[string][]string{"same server": push, "across servers": across} {
		if out, code := s.run(env, "--sync-exec", "inheritfile"); code != 0 || !strings.Contains(out, "file="+stale+"\n") {
			t.Errorf("--sync-exec, %s (%d):\n%s", name, code, out)
		}
	}
}

// A sync that finds containers drops a first release's mark an earlier,
// failed sync left; its stops stay.
func TestSyncRecordStaleFirst(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
		for start := time.Now(); ; {
			if _, err := os.Stat(started); err == nil {
				break
			}
			if time.Since(start) > startWait {
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
	out, code, _ = interrupt("FAKE_SLOW_RUN", "@slow", "--release", "--yes")
	if code != 0 || !strings.Contains(out, "the release goes on on @devopsy@one:slow") || !strings.Contains(out, "Ctrl-C again ends the session, not the release") || !strings.Contains(out, "deployed slowly") || s.current("slow") == "" {
		t.Errorf("during the run step (%d), current %q:\n%s", code, s.current("slow"), out)
	}
}

// A sync whose holder never decided (killed) leaves its files: whatever
// takes the lock next counts the data as a partial copy, which only a
// sync completes. A partial copy is never a parent.
func TestSyncSettle(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "capabilities", "data", "finish"), "#!/bin/sh\n[ \"${FAIL_FINISH:-}\" != \"$1 $2\" ]\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	dev, prod := filepath.Join(s.root, "shop", "dev"), filepath.Join(s.root, "shop", "prod")
	write(t, filepath.Join(dev, remote.SyncStartedFile), "", 0o644)
	write(t, filepath.Join(dev, remote.SyncOpenFile("1.2")), "devopsy@one:prod 1.2\n", 0o644)
	// A stream's spool the killed sync left, dump-sized: the settle
	// removes it, as a finish would (review round 9).
	spool := remote.StreamSpool(dev, "db", "1.2")
	write(t, spool, "rows\n", 0o644)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code == 0 || !strings.Contains(out, "did not complete") {
		t.Errorf("a release after an undecided sync (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--rollback"); code == 0 || !strings.Contains(out, "left its files") || !fileExists(filepath.Join(dev, remote.SyncIncompleteFile)) || fileExists(filepath.Join(dev, remote.SyncStartedFile)) || fileExists(filepath.Join(dev, remote.SyncOpenFile("1.2"))) {
		t.Errorf("a rollback after an undecided sync (%d):\n%s", code, out)
	}
	if fileExists(spool) {
		t.Errorf("the settle of an undecided sync left its stream's spool %s", spool)
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
	if out, code := s.run(nil, "@dev", "--rollback"); code == 0 || !strings.Contains(out, "the copy completed, but the project's data capability refused it: finish child go failed") {
		t.Errorf("a rollback after a refused copy (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--status"); code != 0 || !strings.Contains(out, "from a sync that did not complete: the copy completed, but the project's data capability refused it") || !strings.Contains(out, "from @devopsy@one:prod)") {
		t.Errorf("--status after a refused copy (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--sync", "--yes"); code != 0 || fileExists(filepath.Join(dev, remote.SyncIncompleteFile)) {
		t.Fatalf("a sync completes it (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Errorf("a release after (%d):\n%s", code, out)
	}

	// An export after an undecided sync settles it first, so it warns that
	// the data is a partial copy, as it does for any mark: its shared lock
	// alone could not (settling takes it exclusively), and it exported a
	// half copy as the data, silently.
	write(t, filepath.Join(dev, remote.SyncStartedFile), "", 0o644)
	if _, errs, code := s.runIO(nil, nil, "@dev", "--export", "db"); code != 0 || !strings.Contains(errs, "this environment's data is from a sync that did not complete: exporting it anyway") || !fileExists(filepath.Join(dev, remote.SyncIncompleteFile)) || fileExists(filepath.Join(dev, remote.SyncStartedFile)) {
		t.Errorf("an export after an undecided sync (%d):\n%s", code, errs)
	}
}

// A sync's holder killed outright mid-copy (SIGKILL, a reboot) leaves its
// record (what its prepare stopped, as the sync's own), the claimed file
// with the copy's writers and no mark: the data may be anything from
// untouched to half the parent's. The next lock-taker's settle marks it
// and keeps what the holder stopped down, as a holder deciding on a cut
// copy would: the record's stop lines are no leftover of an operation
// that wrote nothing while the sync's files are there. An export of such
// an environment (its script settles no sync files) must not start them
// either.
func TestSyncSettleKeepsKilledHolderStopsDown(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	dev := filepath.Join(s.root, "shop", "dev")
	record := filepath.Join(dev, ".devopsy-sync.services")
	killed := func() {
		write(t, record, "stop db\nstop app\nown 1.2 db\nown 1.2 app\n", 0o644)
		write(t, filepath.Join(dev, remote.SyncStartedFile), "devopsy@one:prod 1.2\n"+remote.WriterLine("db", 999999, "running")+"\n", 0o644)
		write(t, s.log+".stopped.shop-dev", "db\napp\n", 0o644)
		os.Remove(filepath.Join(dev, remote.SyncIncompleteFile))
		s.logs()
	}
	killed()
	out, code := s.run(nil, "@dev", "--rollback")
	docker, _ := s.logs()
	if code == 0 || !strings.Contains(out, "left its files") || !fileExists(filepath.Join(dev, remote.SyncIncompleteFile)) {
		t.Fatalf("a rollback after a killed holder (%d):\n%s\n%s", code, out, docker)
	}
	down := readString(s.log + ".stopped.shop-dev")
	if strings.Contains(docker, "shop-dev compose -f "+filepath.Join(dev, "current", ".devopsy", "compose.yaml")+" start ") || !strings.Contains(down, "db\n") || !strings.Contains(down, "app\n") {
		t.Errorf("the settle started what the killed holder stopped, on a copy that may be half the parent's (down: %q):\n%s\n%s", down, out, docker)
	}
	if rec := readString(record); !strings.Contains(rec, "stop db\n") || !strings.Contains(rec, "stop app\n") {
		t.Errorf("the record no longer names what stays down (the next sync's finish starts them):\n%q", rec)
	}

	// An export of the environment, before anything settled it.
	killed()
	s.labels("FAKE_DB=mnt+own\nFAKE_APP=mnt")
	defer s.labels("")
	_, errs, _ := s.runIO(nil, nil, "@dev", "--export", "db")
	docker, _ = s.logs()
	down = readString(s.log + ".stopped.shop-dev")
	if strings.Contains(docker, "shop-dev compose -f "+filepath.Join(dev, "current", ".devopsy", "compose.yaml")+" start ") || !strings.Contains(down, "db\n") || !strings.Contains(down, "app\n") {
		t.Errorf("an export started what the killed holder stopped (down: %q):\n%s\n%s", down, errs, docker)
	}
}

// The sync log, appended to by every sync whose session went, keeps its
// last lines once too big.
func TestTrimSyncLog(t *testing.T) {
	t.Parallel()
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
//
// Not parallel: it sets SyncIDEnv (t.Setenv).
func TestClaimWrite(t *testing.T) {
	base := t.TempDir()
	t.Setenv(remote.SyncIDEnv, "100.7")
	if _, err := claimWrite(base); err == nil {
		t.Error("claimed with the gate closed")
	}
	// Another sync's gate (a write left over from an earlier one): the
	// gate's name is the id, so the rename alone decides, with no check
	// before it that another sync's gate could replace meanwhile.
	write(t, filepath.Join(base, remote.SyncOpenFile("200.8")), "vm1:prod 200.8\n", 0o644)
	if _, err := claimWrite(base); err == nil || fileExists(filepath.Join(base, remote.SyncStartedFile)) {
		t.Errorf("claimed another sync's gate: %v", err)
	}
	write(t, filepath.Join(base, remote.SyncOpenFile("100.7")), "vm1:prod 100.7\n", 0o644)
	if first, err := claimWrite(base); err != nil || !first || fileExists(filepath.Join(base, remote.SyncOpenFile("100.7"))) || !fileExists(filepath.Join(base, remote.SyncOpenFile("200.8"))) || readString(filepath.Join(base, remote.SyncStartedFile)) != "vm1:prod 100.7\n" {
		t.Errorf("first claim: %v", err)
	}
	// Later writes of the same sync: not the first (a reset wipes once).
	if first, err := claimWrite(base); err != nil || first {
		t.Errorf("second claim: %v, first %v", err, first)
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
	if err := markIncomplete(base, "", remote.IncompleteFinish, "refused", true); err != nil || readString(mark) != "2026-10-01 00:00:00\tx\tcopy\tcut\n" {
		t.Errorf("kept mark %q (%v)", readString(mark), err)
	}
	if err := markIncomplete(base, "", remote.IncompleteFinish, "refused", false); err != nil || readString(mark) != "2026-10-01 00:00:00\tvm1:prod\tfinish\trefused\n" {
		t.Errorf("mark %q (%v)", readString(mark), err)
	}
	// An import's mark names its service. A sync's replaces it even when
	// told to keep one, its time kept: the data is partly a sync's from
	// then on, and only a sync clears it.
	write(t, mark, remote.IncompleteLine("2026-10-02 00:00:00", "db", remote.IncompleteImport, "cut"), 0o644)
	if err := markIncomplete(base, "", remote.IncompleteSettled, "killed", true); err != nil || readString(mark) != "2026-10-02 00:00:00\tvm1:prod\tsettled\tkilled\n" {
		t.Errorf("a sync's mark over an import's %q (%v)", readString(mark), err)
	}
	os.Remove(mark)
	if err := markIncomplete(base, "db", remote.IncompleteImport, "cut", false); err != nil || !strings.HasSuffix(readString(mark), "\tdb\timport\tcut\n") {
		t.Errorf("an import's mark %q (%v)", readString(mark), err)
	}
}

// A push whose child decided already (its gate closed: the holder gone
// on, a push that outlived its connection) writes nothing into it.
func TestSyncPushGateClosed(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	dev := filepath.Join(s.root, "shop", "dev")
	// An import's devopsy, still running: what isSyncImport looks for is
	// in its arguments and its directory, the child's (": " keeps sh
	// from exec'ing sleep). Another, in another environment (a pid
	// reused after a reboot), is left alone.
	imp := exec.Command("sh", "-c", "sleep 30; :", "devopsy", "--sync-services", "import", "db")
	imp.Dir = filepath.Join(dev, "current")
	other := exec.Command("sh", "-c", "sleep 30; :", "devopsy", "--sync-services", "import", "db")
	other.Dir = filepath.Join(s.root, "shop", "prod", "current")
	ended, otherEnded := make(chan error, 1), make(chan error, 1)
	for _, c := range []struct {
		cmd   *exec.Cmd
		ended chan error
	}{{imp, ended}, {other, otherEnded}} {
		if err := c.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { c.ended <- c.cmd.Wait() }()
		defer func() { _ = c.cmd.Process.Kill() }()
	}
	write(t, filepath.Join(dev, remote.SyncTakenFile), "devopsy@one:prod 1.2\n"+remote.WriterLine("db", imp.Process.Pid, "running")+"\n"+remote.WriterLine("db", other.Process.Pid, "running")+"\n", 0o644)
	write(t, s.log+".shop-dev", "FAKE_ONEOFF=db\nFAKE_SYNC_CONTAINERS='devopsy-sync-1.2-receive devopsy-sync-9.9-rsync-0'\nFAKE_RM_IN_PROGRESS=1\n", 0o644)
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
	// One whose removal Docker had under way already (the push's own,
	// cancelled) is gone all the same: no error.
	if strings.Contains(string(out), "already in progress") {
		t.Errorf("a removal already in progress said as an error:\n%s", out)
	}
	// app, depending on db, stops with it, recorded.
	if rec := readString(filepath.Join(dev, ".devopsy-sync.services")); !strings.Contains(rec, "stop db\n") || !strings.Contains(rec, "stop app\n") || !strings.Contains(docker, " stop db app\n") {
		t.Errorf("the stopped db and app not recorded: %q\n%s", rec, docker)
	}
	if _, err := os.Stat("/proc/self/cmdline"); err == nil {
		select {
		case <-ended:
		case <-time.After(20 * time.Second):
			t.Errorf("the import still runs:\n%s", out)
		}
		select {
		case <-otherEnded:
			t.Errorf("another environment's import was ended:\n%s", out)
		default:
		}
	}
}

// A sync hook runs in the foreground: what it leaves running in the
// background ends when it returns, said, and the hook still succeeds.
func TestSyncHookLeftovers(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	write(t, filepath.Join(s.dot, "capabilities", "data", "finish"), "#!/bin/sh\n[ \"$1 $2\" = \"child go\" ] || exit 0\nsleep 30 >/dev/null 2>&1 &\necho $! >"+pidFile+"\n", 0o755)
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
	// Gone, or a zombie nothing reaps (a container's init may not).
	alive := func() bool {
		if stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
			_, rest, _ := strings.Cut(string(stat), ") ")
			return !strings.HasPrefix(rest, "Z")
		}
		return syscall.Kill(pid, 0) == nil
	}
	deadline := time.Now().Add(5 * time.Second)
	for alive() {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the hook's background process %d still runs", pid)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// --destroy of a child whose sync's holder was killed outright (its
// record left) ends the sync's writers before taking anything down, so
// nothing writes into mnt while it is emptied.
func TestDestroyEndsWriters(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	write(t, filepath.Join(s.root, "shop", "dev", remote.SyncStartedFile), "devopsy@one:prod 1.2\n", 0o644)
	write(t, s.log+".shop-dev", "FAKE_SYNC_CONTAINERS=devopsy-sync-1.2-rsync-0\nFAKE_IMPORT_CONTAINERS=devopsy-import-shop-dev-42-extract-0\nFAKE_OWNERS_CONTAINERS=devopsy-owners-shop-dev-42-0\n", 0o644)
	s.logs()
	out, code := s.run(nil, "@dev", "--destroy", "--yes")
	docker, _ := s.logs()
	// And an --import's container a killed devopsy left, and an owners'
	// step's: not Compose's.
	rm, rmImport, rmOwners, down := strings.Index(docker, "rm -f devopsy-sync-1.2-rsync-0"), strings.Index(docker, "rm -f devopsy-import-shop-dev-42-extract-0"), strings.Index(docker, "rm -f devopsy-owners-shop-dev-42-0"), strings.Index(docker, " down ")
	if code != 0 || rm < 0 || rmImport < 0 || rmOwners < 0 || down < 0 || rm > down || rmImport > down || rmOwners > down {
		t.Errorf("--destroy with a sync's record left (%d):\n%s\n%s", code, out, docker)
	}
}

// devopsy --util-image, which every use of the util image goes through
// (in Go, or from a script): built when missing, a failure failing;
// rebuilt with its base pulled and no cache once older than a week, the
// image it replaced removed (never with -f), a failure only warning.
func TestUtilImage(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	df := s.log + ".dockerfile"
	built := func() string { b, _ := os.ReadFile(s.log + ".util"); os.Remove(s.log + ".util"); return string(b) }

	out, code := s.run(nil, "--util-image")
	docker, _ := s.logs()
	if code != 0 || strings.Contains(docker, " build ") || strings.Contains(out, "devopsy-util") {
		t.Errorf("fresh (%d):\n%s\n%s", code, out, docker)
	}

	old := []string{"FAKE_UTIL_CREATED=2026-10-01T10:00:00.123456789Z"}
	out, code = s.run(old, "--util-image")
	docker, _ = s.logs()
	built()
	if dockerfile, _ := os.ReadFile(df); code != 0 || !strings.Contains(out, "updating devopsy-util:3, built over a week ago") ||
		!strings.Contains(docker, " build -q --pull --no-cache -t devopsy-util:3 -\n") || !strings.Contains(docker, " image rm sha256:old\n") ||
		string(dockerfile) != "FROM alpine:3\nRUN apk add --no-cache rsync tar findutils coreutils zstd\n" {
		t.Errorf("a week old (%d):\n%s\n%s\n%s", code, out, docker, dockerfile)
	}

	out, code = s.run(append(old, "FAIL_UTIL_BUILD=1"), "--util-image")
	docker, _ = s.logs()
	if code != 0 || !strings.Contains(out, "warning: updating devopsy-util:3 failed: going on with the one there") || strings.Contains(docker, " image rm ") {
		t.Errorf("a failed rebuild (%d):\n%s\n%s", code, out, docker)
	}

	missing := []string{"FAKE_UTIL_MISSING=1"}
	out, code = s.run(missing, "--util-image")
	docker, _ = s.logs()
	built()
	if code != 0 || !strings.Contains(out, "building devopsy-util:3") || !strings.Contains(docker, " build -q -t devopsy-util:3 -\n") || strings.Contains(docker, " image rm ") {
		t.Errorf("missing (%d):\n%s\n%s", code, out, docker)
	}
	out, code = s.run(append(missing, "FAIL_UTIL_BUILD=1"), "--util-image")
	if code != 1 || !strings.Contains(out, "error: building devopsy-util:3: exit status 1") {
		t.Errorf("missing, its build failing (%d):\n%s", code, out)
	}
}

// parentChanged says whether the parent's side ran a compose command
// that changes anything: its reads (config, ps) do not.
func parentChanged(docker string) bool {
	for _, line := range strings.Split(docker, "\n") {
		if strings.HasPrefix(line, "shop-prod compose") && !strings.Contains(line, " config ") && !strings.Contains(line, " ps ") && !strings.HasSuffix(line, " config --format json") {
			return true
		}
	}
	return false
}

// Bug: finish abort recreates, like finish go, every stopped service
// whose container's devopsy.digest differs from the current release's
// devopsy-compose.yaml (images.Recreate). In a release that syncs,
// current is already the new release, not live, whose file holds its
// data services on what it fetched (img record skips the child's own
// check of a service the parent's data covers), while the containers
// are the previous release's: an abort (Ctrl-C, the push refusing)
// recreates the child's database on the new release's image over its
// own data, a series bump or a lowered pin (a downgrade) included, and
// img ran then records it. Abort starts nothing new (plan, part 9).
func TestSyncAbortRecreatesNothing(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("release (%d):\n%s", code, out)
	}
	s.logs()
	// The new release, current while its sync runs: its file holds db on
	// what it fetched; db's container is the previous release's.
	write(t, filepath.Join(s.root, "shop", "dev", s.current("dev"), "devopsy-compose.yaml"),
		"services:\n  \"db\":\n    image: \"y@sha256:new\"\n    labels:\n      \"devopsy.digest\": \"y@sha256:new\"\n", 0o644)
	write(t, s.log+".containers.shop-dev", "db y@sha256:old\n", 0o644)
	write(t, filepath.Join(s.root, "shop", "dev", ".devopsy-sync.services"), "stop db\n", 0o644)
	cmd := exec.Command(binary, "--sync-services", "finish", "abort")
	cmd.Dir = filepath.Join(s.root, "shop", "dev", "current")
	cmd.Env = append(append([]string{}, s.env...), "COMPOSE_PROJECT_NAME=shop-dev", "PWD="+cmd.Dir, "FAKE_DB_DOWN=1")
	out, err := cmd.CombinedOutput()
	docker, _ := s.logs()
	if err != nil || !strings.Contains(docker, " start db") || strings.Contains(docker, "up -d --no-deps --wait db") {
		t.Errorf("finish abort (%v): db must start as it was, never be recreated on the release's image:\n%s\n%s", err, out, docker)
	}
}

// The restart after a failed run step brings the previous release back
// on the data as it is then, which a release that syncs changed between
// the two. A release and a rollback refuse a bind source a symbolic link
// in its entry leads out of (the mounts check, --check-mounts restored),
// and a sync's finish refuses to start the new release's code on one;
// the previous release's code, whose binds may differ from the new one's
// (here app binding mnt/storage/public below an entry nobody owns, a
// bind the new release dropped, as the mounts check's fix says), is
// restarted without any of them, and Docker mounts the host's root into
// its app at that start.
func TestRestartAfterFailedRunChecksLeavingBinds(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	write(t, s.log+".shop-dev", "FAKE_APP_MOUNT2=/storage/public\n", 0o644)
	if out, code := s.run(nil, "@dev", "--release"); code != 0 {
		t.Fatalf("dev's first release (%d):\n%s", code, out)
	}
	first := s.current("dev")
	// Only the first release's code binds below the entry (the fake reads
	// the compose file's release through current).
	write(t, s.log+".shop-dev", `case $(readlink -f "$(dirname "${3:-/}")") in *"/dev/`+first+`/"*) FAKE_APP_MOUNT2=/storage/public ;; esac`+"\n", 0o644)
	// Meanwhile the parent's app wrote a link in its entry.
	if err := os.Symlink("/", filepath.Join(s.mnt("prod"), "storage", "public")); err != nil {
		t.Fatal(err)
	}
	out, code := s.run([]string{"FAKE_FAIL_DEPLOY=1"}, "@dev", "--release-sync", "--yes")
	target, err := os.Readlink(filepath.Join(s.mnt("dev"), "storage", "public"))
	if code == 0 || err != nil || target != "/" || s.current("dev") != first {
		t.Fatalf("the release that syncs with a failing run step (%d), current %s (was %s), mnt/storage/public -> %q (%v):\n%s", code, s.current("dev"), first, target, err, out)
	}
	if strings.Contains(out, "restarting "+strings.TrimPrefix(first, "releases/")+" with its own run step") {
		t.Errorf("the previous release, whose app binds mnt/storage/public, was restarted on the synced data, where that is the parent's link to %s: Docker mounts that path of the host into app:\n%s", target, out)
	}
}

// An import on a target stops its set, then starts it again on the same
// data: on data where a bind source already leads out of its entry
// through a link (one the import does not replace), the start gate
// refuses that start, so the import must refuse before it stops anything,
// as a take and an export do (check everything before changing
// anything), never leave the set down under its mark with the data in.
func TestImportRefusesLeavingBindsBeforeStopping(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	// app, without data, depends on db: an import of db stops it with db.
	write(t, s.log+".shop-prod", "FAKE_APP=none\nFAKE_APP_MOUNT2=/storage/public\n", 0o644)
	s.releaseProd(nil)
	if err := os.Symlink("/", filepath.Join(s.mnt("prod"), "storage", "public")); err != nil {
		t.Fatal(err)
	}
	s.logs()
	_, errs, code := s.runIO(nil, tarOf(t, map[string]string{"db/x": "x"}), "@prod", "--import", "db", "--yes")
	docker, _ := s.logs()
	stopped := strings.TrimSpace(readString(s.log + ".stopped.shop-prod"))
	if code == 0 || stopped != "" || readString(filepath.Join(s.mnt("prod"), "db", "ibdata")) != "rows\n" || fileExists(filepath.Join(s.root, "shop", "prod", remote.SyncIncompleteFile)) {
		t.Errorf("an import on data with a link out of its entry at a bind source (%d) left %q stopped, db's data %q:\n%s\n%s", code, stopped, readString(filepath.Join(s.mnt("prod"), "db", "ibdata")), errs, docker)
	}
}

// A child's prepare stops its set before the copy, and its finish starts
// it again: on data where a bind source already leads out of its entry
// through a link in an entry the copy does not replace, finish go refuses
// that start, so the prepare must refuse before anything stops on either
// side (its holder runs before the parent's push), never leave the child
// down and marked, the parent stopped for a copy that could not be used.
func TestSyncChildRefusesLeavingBindsBeforeStopping(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	// The child's app binds below an entry of its own that the parent has
	// not (cache): nothing of the copy replaces it.
	write(t, s.log+".shop-dev", "FAKE_APP_MOUNT2=/cache/public\n", 0o644)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev's release (%d):\n%s", code, out)
	}
	if err := os.MkdirAll(filepath.Join(s.mnt("dev"), "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/", filepath.Join(s.mnt("dev"), "cache", "public")); err != nil {
		t.Fatal(err)
	}
	s.logs()
	out, code := s.run(nil, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	stoppedDev, stoppedProd := strings.TrimSpace(readString(s.log+".stopped.shop-dev")), strings.TrimSpace(readString(s.log+".stopped.shop-prod"))
	if code == 0 || stoppedDev != "" || stoppedProd != "" || fileExists(filepath.Join(s.root, "shop", "dev", remote.SyncIncompleteFile)) {
		t.Errorf("a sync into a child with a link out of its entry at a bind source (%d) left %q stopped on the child, %q on the parent, marked %v:\n%s\n%s", code, stoppedDev, stoppedProd, fileExists(filepath.Join(s.root, "shop", "dev", remote.SyncIncompleteFile)), out, docker)
	}
}

// The child's check before stopping names --reset as the way to start
// the child over without the link, and a reset skips it, since its wipe
// removes the link: here dev's app binds mnt/cache/public, a link to /
// in an entry the parent has not, so a sync is refused, and the reset
// wipes it and copies prod's data, nothing left stopped or marked.
func TestSyncResetWipesLeavingBind(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	write(t, s.log+".shop-dev", "FAKE_APP_MOUNT2=/cache/public\n", 0o644)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev's release (%d):\n%s", code, out)
	}
	if err := os.MkdirAll(filepath.Join(s.mnt("dev"), "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/", filepath.Join(s.mnt("dev"), "cache", "public")); err != nil {
		t.Fatal(err)
	}
	if out, code := s.run(nil, "@dev", "--sync", "--yes"); code == 0 || !strings.Contains(out, "devopsy @devopsy@one:dev --reset") {
		t.Fatalf("a sync into a child with a link out of its entry at a bind source (%d):\n%s", code, out)
	}
	s.logs()
	out, code := s.run(nil, "@dev", "--reset", "--yes")
	docker, _ := s.logs()
	stoppedDev, stoppedProd := strings.TrimSpace(readString(s.log+".stopped.shop-dev")), strings.TrimSpace(readString(s.log+".stopped.shop-prod"))
	if code != 0 || !strings.Contains(docker, " down ") || !strings.Contains(docker, "-wipe ") || stoppedDev != "" || stoppedProd != "" || fileExists(filepath.Join(s.root, "shop", "dev", remote.SyncIncompleteFile)) || fileExists(filepath.Join(s.mnt("dev"), "cache")) || readString(filepath.Join(s.mnt("dev"), "storage", "file.txt")) != "from prod\n" {
		t.Errorf("a reset of a child with a link out of its entry at a bind source (%d) left %q stopped on the child, %q on the parent, marked %v, cache there %v:\n%s\n%s", code, stoppedDev, stoppedProd, fileExists(filepath.Join(s.root, "shop", "dev", remote.SyncIncompleteFile)), fileExists(filepath.Join(s.mnt("dev"), "cache")), out, docker)
	}
}

// An export on a target checks the start gate before it stops its set,
// for the start that follows: with the set down already (app and db
// exited) it stops nothing and starts nothing, so a link out of its
// entry at a bind source is no reason to refuse the read of the data.
func TestExportNothingRunningIgnoresLeavingBinds(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, s.log+".shop-prod", "FAKE_APP_MOUNT2=/storage/public\n", 0o644)
	s.releaseProd(nil)
	if err := os.Symlink("/", filepath.Join(s.mnt("prod"), "storage", "public")); err != nil {
		t.Fatal(err)
	}
	write(t, s.log+".stopped.shop-prod", "app\ndb\n", 0o644)
	s.logs()
	stdout, errs, code := s.runIO(nil, nil, "@prod", "--export", "db")
	docker, _ := s.logs()
	if code != 0 || len(stdout) == 0 || strings.Contains(docker, " stop ") || strings.Contains(docker, " start ") {
		t.Errorf("an export with its set down on data with a link out of its entry at a bind source (%d), %d bytes:\n%s\n%s", code, len(stdout), errs, docker)
	}
}

// A feature only the activation needs (leaving-binds: the mounts check's
// words every activation calls, which a v0.52.0 server lacks) refuses a
// release before anything changes on the server: the upload's own needs
// are the server's, so the project must not be uploaded first and the
// activation's prelude refuse it then, leaving a pending release
// directory, INCOMPLETE and newer than current, which remove failed never
// takes and pruning only once it is older than the keep-th live release.
func TestOlderServerActivationRefusesBeforeUpload(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	old := filepath.Join(filepath.Dir(s.root), "old")
	write(t, filepath.Join(old, "devopsy"), "#!/bin/sh\nif [ \"$1\" = --release-settings ]; then "+binary+" --release-settings | sed 's/ "+remote.ReleaseSnapshotsFeature+" / /'; exit; fi\nexec "+binary+" \"$@\"\n", 0o755)
	var env []string
	for _, kv := range s.env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			kv = "PATH=" + old + ":" + v
		}
		env = append(env, kv)
	}
	releases := filepath.Join(s.root, "shop", "prod", "releases")
	before, _ := os.ReadDir(releases)
	cmd := exec.Command(binary, "@prod", "--release")
	cmd.Dir = s.project
	cmd.Env = env
	cmd.Stdin = strings.NewReader("")
	outb, _ := cmd.CombinedOutput()
	out, code := string(outb), cmd.ProcessState.ExitCode()
	after, _ := os.ReadDir(releases)
	if code == 0 || !strings.Contains(out, "the server's devopsy lacks what this needs ("+remote.ReleaseSnapshotsFeature+")") {
		t.Fatalf("a release on a server lacking %s (%d):\n%s", remote.ReleaseSnapshotsFeature, code, out)
	}
	if len(after) != len(before) {
		t.Errorf("a release refused for %s left a release directory behind (%d releases, %d before):\n%s", remote.ReleaseSnapshotsFeature, len(after), len(before), out)
	}
}

// An upload checks its activation's needs (remote.WithAlsoNeeds), so a
// server lacking one refuses before anything changes: before the upload,
// but only then, after everything else a release does first. A child's
// first release to a server lacking release-snapshots (a v0.51.0 one,
// where every activation is refused) runs its local before steps (the
// project's: a build and push, a notice) and writes the parent's
// inherited variables into the child's shared/inherit.env, creating its
// directory, and only then is refused: the needs are known before any
// session, and the server's devopsy says what it has in the first one
// (the child's current check), so nothing of the release should run on
// a server that refuses it.
func TestOlderServerRefusesBeforeBeforeSteps(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "config.yaml"), "project: shop\ndefaults:\n  mode: image\n  release: deploy\nenvironments:\n  prod: {inherit: [SHARED_KEY]}\n  dev: {parent: prod, release: {before: [local: deploy], run: deploy}}\n", 0o644)
	s.releaseProd(nil)
	write(t, filepath.Join(s.root, "shop", "prod", "shared", ".env"), "SHARED_KEY=from-prod\n", 0o644)
	old := filepath.Join(filepath.Dir(s.root), "old")
	write(t, filepath.Join(old, "devopsy"), "#!/bin/sh\nif [ \"$1\" = --release-settings ]; then "+binary+" --release-settings | sed 's/ "+remote.ReleaseSnapshotsFeature+" / /'; exit; fi\nexec "+binary+" \"$@\"\n", 0o755)
	var env []string
	for _, kv := range s.env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			kv = "PATH=" + old + ":" + v
		}
		env = append(env, kv)
	}
	cmd := exec.Command(binary, "@dev", "--release")
	cmd.Dir = s.project
	cmd.Env = env
	cmd.Stdin = strings.NewReader("")
	outb, _ := cmd.CombinedOutput()
	out, code := string(outb), cmd.ProcessState.ExitCode()
	if code == 0 || !strings.Contains(out, "the server's devopsy lacks what this needs ("+remote.ReleaseSnapshotsFeature+")") {
		t.Fatalf("a child's first release on a server lacking %s (%d):\n%s", remote.ReleaseSnapshotsFeature, code, out)
	}
	if strings.Contains(out, "running 'devopsy deploy' locally (before)") {
		t.Errorf("the release ran its local before step before the server refused it:\n%s", out)
	}
	if dev := filepath.Join(s.root, "shop", "dev"); fileExists(dev) {
		t.Errorf("the release wrote %s before the server refused it (inherit.env: %v):\n%s", dev, fileExists(filepath.Join(dev, "shared", remote.InheritFile)), out)
	}
}

// A sync's push checks the parent's start gate before it stops the
// parent's set, for the start that follows, as an export does: with the
// set down already (app and db exited, a parent in maintenance) it stops
// nothing there and starts nothing, so a link out of its entry at a bind
// source on the parent is no reason to refuse the copy.
func TestSyncPushNothingRunningIgnoresLeavingBinds(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, s.log+".shop-prod", "FAKE_APP_MOUNT2=/storage/public\n", 0o644)
	s.releaseProd(nil)
	if err := os.Symlink("/", filepath.Join(s.mnt("prod"), "storage", "public")); err != nil {
		t.Fatal(err)
	}
	write(t, s.log+".stopped.shop-prod", "app\ndb\n", 0o644)
	s.logs()
	out, code := s.run(nil, "@dev", "--release")
	if code != 0 || strings.Contains(out, "nothing started") || readString(filepath.Join(s.mnt("dev"), "storage", "file.txt")) != "from prod\n" {
		t.Errorf("a child's first release from a parent with its set down and a link out of its entry at a bind source (%d):\n%s", code, out)
	}
}

// An app writes links in its own data that point where it sees that data
// in its container (storage/latest -> /app/storage/file.txt): a sync
// copies them as they are, a snapshot's restore takes them (anyTarget:
// "an app's absolute links made every snapshot unrestorable"), and an
// --import extracts each entry in a container that mounts that entry
// alone, as a restore does. So the same data moved by --export and
// --import is no reason to refuse: the tar devopsy's own export made of
// it must import.
func TestImportAppAbsoluteLink(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev's release (%d):\n%s", code, out)
	}
	if err := os.Symlink("/app/storage/file.txt", filepath.Join(s.mnt("prod"), "storage", "latest")); err != nil {
		t.Fatal(err)
	}
	out, errs, code := s.runIO(nil, nil, "@prod", "--export", "app")
	if code != 0 {
		t.Fatalf("@prod --export app (%d):\n%s", code, errs)
	}
	_, errs, code = s.runIO(nil, out, "@dev", "--import", "app", "--yes")
	if l, _ := os.Readlink(filepath.Join(s.mnt("dev"), "storage", "latest")); code != 0 || l != "/app/storage/file.txt" {
		t.Errorf("prod's storage, with a link the app wrote to where it sees its data, into dev (%d), the link %q:\n%s", code, l, errs)
	}
}

// --export lists each entry's files with find, the entries its starting
// points (cd /x && find "$@" ...), and a snapshot's tar names its entry
// the same way (find "$2"): GNU find takes a first argument that is ! or
// ( as the start of its expression, not a path, so an entry named ! made
// find fail on the entries after it, or, alone, negate the types test
// over the current directory and list nothing: the export wrote a tar
// without the entry's files (review). Refused where labels are read, as
// a leading dash is, before anything stops.
func TestExportEntryNamedBang(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	write(t, s.log+".shop-prod", "FAKE_APP_MOUNT2=/!\n", 0o644)
	write(t, filepath.Join(s.mnt("prod"), "!", "f"), "bang\n", 0o644)
	s.logs()
	_, errs, code := s.runIO(nil, nil, "@prod", "--export", "app")
	if code == 0 || !strings.Contains(errs, "./mnt/!: an entry of mnt a copy carries cannot have whitespace in its name, start with a dash or be ! or (: rename it") {
		t.Errorf("an export of app, whose entries are ! and storage (%d):\n%s", code, errs)
	}
	if docker, _ := s.logs(); strings.Contains(docker, " stop ") {
		t.Errorf("a refused export stopped something:\n%s", docker)
	}
}

// An app's data may hold a FIFO (a mail spool's public/pickup): a sync
// skips it (--no-D) and a snapshot's tar leaves it out (find -type f -o
// -type d -o -type l), while an --import refuses one (errMember). The
// tar devopsy's own --export makes of that data must import all the
// same: the export leaves out what no copy carries, as the others do.
func TestImportExportWithFIFO(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev's release (%d):\n%s", code, out)
	}
	if err := syscall.Mkfifo(filepath.Join(s.mnt("prod"), "storage", "pickup"), 0o622); err != nil {
		t.Fatal(err)
	}
	out, errs, code := s.runIO(nil, nil, "@prod", "--export", "app")
	if code != 0 {
		t.Fatalf("@prod --export app (%d):\n%s", code, errs)
	}
	_, errs, code = s.runIO(nil, out, "@dev", "--import", "app", "--yes")
	if code != 0 || readString(filepath.Join(s.mnt("dev"), "storage", "file.txt")) != "from prod\n" {
		t.Errorf("prod's storage, with a FIFO in it, into dev (%d):\n%s", code, errs)
	}
}

// An app's data may hold two names of one inode: a hard link to a
// symbolic link (storage/b to storage/a, which Linux makes with link(2)
// on a link), or a file hard-linked across two entries (cache/dup to
// storage/file.txt). --export ran GNU tar over find's list without
// --hard-dereference, so the second name came out as a hard link
// member, which an --import refused: a hard link to a link, and one that
// links into another entry (tarLinks.check, tarMember). A snapshot's tar
// takes --hard-dereference for that reason, and a sync's rsync without
// -H copies them as files: the tar devopsy's own --export makes of that
// data must import all the same, as TestImportExportWithFIFO says of a
// FIFO. (macOS's link follows the symbolic link, so only the second
// case arises there, and the fake's cp keeps no hard links: Linux shows
// it.)
func TestImportExportHardLinks(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.labels("FAKE_APP_MOUNT2=/cache")
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev's release (%d):\n%s", code, out)
	}
	storage := filepath.Join(s.mnt("prod"), "storage")
	if err := os.Symlink("file.txt", filepath.Join(storage, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(storage, "a"), filepath.Join(storage, "b")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.mnt("prod"), "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(storage, "file.txt"), filepath.Join(s.mnt("prod"), "cache", "dup")); err != nil {
		t.Fatal(err)
	}
	out, errs, code := s.runIO(nil, nil, "@prod", "--export", "app")
	if code != 0 {
		t.Fatalf("@prod --export app (%d):\n%s", code, errs)
	}
	_, errs, code = s.runIO(nil, out, "@dev", "--import", "app", "--yes")
	dev := s.mnt("dev")
	// storage/b as prod has it: a link on Linux, its target's copy on
	// macOS.
	want := "file.txt"
	if fi, err := os.Lstat(filepath.Join(storage, "b")); err == nil && fi.Mode()&os.ModeSymlink == 0 {
		want = ""
	}
	if l, _ := os.Readlink(filepath.Join(dev, "storage", "b")); code != 0 || readString(filepath.Join(dev, "storage", "file.txt")) != "from prod\n" || readString(filepath.Join(dev, "cache", "dup")) != "from prod\n" || l != want {
		t.Errorf("prod's storage and cache, with a hard link to a link and one across the entries, into dev (%d), storage/b %q:\n%s", code, l, errs)
	}
}

// A database's files may have holes (pages it punched out): a sync
// keeps them (rsync --sparse) and so does a snapshot's tar (--sparse:
// "a database's punched pages came back as zeros, more disk than the
// data took"). An --export of the same data writes each hole as zeros,
// so the stream, the import's spool and the file it extracts take the
// file's whole size: --export then --import must keep holes holes too.
func TestExportImportKeepsHoles(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev's release (%d):\n%s", code, out)
	}
	const size = 64 << 20
	f, err := os.Create(filepath.Join(s.mnt("prod"), "storage", "ibdata"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("page\n"), size-5); err != nil {
		t.Fatal(err)
	}
	f.Close()
	var st syscall.Stat_t
	if err := syscall.Stat(f.Name(), &st); err != nil || st.Blocks*512 >= size/8 {
		t.Skipf("no holes on this filesystem (%v, %d blocks)", err, st.Blocks)
	}
	if v, _ := exec.Command("tar", "--version").Output(); !strings.Contains(string(v), "GNU") {
		t.Skip("the fake's tar is no GNU tar, which keeps holes (--sparse)")
	}
	out, errs, code := s.runIO(nil, nil, "@prod", "--export", "app")
	if code != 0 {
		t.Fatalf("@prod --export app (%d):\n%s", code, errs)
	}
	if len(out) >= size/8 {
		t.Errorf("--export of a %d-byte file with %d bytes of data wrote %d bytes: its hole as zeros", size, st.Blocks*512, len(out))
	}
	_, errs, code = s.runIO(nil, out, "@dev", "--import", "app", "--yes")
	var dst syscall.Stat_t
	if err := syscall.Stat(filepath.Join(s.mnt("dev"), "storage", "ibdata"), &dst); code != 0 || err != nil || dst.Size != size || dst.Blocks*512 >= size/8 {
		t.Errorf("the import of it (%d, %v): %d bytes on disk for %d:\n%s", code, err, dst.Blocks*512, dst.Size, errs)
	}
}

// settleStops takes every unmarked stop line of the record for a leftover
// of an operation killed outright, and --export on a target runs it
// first. A custom service's export hook that itself runs devopsy --export
// of another service (the hooks are the project's: AGENTS.md counts on
// one running devopsy --export) runs it inside the outer export, which
// holds the locks and has its own stops recorded: the inner export's
// settle starts them, in the middle of the outer copy, so the dump is
// taken from a database devopsy had stopped for it and started again
// behind its back. The same holds for a sync's push whose export hook
// does so (its stops are recorded alike).
func TestNestedExportKeepsOuterStops(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, s.log+".shop-prod", "FAKE_APP=stream\nFAKE_DB=mnt+always\n", 0o644)
	s.releaseProd(nil)
	// app's export dumps through devopsy --export of db, then checks db
	// is still stopped, as the outer export left it (DEVOPSY_DATA_STOP).
	write(t, filepath.Join(s.root, "shop", "prod", "current", ".devopsy", "capabilities", "data", "export"),
		"#!/bin/sh\ndevopsy --export db >/dev/null 2>\"$DOCKER_LOG.inner\" || { echo \"inner export failed\" >&2; exit 1; }\n"+
			"grep -qx db \"$DOCKER_LOG.stopped.$COMPOSE_PROJECT_NAME\" || echo \"db ran during the export of app\" >&2\necho dump\n", 0o755)
	s.logs()
	out, errs, code := s.runIO(nil, nil, "@prod", "--export", "app")
	docker, _ := s.logs()
	if code != 0 || string(out) != "dump\n" || !strings.Contains(errs, "stopping db (always)") {
		t.Fatalf("the export of app with db stopped around it (%d) %q:\n%s\n%s", code, out, errs, readString(s.log+".inner"))
	}
	if strings.Contains(errs, "db ran during the export of app") {
		t.Errorf("the inner export started what the outer stopped (said: %q):\n%s\n%s", strings.TrimSpace(readString(s.log+".inner")), errs, docker)
	}
}
