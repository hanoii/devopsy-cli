package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
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
// util container of its own, c named c.name-<n>, that mounts that entry
// alone, without a network (chownEntry). A missing entry is made first,
// 0755 as Docker would, so the container mounts it; one that is a
// symbolic link is refused, since Docker would mount what it points to
// (the mounts check refuses it before a release's switch already).
func ownMnt(ctx context.Context, mnt string, owned map[string]string, c container) error {
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
		if err := dockerRunFresh(ctx, c.with(fmt.Sprintf("-%d", i)), "--network", "none", "--mount", mount, remote.UtilImage, "sh", "-c", ownEntry, "sh", e, owned[e]); err != nil {
			return fmt.Errorf("giving mnt/%s to %s (%s): %w", e, owned[e], cli.MntOwnerLabel, err)
		}
	}
	return nil
}

// dockerRunFresh is dockerRunIO, no stdin, stdout errOut, for a
// container named after devopsy's pid (ownMnt's, extractEntries'): a
// container of that name can only be a leftover of a devopsy killed
// outright whose pid this one reuses, still running (--rm removes it
// only once it ends), and docker run would refuse the name. It goes
// first.
func dockerRunFresh(ctx context.Context, c container, args ...string) error {
	if err := removeNamed(c.name); err != nil {
		return err
	}
	return dockerRunIO(ctx, c, nil, nil, args...)
}

