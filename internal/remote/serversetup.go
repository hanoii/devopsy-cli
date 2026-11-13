package remote

import (
	"fmt"
	"path"

	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// ServerSetupFlag is the hidden flag behind --server-setup, run by its sessions on the server
// as the deploy user: facts (read-only) and apply. A server without it refuses, naming the
// upgrade.
const ServerSetupFlag = "--server-setup-step"

// ServerReadyFlag is the hidden flag an upload calls first (UploadScript), with the server
// as an address names it: what every release needs of the server (the release root, tar and
// flock), checked by stats and path lookups alone.
const ServerReadyFlag = "--server-ready"

// RestartWaitEnv bounds the wait for Docker's restart, in seconds, for tests.
const RestartWaitEnv = "DEVOPSY_INTERNAL_RESTART_WAIT"

// HostRootEnv roots every path the server's setup reads and writes on the host ("" for /),
// for tests: the facts read there, and the root script gets it as its first argument.
const HostRootEnv = "DEVOPSY_INTERNAL_HOST_ROOT"

// ServerSetupScript runs ServerSetupFlag with args on the server, after the prelude's checks,
// refusing a project user (askedAsProjectUser): the server's setup is its deploy user's.
func ServerSetupScript(t *Target, args []string) string {
	return "set -eu\n" + serverPrelude(t) + askedAsProjectUser + devopsyCall("", append([]string{ServerSetupFlag}, args...), true) + "\n"
}

// The lines SetupRootScript prints on stdout, one per change.
const (
	SetupPut      = "put"         // put <path>
	SetupMade     = "made"        // made <root>
	SetupGave     = "gave"        // gave <root>
	SetupRaised   = "raised"      // raised <key>
	SetupMarked   = "marked"      // the restart marker and flag raised
	SetupAsked    = "asked"       // the marker written, Docker's restart about to be asked
	SetupQueued   = "queued"      // Docker's restart queued
	SetupNoSystem = "nosystemctl" // systemctl refused from the chroot: marked instead
)

// SetupRootScript is what an apply runs as root on the host, through the util image chrooted
// into it (the system's files under $host_root, the release root as given), in this order:
// the release root made and given to the deploy user; the sysctl file, the live buffers
// raised only when lower, the mounts drop-in; daemon.json, by dockerd's own check with the
// unit's flags, last, since a put its check refuses ends the run; each file put only when it
// differs and passes its check; then Docker restarted (the marker written first, on a daemon.json dockerd takes,
// daemon-reload, the restart queued with --no-block), or, when daemon.json changed, the
// marker written and the system's restart flag raised. Its arguments, in order: the host
// root, the deploy user's uid:gid, 1 to make or give the root, the root, daemon.json's text,
// the sysctl file's, the drop-in's (each "" to leave it), 1 to raise the buffers, restart
// (now) or mark (the marker and flag, whatever changed; "" marks only when daemon.json
// changed), the InvocationID for the marker, the unit's dockerd flags, one a line, and the
// run's own token, which a restart's marker carries after the id: the id is what pending
// compares, the token which run asked the restart.
var SetupRootScript = `set -eu
umask 022
host_root=$1 owner=$2 give=$3 root=$4 daemon=$5 sysctl=$6 dropin=$7 raise=$8 after=$9 id=${10}
dockerd_flags=${11:-} token=${12:-}
export dockerd_flags
` + hostPut + hostRestartFlag + `# dockerd's own check of a daemon.json ($1) with the flags docker.service starts it with (one
# a line), as Docker starts on it: saying only what it refuses.
validate='set -f; IFS="
"; out=$(dockerd --validate --config-file "$1" $dockerd_flags 2>&1) || { printf "%s\n" "$out" >&2; exit 1; }'
changed=
if [ -n "$give" ]; then
  p=$root
  if [ ! -e "$p" ]; then install -d -m 755 "$p"; echo "` + SetupMade + ` $root"; fi
  if [ -n "$(find -H "$p" -maxdepth 0 -user 0)" ]; then chown "$owner" "$p"; echo "` + SetupGave + ` $root"; fi
fi
if [ -n "$sysctl" ]; then
  mkdir -p "$host_root` + path.Dir(setup.SysctlFile) + `"
  if put "$host_root` + setup.SysctlFile + `" 644 true "$sysctl"; then echo "` + SetupPut + ` ` + setup.SysctlFile + `"; fi
fi
if [ -n "$raise" ]; then
  for k in ` + setup.BufferKeys[0] + ` ` + setup.BufferKeys[1] + `; do
    v=$(cat "$host_root/proc/sys/$(echo "$k" | tr . /)" 2>/dev/null || echo 0)
    case $v in ''|*[!0-9]*) v=0 ;; esac
    if [ "$v" -lt ` + fmt.Sprint(setup.BufferSize) + ` ] && sysctl -qw "$k=` + fmt.Sprint(setup.BufferSize) + `"; then echo "` + SetupRaised + ` $k"; fi
  done
fi
if [ -n "$dropin" ]; then
  mkdir -p "$host_root` + path.Dir(setup.MountsDropIn) + `"
  if put "$host_root` + setup.MountsDropIn + `" 644 true "$dropin"; then
    echo "` + SetupPut + ` ` + setup.MountsDropIn + `"
    # systemd's view of docker.service, so it never says a drop-in changed on disk, whatever
    # follows (a restart the daemon.json put after it may stop); a reload refused waits for
    # the next boot.
    SYSTEMD_IGNORE_CHROOT=1 systemctl daemon-reload || :
  fi
fi
# daemon.json last of the puts: its check (dockerd's) is the one that refuses, and a put it
# refuses ends the run with what came before put.
if [ -n "$daemon" ]; then
  mkdir -p "$host_root` + path.Dir(setup.DaemonFile) + `"
  if put "$host_root` + setup.DaemonFile + `" keep:644 "$validate" "$daemon"; then
    changed=1
    echo "` + SetupPut + ` ` + setup.DaemonFile + `"
  fi
fi
mark() {
  restart_flag ` + setup.RestartMarker + ` "$id"
  echo ` + SetupMarked + `
}
case $after in
  restart)
    # A file Docker refuses would keep it down: never restarted on one, whoever wrote it.
    if [ -e "$host_root` + setup.DaemonFile + `" ] && ! sh -c "$validate" sh "$host_root` + setup.DaemonFile + `"; then
      ` + echoLine(report.ErrorLevel, "dockerd refuses "+setup.DaemonFile+", so Docker would not start again: not restarted", report.Fix("correct the file, then run --server-setup again")) + `
      exit 4
    fi
    # The marker first, with the InvocationID read before the run: a restart that comes ends
    # it by its new id, and one that does not leaves Docker pending, whatever reaches the
    # caller of a run the restart cuts.
    printf '%s\n' "$id${token:+ $token}" >"$host_root` + setup.RestartMarker + `"
    echo ` + SetupAsked + `
    # The unit's start counter cleared: docker.service allows a few starts a minute
    # (StartLimitBurst), a fresh server's install and restarts by hand count, and a restart an
    # operator asked for is no crash loop. Its failure changes nothing of the restart.
    SYSTEMD_IGNORE_CHROOT=1 systemctl reset-failed docker.service || :
    if SYSTEMD_IGNORE_CHROOT=1 systemctl daemon-reload && SYSTEMD_IGNORE_CHROOT=1 systemctl restart --no-block docker.service; then
      echo ` + SetupQueued + `
    else
      ` + echoLine(report.WarnLevel, "systemctl could not restart Docker from devopsy's container") + `
      echo ` + SetupNoSystem + `
      mark
    fi ;;
  mark) mark ;;
  *) [ -z "$changed" ] || mark ;;
esac
`
