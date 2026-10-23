package main

import (
	"context"
	"errors"
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

// runMntOwners implements the hidden --mnt-owners [restored], run by an
// activation in current right after its switch: every owned entry
// created when missing and scanned whole. A failure is a failed run
// step. restored is a rollback's and the restart's after a failed run,
// from the restored release's own labels: labels that cannot be read
// there (names from before a change, ./mnt whole, a compose error) are a
// warning and nothing is given away, as rollbacks never check roles or
// imports: a rollback must not fail exactly when it is needed. A chown
// failing on labels that were read still fails it.
func runMntOwners(cwd string, args []string) int {
	restored := len(args) == 1 && args[0] == mntOwnersRestored
	if len(args) > 0 && !restored {
		report.Error(cli.MntOwnersFlag + " takes nothing, or " + mntOwnersRestored)
		return 2
	}
	// Short and needed whole: a gone session is no reason to stop
	// halfway.
	signal.Ignore(syscall.SIGPIPE, syscall.SIGHUP)
	owners, mnt, err := projectOwners(cwd)
	if err != nil && restored {
		report.Warn("the entries of mnt keep their owners: the restored release's labels cannot be read", err.Error())
		return 0
	}
	if err == nil {
		err = ownMnt(context.Background(), mnt, owners, fmt.Sprintf("devopsy-owners-%s-%d", composeName(cwd), os.Getpid()))
	}
	if err != nil {
		report.Fail(err)
		return 1
	}
	return 0
}

// mntOwnersRestored is --mnt-owners' word for a restored release
// (runMntOwners).
const mntOwnersRestored = "restored"

// runCheckMounts implements the hidden --check-mounts, run by an
// activation in the new release before its switch, after its prepare
// steps and --missing-vars (cli.CheckMounts). The labels are read as
// --mnt-owners reads them after the switch (cli.SyncServices, the
// leftover sync capability included), so a release this passes never
// fails there for its labels. A compose configuration that cannot be
// read is no refusal, as for --check-names: the release's up says what
// is wrong with it. With an entry owned, it also refuses an owned entry
// that is a symbolic link (ownedLinks), and readies the util image the
// owners' step runs after the switch, so a registry down refuses the
// release instead of failing it there.
func runCheckMounts(cwd string, args []string) int {
	if len(args) > 0 {
		report.Error(cli.CheckMountsFlag + " takes nothing")
		return 2
	}
	projectDir, services, err := cli.SyncServices(cwd, os.Environ())
	if errors.Is(err, cli.ErrComposeConfig) {
		return 0
	}
	if err != nil {
		err = &report.Failure{Msg: "the mounts check refuses the release, whose labels cannot be read: it does not go live", Details: []string{err.Error()}}
	}
	if err == nil {
		err = cli.CheckMounts(services)
	}
	if err == nil {
		err = checkOwned(projectDir, services)
	}
	if err != nil {
		report.Fail(err)
		return 1
	}
	return 0
}

// checkOwned refuses owned paths of mnt through a symbolic link, and
// readies the util image, when the project owns any entry.
func checkOwned(projectDir string, services []cli.SyncService) error {
	owned, err := cli.MntOwners(services)
	if err != nil || len(owned) == 0 {
		return err
	}
	maps.Copy(owned, cli.OwnedPaths(services, owned))
	mnt, err := dataMnt(projectDir)
	if err != nil {
		return err
	}
	if links := ownedLinks(mnt, owned); len(links) > 0 {
		var details []string
		for _, l := range links {
			details = append(details, "mnt/"+l+" is a symbolic link")
		}
		return &report.Failure{
			Msg:     "an owned entry of mnt goes through a symbolic link: Docker would mount what it points to, and devopsy give that to the entry's owner: the release does not go live",
			Details: details,
			Fixes:   []report.Remedy{report.Fix("make each a directory of its own in shared/mnt, moving what it points to there")},
		}
	}
	return ensureUtilImage(context.Background())
}

// ownedLinks are the paths of mnt, among owned's and their parents
// below mnt, that are symbolic links, sorted.
func ownedLinks(mnt string, owned map[string]string) []string {
	var links []string
	for p := range owned {
		parts := strings.Split(p, "/")
		for i := range parts {
			q := strings.Join(parts[:i+1], "/")
			fi, err := os.Lstat(filepath.Join(mnt, q))
			if err != nil {
				break
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				if !slices.Contains(links, q) {
					links = append(links, q)
				}
				break
			}
		}
	}
	slices.Sort(links)
	return links
}

// localMnt readies the owned paths of mnt before compose starts
// anything locally (cli.Plan.Starts): each missing one (an entry,
// a bind source below one) is made, and on Linux an entry whose top
// directory is not its owner's is given away in a util container, only
// then, and scanned whole there: an entry whose top is right is never
// scanned, since that would run before every such command. On macOS the
// top is never checked: Docker Desktop and OrbStack let any container
// user write the Mac's files, and show them the owner Docker asks for,
// whatever the Mac says. Labels that cannot be read (names from before
// a change, a compose error) are a warning and the command goes on: it
// is compose's, which says what is wrong with its files, and a label
// devopsy reads must never stop a plain up (on a server, a rollback's
// run step: cli.Plan.Starts is never set in a release there).
func localMnt(cwd string) error {
	owners, mnt, err := projectOwners(cwd)
	if err != nil {
		report.Warn("the entries of mnt are left as they are: the labels cannot be read", err.Error())
		return nil
	}
	if len(owners) == 0 {
		return nil
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