// ownersContainer is the owners' container of a release's switch and of
// a local command in the project at cwd (ownMnt), remote.TaskOwners: what
// --mnt-owners removes, a killed devopsy's (never locally, where another
// terminal's step may run).
func ownersContainer(cwd string) container {
	return newContainer("owners", composeName(cwd), strconv.Itoa(os.Getpid()), remote.TaskOwners)
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
		// A project user's mnt is the deploy user's: the door makes its entries.
		if errors.Is(err, fs.ErrPermission) && !reachesDocker() {
			return doorEntry(mnt, e, false)
		}
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
// entry to owner, from its labels (every profile, cli.MntOwners), its mnt
// resolved, and its owned named volumes (cli.OwnedVolumes) with the
// services they need to be made. Only entries: a bind source below one
// is refused by the mounts check (cli.CheckMounts).
func projectOwners(cwd string) (o projectOwned, err error) {
	projectDir, services, err := cli.SyncServices(cwd, os.Environ())
	if err != nil {
		return o, err
	}
	o.services = services
	if o.entries, err = cli.MntOwners(services); err != nil {
		return o, err
	}
	if o.volumes, err = cli.OwnedVolumes(services); err != nil {
		return o, err
	}
	if len(o.entries) > 0 {
		o.mnt, err = dataMnt(projectDir)
	}
	return o, err
}

// projectOwned is what projectOwners reads.
type projectOwned struct {
	entries  map[string]string
	mnt      string
	volumes  map[string]cli.OwnedVolume
	services []cli.SyncService
}

// ownVolumeRoot gives the volume mounted at /vol/$1 its owner $2, its root
// alone: what a service writes in it is its own.
const ownVolumeRoot = `chown "$2" "/vol/$1"`

// ownVolumes gives each owned named volume's root, only its root, to its
// owner (cli.OwnedVolumes), each made first when missing (ensureVolumes,
// project the environment's compose name, at cwd), in a util container of
// c's named c.name-vol-<n> without a network, mounting the volume alone
// at a path its image lacks (/vol/<key>), so the mount fills nothing.
// Every owners' step does it again: cheap, and it heals a volume Docker
// made first. A volume whose driver refuses the chown (a share) fails it.
func ownVolumes(ctx context.Context, cwd, project string, owned map[string]cli.OwnedVolume, services []cli.SyncService, c container) error {
	if err := readyVolumes(ctx, cwd, project, owned, services); err != nil {
		return err
	}
	return giveVolumes(ctx, owned, c)
}

// readyVolumes makes each owned volume Docker lacks (ensureVolumes) and
// readies the util image that gives their roots away.
func readyVolumes(ctx context.Context, cwd, project string, owned map[string]cli.OwnedVolume, services []cli.SyncService) error {
	if len(owned) == 0 {
		return nil
	}
	var vols []cli.Volume
	for _, k := range slices.Sorted(maps.Keys(owned)) {
		vols = append(vols, owned[k].Volume)
	}
	if err := ensureVolumes(ctx, cwd, project, vols, services); err != nil {
		return err
	}
	return ensureUtilImage(ctx)
}

// giveVolumes gives the roots of owned volumes readyVolumes made to their
// owners, as ownVolumes says.
func giveVolumes(ctx context.Context, owned map[string]cli.OwnedVolume, c container) error {
	if len(owned) == 0 {
		return nil
	}
	keys := slices.Sorted(maps.Keys(owned))
	var parts []string
	for _, k := range keys {
		parts = append(parts, "the volume "+k+"'s root to "+owned[k].Owner)
	}
	report.Info("giving " + strings.Join(parts, ", "))
	for i, k := range keys {
		if err := ownVolume(ctx, c.with(fmt.Sprintf("-vol-%d", i)), owned[k]); err != nil {
			return err
		}
	}
	return nil
}

// ownProjectVolumes gives the roots of the project at cwd's owned volumes (ownVolumes), after
// what writes into its data (an import, a restore, a sync's finish): a tar or a copy carries its
// maker's owners, the root's included.
func ownProjectVolumes(ctx context.Context, cwd string, c container) error {
	owned, err := projectOwners(cwd)
	if err != nil {
		return err
	}
	return ownVolumes(ctx, cwd, composeName(cwd), owned.volumes, owned.services, c)
}

// ownVolume is ownVolumes' container for the volume v, as c.
func ownVolume(ctx context.Context, c container, v cli.OwnedVolume) error {
	if err := dockerRunFresh(ctx, c, "--network", "none", "-v", v.Name+":/vol/"+v.Key, remote.UtilImage, "sh", "-c", ownVolumeRoot, "sh", v.Key, v.Owner); err != nil {
		return &report.Failure{Msg: fmt.Sprintf("giving the volume %s's root to %s (%s): %v", v.Key, v.Owner, cli.MntOwnerLabel, err),
			Remedies: []report.Remedy{report.Fix("give the service a volume whose driver takes a chown (a share's may refuse one), or mount it without " + cli.MntOwnerLabel + " and have its own user there")}}
	}
	return nil
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
	owned, err := projectOwners(cwd)
	if err != nil && restored {
		report.Warn("the entries of mnt and the named volumes keep their owners: the restored release's labels cannot be read", report.Messages(err)...)
		return 0
	}
	// Under the release lock no other owners' step of this environment
	// runs: what one a devopsy killed outright left may still chown, and
	// goes before this one scans again.
	if err == nil {
		err = removeLeft(composeName(cwd), remote.TaskOwners)
	}
	if err == nil {
		err = ownStarted(context.Background(), cwd, owned, owned.entries)
	}
	if err != nil {
		return failErr(err)
	}
	return 0
}

// ownStarted gives entries (of owned.entries, entry to owner) and every
// owned volume's root to their owners where the services may run: a
// release's owners' step after its switch, a local start's before
// compose. What changes owner stops first (stopForNewOwners).
func ownStarted(ctx context.Context, cwd string, owned projectOwned, entries map[string]string) error {
	c := ownersContainer(cwd)
	if err := readyVolumes(ctx, cwd, composeName(cwd), owned.volumes, owned.services); err != nil {
		return err
	}
	if err := stopForNewOwners(ctx, cwd, owned, entries, c); err != nil {
		return err
	}
	if err := ownMnt(ctx, owned.mnt, entries, c); err != nil {
		return err
	}
	return giveVolumes(ctx, owned.volumes, c)
}

// statOwner is a file's owner as the host has it, <uid>:<gid>.
func statOwner(fi fs.FileInfo) string {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", st.Uid, st.Gid)
	}
	return ""
}

// volumeRoots prints each volume's key and its root's owner, each volume
// mounted read-only at /vol/<key>, where giveVolumes mounts it.
const volumeRoots = `for k; do printf '%s %s\n' "$k" "$(stat -c %u:%g "/vol/$k")"; done`

// readVolumeRoots reads the owners of owned volumes' roots, key to
// <uid>:<gid>, in one util container, c's named c.name-roots, without a
// network.
func readVolumeRoots(ctx context.Context, owned map[string]cli.OwnedVolume, c container) (map[string]string, error) {
	keys := slices.Sorted(maps.Keys(owned))
	args := []string{"--network", "none"}
	for _, k := range keys {
		args = append(args, "-v", owned[k].Name+":/vol/"+k+":ro")
	}
	c = c.with("-roots")
	if err := removeNamed(c.name); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := dockerRunIO(ctx, c, nil, &out, append(append(args, remote.UtilImage, "sh", "-c", volumeRoots, "sh"), keys...)...); err != nil {
		return nil, fmt.Errorf("reading the owners of the named volumes' roots: %w", err)
	}
	roots := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if k, o, ok := strings.Cut(line, " "); ok {
			roots[k] = o
		}
	}
	return roots, nil
}

