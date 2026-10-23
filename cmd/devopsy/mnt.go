package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// The owners of mnt's entries (cli.MntOwnerLabel): devopsy creates and
// gives them away wherever it creates or writes entries, as root in its
// util image, since the deploy user cannot chown and devopsy copies
// owners exactly (rsync --numeric-ids, tar --numeric-owner).

// chownEntry is a shell function giving the entry of /mnt $1 and
// everything in it to $2 (<uid>:<gid>), scanned whole: what devopsy did
// not write (an upload by hand, data from before the label) has the
// wrong owner deep in a tree under a correct top. Never through a
// symbolic link the app made in its entries: not the file's own (-h,
// and find's default -P), nor one swapped in for a directory on the way
// (-execdir: chown runs in the file's directory, which find entered
// safely, on ./<name>, so it cannot reach another entry).
//
// The scan runs while the services may run (a release's old containers,
// an import's service): a file one removes after find saw it fails its
// chown, and find with it. A vanished file has no owner to fix, so a
// failed scan is looked at again, chowning nothing: only something left
// with another owner fails it (measured: 31 of 200 scans of a busy entry
// failed before).
const chownEntry = `chown_entry() {
  ce_err=$(find "/mnt/$1" \( ! -user "${2%%:*}" -o ! -group "${2#*:}" \) -execdir chown -h "$2" {} + 2>&1) && return 0
  ce_left=$(find "/mnt/$1" \( ! -user "${2%%:*}" -o ! -group "${2#*:}" \) -print -quit 2>/dev/null) || true
  [ -n "$ce_left" ] || return 0
  printf '%s\n' "$ce_err" >&2
  return 1
}
`

// ownPaths is a shell function: each argument <path>=<uid>:<gid>, a
// path of /mnt (an entry, or a bind source below one), has each of its
// directories missing made (0755, as Docker would) and given to its
// owner, never past a symbolic link; then each entry among them (a path
// without /) is scanned whole (chownEntry). An owner has no =.
const ownPaths = `own_paths() {
  for po; do
    o=${po##*=} d=/mnt rest=${po%=*}/
    while [ -n "$rest" ]; do
      d=$d/${rest%%/*} rest=${rest#*/}
      [ ! -L "$d" ] || break
      if [ ! -e "$d" ]; then
        mkdir -m 0755 "$d"
        chown -h "$o" "$d"
      fi
    done
  done
  for po; do
    case ${po%=*} in */*) ;; *) chown_entry "${po%=*}" "${po##*=}" ;; esac
  done
}
`

// ownEntries is the util container's script for owned paths, its
// arguments (ownPaths).
const ownEntries = "set -eu\n" + chownEntry + ownPaths + `own_paths "$@"
`

// ownerArgs are owned paths (path to owner: entries, and bind sources
// below them) as ownEntries' arguments, sorted, and the line that says
// them: the entries, and the paths below an entry not among them.
func ownerArgs(owned map[string]string) (args []string, line string) {
	paths := slices.Sorted(maps.Keys(owned))
	for _, p := range paths {
		args = append(args, p+"="+owned[p])
	}
	var parts []string
	for _, a := range args {
		i := strings.LastIndex(a, "=")
		p, o := a[:i], a[i+1:]
		if e, _, below := strings.Cut(p, "/"); below && owned[e] != "" {
			continue
		}
		parts = append(parts, "mnt/"+p+" to "+o)
	}
	return args, "giving " + strings.Join(parts, ", ")
}

// ownMnt makes the owned paths of mnt (resolved: on servers shared/mnt)
// missing and gives the entries among them to their owners, scanned
// whole, in one util container named name, without a network.
func ownMnt(ctx context.Context, mnt string, owned map[string]string, name string) error {
	if len(owned) == 0 {
		return nil
	}
	if err := ensureUtilImage(ctx); err != nil {
		return err
	}
	args, line := ownerArgs(owned)
	report.Info(line)
	run := append([]string{"--network", "none", "-v", mnt + ":/mnt", remote.UtilImage, "sh", "-c", ownEntries, "sh"}, args...)
	if err := dockerRun(ctx, name, run...); err != nil {
		return fmt.Errorf("giving mnt's entries to their owners (%s): %w", cli.MntOwnerLabel, err)
	}
	return nil
}

