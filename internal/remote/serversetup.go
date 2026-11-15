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
	// SetupOther: other <user> <evidence>, the deploy user's record not made, the server showing
	// another deploy user (a docker member's setup record, a release root of another's), and
	// nothing else applied (exit 4).
	SetupOther = "other"
)

// hostDropIn puts the mounts drop-in, $dropin ("" to leave it), under $host_root, says so and
// sets reload: --server-setup's root run and --project-users setup's (ProjectUsersSetupScript)
// both keep it.
var hostDropIn = `if [ -n "$dropin" ]; then
  mkdir -p "$host_root` + path.Dir(setup.MountsDropIn) + `"
  if put "$host_root` + setup.MountsDropIn + `" 644 true "$dropin"; then
    echo "` + SetupPut + ` ` + setup.MountsDropIn + `"
    reload=1
  fi
fi
`

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
// compares, the token which run asked the restart; then the deploy user's record's text ("" to
// leave it), put first of all.
var SetupRootScript = `set -eu
umask 022
host_root=$1 owner=$2 give=$3 root=$4 daemon=$5 sysctl=$6 dropin=$7 raise=$8 after=$9 id=${10}
dockerd_flags=${11:-} token=${12:-} deploy_user=${13:-}
export dockerd_flags
` + hostPut + hostRestartFlag + `# dockerd's own check of a daemon.json ($1) with the flags docker.service starts it with (one
# a line), as Docker starts on it: saying only what it refuses.
validate='set -f; IFS="
"; out=$(dockerd --validate --config-file "$1" $dockerd_flags 2>&1) || { printf "%s\n" "$out" >&2; exit 1; }'
changed=
# The deploy user's record, put only while it is missing or names the user it would: a record
# naming another is never rewritten, read here as root under this run's own write, whatever the
# facts read before (another user's setup at once). A missing one is made by a link of a whole
# file, which fails where a run at once made it first.
if [ -n "$deploy_user" ]; then
  du=$host_root` + setup.DeployUserFile + `
  mkdir -p "${du%/*}"
  ` + deployUserNameShell + `du_tmp=$(mktemp "$du.XXXXXX")
  printf '%s' "$deploy_user" >"$du_tmp" && chmod 644 "$du_tmp"
  du_want=$(deploy_user_name "$du_tmp")
  du_made= du_other=
  if [ ! -e "$du" ] && [ ! -L "$du" ]; then
    # With no record yet, a server that shows another deploy user is no one's to record: a
    # docker member other than this one with devopsy's setup record in its home's config, else
    # a release root another user than root owns (cmd/devopsy's otherDeployUser, as this login
    # could see it).
    du_gid=$(awk -F: '$1 == "docker" { print $3 }' "$host_root/etc/group" 2>/dev/null || :)
    for u in $(awk -F: '$1 == "docker" { gsub(",", " ", $4); print $4 }' "$host_root/etc/group" 2>/dev/null || :) \
      $(awk -F: -v g="$du_gid" 'g != "" && $4 == g { print $1 }' "$host_root/etc/passwd" 2>/dev/null || :); do
      [ "$u" != "$du_want" ] || continue
      h=$(awk -F: -v u="$u" '$1 == u { print $6; exit }' "$host_root/etc/passwd" 2>/dev/null || :)
      if [ -n "$h" ] && [ -e "$host_root$h/.config/devopsy/` + setup.RecordFile + `" ]; then du_other="$u its setup record $h/.config/devopsy/` + setup.RecordFile + `"; break; fi
    done
    if [ -z "$du_other" ] && [ -e "$root" ]; then
      o=$(ls -nd "$root" | awk '{ print $3 }')
      if [ "$o" != 0 ] && [ "$o" != "${owner%%:*}" ]; then
        on=$(awk -F: -v i="$o" '$3 == i { print $1; exit }' "$host_root/etc/passwd" 2>/dev/null || :)
        du_other="${on:-$o} the release root $root is its"
      fi
    fi
    if [ -n "$du_other" ]; then
      rm -f "$du_tmp"
      echo "` + SetupOther + ` $du_other"
      exit 4
    fi
    if ln "$du_tmp" "$du" 2>/dev/null; then du_made=1; fi
  fi
  rm -f "$du_tmp"
  if [ "$(deploy_user_name "$du")" != "$du_want" ]; then
    ` + echoLine(report.ErrorLevel, setup.DeployUserFile+" names another deploy user than this setup runs as: left as it is, and nothing else applied") + `
    exit 4
  fi
  if [ -n "$du_made" ] || put "$du" 644 true "$deploy_user"; then echo "` + SetupPut + ` ` + setup.DeployUserFile + `"; fi
fi
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
reload=
` + hostDropIn + `if [ -n "$reload" ]; then
  # systemd's view of docker.service, so it never says a drop-in changed on disk, whatever
  # follows (a restart the daemon.json put after it may stop); a reload refused waits for
  # the next boot.
  SYSTEMD_IGNORE_CHROOT=1 systemctl daemon-reload || :
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
    # (StartLimitBurst), a fresh server's install and restarts by hand count, and a restart
    # someone asked for is no crash loop. Its failure changes nothing of the restart.
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