// stopForNewOwners stops the running services that mount what is about
// to change owner, with their dependents (cli.WithDependents), in one
// docker compose stop, saying what and why: entries (entry to owner)
// whose top another owner has, and owned volumes whose root another has
// (readVolumeRoots). A service
// left running as the old owner loses its own files under it: MariaDB's
// shutdown marks its tc.log and cannot delete it, and refuses the next
// start. The step that starts them comes after (the run step, the local
// command). An entry or a root that is its owner's already stops nothing.
func stopForNewOwners(ctx context.Context, cwd string, owned projectOwned, entries map[string]string, c container) error {
	var changing []string
	for _, e := range slices.Sorted(maps.Keys(entries)) {
		if fi, err := os.Lstat(filepath.Join(owned.mnt, e)); err == nil && fi.Mode()&os.ModeSymlink == 0 && statOwner(fi) != entries[e] {
			changing = append(changing, e)
		}
	}
	var volumes []string
	if len(owned.volumes) > 0 {
		roots, err := readVolumeRoots(ctx, owned.volumes, c)
		if err != nil {
			return err
		}
		for _, k := range slices.Sorted(maps.Keys(roots)) {
			if v, ok := owned.volumes[k]; ok && roots[k] != v.Owner {
				volumes = append(volumes, k)
			}
		}
	}
	if len(changing) == 0 && len(volumes) == 0 {
		return nil
	}
	var direct []string
	for _, s := range owned.services {
		mounts := slices.ContainsFunc(s.Binds, func(b string) bool {
			return slices.ContainsFunc(changing, func(e string) bool { return b == e || strings.HasPrefix(b, e+"/") })
		}) || slices.ContainsFunc(s.VolumeMounts, func(m cli.VolumeMount) bool { return slices.Contains(volumes, m.Key) })
		if mounts {
			direct = append(direct, s.Name)
		}
	}
	set := cli.WithDependents(owned.services, direct, "its data changes owner")
	up, err := upServices(cwd)
	if err != nil {
		return err
	}
	stop := intersect(cli.StoppedNames(set), up)
	if len(stop) == 0 {
		return nil
	}
	report.Info(cli.StoppingLine(set, stop))
	if err := compose(cwd, errOut, append([]string{"stop"}, stop...)...); err != nil {
		return fmt.Errorf("docker compose stop: %w", err)
	}
	return nil
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
		report.Warn("the release's labels cannot be read: its bind sources are not checked before the start", report.Messages(err)...)
		return nil
	}
	bad, fixes := cli.LeavingLines(services, nil)
	if len(bad) == 0 {
		return nil
	}
	// The message whole in its first line: callers wrap it (docker
	// compose start: ...), which keeps only that.
	return &report.Failure{Msg: "nothing started: " + strings.Join(bad, "; ") + ": " + strings.Join(fixes, "; or ")}
}

// fail says msg as an error, for a command to return its code.
func fail(msg string) int {
	report.Error(msg)
	return 1
}

// failErr says err whole (report.Fail: its details, fixes and the
// failures it carries, each data service without its version among them)
// and is a command's exit 1: never err.Error() alone, which keeps only
// the first message.
func failErr(err error, more ...report.Remedy) int {
	report.Fail(err, more...)
	return 1
}

// mntOwnersRestored is --mnt-owners' word for a restored release
// (runMntOwners).
const mntOwnersRestored = "restored"

