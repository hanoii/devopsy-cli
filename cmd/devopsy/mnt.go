package main

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
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
// wrong owner deep in a tree under a correct top. Only what differs is
// changed, in batches (-exec ... +: -execdir ran chown once per
// directory, 22.7 s against 0.43 s for 20,200 directories), never
// through the file's own link (-h, and find's default -P). It runs in a
// container that mounts that entry alone (ownMnt): a directory the app
// swaps for a link during the scan leads chown only into the entry
// itself or the throwaway container, never another entry or the host.
//
// The scan runs while the services may run (a release's old containers,
// an import's service): a file one removes after find saw it fails its
// chown, and find with it. A vanished file has no owner to fix, so a
// failed scan is looked at again, chowning nothing: only something left
// with another owner fails it (measured: 31 of 200 scans of a busy entry
// failed before), or a look that fails itself (a directory find cannot
// read: whatever is in it was never seen). -ignore_readdir_race keeps a
// file removed between find's reading of its directory and its look at
// it from failing either.
//
// Only files, directories and symbolic links are given away, in both
// looks: a device node, a FIFO or a socket keeps its owner, and is no
// file left with another owner either. Why: the entry's owner given a
// block device of the server's disk has the raw disk; devopsy never
// makes one (rsync --no-D, an mnt import refuses them), and one made
// otherwise (by root on the host) stays root's.
const chownEntry = `chown_entry() {
  ce_err=$(find "/mnt/$1" -ignore_readdir_race \( -type f -o -type d -o -type l \) \( ! -user "${2%%:*}" -o ! -group "${2#*:}" \) -exec chown -h "$2" {} + 2>&1) && return 0
  if ! ce_left=$(find "/mnt/$1" -ignore_readdir_race \( -type f -o -type d -o -type l \) \( ! -user "${2%%:*}" -o ! -group "${2#*:}" \) -print -quit); then
    printf '%s\n' "$ce_err" >&2
    return 1
  fi
  [ -n "$ce_left" ] || return 0
  printf '%s\n' "$ce_err" >&2
  return 1
}
`

// ownEntry is the util container's script for one owned entry, its
// arguments the entry and its owner (chownEntry).
const ownEntry = "set -eu\n" + chownEntry + `chown_entry "$1" "$2"
`

// ownerLine says what ownMnt gives away: each entry and its owner.
func ownerLine(entries []string, owned map[string]string) string {
	var parts []string
	for _, e := range entries {
		parts = append(parts, "mnt/"+e+" to "+owned[e])
	}
	return "giving " + strings.Join(parts, ", ")
}

// ownMnt gives the owned entries of mnt (resolved: on servers
// shared/mnt), entry to owner, to their owners, each scanned whole in a
// util container of its own, named name-<n>, that mounts that entry
// alone, without a network (chownEntry). A missing entry is made first,
// 0755 as Docker would, so the container mounts it; one that is a
// symbolic link is refused, since Docker would mount what it points to
// (the mounts check refuses it before a release's switch already).
func ownMnt(ctx context.Context, mnt string, owned map[string]string, name string) error {
	if len(owned) == 0 {
		return nil
	}
	entries := slices.Sorted(maps.Keys(owned))
	for _, e := range entries {
		if err := makeEntry(mnt, e); err != nil {
			return err
		}
	}
	if err := ensureUtilImage(ctx); err != nil {
		return err
	}
	report.Info(ownerLine(entries, owned))
	for i, e := range entries {
		mount, err := bindMount(filepath.Join(mnt, e), "/mnt/"+e)
		if err != nil {
			return err
		}
		if err := dockerRunFresh(ctx, fmt.Sprintf("%s-%d", name, i), nil, "--network", "none", "--mount", mount, remote.UtilImage, "sh", "-c", ownEntry, "sh", e, owned[e]); err != nil {
			return fmt.Errorf("giving mnt/%s to %s (%s): %w", e, owned[e], cli.MntOwnerLabel, err)
		}
	}
	return nil
}

// dockerRunFresh is dockerRunIO, stdout errOut, for a container named
// after devopsy's pid (ownMnt's, extractEntries'): a container of that
// name can only be a leftover of a devopsy killed outright whose pid this
// one reuses, still running (--rm removes it only once it ends), and
// docker run would refuse the name. It goes first.
func dockerRunFresh(ctx context.Context, name string, stdin io.Reader, args ...string) error {
	if err := removeContainers("name=^/?" + regexp.QuoteMeta(name) + "$"); err != nil {
		return err
	}
	return dockerRunIO(ctx, name, stdin, nil, args...)
}

// ownersPrefix starts the names of the owners' containers of a release's
// switch and of a local command (ownMnt): it, the compose project,
// devopsy's pid and -<n>.
const ownersPrefix = "devopsy-owners-"

// ownersContainers is docker ps' filter for the owners' containers of
// the project at cwd (ownersPrefix), never another compose project's:
// shop-prod-1's start shop-prod- too, then three numbers, not two.
func ownersContainers(cwd string) string {
	return "name=^/?" + ownersPrefix + composeName(cwd) + "-[0-9]+-[0-9]+$"
}

