package main

import "github.com/hanoii/devopsy-cli/internal/remote"

// The scripts devopsy's util containers run, as root on the host's files, named
// so that the door can take exactly these from a project user and nothing else
// (doorKnown). One given by a caller is never run.
const (
	// exportTarScript is an entry's or a volume's tar on stdout, for --export.
	exportTarScript = `set -o pipefail; cd /x && find "$@" \( -type f -o -type d -o -type l \) -print0 | tar -cf - --numeric-owner --hard-dereference --sparse --null --no-recursion -T -`
	// reflinkProbeScript tries one file of each source with a reflink into /c.
	reflinkProbeScript = `t=/c/.reflink.$$
for src; do
  f=$(find "$src" -type f -print -quit 2>/dev/null) || f=
  [ -n "$f" ] || continue
  cp --reflink=always "$f" "$t" 2>/dev/null || { rm -f "$t"; exit 1; }
  rm -f "$t"
done`
	// snapshotTarScript is the tar of one entry or volume, into a snapshot.
	snapshotTarScript = `set -o pipefail; cd "/m/$1" && find "$2" \( -type f -o -type d -o -type l \) -print0 | tar -cf - --numeric-owner --hard-dereference --sparse --null --no-recursion -T - | nice zstd -q -T0 -f -o "/o/$3" && chown "$4" "/o/$3" && chmod 600 "/o/$3"`
	// restoreStreamScript decompresses a snapshot's tar into a FIFO.
	restoreStreamScript = `zstd -dcq -- "/s/$1" >/o/tar`
	// spaceProbeScript says, per path, its filesystem (its parent's while
	// missing), that one's free kilobytes, and the path's own use.
	spaceProbeScript = `for p; do d=$p; [ -e "$d" ] || d=${d%/*}; set -- $(df -Pk "$d" | tail -n 1); u=0; [ ! -e "$p" ] || u=$(du -sk "$p" | cut -f1); printf '%s\t%s\t%s\t%s\n' "$p" "$1" "$4" "$u"; done`
	// receiveScript is a sync's receive: rsync, its $0, over the session (remote.ReceiveShim).
	receiveScript = remote.ReceiveShim
	// rsyncDaemonScript is a sync's read-only rsync daemon, from its settings: they wait
	// for each session, which runs door.DaemonServe through docker exec.
	rsyncDaemonScript = `printf "%s\n" "$RSYNCD_CONF" > /tmp/rsyncd.conf && exec sleep infinity`
)