// projectOwners are the owned paths of the project at cwd's mnt, path to
// owner, from its labels (every profile): each owned entry
// (cli.MntOwners), and each bind source below one (cli.OwnedPaths), so
// Docker never makes a part of it root's. And its mnt resolved.
func projectOwners(cwd string) (owned map[string]string, mnt string, err error) {
	projectDir, services, err := cli.SyncServices(cwd, os.Environ())
	if err != nil {
		return nil, "", err
	}
	owned, err = cli.MntOwners(services)
	if err != nil || len(owned) == 0 {
		return owned, "", err
	}
	maps.Copy(owned, cli.OwnedPaths(services, owned))
	mnt, err = dataMnt(projectDir)
	return owned, mnt, err
}

// runMntOwners implements the hidden --mnt-owners, run by an activation
// in current right after its switch (a rollback's and a restart's too,
// from the restored release's own labels): every owned entry created
// when missing and scanned whole. A failure is a failed run step.
func runMntOwners(cwd string, args []string) int {
	if len(args) > 0 {
		report.Error(cli.MntOwnersFlag + " takes nothing")
		return 2
	}
	// Short and needed whole: a gone session is no reason to stop
	// halfway.
	signal.Ignore(syscall.SIGPIPE, syscall.SIGHUP)
	owners, mnt, err := projectOwners(cwd)
	if err == nil {
		err = ownMnt(context.Background(), mnt, owners, fmt.Sprintf("devopsy-owners-%s-%d", composeName(cwd), os.Getpid()))
	}
	if err != nil {
		report.Fail(err)
		return 1
	}
	return 0
}

// runCheckMounts implements the hidden --check-mounts, run by an
// activation in the new release before its switch (cli.CheckMounts). A
// compose configuration that cannot be read is no refusal, as for
// --check-names: the release's up says what is wrong with it.
func runCheckMounts(cwd string, args []string) int {
	if len(args) > 0 {
		report.Error(cli.CheckMountsFlag + " takes nothing")
		return 2
	}
	projectDir, err := cli.FindProjectDir(cwd)
	if err != nil {
		report.Fail(err)
		return 1
	}
	out, err := cli.ComposeConfig(cwd, os.Environ())
	if err != nil {
		return 0
	}
	services, err := cli.ParseSyncServices(out, projectDir)
	if err == nil {
		err = cli.CheckMounts(services)
	}
	if err != nil {
		report.Fail(err)
		return 1
	}
	return 0
}

// localMnt readies the owned paths of mnt before compose starts
// anything locally (up, start, run, create): each missing one (an entry,
// a bind source below one) is made, and on Linux an entry whose top
// directory is not its owner's is given away in a util container, only
// then, and scanned whole there: an entry whose top is right is never
// scanned, since that would run before every such command. On macOS the
// top is never checked: Docker Desktop and OrbStack let any container
// user write the Mac's files, and show them the owner Docker asks for,
// whatever the Mac says.
func localMnt(cwd string) error {
	owners, mnt, err := projectOwners(cwd)
	if err != nil || len(owners) == 0 {
		return err
	}
	wrong := map[string]string{}
	for e, o := range owners {
		p := filepath.Join(mnt, e)
		fi, err := os.Lstat(p)
		switch {
		case err != nil && runtime.GOOS == "darwin":
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case err != nil:
			wrong[e] = o
		case runtime.GOOS != "darwin" && !strings.Contains(e, "/"):
			// A bind source below an entry is only made when missing.
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && fmt.Sprintf("%d:%d", st.Uid, st.Gid) != o {
				wrong[e] = o
			}
		}
	}
	return ownMnt(context.Background(), mnt, wrong, fmt.Sprintf("devopsy-owners-%s-%d", composeName(cwd), os.Getpid()))
}