// makeEntry makes the entry e of mnt when missing, 0755 whatever the
// umask (as Docker makes a bind source), and refuses one that is a
// symbolic link.
func makeEntry(mnt, e string) error {
	p := filepath.Join(mnt, e)
	fi, err := os.Lstat(p)
	switch {
	case err == nil && fi.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("mnt/%s is a symbolic link: Docker would mount what it points to, and devopsy give that to its owner: make it a directory of its own, moving what it points to there", e)
	case err == nil:
		return nil
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(p, 0o755); err != nil {
		return err
	}
	return os.Chmod(p, 0o755)
}

// bindMount is docker run's --mount for a bind of source at target,
// its fields quoted as CSV, as Docker parses them, so an entry's name
// with a comma or a colon stays one path.
func bindMount(source, target string) (string, error) {
	var b strings.Builder
	w := csv.NewWriter(&b)
	if err := w.Write([]string{"type=bind", "source=" + source, "target=" + target}); err != nil {
		return "", err
	}
	w.Flush()
	return strings.TrimSuffix(b.String(), "\n"), w.Error()
}

// projectOwners are the owned entries of the project at cwd's mnt,
// entry to owner, from its labels (every profile, cli.MntOwners), and
// its mnt resolved. Only entries: a bind source below one is refused by
// the mounts check (cli.CheckMounts).
func projectOwners(cwd string) (owned map[string]string, mnt string, err error) {
	projectDir, services, err := cli.SyncServices(cwd, os.Environ())
	if err != nil {
		return nil, "", err
	}
	owned, err = cli.MntOwners(services)
	if err != nil || len(owned) == 0 {
		return owned, "", err
	}
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
	// After the switch, nothing interrupts: needed whole, whatever
	// happens to the session. Go would die of a SIGPIPE writing to a gone
	// one, and of a SIGTERM even ignored by the script (it only keeps an
	// inherited SIGINT and SIGHUP ignored), as runImages says.
	signal.Ignore(syscall.SIGPIPE, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	owners, mnt, err := projectOwners(cwd)
	if err != nil && restored {
		report.Warn("the entries of mnt keep their owners: the restored release's labels cannot be read", err.Error())
		return 0
	}
	// Under the release lock no other owners' step of this environment
	// runs: what one a devopsy killed outright left may still chown, and
	// goes before this one scans again.
	if err == nil {
		err = removeContainers(ownersContainers(cwd))
	}
	if err == nil {
		err = ownMnt(context.Background(), mnt, owners, fmt.Sprintf("%s%s-%d", ownersPrefix, composeName(cwd), os.Getpid()))
	}
	if err != nil {
		report.Fail(err)
		return 1
	}
	return 0
}

// checkStartBinds refuses to start anything of the release at cwd while a
// bind source of its services leads out of its entry through a symbolic
// link in the data as it is now (cli.SyncService.LeavingBinds): Docker
// resolves bind sources at every start, so a link a restore, a sync or an
// import brought would mount that of the host (review: every hole the
// checks per flow left was a start none preceded). One place every start
// goes through, a project's command and devopsy's own (composeRun); the
// checks before each flow stay, refusing before anything changes. Labels
// it cannot read are a warning and the start goes on, as for the owners'
// step: a label devopsy reads never stops compose.
func checkStartBinds(cwd string) error {
	_, services, err := cli.SyncServices(cwd, os.Environ())
	if errors.Is(err, cli.ErrComposeConfig) {
		return nil
	}
	if err != nil {
		report.Warn("the release's labels cannot be read: its bind sources are not checked before the start", err.Error())
		return nil
	}
	var bad []string
	for _, s := range services {
		for _, l := range s.LeavingBinds {
			bad = append(bad, fmt.Sprintf("%s mounts ./mnt/%s through a symbolic link out of its entry: Docker would mount that of the host", s.Name, l))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	// The message whole in its first line: callers wrap it (docker
	// compose start: ...), which keeps only that.
	return &report.Failure{Msg: "nothing started: " + strings.Join(bad, "; ") + ": remove the link in shared/mnt (a restored snapshot's, a sync's, or one made by hand), or bind-mount where it leads explicitly"}
}

// mntOwnersRestored is --mnt-owners' word for a restored release
// (runMntOwners).
const mntOwnersRestored = "restored"

// runCheckMounts implements the hidden --check-mounts [restored], run
// by an activation in the new release before its switch, after its
// prepare steps and --missing-vars (cli.CheckMounts). The labels are
// read as --mnt-owners reads them after the switch (cli.SyncServices,
// the leftover sync capability included), so a release this passes
// never fails there for its labels. A compose configuration that cannot
// be read is no refusal, as for --check-names: the release's up says
// what is wrong with it. With an entry owned, it also refuses an owned
// entry that is a symbolic link (ownedLinks), and readies the util image
// the owners' step runs after the switch, so a registry down refuses the
// release instead of failing it there.
//
// restored is a rollback's, in the release it restores: what
// --mnt-owners restored needs after the switch and only that, so an
// image pruned since fails the rollback before it changes anything. Its
// labels refuse nothing, as rollbacks never check roles or imports:
// labels this devopsy cannot read are a warning, and the owners' step
// gives nothing away then.
func runCheckMounts(cwd string, args []string) int {
	restored := len(args) > 0 && args[0] == mntOwnersRestored
	if len(args) > 0 && !restored || len(args) > 2 {
		report.Error(cli.CheckMountsFlag + " takes nothing, or " + mntOwnersRestored + " [<snapshot> | restart]")
		return 2
	}
	// A restore's: the entries its snapshot carries are replaced, so the
	// links in them now are no matter (the snapshot's own are checked in
	// its tars); the rest stay as they are.
	// The restart after a failed run step names itself (restart), a
	// restore its snapshot, a rollback nothing.
	var m *manifest
	what := "the rollback: it does not go live"
	switch {
	case len(args) == 2 && args[1] == "restart":
		what = "the restart of the release before: it stays down until a release whose mounts it accepts"
	case len(args) == 2:
		var err error
		if m, err = readManifest(args[1]); err != nil {
			report.Fail(err)
			return 1
		}
		what = "the restore: nothing restored"
	}
	projectDir, services, err := cli.SyncServices(cwd, os.Environ())
	var carried []string
	if m != nil {
		carried = restoredEntries(m, services)
	}
	if restored && err == nil {
		// Two owners on one entry: --mnt-owners restored reads them as
		// labels it cannot read.
		_, err = cli.MntOwners(services)
	}
	switch {
	case restored && err != nil:
		report.Warn("the restored release's labels cannot be read: the entries of mnt will keep their owners", err.Error())
		return 0
	case errors.Is(err, cli.ErrComposeConfig):
		return 0
	case err != nil:
		err = &report.Failure{Msg: "the mounts check refuses the release, whose labels cannot be read: it does not go live", Details: []string{err.Error()}}
	case !restored:
		err = cli.CheckMounts(services)
	default:
		err = cli.CheckLeavingBinds(services, carried, what)
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

// checkOwned refuses owned entries of mnt that are symbolic links, and
// readies the util image, when the project owns any entry.
func checkOwned(projectDir string, services []cli.SyncService) error {
	owned, err := cli.MntOwners(services)
	if err != nil || len(owned) == 0 {
		return err
	}
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
			Msg:     "an owned entry of mnt is a symbolic link: Docker would mount what it points to, and devopsy give that to the entry's owner: the release does not go live",
			Details: details,
			Fixes:   []report.Remedy{report.Fix("make each a directory of its own in shared/mnt, moving what it points to there")},
		}
	}
	return ensureUtilImage(context.Background())
}

// ownedLinks are the owned entries of mnt that are symbolic links,
// sorted.
func ownedLinks(mnt string, owned map[string]string) []string {
	var links []string
	for e := range owned {
		if fi, err := os.Lstat(filepath.Join(mnt, e)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			links = append(links, e)
		}
	}
	slices.Sort(links)
	return links
}

// localMnt readies the owned entries of mnt before compose starts
// anything locally (cli.Plan.Starts): each missing one is made, and on
// Linux one missing or whose top directory is not its owner's is given
// away in a util container, only then, and scanned whole there: an
// entry whose top is right is never scanned, since that would run
// before every such command. On macOS the top is never checked: Docker
// Desktop and OrbStack let any container user write the Mac's files,
// and show them the owner Docker asks for, whatever the Mac says.
// Labels that cannot be read (names from before a change, a compose
// error) are a warning and the command goes on: it is compose's, which
// says what is wrong with its files, and a label devopsy reads must
// never stop a plain up (on a server, a rollback's run step:
// cli.Plan.Starts is never set in a release there). For the same reason
// an owned entry that is a symbolic link (an entry shared between
// worktrees, a database on another disk) is a warning, left as it is:
// Docker mounts what it points to, which is the developer's choice
// locally (on a server the mounts check refuses one).
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
	for _, e := range slices.Sorted(maps.Keys(owners)) {
		o := owners[e]
		fi, err := os.Lstat(filepath.Join(mnt, e))
		switch {
		case err == nil && fi.Mode()&os.ModeSymlink != 0:
			report.Warn("mnt/"+e+" is a symbolic link: devopsy leaves it and what it points to as they are, not giving it to "+o, "Docker mounts what it points to")
		case runtime.GOOS == "darwin":
			if err := makeEntry(mnt, e); err != nil {
				return err
			}
		case err != nil:
			wrong[e] = o
		default:
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && fmt.Sprintf("%d:%d", st.Uid, st.Gid) != o {
				wrong[e] = o
			}
		}
	}
	return ownMnt(context.Background(), mnt, wrong, fmt.Sprintf("%s%s-%d", ownersPrefix, composeName(cwd), os.Getpid()))
}