// runCheckMounts implements the hidden --check-mounts [restored
// [<snapshot> | restart]], run
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
			return failErr(err)
		}
		what = "the restore: nothing restored"
	}
	projectDir, services, err := cli.SyncServices(cwd, os.Environ())
	var carried []string
	if m != nil {
		carried = restoredEntries(m, services)
	}
	if restored && err == nil {
		// Two owners on one entry, or a volume's refused: --mnt-owners
		// restored reads them as labels it cannot read.
		if _, err = cli.MntOwners(services); err == nil {
			_, err = cli.OwnedVolumes(services)
		}
	}
	switch {
	case restored && err != nil:
		report.Warn("the restored release's labels cannot be read: the entries of mnt and the named volumes will keep their owners", report.Messages(err)...)
		return 0
	case errors.Is(err, cli.ErrComposeConfig):
		return 0
	case err != nil:
		// A Failure (each data service without its version) is said whole,
		// its details and the services after the first included (review:
		// flattened into one detail, only the first's message reached the
		// release).
		var f *report.Failure
		if !errors.As(err, &f) {
			err = &report.Failure{Msg: "the mounts check refuses the release, whose labels cannot be read: it does not go live", Details: []string{err.Error()}}
		}
	case !restored:
		err = cli.CheckMounts(services)
	default:
		err = cli.CheckLeavingBinds(services, carried, what)
	}
	if err == nil {
		err = checkOwned(projectDir, services)
	}
	if err != nil {
		return failErr(err)
	}
	return 0
}

// checkOwned refuses owned entries of mnt that are symbolic links, and
// readies the util image, when the project owns any entry or volume.
func checkOwned(projectDir string, services []cli.SyncService) error {
	owned, err := cli.MntOwners(services)
	if err != nil {
		return err
	}
	vols, err := cli.OwnedVolumes(services)
	if err != nil {
		return err
	}
	if len(owned) == 0 {
		if len(vols) == 0 {
			return nil
		}
		return ensureUtilImage(context.Background())
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
			Msg:      "an owned entry of mnt is a symbolic link: Docker would mount what it points to, and devopsy give that to the entry's owner: the release does not go live",
			Details:  details,
			Remedies: []report.Remedy{report.Fix("make each a directory of its own in shared/mnt, moving what it points to there")},
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

// localMnt readies the owned entries of mnt and named volumes before
// compose starts anything locally (cli.Plan.Starts): each missing entry is
// made, and on Linux one missing or whose top directory is not its
// owner's is given away in a util container, only then, and scanned whole
// there: an entry whose top is right is never scanned, since that would
// run before every such command. On macOS the top is never checked:
// Docker Desktop and OrbStack let any container user write the Mac's
// files, and show them the owner Docker asks for, whatever the Mac says.
// An owned volume is made and its root given on macOS as on Linux
// (ownVolumes): a volume is Docker's own, whose owners nothing fakes.
// Labels that cannot be read (names from before a change, a compose
// error) are a warning and the command goes on: it is compose's, which
// says what is wrong with its files, and a label devopsy reads must
// never stop a plain up (on a server, a rollback's run step:
// cli.Plan.Starts is never set in a release there). An owned volume
// refused (cli.OwnedVolumes: a mount without nocopy) stops it: the start
// would fill the volume from the image and leave its owner a root it
// cannot write. For the same reason as labels, an owned entry that is a
// symbolic link (an entry shared between worktrees, a database on
// another disk) is a warning, left as it is: Docker mounts what it points
// to, which is the developer's choice locally (on a server the mounts
// check refuses one).
func localMnt(cwd string) error {
	projectDir, services, err := cli.SyncServices(cwd, os.Environ())
	if err != nil {
		report.Warn("the entries of mnt and the named volumes are left as they are: the labels cannot be read", report.Messages(err)...)
		return nil
	}
	owned := projectOwned{services: services}
	if owned.volumes, err = cli.OwnedVolumes(services); err != nil {
		return err
	}
	owned.entries, err = cli.MntOwners(services)
	if err == nil && len(owned.entries) > 0 {
		owned.mnt, err = dataMnt(projectDir)
	}
	if err != nil {
		report.Warn("the entries of mnt are left as they are: the labels cannot be read", report.Messages(err)...)
		owned.entries = nil
	}
	wrong := map[string]string{}
	for _, e := range slices.Sorted(maps.Keys(owned.entries)) {
		o := owned.entries[e]
		fi, err := os.Lstat(filepath.Join(owned.mnt, e))
		switch {
		case err == nil && fi.Mode()&os.ModeSymlink != 0:
			report.Warn("mnt/"+e+" is a symbolic link: devopsy leaves it and what it points to as they are, not giving it to "+o, "Docker mounts what it points to")
		case runtime.GOOS == "darwin":
			if err := makeEntry(owned.mnt, e); err != nil {
				return err
			}
		case err != nil:
			wrong[e] = o
		default:
			if statOwner(fi) != o {
				wrong[e] = o
			}
		}
	}
	return ownStarted(context.Background(), cwd, owned, wrong)
}
