package remote

import (
	"path"

	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// The lines ProjectUsersSetupScript prints on stdout, one per change, beside SetupPut,
// SetupMade and SetupGave.
const (
	DoorSetupMode    = "mode"    // mode <space>: set to 0755
	DoorSetupGroup   = "group"   // group <group>: made
	DoorSetupMember  = "member"  // member <user>: put in the group
	DoorSetupMounted = "mounted" // mounted <space>: its unit enabled and started
	DoorSetupHidden  = "hidden"  // /proc remounted by its unit
	DoorSetupPolkit  = "polkit"  // polkit restarted with the group
)

// safePath is safe_path, which says whether no user but root and the deploy user ($owner's uid)
// can change any part of $space's path, judged right before the root run acts on it, as
// devopsy's facts judged it (swappable): no part a link, every parent root's or the deploy
// user's, and none others may write unless it is sticky and the part in it is there, no link,
// root's or the deploy user's. Once it holds, only root and the deploy user can swap a part, so
// the make, chown -h, chmod and mount that follow act on the space itself. A part not there yet
// is made by root under the last one there. ls -ldn's fields are its type and mode, its links,
// then its owner's number.
var safePath = `safe_path() {
  sp_uid=${owner%%:*} sp_cur=/ sp_rest=${space#/}
  while [ -n "$sp_rest" ]; do
    sp_next=${sp_cur%/}/${sp_rest%%/*}
    case $sp_rest in */*) sp_rest=${sp_rest#*/} ;; *) sp_rest= ;; esac
    set -- $(ls -ldn "$sp_cur" 2>/dev/null)
    [ $# -ge 3 ] || return 1
    case $1 in d*) ;; *) return 1 ;; esac
    [ "$3" = 0 ] || [ "$3" = "$sp_uid" ] || return 1
    case $1 in ?????w*|????????w*)
      case $1 in ?????????[tT]*) ;; *) return 1 ;; esac
      set -- $(ls -ldn "$sp_next" 2>/dev/null)
      [ $# -ge 3 ] || return 1
      case $1 in l*) return 1 ;; esac
      [ "$3" = 0 ] || [ "$3" = "$sp_uid" ] || return 1 ;;
    esac
    [ -e "$sp_next" ] || [ -L "$sp_next" ] || return 0
    sp_cur=$sp_next
  done
  [ ! -L "$space" ]
}
`

// ProjectUsersSetupScript is what --project-users setup's apply runs as root on the host,
// through the util image chrooted into it, its system files under $host_root and the space as
// the host names it, in this order: the space made or given to the deploy user, its mode set
// back to 0755, its mount unit put; the group made, the deploy user put in it, /proc's unit put
// with the group's number, polkit's drop-in; then systemd reloaded and the space's unit enabled
// and started; the mounts drop-in put, naming the space only once its mount holds (a
// RequiresMountsFor= of a mount that fails keeps Docker from starting at the next boot, so a
// mount that failed gets the drop-in without the space, as --server-setup then wants it);
// /proc's unit enabled and run again (a oneshot that remains active runs again only by a
// restart), polkit restarted. A mount goes only through systemd, never mount in the container:
// a mount made in the container's namespace does not reach the host. Each file is put only when
// it differs, and systemctl enable runs --quiet: the run says its own line per change, and
// systemd's links made would show raw among devopsy's. A step that fails is said and the next
// independent one goes (the processes beside a mount that failed), the run then exiting 6. Its arguments, in order: the host root,
// the deploy user's uid:gid, the space, 1 to make or give it, 1 to set its mode, its unit's
// path, the unit's text ("" to leave it), 1 to enable and start it, the drop-in's text ("" to
// leave it), 1 for the processes, the deploy user, /proc's unit's text with setup.GIDMark for
// the group's number, polkit's drop-in's text ("" where there is no polkit), the unit's name as
// a shell word (QuoteWord), for the command a failed mount offers, and the drop-in's text
// without the space. The space is touched only while no other user can change a part of its
// path (safe_path), else it is left, said, and the drop-in goes without it. systemd's name for
// a path with a - holds a backslash (\x2d), which a shell drops from a word copied unquoted and
// some shells' echo reads as an escape, so the unit's path is said with printf.
var ProjectUsersSetupScript = `set -eu
umask 022
host_root=$1 owner=$2 space=$3 give=$4 mode=$5 unit=$6 unit_text=$7 mount=$8 dropin=$9 procs=${10}
user=${11} proc_text=${12} polkit=${13} unit_word=${14} dropin_bare=${15}
sd() { SYSTEMD_IGNORE_CHROOT=1 systemctl "$@"; }
` + hostPut + safePath + `reload= failed= held=1
if [ -n "$give$mode$unit_text$mount" ] && ! safe_path; then
  ` + echoLine(report.ErrorLevel, "$space, or a directory above it, could be changed by another user: devopsy did not touch it") + `
  give= mode= unit_text= mount= failed=1 held=
fi
if [ -n "$give" ]; then
  if [ ! -e "$space" ]; then install -d -m 755 "$space"; echo "` + SetupMade + ` $space"; fi
  if [ -n "$(find "$space" -maxdepth 0 -user 0)" ]; then chown -h "$owner" "$space"; echo "` + SetupGave + ` $space"; fi
fi
if [ -n "$mode" ]; then chmod 755 "$space"; echo "` + DoorSetupMode + ` $space"; fi
if [ -n "$unit_text" ]; then
  mkdir -p "$host_root${unit%/*}"
  if put "$host_root$unit" 644 true "$unit_text"; then printf '%s\n' "` + SetupPut + ` $unit"; reload=1; fi
fi
restart_polkit=
if [ -n "$procs" ]; then
  group=` + setup.ProcGroup + `
  getent group "$group" >/dev/null || { groupadd --system "$group"; echo "` + DoorSetupGroup + ` $group"; }
  gid=$(getent group "$group" | cut -d: -f3)
  case $gid in ''|*[!0-9]*)
    ` + echoLine(report.ErrorLevel, "the group "+setup.ProcGroup+" has no number") + `
    procs= failed=1 ;;
  esac
fi
if [ -n "$procs" ]; then
  getent group "$group" | cut -d: -f4 | tr , '\n' | grep -qxF "$user" || { usermod -aG "$group" "$user"; echo "` + DoorSetupMember + ` $user"; }
  mkdir -p "$host_root` + path.Dir(setup.ProcUnit) + `"
  if put "$host_root` + setup.ProcUnit + `" 644 true "${proc_text%%` + setup.GIDMark + `*}$gid${proc_text#*` + setup.GIDMark + `}"; then
    echo "` + SetupPut + ` ` + setup.ProcUnit + `"
    reload=1
  fi
  if [ -n "$polkit" ]; then
    mkdir -p "$host_root` + path.Dir(setup.PolkitDropIn) + `"
    if put "$host_root` + setup.PolkitDropIn + `" 644 true "$polkit"; then
      echo "` + SetupPut + ` ` + setup.PolkitDropIn + `"
      reload=1 restart_polkit=1
    fi
  fi
fi
if [ -n "$reload" ]; then
  sd daemon-reload >&2 || { ` + echoLine(report.ErrorLevel, "systemctl could not reload systemd from devopsy's container: nothing was mounted") + `; exit 6; }
fi
if [ -n "$mount" ]; then
  if sd enable --quiet --now "${unit##*/}" >&2; then
    echo "` + DoorSetupMounted + ` $space"
  else
    ` + echoLine(report.ErrorLevel, "systemd did not mount $space", report.Hint("what it said, as root:", "journalctl -u $unit_word")) + `
    failed=1 held=
  fi
fi
[ -n "$held" ] || dropin=$dropin_bare
reload=
` + hostDropIn + `if [ -n "$reload" ]; then
  # systemd's view of docker.service, so it never says the drop-in changed on disk; a reload
  # refused waits for the next boot.
  sd daemon-reload >&2 || :
fi
if [ -n "$procs" ]; then
  if { sd enable --quiet ` + setup.ProcUnitName + ` && sd restart ` + setup.ProcUnitName + `; } >&2; then
    echo ` + DoorSetupHidden + `
    if [ -n "$restart_polkit" ]; then sd try-restart polkit.service >&2 || :; echo ` + DoorSetupPolkit + `; fi
  else
    ` + echoLine(report.ErrorLevel, "systemd did not remount /proc", report.Hint("what it said, as root:", "journalctl -u "+setup.ProcUnitName)) + `
    failed=1
  fi
fi
[ -z "$failed" ] || exit 6
`
