package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"golang.org/x/term"
)

// A snapshot's steps on its server (remote.SnapshotScript holds the
// locks; the comments there say which each runs under). What a snapshot
// is: remote's package comment of SnapshotScript.

// snapshotPaths are where a snapshot's steps work: the environment's
// directory, where its snapshots go, and among them the mirror, when
// its takes keep one (takeMirror).
type snapshotPaths struct{ base, snaps, mirror string }

func snapshotPathsOf(cwd string) snapshotPaths {
	base := cmp.Or(os.Getenv(images.BaseEnv), filepath.Dir(cwd))
	snaps := os.Getenv(images.SnapshotsEnv)
	if snaps == "" {
		// Outside a script's prelude (--sync-exec): from the server's own
		// settings, as basePrelude finds it.
		snaps = filepath.Join(base, "snapshots")
		if r, err := remote.ReleaseSettings(); err == nil && r.Snapshots.Root != "" {
			if rel, err := filepath.Rel(r.Root, base); err == nil && !strings.HasPrefix(rel, "..") {
				snaps = filepath.Join(r.Snapshots.Root, rel)
			}
		}
	}
	return snapshotPaths{base: base, snaps: snaps, mirror: filepath.Join(snaps, remote.SnapshotMirror)}
}

// takeMirror says whether a take keeps a mirror: its environment asks
// (remote.SnapshotMirrorEnv, snapshots: {mirror: true}) and the server
// does not turn mirrors off (its snapshots: {mirror: false}); refused
// says the server refused one asked for.
func takeMirror() (mirror, refused bool) {
	if os.Getenv(remote.SnapshotMirrorEnv) != "1" {
		return false, false
	}
	if r, err := remote.ReleaseSettings(); err == nil && r.Snapshots.MirrorsOff() {
		return false, true
	}
	return true, false
}

// fileText is path's content, "" when it cannot be read.
func fileText(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

// snapshotCheckCopy is snapshotCheck for the way the take copies d,
// saying it: a clone by reflinks, else the mirror or none, warned when
// the server refused a mirror asked for.
func snapshotCheckCopy(ctx context.Context, cwd string, p snapshotPaths, d *snapshotData) error {
	how := takeCopy(ctx, cwd, p, d)
	switch _, refused := takeMirror(); {
	case how == "reflink":
		report.Info("the data clones by reflinks: the services stop for the clone, seconds")
	case refused:
		report.Warn("this server keeps no snapshot mirrors (its snapshots: {mirror: false}): the services stay stopped while the tars are written")
	}
	return snapshotCheck(ctx, p, d, how == "mirror")
}

// snapshotClone, in the environment's own snapshots/ directory (the
// release root's filesystem, whatever the snapshot root), holds a take's
// reflink clone of the data while its tars are made: <id>/mnt, <id>/volumes
// and <id>/.devopsy-complete once the stopped pass completed it.
const snapshotClone = ".clone"

// clone is the clones' directory.
func (p snapshotPaths) clone() string { return filepath.Join(p.base, "snapshots", snapshotClone) }

// takeCopy is how a take copies d: reflink when every entry and volume
// clones into the clone's place by reflinks (reflinks), else mirror when
// it keeps one (takeMirror), else none. Reflinks need no mirror: a take
// that clones removes one left over.
func takeCopy(ctx context.Context, cwd string, p snapshotPaths, d *snapshotData) string {
	if reflinks(ctx, cwd, p, d) {
		return "reflink"
	}
	if mirror, _ := takeMirror(); mirror {
		return "mirror"
	}
	return "none"
}

// reflinks says whether d's entries and volumes clone into the clone's
// place by reflinks (the same XFS or btrfs, say): one regular file of
// each tried with cp --reflink=always, as root in the util image, the
// copy removed at once (an entry or volume without a file has nothing to
// try). Read-only on the data; nothing stops.
func reflinks(ctx context.Context, cwd string, p snapshotPaths, d *snapshotData) bool {
	if len(d.entries)+len(d.volumes) == 0 {
		return false
	}
	if err := os.MkdirAll(p.clone(), 0o700); err != nil {
		return false
	}
	// Its image first: a server's first take has none yet.
	if err := ensureUtilImage(ctx); err != nil {
		return false
	}
	mounts, srcs := []string{"-v", d.mnt + ":/m/mnt:ro", "-v", p.clone() + ":/c"}, []string{}
	for _, e := range d.entries {
		srcs = append(srcs, "/m/mnt/"+e)
	}
	for _, v := range d.volumes {
		mounts = append(mounts, "-v", v.Name+":/m/volumes/"+v.Key+":ro")
		srcs = append(srcs, "/m/volumes/"+v.Key)
	}
	args := append(append([]string{"--network", "none"}, mounts...), remote.UtilImage, "sh", "-c", `t=/c/.reflink.$$
for src; do
  f=$(find "$src" -type f -print -quit 2>/dev/null) || f=
  [ -n "$f" ] || continue
  cp --reflink=always "$f" "$t" 2>/dev/null || { rm -f "$t"; exit 1; }
  rm -f "$t"
done`, "sh")
	return dockerRun(ctx, "devopsy-snapshot-"+composeName(cwd)+"-reflink", append(args, srcs...)...) == nil
}

// cloneData clones d into the clone's <id>, by reflinks, as root in the
// util image (owners and modes kept: cp -a), then marks it complete.
func cloneData(ctx context.Context, cwd string, p snapshotPaths, d *snapshotData, id string) error {
	dir := filepath.Join(p.clone(), id)
	mounts := []string{"-v", d.mnt + ":/m/mnt:ro", "-v", dir + ":/c"}
	script := `set -e
mkdir -p /c/mnt /c/volumes
`
	for _, e := range d.entries {
		script += `cp -a --reflink=always "/m/mnt/` + e + `" /c/mnt/
`
	}
	for _, v := range d.volumes {
		mounts = append(mounts, "-v", v.Name+":/m/volumes/"+v.Key+":ro")
		script += `mkdir -p "/c/volumes/` + v.Key + `"
cp -a --reflink=always "/m/volumes/` + v.Key + `/." "/c/volumes/` + v.Key + `/"
`
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := dockerRun(ctx, "devopsy-snapshot-"+composeName(cwd)+"-"+id+"-clone", append(append([]string{"--network", "none"}, mounts...), remote.UtilImage, "sh", "-c", script)...); err != nil {
		return fmt.Errorf("the reflink clone: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, remote.SnapshotComplete), []byte(id+"\n"), 0o600)
}

// removeClone removes the clone of id (every clone, id ""), root's, as
// root.
func removeClone(ctx context.Context, cwd string, p snapshotPaths, id string) error {
	target := "/s/" + snapshotClone
	if id != "" {
		target += "/" + id
	}
	if _, err := os.Lstat(filepath.Join(p.base, "snapshots", strings.TrimPrefix(target, "/s/"))); err != nil {
		return nil
	}
	return dockerRun(ctx, "devopsy-snapshot-"+composeName(cwd)+"-unclone", "--network", "none", "-v", filepath.Join(p.base, "snapshots")+":/s", remote.UtilImage, "rm", "-rf", target)
}

// removeMirror removes the environment's mirror, root's, as root: a take
// without one finds it left from when it had one.
func removeMirror(ctx context.Context, cwd string, p snapshotPaths) error {
	if _, err := os.Lstat(p.mirror); err != nil {
		return nil
	}
	report.Info("removing the snapshots' mirror: this environment keeps none (snapshots: {mirror: true} keeps one)")
	return dockerRun(ctx, "devopsy-snapshot-"+composeName(cwd)+"-unmirror", "--network", "none", "-v", p.snaps+":/s", remote.UtilImage, "rm", "-rf", "/s/"+remote.SnapshotMirror)
}

// snapshotData is what a snapshot takes: every service with data, its
// entries of mnt (with whether each is a file) and its named volumes,
// whatever its copy label, excluded ones left out; only those there.
type snapshotData struct {
	services []cli.SyncService
	data     []cli.SyncService
	entries  []string
	files    []string
	volumes  []cli.Volume
	mnt      string
}

func readSnapshotData(ctx context.Context, cwd string) (*snapshotData, error) {
	projectDir, services, err := cli.SyncServices(cwd, os.Environ())
	if err != nil {
		return nil, err
	}
	d := &snapshotData{services: services}
	if d.mnt, err = dataMnt(projectDir); err != nil {
		return nil, fmt.Errorf("the project's mnt: %w", err)
	}
	for _, s := range services {
		if !s.HasData {
			continue
		}
		d.data = append(d.data, s)
		for _, e := range s.Dirs {
			fi, err := os.Stat(filepath.Join(d.mnt, e))
			if err != nil || slices.Contains(d.entries, e) {
				continue
			}
			d.entries = append(d.entries, e)
			if !fi.IsDir() {
				d.files = append(d.files, e)
			}
		}
		for _, v := range s.Volumes {
			if slices.ContainsFunc(d.volumes, func(x cli.Volume) bool { return x.Key == v.Key }) {
				continue
			}
			ok, err := volumeExists(ctx, v.Name)
			if err != nil {
				return nil, err
			}
			if ok {
				d.volumes = append(d.volumes, v)
			}
		}
	}
	sort.Strings(d.entries)
	sort.Slice(d.volumes, func(i, j int) bool { return d.volumes[i].Key < d.volumes[j].Key })
	return d, nil
}

// runSnapshotStep implements the hidden --snapshot-step <word>: leftovers,
// check, mirror, stopped <id> <release>, tar <id>.
func runSnapshotStep(cwd string, args []string) int {
	fail := func(msg string) int {
		report.Error(msg)
		return 1
	}
	if len(args) == 0 {
		return fail("usage: devopsy " + remote.SnapshotStepFlag + " leftovers|check|mirror|stopped <id> <release>|tar <id>")
	}
	p := snapshotPathsOf(cwd)
	switch args[0] {
	case "leftovers":
		if err := snapshotLeftovers(cwd, p); err != nil {
			return fail(err.Error())
		}
		return 0
	case "check":
		ctx, stop := interruptible()
		defer stop()
		d, err := readSnapshotData(ctx, cwd)
		if err != nil {
			return fail(err.Error())
		}
		if err := snapshotCheckCopy(ctx, cwd, p, d); err != nil {
			report.Fail(err)
			return 1
		}
		return 0
	case "mirror":
		ctx, stop := interruptible()
		defer stop()
		d, err := readSnapshotData(ctx, cwd)
		if err != nil {
			return fail(err.Error())
		}
		if takeCopy(ctx, cwd, p, d) != "mirror" {
			// No first pass: the stopped one clones the data or writes the
			// tars.
			if err := removeMirror(ctx, cwd, p); err != nil {
				return fail("removing the snapshots' mirror: " + err.Error())
			}
			return 0
		}
		_ = os.Remove(filepath.Join(p.mirror, remote.SnapshotComplete))
		if err := copyMirror(ctx, p, d, "devopsy-snapshot-"+composeName(cwd)+"-mirror"); err != nil {
			return fail(err.Error())
		}
		return 0
	case "stopped":
		if len(args) != 3 {
			return fail("usage: devopsy " + remote.SnapshotStepFlag + " stopped <id> <release>")
		}
		return snapshotStopped(cwd, p, args[1], args[2], "")
	case "list":
		snapshotList(p)
		return 0
	case "remove":
		if len(args) < 2 {
			return fail("usage: devopsy " + remote.SnapshotStepFlag + " remove <id>...")
		}
		code := 0
		for _, id := range args[1:] {
			if err := removeSnapshot(cwd, p, id); err != nil {
				report.Fail(err)
				code = 1
			}
		}
		// And the release snapshots whose release is gone, as a take's
		// pruning removes them (counts need the environment's keeps,
		// which only a take is sent).
		pruneGone(cwd, p, 10*time.Minute)
		return code
	case "make-release":
		if len(args) != 3 {
			return fail("usage: devopsy " + remote.SnapshotStepFlag + " make-release <snapshot> <release>")
		}
		if err := snapshotMakeRelease(cwd, p, args[1], args[2]); err != nil {
			return fail(err.Error())
		}
		return 0
	case "imported":
		if len(args) != 2 {
			return fail("usage: devopsy " + remote.SnapshotStepFlag + " imported <directory>")
		}
		if err := snapshotImported(cwd, p, args[1]); err != nil {
			return fail(err.Error())
		}
		return 0
	case "check-fit":
		if len(args) != 2 {
			return fail("usage: devopsy " + remote.SnapshotStepFlag + " check-fit <snapshot>")
		}
		if err := snapshotCheckFit(cwd, p, args[1]); err != nil {
			report.Fail(err)
			return 1
		}
		return 0
	case "check-tars":
		if len(args) != 2 {
			return fail("usage: devopsy " + remote.SnapshotStepFlag + " check-tars <snapshot>")
		}
		if err := snapshotCheckTars(cwd, p, args[1]); err != nil {
			return fail(err.Error())
		}
		return 0
	case "restore-data":
		if len(args) != 3 {
			return fail("usage: devopsy " + remote.SnapshotStepFlag + " restore-data <snapshot> <release>")
		}
		return snapshotRestoreData(cwd, p, args[1], args[2])
	case "receive":
		if len(args) != 2 {
			return fail("usage: devopsy " + remote.SnapshotStepFlag + " receive <directory>")
		}
		if err := snapshotReceive(os.Stdin, args[1]); err != nil {
			return fail(err.Error())
		}
		return 0
	case "release-check":
		// Before a release's fetch: whether it takes the snapshot of what
		// it replaces (3: skipped, said), and whether it would fit. It
		// holds the snapshot lock: what a take or a detached tar killed
		// outright left is settled first, as a manual take's leftovers.
		ctx, stop := interruptible()
		defer stop()
		if err := snapshotLeftovers(cwd, p); err != nil {
			report.Warn("a leftover snapshot: " + err.Error())
		}
		if readMark(p.base) != nil {
			report.Info("no release snapshot: the environment is marked, and a snapshot of its data would restore as complete")
			return 3
		}
		d, err := readSnapshotData(ctx, cwd)
		if err != nil {
			return fail(err.Error())
		}
		if len(d.data) == 0 {
			report.Info("no release snapshot: no service has data (" + cli.DataLabel + "=true)")
			return 3
		}
		if err := snapshotCheckCopy(ctx, cwd, p, d); err != nil {
			report.Fail(err)
			return 1
		}
		return 0
	case "release-stopped":
		if len(args) != 4 {
			return fail("usage: devopsy " + remote.SnapshotStepFlag + " release-stopped <id> <release> <next release>")
		}
		return snapshotStopped(cwd, p, args[1], args[2], args[3])
	case "release-tar":
		// Detached, after a release's stopped pass: the tars from the
		// mirror (a take without one wrote them already), then, once the
		// activation (pid) ended, the pruning, which needs the image lock
		// it held.
		if len(args) != 3 {
			return fail("usage: devopsy " + remote.SnapshotStepFlag + " release-tar <id> <pid>")
		}
		signal.Ignore(syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)
		if err := snapshotTar(cwd, p, args[1]); err != nil {
			return fail(err.Error())
		}
		if pid, err := strconv.Atoi(args[2]); err == nil {
			for start := time.Now(); syscall.Kill(pid, 0) == nil && time.Since(start) < time.Hour; time.Sleep(time.Second) {
			}
		}
		// The release current is now (the activation's or the one it went
		// back to): what its run step left of the record is no take's.
		pruneSnapshots(filepath.Join(p.base, "current"), p, 10*time.Minute)
		return 0
	case "release-record":
		// After a release's switch (live): its snapshot's lines become
		// the release's own stop lines, which a settle never starts; once
		// its run step ended (done), they go: the run step, or the restart
		// of the release before, started them.
		if len(args) != 3 || (args[1] != "live" && args[1] != "done") {
			return fail("usage: devopsy " + remote.SnapshotStepFlag + " release-record live|done <next release>")
		}
		if err := releaseRecord(cwd, args[1], args[2]); err != nil {
			return fail(err.Error())
		}
		return 0
	case "restored":
		if len(args) != 2 {
			return fail("usage: devopsy " + remote.SnapshotStepFlag + " restored <snapshot>")
		}
		if err := snapshotRestored(cwd, p, args[1]); err != nil {
			return fail(err.Error())
		}
		return 0
	case "tar":
		if len(args) != 2 {
			return fail("usage: devopsy " + remote.SnapshotStepFlag + " tar <id>")
		}
		// Past the stopped pass nothing interrupts it: a signal, a session
		// gone (its writes go to the server's log).
		signal.Ignore(syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)
		if err := snapshotTar(cwd, p, args[1]); err != nil {
			return fail(err.Error())
		}
		pruneSnapshots(cwd, p, 0)
		return 0
	}
	return fail("unknown " + remote.SnapshotStepFlag + " word: " + args[0])
}

// snapshotCheck refuses, before anything stops: nothing with data, a
// service with data writing a bind from outside the project (no copy or
// snapshot carries it), and too little space for the mirror and the
// tars, the data measured as root in the util image (volumes are root's).
func snapshotCheck(ctx context.Context, p snapshotPaths, d *snapshotData, mirror bool) error {
	if len(d.data) == 0 {
		return errors.New("nothing to snapshot: no service has data (" + cli.DataLabel + "=true)")
	}
	var bad []string
	for _, s := range d.data {
		for _, b := range s.OutsideBinds {
			bad = append(bad, fmt.Sprintf("%s has data and mounts %s writable, from outside the project: a snapshot cannot hold it", s.Name, b))
		}
	}
	if len(bad) > 0 {
		return &report.Failure{Msg: "no snapshot taken", Details: bad, Fixes: []report.Remedy{report.Fix("mount its data from ./mnt/<name> or a named volume, or that path read-only (:ro)")}}
	}
	if err := ensureUtilImage(ctx); err != nil {
		return err
	}
	data, err := duData(ctx, d)
	if err != nil {
		return err
	}
	// The tars at most the data again (compressed, usually far less), and
	// a mirror, kept among the snapshots, what it lacks; a tenth more
	// each, a margin.
	need := data * 11 / 10
	if mirror {
		mirrored, err := duDirs(ctx, p.mirror)
		if err != nil {
			return err
		}
		need += max(data-mirrored, 0) * 11 / 10
	}
	if free := freeBytes(p.snaps); free < need {
		return fmt.Errorf("not enough space for a snapshot at %s: %s needed (the data is %s), %s free: nothing stopped", p.snaps, size(need), size(data), size(free))
	}
	return nil
}

// duData is the size of d's entries and volumes, in bytes, from du -sk
// as root in the util image.
func duData(ctx context.Context, d *snapshotData) (int64, error) {
	if len(d.entries)+len(d.volumes) == 0 {
		return 0, nil
	}
	run := []string{"-v", d.mnt + ":/m:ro"}
	paths := []string{}
	for _, e := range d.entries {
		paths = append(paths, "/m/"+e)
	}
	for _, v := range d.volumes {
		run = append(run, "-v", v.Name+":/v/"+v.Key+":ro")
		paths = append(paths, "/v/"+v.Key)
	}
	return du(ctx, run, paths)
}

// duDirs is the size of dirs that exist, as duData measures.
func duDirs(ctx context.Context, dirs ...string) (int64, error) {
	var run, paths []string
	for i, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		run = append(run, "-v", dir+":/d"+strconv.Itoa(i)+":ro")
		paths = append(paths, "/d"+strconv.Itoa(i))
	}
	if len(paths) == 0 {
		return 0, nil
	}
	return du(ctx, run, paths)
}

func du(ctx context.Context, mounts, paths []string) (int64, error) {
	args := append(append([]string{"run", "--rm", "--network", "none"}, mounts...), append([]string{remote.UtilImage, "du", "-sk"}, paths...)...)
	trace(append([]string{"docker"}, args...))
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return 0, fmt.Errorf("measuring the data (du): %w", err)
	}
	var total int64
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		k, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("measuring the data (du): %q", line)
		}
		total += k * 1024
	}
	return total, nil
}

func freeBytes(dir string) int64 {
	var st syscall.Statfs_t
	for d := dir; ; d = filepath.Dir(d) {
		if err := syscall.Statfs(d, &st); err == nil {
			return int64(st.Bavail) * int64(st.Bsize)
		}
		if d == filepath.Dir(d) {
			return 0
		}
	}
}

// size is n bytes for people.
func size(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// copyMirror rsyncs d into the mirror (mnt/<entry>, volumes/<key>), as
// a sync does (rsyncEntry, rsyncVolume: --delete, so the mirror ends
// equal), and removes what the mirror has that d no longer does. Run
// twice by a take: while everything runs, then stopped.
func copyMirror(ctx context.Context, p snapshotPaths, d *snapshotData, name string) error {
	mnt, vols := filepath.Join(p.mirror, "mnt"), filepath.Join(p.mirror, "volumes")
	if err := os.MkdirAll(mnt, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(vols, 0o700); err != nil {
		return err
	}
	var stale []string
	for _, dir := range []string{mnt, vols} {
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			keep := slices.Contains(d.entries, e.Name())
			if dir == vols {
				keep = slices.ContainsFunc(d.volumes, func(v cli.Volume) bool { return v.Key == e.Name() })
			}
			if !keep {
				stale = append(stale, "/m/"+filepath.Base(dir)+"/"+e.Name())
			}
		}
	}
	if len(stale) > 0 {
		if err := dockerRun(ctx, name+"-stale", append([]string{"--network", "none", "-v", p.mirror + ":/m", remote.UtilImage, "rm", "-rf"}, stale...)...); err != nil {
			return fmt.Errorf("the mirror's old entries: %w", err)
		}
	}
	for i, e := range d.entries {
		if err := rsyncEntry(ctx, fmt.Sprintf("%s-%d", name, i), d.mnt, mnt, e, slices.Contains(d.files, e)); err != nil {
			return err
		}
	}
	for i, v := range d.volumes {
		dst := filepath.Join(vols, v.Key)
		if err := os.MkdirAll(dst, 0o700); err != nil {
			return err
		}
		if err := rsyncVolume(ctx, fmt.Sprintf("%s-v%d", name, i), v.Name+":/src/"+v.Key+":ro", dst+":/dst/"+v.Key, v.Key); err != nil {
			return err
		}
	}
	return nil
}

// snapshotStopped is a take's stopped pass, under the release, sync and
// snapshot locks: what a killed take left settled first, every service
// with data and what depends on it stopped (recorded as snapshot <id>
// lines), the mirror brought in line, the services started again and
// waited for until healthy; then, nothing stopped any more, the
// snapshot's directory (<id>.tmp) with the release's code and listing,
// the manifest, the environment's variables and image history, and the
// release's images bookmarked for it. A signal or a session gone before
// the services are back cancels it, the services started again.
func snapshotStopped(cwd string, p snapshotPaths, id, rel, next string) int {
	fail := func(msg string) int {
		report.Error(msg)
		return 1
	}
	ctx, stopSig := interruptible()
	defer stopSig()
	if err := settleSnapshots(cwd, p.base); err != nil {
		return fail(err.Error())
	}
	d, err := readSnapshotData(ctx, cwd)
	if err != nil {
		return fail(err.Error())
	}
	var names []string
	for _, s := range d.data {
		names = append(names, s.Name)
	}
	set := cli.WithDependents(d.services, names, "snapshot")
	how := takeCopy(ctx, cwd, p, d)
	mirror := how == "mirror"
	tmp := filepath.Join(p.snaps, id+".tmp")
	stopped, restore, err := stopSetFor(cwd, set, true, id)
	switch {
	case err != nil:
	case how == "reflink":
		// A clone by reflinks: seconds whatever the size; the tars after.
		// One that fails though the check passed (another filesystem
		// mounted inside an entry, a file that will not reflink) falls back
		// to the tars from the data, stopped already, never failing every
		// take there.
		if err = cloneData(ctx, cwd, p, d, id); err != nil && ctx.Err() == nil {
			report.Warn(err.Error() + ": writing the tars instead, the services stopped")
			_ = removeClone(context.Background(), cwd, p, id)
			how = "none"
			err = dataTars(ctx, cwd, d, tmp, id)
			if err == nil {
				err = os.WriteFile(filepath.Join(tmp, snapshotTarredFile), nil, 0o600)
			}
		}
	case mirror:
		err = copyMirror(ctx, p, d, "devopsy-snapshot-"+composeName(cwd)+"-stopped")
		if err == nil {
			err = os.WriteFile(filepath.Join(p.mirror, remote.SnapshotComplete), []byte(id+"\n"), 0o600)
		}
	default:
		// No mirror: the tars, from the stopped data, are the downtime.
		report.Info("writing snapshot " + id + "'s tars, the services stopped")
		err = dataTars(ctx, cwd, d, tmp, id)
		if err == nil {
			// Its tars are complete: a take cut before its rename is
			// finished by the next (snapshotLeftovers), a mirror take's
			// .tmp, whose tars come after its manifest, never.
			err = os.WriteFile(filepath.Join(tmp, snapshotTarredFile), nil, 0o600)
		}
	}
	undo := func() {
		_ = os.Remove(filepath.Join(p.mirror, remote.SnapshotComplete))
		switch how {
		case "reflink":
			_ = removeClone(context.Background(), cwd, p, id)
		case "none":
			_ = os.RemoveAll(tmp)
		}
	}
	// A release's snapshot (next, the release that replaces rel) leaves
	// them stopped: the release's run step starts them under its code.
	if next == "" || err != nil {
		if rerr := restore(); rerr != nil {
			undo()
			if err != nil {
				report.Error(err.Error())
			}
			return fail(rerr.Error() + ": no snapshot taken")
		}
	}
	if err != nil {
		undo()
		if ctx.Err() != nil {
			return fail("interrupted: no snapshot taken, the services started again")
		}
		return fail(err.Error() + ": no snapshot taken")
	}
	// The mirror, or the tars, hold the snapshot: from here on nothing
	// interrupts.
	stopSig()
	signal.Ignore(syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)
	kind := "manual"
	if next != "" {
		kind = "release"
		// What it left stopped, for the release to clear once its run step
		// ended (release-record done).
		if err := os.WriteFile(filepath.Join(next, releaseStoppedFile), []byte(strings.Join(stopped, "\n")+"\n"), 0o644); err != nil {
			return fail(err.Error())
		}
	}
	if err := writeSnapshotDir(cwd, p, d, tmp, id, rel, kind, how); err != nil {
		return fail(err.Error())
	}
	if how == "none" {
		// Complete already: the tar step only prunes.
		if err := os.Rename(tmp, filepath.Join(p.snaps, id)); err != nil {
			return fail(err.Error())
		}
		report.Info("snapshot " + id + " taken")
	}
	return 0
}

// writeSnapshotDir makes the snapshot's directory tmp but its tars.
func writeSnapshotDir(cwd string, p snapshotPaths, d *snapshotData, tmp, id, rel, kind, how string) error {
	if err := os.MkdirAll(filepath.Join(tmp, remote.SnapshotData), 0o700); err != nil {
		return err
	}
	// The release, its logs left out.
	if out, err := exec.Command("cp", "-a", filepath.Join(rel, remote.CodeDir), filepath.Join(tmp, remote.CodeDir)).CombinedOutput(); err != nil {
		return fmt.Errorf("copying the release's code: %v %s", err, out)
	}
	files, _ := os.ReadDir(rel)
	for _, f := range files {
		if f.IsDir() || !f.Type().IsRegular() || f.Name() == remote.LogFile || f.Name() == remote.ServerLogFile {
			continue
		}
		if err := copyFile(filepath.Join(rel, f.Name()), filepath.Join(tmp, f.Name())); err != nil {
			return err
		}
	}
	for from, to := range map[string]string{
		filepath.Join(p.base, "shared", ".env"):        "shared.env",
		filepath.Join(p.base, "shared", "inherit.env"): "inherit.env",
		filepath.Join(p.base, images.HistoryFile):      "data-images",
	} {
		if fileExists(from) {
			if err := copyFile(from, filepath.Join(tmp, remote.SnapshotData, to)); err != nil {
				return err
			}
		}
	}
	var m strings.Builder
	fmt.Fprintf(&m, "format\t%s\nkind\t%s\nrelease\t%s\ntaken\t%s\ncopy\t%s\n", remote.SnapshotFormat, kind, filepath.Base(rel), time.Now().UTC().Format(time.RFC3339), how)
	for _, s := range d.data {
		var es []string
		for _, e := range s.Dirs {
			if slices.Contains(d.entries, e) {
				es = append(es, e)
			}
		}
		var vs []string
		for _, v := range s.Volumes {
			if slices.ContainsFunc(d.volumes, func(x cli.Volume) bool { return x.Key == v.Key }) {
				vs = append(vs, v.Key)
			}
		}
		fmt.Fprintf(&m, "service\t%s\t%s\t%s\t%s\n", s.Name, s.Copy, strings.Join(es, " "), strings.Join(vs, " "))
	}
	for _, e := range d.entries {
		kind := "dir"
		if slices.Contains(d.files, e) {
			kind = "file"
		}
		fmt.Fprintf(&m, "entry\t%s\t%s\n", e, kind)
	}
	for _, v := range d.volumes {
		fmt.Fprintf(&m, "volume\t%s\t%s\n", v.Key, v.Name)
	}
	set := &images.Set{Docker: images.CLI{}, Name: composeName(cwd), Out: errOut, List: images.ListCompose, Left: os.Getenv(remote.ImagesLeftEnv), Mode: os.Getenv(remote.ImagesModeEnv), Base: p.base, Snapshots: p.snaps}
	if err := os.WriteFile(filepath.Join(tmp, remote.SnapshotData, "images"), []byte(set.SnapshotData(p.base, rel)), 0o600); err != nil {
		return err
	}
	unlock, err := imagesShared(context.Background())
	if err != nil {
		return err
	}
	err = set.BookmarkFrom(tmp, rel)
	unlock()
	if err != nil {
		return fmt.Errorf("bookmarking the release's images: %w", err)
	}
	// The manifest last: a .tmp with one is complete but for its tars,
	// which a take cut off after its stopped pass leaves to the next
	// (snapshotLeftovers); without one it is removed, its bookmarks too.
	return os.WriteFile(filepath.Join(tmp, remote.SnapshotManifest), []byte(m.String()), 0o600)
}

func copyFile(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, data, 0o600)
}

// manifest is a snapshot's manifest, read.
type manifest struct {
	// copy is how the take copied the data: reflink, mirror or none.
	format, kind, release, taken, copy string
	entries, files               []string
	volumes                      map[string]string
	// services are its services with data, each with its entries and
	// volumes' keys.
	services []manifestService
}

type manifestService struct {
	name             string
	entries, volumes []string
}

func readManifest(dir string) (*manifest, error) {
	f, err := os.Open(filepath.Join(dir, remote.SnapshotManifest))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m := &manifest{volumes: map[string]string{}}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Split(sc.Text(), "\t")
		switch {
		case fs[0] == "format" && len(fs) > 1:
			m.format = fs[1]
		case fs[0] == "copy" && len(fs) > 1:
			m.copy = fs[1]
		case fs[0] == "kind" && len(fs) > 1:
			m.kind = fs[1]
		case fs[0] == "release" && len(fs) > 1:
			m.release = fs[1]
		case fs[0] == "taken" && len(fs) > 1:
			m.taken = fs[1]
		case fs[0] == "entry" && len(fs) > 2:
			m.entries = append(m.entries, fs[1])
			if fs[2] == "file" {
				m.files = append(m.files, fs[1])
			}
		case fs[0] == "volume" && len(fs) > 2:
			m.volumes[fs[1]] = fs[2]
		case fs[0] == "service" && len(fs) > 4:
			m.services = append(m.services, manifestService{name: fs[1], entries: strings.Fields(fs[3]), volumes: strings.Fields(fs[4])})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// An imported snapshot's manifest is anyone's: what it names becomes
	// paths and volume mounts as root.
	for _, n := range append(slices.Clone(m.entries), slices.Collect(maps.Keys(m.volumes))...) {
		if n == "" || n == "." || n == ".." || strings.ContainsAny(n, "/\\ \t\n\r") {
			return nil, fmt.Errorf("%s names %q: no entry of mnt or volume key", filepath.Join(dir, remote.SnapshotManifest), n)
		}
	}
	return m, nil
}

// snapshotTar makes a snapshot's tars from the mirror, one .tar.zst per
// entry and volume (tarInto), then renames <id>.tmp into place: only
// then is it a snapshot. A take without a mirror wrote its tars while
// stopped and renamed it already (dataTars).
func snapshotTar(cwd string, p snapshotPaths, id string) error {
	if fileExists(filepath.Join(p.snaps, id, remote.SnapshotManifest)) {
		return nil
	}
	tmp := filepath.Join(p.snaps, id+".tmp")
	m, err := readManifest(tmp)
	if err != nil {
		return fmt.Errorf("snapshot %s's manifest: %w", id, err)
	}
	ctx := context.Background()
	if err := ensureUtilImage(ctx); err != nil {
		return err
	}
	name := "devopsy-snapshot-" + composeName(cwd) + "-" + id + "-tar"
	// From its clone when it has one, else from the mirror.
	clone := filepath.Join(p.clone(), id)
	cloned := strings.TrimSpace(fileText(filepath.Join(clone, remote.SnapshotComplete))) == id
	mount := []string{"-v", p.mirror + ":/m:ro"}
	if cloned {
		mount = []string{"-v", clone + ":/m:ro"}
	}
	for _, e := range m.entries {
		report.Info("snapshot " + id + ": mnt/" + e)
		if err := tarInto(ctx, name, filepath.Join(tmp, remote.SnapshotData, "mnt."+e+".tar.zst"), mount, "mnt", e); err != nil {
			return err
		}
	}
	for _, k := range slices.Sorted(maps.Keys(m.volumes)) {
		report.Info("snapshot " + id + ": the volume " + k)
		if err := tarInto(ctx, name, filepath.Join(tmp, remote.SnapshotData, "volume."+k+".tar.zst"), mount, "volumes", k); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, filepath.Join(p.snaps, id)); err != nil {
		return err
	}
	if cloned {
		if err := removeClone(ctx, cwd, p, id); err != nil {
			report.Warn("removing snapshot " + id + "'s clone: " + err.Error())
		}
	}
	report.Info("snapshot " + id + " taken")
	return nil
}

// dataTars writes a take's tars into tmp from d itself, its services
// stopped: a take without a mirror. The data's entries and volumes are
// mounted read-only where the mirror has them (/m/mnt, /m/volumes/<key>),
// so the tars are the same.
func dataTars(ctx context.Context, cwd string, d *snapshotData, tmp, id string) error {
	if err := os.MkdirAll(filepath.Join(tmp, remote.SnapshotData), 0o700); err != nil {
		return err
	}
	if err := ensureUtilImage(ctx); err != nil {
		return err
	}
	name := "devopsy-snapshot-" + composeName(cwd) + "-" + id + "-tar"
	for _, e := range d.entries {
		report.Info("snapshot " + id + ": mnt/" + e)
		if err := tarInto(ctx, name, filepath.Join(tmp, remote.SnapshotData, "mnt."+e+".tar.zst"), []string{"-v", d.mnt + ":/m/mnt:ro"}, "mnt", e); err != nil {
			return err
		}
	}
	for _, v := range d.volumes {
		report.Info("snapshot " + id + ": the volume " + v.Key)
		if err := tarInto(ctx, name, filepath.Join(tmp, remote.SnapshotData, "volume."+v.Key+".tar.zst"), []string{"-v", v.Name + ":/m/volumes/" + v.Key + ":ro"}, "volumes", v.Key); err != nil {
			return err
		}
	}
	return nil
}

// tarInto writes file, the tar of /m/<dir>/<entry> compressed, from a
// util container (name) with mounts, under nice, as root (the mirror and
// the volumes are root's), into file's directory mounted, then gives it to
// the deploy user (0600). Never through the container's stdout: Docker's
// stream ran at about 12 MB/s on a small server where the same tar to a
// file took a tenth of the time (measured on DigitalOcean, 2026-10-26).
func tarInto(ctx context.Context, name, file string, mounts []string, dir, entry string) error {
	args := append(append([]string{"--network", "none"}, mounts...), "-v", filepath.Dir(file)+":/o", remote.UtilImage,
		// Only files, directories and links, as the mirror's rsync --no-D:
		// a FIFO or a device node is nothing a restore takes (tarTypes).
		"sh", "-c", `set -o pipefail; cd "/m/$1" && find "$2" \( -type f -o -type d -o -type l \) -print0 | tar -cf - --numeric-owner --null --no-recursion -T - | nice zstd -q -T0 -f -o "/o/$3" && chown "$4" "/o/$3" && chmod 600 "/o/$3"`,
		"sh", dir, entry, filepath.Base(file), fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()))
	if err := dockerRun(ctx, name, args...); err != nil {
		_ = os.Remove(file)
		return fmt.Errorf("the tar of %s/%s: %w", dir, entry, err)
	}
	return nil
}

// snapshotLeftovers is what a snapshot lock's holder does first: a take
// cut after its stopped pass (the mirror's marker names it) is finished
// from the mirror, any other .tmp removed with its bookmarks.
func snapshotLeftovers(cwd string, p snapshotPaths) error {
	// An import holds the snapshot lock from before its extraction to its
	// end, so one found while holding it was killed outright: nothing of
	// it is bookmarked before it is renamed into place.
	imports, _ := filepath.Glob(filepath.Join(p.snaps, ".import.*"))
	for _, d := range imports {
		report.Info("removing " + filepath.Base(d) + ", a snapshot import cut off before it was complete")
		if err := os.RemoveAll(d); err != nil {
			report.Warn(err.Error())
		}
	}
	tmps, _ := filepath.Glob(filepath.Join(p.snaps, "*.tmp"))
	marker, _ := os.ReadFile(filepath.Join(p.mirror, remote.SnapshotComplete))
	for _, tmp := range tmps {
		id := strings.TrimSuffix(filepath.Base(tmp), ".tmp")
		// Complete but for its tars: its manifest, written last, and the
		// images listing a restore needs.
		complete := fileExists(filepath.Join(tmp, remote.SnapshotManifest)) && fileExists(filepath.Join(tmp, images.ListFile))
		cloned := strings.TrimSpace(fileText(filepath.Join(p.clone(), id, remote.SnapshotComplete))) == id
		if complete && (strings.TrimSpace(string(marker)) == id || cloned) {
			report.Info("finishing snapshot " + id + ", cut off after its stopped pass")
			if err := snapshotTar(cwd, p, id); err != nil {
				return err
			}
			continue
		}
		// A take without a mirror marks its tars complete before its
		// manifest: one cut before its rename is complete.
		if complete && fileExists(filepath.Join(tmp, snapshotTarredFile)) {
			report.Info("finishing snapshot " + id + ", cut off before its rename")
			if err := os.Rename(tmp, filepath.Join(p.snaps, id)); err != nil {
				return err
			}
			continue
		}
		report.Info("removing snapshot " + id + ", cut off before it was complete")
		if err := removeSnapshotDir(cwd, p, tmp, 0); err != nil {
			report.Warn(err.Error())
		}
	}
	// A clone whose take is gone (renamed and its clone not removed, or
	// removed with its .tmp) is nobody's.
	clones, _ := os.ReadDir(p.clone())
	for _, c := range clones {
		if c.IsDir() && !fileExists(filepath.Join(p.snaps, c.Name()+".tmp")) {
			if err := removeClone(context.Background(), cwd, p, c.Name()); err != nil {
				report.Warn("removing a leftover clone: " + err.Error())
			}
		}
	}
	return nil
}

// removeSnapshotDir removes a snapshot's directory, its bookmarks first,
// under the exclusive image lock when it is free (else it stays, for the
// next holder of the snapshot lock).
func removeSnapshotDir(cwd string, p snapshotPaths, dir string, wait time.Duration) error {
	// The image lock, exclusive, waited for up to wait and kept to the end.
	var unlock func()
	for start := time.Now(); ; time.Sleep(250 * time.Millisecond) {
		u, ok, err := imagesExclusiveNow()
		if err != nil {
			return err
		}
		if ok {
			unlock = u
			break
		}
		if wait == 0 {
			return errors.New("the server's images are in use: " + filepath.Base(dir) + " stays until the next snapshot")
		}
		if time.Since(start) > wait {
			return errors.New("the server's images are still in use after " + wait.String() + ": " + filepath.Base(dir) + " stays")
		}
	}
	defer unlock()
	set := &images.Set{Docker: images.CLI{}, Name: composeName(cwd), Out: errOut, Left: os.Getenv(remote.ImagesLeftEnv), Base: p.base, Snapshots: p.snaps}
	if err := set.Remove(dir); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// removeSnapshot removes the snapshot id by hand (--snapshots remove),
// waiting for the server's images up to 10 minutes; one a restore's mark
// names stays.
func removeSnapshot(cwd string, p snapshotPaths, id string) error {
	dir := filepath.Join(p.snaps, id)
	if !remote.ReleaseIDFormat.MatchString(id) || !fileExists(filepath.Join(dir, remote.SnapshotManifest)) {
		return errors.New("no snapshot " + id + ": nothing removed")
	}
	if m := readMark(p.base); m != nil && m.Code == remote.IncompleteRestore && m.From == id {
		return &report.Failure{Msg: "the restore of snapshot " + id + " did not complete: it stays until a restore completes", Fixes: restoreRemedies(cwd, id)}
	}
	report.Info("removing snapshot " + id)
	return removeSnapshotDir(cwd, p, dir, 10*time.Minute)
}

// snapshotList prints p's snapshots for SnapshotsScript, newest first:
// id, kind, release, taken, size in bytes, state.
func snapshotList(p snapshotPaths) {
	ents, _ := os.ReadDir(p.snaps)
	// A take runs while its holding file is there and the lock is held, as
	// --status reads it (SnapshotRunning): any other holder removed the
	// file of a take killed outright.
	busy := false
	if f, err := os.OpenFile(filepath.Join(p.base, remote.SnapshotLockFile), os.O_RDONLY, 0); err == nil {
		busy = fileExists(filepath.Join(p.base, remote.SnapshotHoldingFile)) && syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) != nil
		f.Close()
	}
	var lines []string
	for _, e := range ents {
		id, tmp := strings.CutSuffix(e.Name(), ".tmp")
		if !e.IsDir() || !remote.ReleaseIDFormat.MatchString(id) {
			continue
		}
		dir := filepath.Join(p.snaps, e.Name())
		m, err := readManifest(dir)
		if err != nil {
			m = &manifest{}
		}
		var bytes int64
		_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if fi, err := d.Info(); err == nil {
					bytes += fi.Size()
				}
			}
			return nil
		})
		state := ""
		if tmp {
			state = "incomplete"
			if busy {
				state = "in progress"
			}
		}
		lines = append(lines, strings.Join([]string{id, m.kind, m.release, m.taken, strconv.FormatInt(bytes, 10), state, m.copy}, "\t"))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(lines)))
	for _, l := range lines {
		fmt.Println(l)
	}
	// Whether a mirror is kept (root's: its size is not the deploy user's
	// to read), on a line of its own the snapshots' parsers skip.
	if _, err := os.Lstat(p.mirror); err == nil {
		fmt.Println("mirror\tkept")
	}
}

// imagesExclusiveNow takes the server's image lock exclusively when it is
// free at once (ok false when busy).
func imagesExclusiveNow() (unlock func(), ok bool, err error) {
	path := os.Getenv(remote.ImagesLockEnv)
	if path == "" {
		return func() {}, true, nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return nil, false, fmt.Errorf("the server's image lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("the server's image lock: %w", err)
	}
	return func() { f.Close() }, true, nil
}

// manualSnapshots are the ids of p's manual snapshots, oldest first.
func manualSnapshots(p snapshotPaths) []string {
	ents, _ := os.ReadDir(p.snaps)
	var ids []string
	for _, e := range ents {
		if !e.IsDir() || !remote.ReleaseIDFormat.MatchString(e.Name()) {
			continue
		}
		if m, err := readManifest(filepath.Join(p.snaps, e.Name())); err == nil && m.kind == "manual" {
			ids = append(ids, e.Name())
		}
	}
	sort.Strings(ids)
	return ids
}

// snapshotTarredFile, in a take's <id>.tmp, says its tars, written
// while stopped, are complete (a take without a mirror).
const snapshotTarredFile = "devopsy-tarred"

// releaseStoppedFile, in a release that took a snapshot of the one it
// replaced, names what its stopped pass left stopped (release-record).
const releaseStoppedFile = "devopsy-snapshot-stopped"

// releaseRecord turns a release snapshot's lines in the record into the
// release's own stop lines once it is current (live), so a settle past
// the switch (a release that syncs runs one) never starts them; once the
// run step ended (done) those stop lines go.
func releaseRecord(cwd, how, next string) error {
	rec, ok := readSyncRecord(cwd)
	if !ok {
		return nil
	}
	switch how {
	case "live":
		for _, name := range rec.snaps {
			if !slices.Contains(rec.stops, name) {
				rec.stops = append(rec.stops, name)
			}
		}
		rec.snaps, rec.snapID = nil, ""
	case "done":
		data, _ := os.ReadFile(filepath.Join(next, releaseStoppedFile))
		names := strings.Fields(string(data))
		rec.stops = slices.DeleteFunc(rec.stops, func(s string) bool { return slices.Contains(names, s) })
	}
	if !rec.first && len(rec.stops) == 0 && len(rec.own) == 0 && len(rec.snaps) == 0 && len(rec.writers) == 0 {
		return removeIfThere(filepath.Join(filepath.Dir(cwd), syncState))
	}
	return rec.write(cwd)
}

// removeIfThere removes path, nothing when it is gone.
func removeIfThere(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// pruneSnapshots removes, under the snapshot lock, the oldest manual
// snapshots past their keep (remote.SnapshotKeepEnv, at most the
// server's snapshots: max_keep), every release snapshot whose release is
// gone and the oldest past theirs (remote.SnapshotReleaseKeepEnv,
// remote.KeepAll for every one whose release is kept), never the one a
// restore's mark names (running that restore again is the way out of
// it; it still counts). wait bounds a wait for the image lock.
func pruneSnapshots(cwd string, p snapshotPaths, wait time.Duration) {
	keep, _ := strconv.Atoi(os.Getenv(remote.SnapshotKeepEnv))
	if keep < 1 {
		keep = remote.DefaultManualSnapshots.Keep
	}
	if r, err := remote.ReleaseSettings(); err == nil && keep > r.Snapshots.MaxKeep {
		report.Warn(fmt.Sprintf("keeping %d snapshots, this server's snapshots: max_keep, not %d", r.Snapshots.MaxKeep, keep))
		keep = r.Snapshots.MaxKeep
	}
	marked := ""
	if m := readMark(p.base); m != nil && m.Code == remote.IncompleteRestore {
		marked = m.From
	}
	remove := func(id, why string) bool {
		report.Info("removing snapshot " + id + " (" + why + ")")
		if err := removeSnapshotDir(cwd, p, filepath.Join(p.snaps, id), wait); err != nil {
			report.Warn(err.Error())
			return false
		}
		return true
	}
	ids := manualSnapshots(p)
	for n := len(ids); n > keep; n-- {
		i := slices.IndexFunc(ids, func(id string) bool { return id != marked })
		if i < 0 {
			break
		}
		if !remove(ids[i], "keep "+strconv.Itoa(keep)) {
			return
		}
		ids = slices.Delete(ids, i, i+1)
	}
	rkeep, err := strconv.Atoi(os.Getenv(remote.SnapshotReleaseKeepEnv))
	if err != nil || rkeep == 0 {
		rkeep = remote.DefaultReleaseSnapshots.Keep
	}
	if !pruneGone(cwd, p, wait) {
		return
	}
	// One per release, its newest: a failed or retried release takes
	// another of the same one, and keep counts releases, never copies.
	var kept []string
	all := releaseSnapshots(p)
	for i, rs := range all {
		if rs.id != marked && slices.ContainsFunc(all[i+1:], func(o releaseSnapshot) bool { return o.release == rs.release }) {
			if !remove(rs.id, "a newer one of release "+rs.release+" is kept") {
				return
			}
			continue
		}
		kept = append(kept, rs.id)
	}
	for n := len(kept); rkeep != remote.KeepAll && n > rkeep; n-- {
		i := slices.IndexFunc(kept, func(id string) bool { return id != marked })
		if i < 0 {
			break
		}
		if !remove(kept[i], "keep "+strconv.Itoa(rkeep)+" release snapshots") {
			return
		}
		kept = slices.Delete(kept, i, i+1)
	}
}

// pruneGone removes, under the snapshot lock, every release snapshot
// whose release is gone, never the one a restore's mark names; false
// when a removal failed (said).
func pruneGone(cwd string, p snapshotPaths, wait time.Duration) bool {
	marked := ""
	if m := readMark(p.base); m != nil && m.Code == remote.IncompleteRestore {
		marked = m.From
	}
	for _, rs := range releaseSnapshots(p) {
		if rs.id == marked || fileExists(filepath.Join(p.base, "releases", rs.release)) {
			continue
		}
		report.Info("removing snapshot " + rs.id + " (its release " + rs.release + " is gone)")
		if err := removeSnapshotDir(cwd, p, filepath.Join(p.snaps, rs.id), wait); err != nil {
			report.Warn(err.Error())
			return false
		}
	}
	return true
}

// releaseSnapshot is a release snapshot's id and the release it belongs
// to (the one its release replaced).
type releaseSnapshot struct{ id, release string }

// releaseSnapshots are the environment's release snapshots, oldest first.
func releaseSnapshots(p snapshotPaths) []releaseSnapshot {
	ents, _ := os.ReadDir(p.snaps)
	var rs []releaseSnapshot
	for _, e := range ents {
		if !e.IsDir() || !remote.ReleaseIDFormat.MatchString(e.Name()) {
			continue
		}
		if m, err := readManifest(filepath.Join(p.snaps, e.Name())); err == nil && m.kind == "release" {
			rs = append(rs, releaseSnapshot{e.Name(), m.release})
		}
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].id < rs[j].id })
	return rs
}

// settleSnapshots starts what a take killed outright left stopped (the
// record's snapshot lines), unless the environment is marked: a mark
// keeps its set down. Every stop under the release lock runs it first,
// so a snapshot line it finds is a leftover by construction.
func settleSnapshots(cwd, base string) error {
	rec, ok := readSyncRecord(cwd)
	if !ok || len(rec.snaps) == 0 {
		return nil
	}
	if readMark(base) != nil {
		return nil
	}
	report.Info("starting what snapshot " + rec.snapID + ", cut off, left stopped: " + strings.Join(rec.snaps, " "))
	if err := compose(cwd, errOut, append([]string{"start"}, rec.snaps...)...); err != nil {
		return fmt.Errorf("docker compose start: %w", err)
	}
	if err := waitHealthy(cwd, rec.snaps); err != nil {
		return err
	}
	rec, _ = readSyncRecord(cwd)
	rec.snaps, rec.snapID = nil, ""
	if !rec.first && len(rec.stops) == 0 && len(rec.own) == 0 && len(rec.writers) == 0 {
		return os.Remove(filepath.Join(filepath.Dir(cwd), syncState))
	}
	return rec.write(cwd)
}

// runSnapshots implements devopsy @<target> --snapshots [remove
// <id>... [--yes]]: the snapshots there, or some removed.
func runSnapshots(t *remote.Target, projectName string, args []string, color bool) int {
	usage := "usage: devopsy @" + targetRef(t) + " --snapshots [remove <id>... [--yes] | restore <id> [--no-code] [--with-env] [--yes]]"
	yes := false
	var words []string
	for _, a := range args {
		switch a {
		case "--help", "-h":
			fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--snapshots"]))
			return 0
		case "--yes", "-y":
			yes = true
		case "--no-code", "--with-env":
		default:
			if strings.HasPrefix(a, "-") {
				report.Error(usage)
				return 2
			}
			words = append(words, a)
		}
	}
	if len(words) > 0 && words[0] == "restore" {
		return runRestore(t, projectName, args)
	}
	if len(words) > 0 && words[0] == "export" {
		if len(words) != 2 {
			report.Error("usage: devopsy @" + targetRef(t) + " --snapshots export <id>")
			return 2
		}
		if term.IsTerminal(int(os.Stdout.Fd())) {
			report.Error("--snapshots export writes the snapshot to stdout: redirect it (> snapshot.tar) or pipe it (| aws s3 cp - s3://...)")
			return 1
		}
		id, code := resolveSnapshot(t, projectName, words[1], "nothing exported")
		if code != 0 {
			return code
		}
		code, err := remote.SSHWith(t, remote.Bounded, remote.SnapshotExportScript(t, id), bytes.NewReader(nil), os.Stdout, false, nil)
		if err != nil {
			report.Fail(err)
			return 1
		}
		return code
	}
	if len(words) > 0 && words[0] == "import" {
		if len(words) != 1 {
			report.Error("usage: devopsy @" + targetRef(t) + " --snapshots import < <file>")
			return 2
		}
		if term.IsTerminal(int(os.Stdin.Fd())) {
			report.Error("--snapshots import reads a snapshot from stdin: < snapshot.tar, or a pipe (aws s3 cp s3://... - |)")
			return 1
		}
		code, err := remote.SSHContext(remote.WithGate(context.Background()), t, remote.Bounded, remote.SnapshotImportScript(t, projectName), os.Stdin, nil, false, nil)
		if err != nil {
			report.Fail(err)
			return 1
		}
		return code
	}
	if len(words) > 0 && (words[0] != "remove" || len(words) == 1) {
		report.Error(usage)
		return 2
	}
	var out bytes.Buffer
	code, err := remote.SSH(t, remote.SnapshotsScript(t, projectName), bytes.NewReader(nil), &out, false)
	if err != nil {
		report.Fail(err)
		return 1
	}
	if code != 0 {
		return code
	}
	type snap struct{ id, kind, release, taken, size, state, copy string }
	var snaps []snap
	mirrored := strings.Contains(out.String(), "mirror\tkept\n")
	// Lines one by one: a taken snapshot's state is empty, its line
	// ending in a tab.
	for _, line := range strings.Split(out.String(), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 6 {
			continue
		}
		n, _ := strconv.ParseInt(f[4], 10, 64)
		taken := f[3]
		if tm, err := time.Parse(time.RFC3339, f[3]); err == nil {
			taken = tm.UTC().Format("2006-01-02 15:04 UTC")
		}
		how := ""
		if len(f) > 6 {
			how = f[6]
		}
		snaps = append(snaps, snap{f[0], f[1], f[2], taken, size(n), f[5], how})
	}
	if len(words) == 0 {
		if len(snaps) == 0 {
			fmt.Println("no snapshots on " + t.Address + " (devopsy @" + targetRef(t) + " --snapshot takes one)")
			return 0
		}
		fmt.Printf("%-15s %-7s %-15s %-21s %s\n", "SNAPSHOT", "KIND", "RELEASE", "TAKEN", "SIZE")
		for _, s := range snaps {
			line := fmt.Sprintf("%-15s %-7s %-15s %-21s %s", s.id, s.kind, s.release, s.taken, s.size)
			if s.state != "" {
				line += "  " + strings.ToUpper(s.state)
			}
			fmt.Println(line)
		}
		switch {
		case snaps[0].copy == "reflink":
			fmt.Println("the last take cloned the data by reflinks: down for seconds")
		case mirrored:
			fmt.Println("a mirror of the data is kept beside them: a take is down only for what changed since its first pass")
		default:
			fmt.Println("no mirror: a take is down while its tars are written (snapshots: {mirror: true} keeps one, reflinks need none)")
		}
		return 0
	}
	// Every word resolved first: an exact id, else a unique prefix; one
	// that names none or several refuses them all.
	var ids, picked []string
	for _, s := range snaps {
		if s.state == "" {
			ids = append(ids, s.id)
		}
	}
	for _, w := range words[1:] {
		var match []string
		for _, id := range ids {
			if id == w {
				match = []string{id}
				break
			}
			if strings.HasPrefix(id, w) {
				match = append(match, id)
			}
		}
		switch {
		case len(match) == 0:
			report.Fail(&report.Failure{Msg: fmt.Sprintf("%s has no snapshot %s: nothing removed", t.Address, w), Details: []string{"its snapshots: " + cmp.Or(strings.Join(ids, " "), "none")}})
			return 1
		case len(match) > 1:
			report.Fail(&report.Failure{Msg: fmt.Sprintf("%s starts several snapshots (%s): nothing removed", w, strings.Join(match, " "))})
			return 1
		}
		if !slices.Contains(picked, match[0]) {
			picked = append(picked, match[0])
		}
	}
	if !yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			report.Fail(&report.Failure{
				Msg:   "--snapshots remove asks first, and there is no terminal to ask at",
				Fixes: []report.Remedy{report.Fix("add --yes:", "devopsy @"+targetRef(t)+" --snapshots "+strings.Join(args, " ")+" --yes")},
			})
			return 1
		}
		what := "this snapshot"
		if len(picked) > 1 {
			what = fmt.Sprintf("these %d snapshots", len(picked))
		}
		report.Info(fmt.Sprintf("this removes %s from %s:%s (%s), and the images no release or other snapshot holds", what, t.Host, t.Path, strings.Join(picked, ", ")))
		if !report.Default().Confirm(stdinReader, "remove "+what+"?") {
			report.Error("nothing removed")
			return 1
		}
	}
	code, err = remote.SSH(t, remote.SnapshotsRemoveScript(t, projectName, picked), bytes.NewReader(nil), os.Stdout, false)
	if err != nil {
		report.Fail(err)
		return 1
	}
	return code
}

// takingSnapshot is the take's line naming the snapshot and the release
// it is taken in, whose log the session's is appended to.
var takingSnapshot = regexp.MustCompile(regexp.QuoteMeta(report.Prefix) + `taking snapshot [0-9]{14} of ([0-9]{14})`)

// runSnapshot implements devopsy @<target> --snapshot: the take's one
// session (remote.SnapshotScript), watched as an activation is
// (liveSession), its log appended to the current release's.
func runSnapshot(t *remote.Target, projectName string, args []string, color bool) int {
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--snapshot"]))
		return 0
	}
	if len(args) > 0 {
		report.Error("usage: devopsy @" + targetRef(t) + " --snapshot")
		return 2
	}
	if !t.SnapshotsManual.Enabled {
		report.Error("manual snapshots are off for " + t.Name + ": its config's snapshots: manual is false")
		return 1
	}
	rlog := newReleaseLog("snapshot", t)
	log := report.Default().Tee(rlog.Line)
	log.Info(fmt.Sprintf("taking a snapshot of %s (%s:%s)", t.Address, t.Host, t.Path))
	code := liveSession(t, remote.SnapshotScript(t, projectName, t.SnapshotsManual.Keep), rlog, log, snapshotWords(t))
	rlog.Line(report.Line(report.InfoLevel, fmt.Sprintf("snapshot exited %d", code)))
	text := rlog.Text()
	if m := takingSnapshot.FindSubmatch(text); m != nil {
		if c, err := remote.SSHWith(t, remote.Bounded, remote.LogSaveScript(t, string(m[1]), true), bytes.NewReader(append([]byte("\n"), text...)), io.Discard, false, nil); err != nil || c != 0 {
			report.Warn("could not save the snapshot's log on the server")
		}
	}
	return code
}

// snapshotWords are what liveSession says of a take.
func snapshotWords(t *remote.Target) liveWords {
	logs := "devopsy @" + targetRef(t) + " --log shows how it ends"
	return liveWords{
		goesOn:     []string{"interrupted: the snapshot goes on on @" + t.Address + ", past its stopped pass", logs + "; Ctrl-C again ends the session, not the snapshot"},
		cancelling: []string{"interrupted: cancelling the snapshot on @" + t.Address + ", its services started again"},
		slow:       []string{"the server is cancelling it, first waiting for a lock if a release, a sync or another snapshot holds it", "Ctrl-C again leaves now"},
		lostAfter:  []string{"the connection to @" + t.Address + " was lost after the stopped pass: the snapshot goes on there", logs},
		lostBefore: []string{"the connection to @" + t.Address + " failed or was lost before the stopped pass ended: the server cancels the snapshot once it notices, within a minute, its services started again"},
		hadGone:    []string{"it was past its stopped pass already, and went on", logs},
	}
}

// devopsyTargetLines are the lines devopsy writes into a release's
// devopsy-target.env about where it runs, which a restore takes from the
// current release, never the snapshot's (an imported snapshot names
// another server): the rest, the config's env and the commit, are the
// snapshot's, and its imports section the current release's too.
var devopsyTargetLines = []string{"COMPOSE_PROJECT_NAME", "DEVOPSY_PROJECT", "DEVOPSY_INSTANCE", "DEVOPSY_ENVIRONMENT", "DEVOPSY_TARGET", "DEVOPSY_PARENT", "DEVOPSY_PARENT_TARGET"}

// mergeTargetEnv is the restored release's devopsy-target.env: snap's
// own lines but the ones devopsyTargetLines names, then current's of
// those, then current's imports.
func mergeTargetEnv(snap, current string) string {
	split := func(s string) (lines []string, imports string) {
		head, tail, found := strings.Cut(s, cli.ImportedMarker)
		if found {
			imports = cli.ImportedMarker + tail
		}
		return strings.Split(strings.TrimRight(head, "\n"), "\n"), imports
	}
	key := func(line string) string {
		k, _, _ := strings.Cut(line, "=")
		return strings.TrimSpace(k)
	}
	sl, _ := split(snap)
	cl, imports := split(current)
	var b strings.Builder
	for _, l := range sl {
		if l != "" && !slices.Contains(devopsyTargetLines, key(l)) {
			b.WriteString(l + "\n")
		}
	}
	for _, l := range cl {
		if slices.Contains(devopsyTargetLines, key(l)) {
			b.WriteString(l + "\n")
		}
	}
	if imports != "" {
		b.WriteString(imports)
	}
	return b.String()
}

// snapshotMakeRelease makes the restore's release rel from snapshot
// snap's copy of its release: its code, its files but its logs, its
// devopsy-target.env merged with the current release's (mergeTargetEnv),
// a record naming the snapshot, pending until the switch, and the
// snapshot's images bookmarked for it, which its images restore from.
func snapshotMakeRelease(cwd string, p snapshotPaths, snap, rel string) error {
	m, err := readManifest(snap)
	if err != nil {
		return fmt.Errorf("snapshot %s: %w", filepath.Base(snap), err)
	}
	if m.format != remote.SnapshotFormat {
		return fmt.Errorf("snapshot %s is of a format this devopsy does not know (%s): upgrade the server's devopsy", filepath.Base(snap), cmp.Or(m.format, "none"))
	}
	current, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return err
	}
	current = filepath.Dir(current)
	if err := os.Mkdir(rel, 0o755); err != nil {
		return err
	}
	if out, err := exec.Command("cp", "-a", filepath.Join(snap, remote.CodeDir), filepath.Join(rel, remote.CodeDir)).CombinedOutput(); err != nil {
		return fmt.Errorf("copying the snapshot's code: %v %s", err, out)
	}
	files, _ := os.ReadDir(snap)
	for _, f := range files {
		// Only what the new release needs of the snapshot's: the rest (its
		// marks, an image history forced, the images file the activation
		// writes) is the old release's state, and an imported one anyone's.
		switch name := f.Name(); {
		case name == remote.TargetEnvFile:
			s, _ := os.ReadFile(filepath.Join(snap, name))
			c, _ := os.ReadFile(filepath.Join(current, name))
			if err := os.WriteFile(filepath.Join(rel, name), []byte(mergeTargetEnv(string(s), string(c))), 0o644); err != nil {
				return err
			}
		case name == remote.RecordFile:
			var r remote.Record
			data, _ := os.ReadFile(filepath.Join(snap, name))
			_ = json.Unmarshal(data, &r)
			r.Time, r.By, r.Restore = time.Now().UTC().Format(time.RFC3339), cmp.Or(os.Getenv(remote.RestoreByEnv), r.By), filepath.Base(snap)
			if err := os.WriteFile(filepath.Join(rel, name), []byte(r.JSON()), 0o644); err != nil {
				return err
			}
		}
	}
	if err := os.WriteFile(filepath.Join(rel, remote.PendingFile), nil, 0o644); err != nil {
		return err
	}
	unlock, err := imagesShared(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	set := &images.Set{Docker: images.CLI{}, Name: composeName(cwd), Out: errOut, Left: os.Getenv(remote.ImagesLeftEnv), Mode: os.Getenv(remote.ImagesModeEnv), Base: p.base, Snapshots: p.snaps}
	return set.BookmarkFrom(rel, snap)
}

// snapshotTar is one of a snapshot's tars: its file in data/, what it
// holds (an entry of mnt, a file or not, or a volume) and its name.
type snapshotTarFile struct {
	file, name string
	isFile     bool
	volume     string
}

func snapshotTars(m *manifest) []snapshotTarFile {
	var ts []snapshotTarFile
	for _, e := range m.entries {
		ts = append(ts, snapshotTarFile{file: "mnt." + e + ".tar.zst", name: e, isFile: slices.Contains(m.files, e)})
	}
	for _, k := range slices.Sorted(maps.Keys(m.volumes)) {
		ts = append(ts, snapshotTarFile{file: "volume." + k + ".tar.zst", name: k, volume: m.volumes[k]})
	}
	return ts
}

// unzstd streams the tar file of snap's data/, decompressed in the util
// image, to f; wait ends it.
func unzstd(ctx context.Context, snap, file, name string) (r io.ReadCloser, wait func() error, err error) {
	args := []string{"run", "--rm", "--name", name, "--network", "none", "-v", filepath.Join(snap, remote.SnapshotData) + ":/s:ro", remote.UtilImage, "zstd", "-dcq", "/s/" + file}
	trace(append([]string{"docker"}, args...))
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stderr = errOut
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return out, cmd.Wait, nil
}

// snapshotCheckTars reads and checks each of snap's tars whole, as an
// import checks its tar (tarEntriesIn: complete, nothing reaching out of
// its entry, only files, directories and links), each carrying its own
// entry or volume alone and of the kind the manifest says; and has the
// images its data services ran there (pulled by digest when gone).
func snapshotCheckTars(cwd string, p snapshotPaths, snap string) error {
	ctx, stop := interruptible()
	defer stop()
	m, err := readManifest(snap)
	if err != nil {
		return err
	}
	if m.format != remote.SnapshotFormat {
		return fmt.Errorf("snapshot %s is of a format this devopsy does not know (%s): upgrade the server's devopsy", filepath.Base(snap), cmp.Or(m.format, "none"))
	}
	if err := ensureUtilImage(ctx); err != nil {
		return err
	}
	for _, t := range snapshotTars(m) {
		r, wait, err := unzstd(ctx, snap, t.file, "devopsy-snapshot-"+composeName(cwd)+"-"+filepath.Base(snap)+"-check")
		if err != nil {
			return err
		}
		es, err := tarEntriesIn(r)
		_, _ = io.Copy(io.Discard, r)
		if werr := wait(); err == nil && werr != nil {
			err = werr
		}
		switch {
		case err != nil:
		case len(es) != 1 || es[0].name != t.name:
			err = errors.New("it carries something else than " + t.name)
		case es[0].file != t.isFile:
			err = errors.New("it carries " + t.name + " as another kind than the manifest says")
		case !slices.Equal(es[0].forms, []string{t.name}):
			// A take names members plainly; the extraction matches by that
			// name alone (GNU tar takes ./storage/x only by ./storage).
			err = errors.New("it names " + t.name + "'s members otherwise than " + t.name + "/...")
		}
		if err != nil {
			return fmt.Errorf("snapshot %s's %s: %v: nothing restored", filepath.Base(snap), t.file, err)
		}
	}
	data, _ := os.ReadFile(filepath.Join(snap, remote.SnapshotData, "images"))
	set := &images.Set{Docker: images.CLI{}, Name: composeName(cwd), Out: errOut}
	if err := set.SnapshotPull(snap, string(data)); err != nil {
		return fmt.Errorf("snapshot %s: %w", filepath.Base(snap), err)
	}
	return nil
}

// snapshotImported takes the snapshot an import extracted into tmp: one
// directory, a snapshot's id, of a format this devopsy knows, its tars
// whole; renamed into place under a new id (the server's), a manual
// snapshot of this environment's from then on.
func snapshotImported(cwd string, p snapshotPaths, tmp string) error {
	ents, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	if len(ents) != 1 || !ents[0].IsDir() || !remote.ReleaseIDFormat.MatchString(ents[0].Name()) {
		return errors.New("stdin is no snapshot (one directory named by its id): nothing imported")
	}
	old := ents[0].Name()
	dir := filepath.Join(tmp, old)
	m, err := readManifest(dir)
	if err != nil {
		return fmt.Errorf("the snapshot has no manifest (%v): nothing imported", err)
	}
	if m.format != remote.SnapshotFormat {
		return fmt.Errorf("the snapshot is of a format this devopsy does not know (%s): upgrade the server's devopsy: nothing imported", cmp.Or(m.format, "none"))
	}
	if err := snapshotCheckTars(cwd, p, dir); err != nil {
		return err
	}
	var ids []string
	if es, err := os.ReadDir(p.snaps); err == nil {
		for _, e := range es {
			ids = append(ids, e.Name())
		}
	}
	id := remote.NextReleaseID(time.Now(), ids)
	// Imported, it is a snapshot someone chose to bring, as a take is: a
	// release snapshot's release is not here, and pruning would take it for
	// one whose release went.
	if m.kind != "manual" {
		mf := filepath.Join(dir, remote.SnapshotManifest)
		if err := os.WriteFile(mf, []byte(strings.Replace(fileText(mf), "\nkind\t"+m.kind+"\n", "\nkind\tmanual\n", 1)), 0o600); err != nil {
			return err
		}
	}
	if err := os.Rename(dir, filepath.Join(p.snaps, id)); err != nil {
		return err
	}
	_ = os.RemoveAll(tmp)
	// The images it lists that this server has, bookmarked for it; the
	// rest come back at its restore, pulled by digest or built from its
	// code.
	unlock, err := imagesShared(context.Background())
	if err != nil {
		return err
	}
	set := &images.Set{Docker: images.CLI{}, Name: composeName(cwd), Out: io.Discard, Left: os.Getenv(remote.ImagesLeftEnv), Base: p.base, Snapshots: p.snaps}
	_ = set.BookmarkFrom(filepath.Join(p.snaps, id), filepath.Join(p.snaps, id))
	unlock()
	report.Info("imported snapshot "+old+" as "+id+" (taken "+m.taken+" in release "+m.release+")", "to restore it:", report.CmdMark+"devopsy @"+targetAddress(cwd)+" --snapshots restore "+id)
	return nil
}

// snapshotCheckFit refuses a --no-code restore whose snapshot the current
// release's labels do not fit, naming each mismatch (devopsy would have
// to guess which service to stop, hold on the snapshot's image and give
// the files to): a data service of the snapshot gone or without data
// now, an entry or a volume it held that no service of that name mounts
// now, or one another service with data mounts now; and on a marked
// environment one that leaves a service with data of the current labels,
// or any of its entries and volumes, out (the run step would start it on
// its half-written data).
func snapshotCheckFit(cwd string, p snapshotPaths, snap string) error {
	m, err := readManifest(snap)
	if err != nil {
		return err
	}
	_, services, err := cli.SyncServices(cwd, os.Environ())
	if err != nil {
		return err
	}
	var bad []string
	for _, ms := range m.services {
		cur, ok := cli.SyncFind(services, ms.name)
		if !ok || !cur.HasData {
			bad = append(bad, "the snapshot has data for "+ms.name+", which the current release has no service with data of")
			continue
		}
		for _, e := range ms.entries {
			if !slices.Contains(cur.Dirs, e) {
				bad = append(bad, ms.name+" held mnt/"+e+", which it no longer mounts")
			}
			for _, o := range services {
				if o.Name != ms.name && o.HasData && slices.Contains(o.Dirs, e) {
					bad = append(bad, "mnt/"+e+", "+ms.name+"'s in the snapshot, is "+o.Name+"'s now")
				}
			}
		}
		for _, k := range ms.volumes {
			if !slices.Contains(cli.VolumeKeys(cur.Volumes), k) {
				bad = append(bad, ms.name+" held the volume "+k+", which it no longer mounts")
			}
			for _, o := range services {
				if o.Name != ms.name && o.HasData && slices.Contains(cli.VolumeKeys(o.Volumes), k) {
					bad = append(bad, "the volume "+k+", "+ms.name+"'s in the snapshot, is "+o.Name+"'s now")
				}
			}
		}
	}
	if readMark(p.base) != nil {
		// All of each, as a sync clears a mark (copiesService): an entry
		// or a volume the snapshot lacks would keep the half-written data.
		for _, s := range services {
			if !s.HasData {
				continue
			}
			i := slices.IndexFunc(m.services, func(ms manifestService) bool { return ms.name == s.Name })
			if i < 0 {
				bad = append(bad, "the environment is marked, and the snapshot has no data for "+s.Name+", which would run on what is there")
				continue
			}
			for _, e := range s.Dirs {
				if !slices.Contains(m.services[i].entries, e) {
					bad = append(bad, "the environment is marked, and the snapshot has no mnt/"+e+" of "+s.Name+", which would stay as it is")
				}
			}
			for _, k := range cli.VolumeKeys(s.Volumes) {
				if !slices.Contains(m.services[i].volumes, k) {
					bad = append(bad, "the environment is marked, and the snapshot has no volume "+k+" of "+s.Name+", which would stay as it is")
				}
			}
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return &report.Failure{Msg: "snapshot " + filepath.Base(snap) + " does not fit the current release's labels: nothing restored", Details: bad,
		Fixes: []report.Remedy{report.Fix("restore it whole, its code included:", "devopsy @"+targetAddress(cwd)+" --snapshots restore "+filepath.Base(snap))}}
}

// snapshotRestoreData writes snap's data into the environment, for the
// release rel being restored, as an import writes its tar: the current
// release's services with data and their dependents stopped (recorded),
// a restore mark right before the first write, each entry and volume
// emptied and extracted in its own util container, the data services
// held on the images that ran on the data, with its history; then the
// mark goes (it covers every service with data the restored code runs),
// and so do its stop lines: the run step starts them.
func snapshotRestoreData(cwd string, p snapshotPaths, snap, rel string) int {
	fail := func(msg string) int {
		report.Error(msg)
		return 1
	}
	signal.Ignore(syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)
	ctx := context.Background()
	id := filepath.Base(snap)
	m, err := readManifest(snap)
	if err != nil {
		return fail(err.Error())
	}
	// What its snapshot of the data it replaces left stopped (release
	// snapshots on, its pre-restore pass in this same session) is the
	// restore's: its stop lines, kept down for its data step, never started
	// by the settle below as a take's cut off.
	if rec, ok := readSyncRecord(cwd); ok && len(rec.snaps) > 0 && os.Getenv(remote.SnapshotReleaseKeepEnv) != "" {
		for _, name := range rec.snaps {
			if !slices.Contains(rec.stops, name) {
				rec.stops = append(rec.stops, name)
			}
		}
		rec.snaps, rec.snapID = nil, ""
		if err := rec.write(cwd); err != nil {
			return fail(err.Error() + ": nothing restored")
		}
	}
	if err := settleSnapshots(cwd, p.base); err != nil {
		return fail(err.Error())
	}
	projectDir, services, err := cli.SyncServices(cwd, os.Environ())
	if err != nil {
		return fail(err.Error())
	}
	switch mk := readMark(p.base); {
	case mk.IsImport():
		if err := endLeftoverImport(cwd, p.base, services, mk.From); err != nil {
			return fail(err.Error())
		}
	case mk != nil && mk.Code == remote.IncompleteRestore:
		if err := removeContainers(importContainers(cwd)); err != nil {
			return fail(err.Error())
		}
	}
	mnt, err := dataMnt(projectDir)
	if err != nil {
		return fail("the project's mnt: " + err.Error())
	}
	// Every tar goes where the restored code's services with data have it,
	// a volume by the name compose gives its key there, never by what the
	// manifest says (an imported one is anyone's), all checked before
	// anything stops.
	_, restored, err := cli.SyncServices(filepath.Join(rel, remote.CodeDir), os.Environ())
	if err != nil {
		return fail("the restored code's labels: " + err.Error() + ": nothing restored")
	}
	entries, volumes := map[string]bool{}, map[string]cli.Volume{}
	for _, s := range restored {
		if s.HasData {
			for _, d := range s.Dirs {
				entries[d] = true
			}
			for _, v := range s.Volumes {
				volumes[v.Key] = v
			}
		}
	}
	owners, err := cli.MntOwners(restored)
	if err != nil {
		return fail("the restored code's labels: " + err.Error() + ": nothing restored")
	}
	tars := snapshotTars(m)
	for _, t := range tars {
		if _, ok := volumes[t.name]; t.volume != "" && !ok || t.volume == "" && !entries[t.name] {
			return fail(fmt.Sprintf("snapshot %s holds %s, which no service with data of the restored code has: nothing restored", id, map[bool]string{true: "the volume " + t.name, false: "mnt/" + t.name}[t.volume != ""]))
		}
		// As an import refuses: a mount point of another kind would fail
		// the extraction once the entry was emptied.
		if t.volume != "" {
			continue
		}
		if e := symlinkEntry(mnt, []string{t.name}); e != "" {
			return fail("mnt/" + e + " is a symlink: the restore would not reach what it points to: make it a directory, mounting that there: nothing restored")
		}
		if fi, err := os.Stat(filepath.Join(mnt, t.name)); err == nil && fi.IsDir() == t.isFile {
			as, is := "a directory", "a file"
			if t.isFile {
				as, is = is, as
			}
			return fail(fmt.Sprintf("snapshot %s holds mnt/%s as %s, and it is %s here: make it %s first: nothing restored", id, t.name, as, is, as))
		}
	}
	// On a marked environment the restore clears the mark, so it must write
	// all of the data a service with data has now, the current release's
	// and the restored one's, as a sync clears one (copiesService): an
	// entry or a volume there now that the snapshot lacks (absent at the
	// take) would keep what the cut copy left.
	if readMark(p.base) != nil {
		var bad []string
		for _, s := range append(slices.Clone(services), restored...) {
			if !s.HasData {
				continue
			}
			for _, d := range s.Dirs {
				if _, err := os.Lstat(filepath.Join(mnt, d)); err == nil && !slices.Contains(m.entries, d) && !slices.Contains(bad, "mnt/"+d) {
					bad = append(bad, "mnt/"+d)
				}
			}
			for _, v := range s.Volumes {
				if _, ok := m.volumes[v.Key]; ok || slices.Contains(bad, "the volume "+v.Key) {
					continue
				}
				if there, err := volumeExists(ctx, v.Name); err != nil {
					return fail(err.Error() + ": nothing restored")
				} else if there {
					bad = append(bad, "the volume "+v.Key)
				}
			}
		}
		if len(bad) > 0 {
			return fail("the environment is marked, and snapshot " + id + " has no " + strings.Join(bad, ", ") + ", which would stay as the cut copy left it: nothing restored")
		}
	}
	// Stopped: the current release's services with data, and any of its
	// services that mounts what the restore writes, a label or not.
	var names []string
	for _, s := range services {
		writes := slices.ContainsFunc(s.Dirs, func(d string) bool { return slices.Contains(m.entries, d) }) ||
			slices.ContainsFunc(s.Volumes, func(v cli.Volume) bool { _, ok := m.volumes[v.Key]; return ok })
		if s.HasData || writes {
			names = append(names, s.Name)
		}
	}
	set := cli.WithDependents(services, names, "restore")
	_, restore, err := stopSet(cwd, set, true)
	if err != nil {
		if rerr := restore(); rerr != nil {
			report.Error(rerr.Error())
		}
		return fail(err.Error() + ": nothing restored")
	}
	if err := markIncomplete(p.base, id, remote.IncompleteRestore, "it failed or was cut after its first write: its data may be partial", false); err != nil {
		if rerr := restore(); rerr != nil {
			report.Error(rerr.Error())
		}
		return fail(err.Error() + ": nothing restored")
	}
	// From the mark on, the restore rule: whatever fails keeps the set
	// down under the mark, its ways out a restore again or another.
	cname := fmt.Sprintf("%s%s-%d", remote.ImportContainerPrefix, composeName(cwd), os.Getpid())
	for i, t := range tars {
		e := tarEntry{name: t.name, forms: []string{t.name}, file: t.isFile}
		if t.volume != "" {
			v := volumes[t.name]
			e.volume = &v
		}
		report.Info("restoring " + map[bool]string{true: "the volume ", false: "mnt/"}[t.volume != ""] + t.name)
		r, wait, err := unzstd(ctx, snap, t.file, fmt.Sprintf("%s-unzstd-%d", cname, i))
		if err == nil {
			err = extractEntry(ctx, mnt, r, e, fmt.Sprintf("%s-extract-%d", cname, i), composeName(cwd))
			// What the extraction left unread, so the decompression ends.
			_, _ = io.Copy(io.Discard, r)
			if werr := wait(); err == nil && werr != nil {
				err = werr
			}
		}
		if err != nil {
			report.Fail(&report.Failure{Msg: err.Error() + ": the services stay down", Fixes: restoreRemedies(cwd, id)})
			return 1
		}
	}
	// What it extracted goes to its owner, as an import's does: the tar
	// carries the owners of when it was taken, or of another server.
	owned := map[string]string{}
	for _, e := range m.entries {
		if o := owners[e]; o != "" {
			owned[e] = o
		}
	}
	if err := ownMnt(ctx, mnt, owned, cname+"-owners"); err != nil {
		report.Fail(&report.Failure{Msg: err.Error() + ": the services stay down", Fixes: restoreRemedies(cwd, id)})
		return 1
	}
	// The images that ran on the data, held, with their history.
	data, _ := os.ReadFile(filepath.Join(snap, remote.SnapshotData, "images"))
	if len(data) > 0 {
		set := &images.Set{Docker: images.CLI{}, Name: composeName(cwd), Out: errOut, List: images.ListCompose, Left: os.Getenv(remote.ImagesLeftEnv), Mode: os.Getenv(remote.ImagesModeEnv), Base: p.base, Snapshots: p.snaps}
		unlock, err := imagesShared(ctx)
		if err != nil {
			return fail(err.Error())
		}
		err = set.ImportDone(p.base, rel, string(data))
		unlock()
		if err != nil {
			return fail(err.Error())
		}
	}
	if os.Getenv(remote.RestoreEnvEnv) != "" {
		for from, to := range map[string]string{"shared.env": ".env", "inherit.env": remote.InheritFile} {
			if src := filepath.Join(snap, remote.SnapshotData, from); fileExists(src) {
				if err := copyFile(src, filepath.Join(p.base, "shared", to+".restoring")); err != nil {
					return fail(err.Error())
				}
				if err := os.Rename(filepath.Join(p.base, "shared", to+".restoring"), filepath.Join(p.base, "shared", to)); err != nil {
					return fail(err.Error())
				}
				report.Info("shared/" + to + " restored from the snapshot")
			}
		}
	} else if keys, differs := envKeysDiffer(filepath.Join(snap, remote.SnapshotData, "shared.env"), filepath.Join(p.base, "shared", ".env")); differs {
		in := " in " + strings.Join(keys, ", ")
		if len(keys) == 0 {
			in = ""
		}
		report.Warn("the snapshot's shared/.env differs from the environment's"+in+": kept as it is", "--with-env brings it back too:", report.CmdMark+"devopsy @"+targetAddress(cwd)+" --snapshots restore "+id+" --with-env")
	}
	// The mark and the record stay until the run step has started the set
	// (restored): a shell killed before it leaves the set down, marked.
	report.Info("snapshot " + id + "'s data is in")
	return 0
}

// snapshotReceive extracts the tar on in (an exported snapshot) into dir
// with GNU tar, reading the same stream with archive/tar to its end: a
// stream cut at a member's boundary (a dropped session, a pipe that
// died, a truncated file) has no end-of-archive blocks, and GNU tar
// takes it for complete.
func snapshotReceive(in io.Reader, dir string) error {
	cmd := exec.Command("tar", "-xmf", "-", "-C", dir)
	cmd.Stdout, cmd.Stderr = errOut, errOut
	w, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// What tar stops reading past its end (the record's padding) is no
	// failure of the stream: tar's own exit says how it went.
	tw := &lenientWriter{w: w}
	r := &endReader{r: io.TeeReader(in, tw)}
	tr := tar.NewReader(r)
	var rerr error
	for {
		if _, err := tr.Next(); err == io.EOF {
			if r.atEOF {
				rerr = errNoTarEnd
			}
			break
		} else if err != nil {
			rerr = err
			break
		}
	}
	if rerr == nil {
		_, rerr = io.Copy(tw, in)
	}
	_ = w.Close()
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("stdin is not a complete snapshot: tar: %v: nothing imported", err)
	}
	if rerr != nil {
		return fmt.Errorf("stdin is not a complete snapshot: %v: nothing imported", rerr)
	}
	return nil
}

// lenientWriter passes writes on until one fails, then takes the rest
// without writing.
type lenientWriter struct {
	w   io.Writer
	err error
}

func (l *lenientWriter) Write(p []byte) (int, error) {
	if l.err == nil {
		_, l.err = l.w.Write(p)
	}
	return len(p), nil
}

// restoreRemedies are a restore mark's ways out, as commands that work
// copied (the environment's address).
func restoreRemedies(cwd, id string) []report.Remedy {
	var rs []report.Remedy
	for _, f := range remote.RestoreFixes(targetAddress(cwd), id, projectVar(cwd, "DEVOPSY_PARENT") != "") {
		rs = append(rs, report.Fix(f[0], f[1]))
	}
	return rs
}

// snapshotRestored completes a restore once its run step went through:
// its mark (still its own) and the record of what it stopped, whole (a
// restore run again after a cut one stopped nothing itself: the lines
// are the first one's, and the run step started them all).
func snapshotRestored(cwd string, p snapshotPaths, snap string) error {
	id := filepath.Base(snap)
	if m := readMark(p.base); m != nil && m.Code == remote.IncompleteRestore && m.From == id {
		if err := os.Remove(filepath.Join(p.base, remote.SyncIncompleteFile)); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(p.base, syncState)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	report.Info("the restore of snapshot " + id + " is complete")
	return nil
}

// envKeysDiffer are the keys whose values differ between two .env files,
// sorted, read with compose's parser (never the values: a line of a
// value over several lines is no key). differs without keys is a file
// that does not parse.
func envKeysDiffer(a, b string) (keys []string, differs bool) {
	read := func(p string) ([]byte, map[string]string, bool) {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, map[string]string{}, true
		}
		m, err := dotenv.UnmarshalBytesWithLookup(data, nil)
		return data, m, err == nil
	}
	da, ma, oka := read(a)
	db, mb, okb := read(b)
	if !oka || !okb {
		return nil, !bytes.Equal(da, db)
	}
	for k, v := range ma {
		if w, ok := mb[k]; !ok || w != v {
			keys = append(keys, k)
		}
	}
	for k := range mb {
		if _, ok := ma[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, len(keys) > 0
}

// restoringUnder is a --no-code restore's line naming the release whose
// log the session's is appended to.
var restoringUnder = regexp.MustCompile(regexp.QuoteMeta(report.Prefix) + `restoring snapshot [0-9]{14}'s data under ([0-9]{14})`)

// restoreDataWords are what liveSession says of a --no-code restore: past
// the disarm it writes the data, on its own.
func restoreDataWords(t *remote.Target) liveWords {
	w := releaseWords(t)
	logs := "devopsy @" + targetRef(t) + " --log shows how it ends"
	w.goesOn = []string{"interrupted: the restore goes on on @" + t.Address + ", writing the data", logs + "; Ctrl-C again ends the session, not the restore"}
	w.cancelling = []string{"interrupted: cancelling the restore on @" + t.Address}
	w.lostAfter = []string{"the connection to @" + t.Address + " was lost while it wrote the data: the restore goes on there", logs}
	w.lostBefore = []string{"the connection to @" + t.Address + " failed or was lost before the restore wrote anything: the server cancels it once it notices"}
	w.hadGone = []string{"it was writing the data already, and went on", logs}
	return w
}

// restoringAs is a restore's line naming the release it makes, whose log
// the session's is.
var restoringAs = regexp.MustCompile(regexp.QuoteMeta(report.Prefix) + `restoring snapshot [0-9]{14} as release ([0-9]{14})`)

// runRestore implements devopsy @<target> --snapshots restore <id>
// [--no-code] [--with-env] [--yes]: the snapshot's id resolved (a unique
// prefix), the environment's full name asked for, then the restore's one
// session, an activation's (liveSession), its log the new release's.
// resolveSnapshot is the complete snapshot word names on t: its id, else
// the one id it starts; else a failure saying nothing, and its code.
func resolveSnapshot(t *remote.Target, projectName, word, nothing string) (string, int) {
	var out bytes.Buffer
	code, err := remote.SSH(t, remote.SnapshotsScript(t, projectName), bytes.NewReader(nil), &out, false)
	if err != nil || code != 0 {
		if err != nil {
			report.Fail(err)
		}
		return "", max(code, 1)
	}
	var ids, match []string
	for _, line := range strings.Split(out.String(), "\n") {
		if f := strings.Split(line, "\t"); len(f) >= 6 && f[5] == "" {
			ids = append(ids, f[0])
		}
	}
	for _, id := range ids {
		if id == word {
			match = []string{id}
			break
		}
		if strings.HasPrefix(id, word) {
			match = append(match, id)
		}
	}
	switch {
	case len(match) == 0:
		report.Fail(&report.Failure{Msg: fmt.Sprintf("%s has no snapshot %s: %s", t.Address, word, nothing), Details: []string{"its snapshots: " + cmp.Or(strings.Join(ids, " "), "none")}})
		return "", 1
	case len(match) > 1:
		report.Fail(&report.Failure{Msg: fmt.Sprintf("%s starts several snapshots (%s): %s", word, strings.Join(match, " "), nothing)})
		return "", 1
	}
	return match[0], 0
}

func runRestore(t *remote.Target, projectName string, args []string) int {
	usage := "usage: devopsy @" + targetRef(t) + " --snapshots restore <id> [--no-code] [--with-env] [--yes]"
	var words []string
	yes, noCode, withEnv := false, false, false
	for _, a := range args {
		switch a {
		case "--yes", "-y":
			yes = true
		case "--no-code":
			noCode = true
		case "--with-env":
			withEnv = true
		default:
			words = append(words, a)
		}
	}
	if len(words) != 2 {
		report.Error(usage)
		return 2
	}
	id, code := resolveSnapshot(t, projectName, words[1], "nothing restored")
	if code != 0 {
		return code
	}
	what := "snapshot " + id + "'s data, code and the images that ran on them"
	if noCode {
		what = "snapshot " + id + "'s data and the images that ran on it, under its current release"
	}
	if withEnv {
		what += ", and its shared/.env"
	}
	if !yes {
		how := ", as a new release"
		if noCode {
			how = ""
		}
		if err := typeToGoOn(fmt.Sprintf("this replaces @%s's data with %s%s (%s:%s): what it holds now goes; devopsy @%s --snapshot first keeps it", t.Address, what, how, t.Host, t.Path, targetRef(t)), confirmWord(t), "nothing restored", false); err != nil {
			report.Fail(err)
			return 1
		}
	}
	rlog := newReleaseLog("restore", t)
	log := report.Default().Tee(rlog.Line)
	log.Info(fmt.Sprintf("restoring %s (%s:%s) from snapshot %s", t.Address, t.Host, t.Path, id))
	opts := &remote.RestoreOptions{Snapshot: id, WithEnv: withEnv, By: remote.Whoami()}
	if noCode {
		code = liveSession(t, remote.RestoreDataScript(t, projectName, opts), rlog, log, restoreDataWords(t))
		rlog.Line(report.Line(report.InfoLevel, fmt.Sprintf("restore exited %d", code)))
		text := rlog.Text()
		if m := restoringUnder.FindSubmatch(text); m != nil {
			if c, err := remote.SSHWith(t, remote.Bounded, remote.LogSaveScript(t, string(m[1]), true), bytes.NewReader(append([]byte("\n"), text...)), io.Discard, false, nil); err != nil || c != 0 {
				report.Warn("could not save the restore's log on the server")
			}
		}
		return code
	}
	code = liveSession(t, remote.RestoreScript(t, projectName, opts), rlog, log, releaseWords(t))
	rlog.Line(report.Line(report.InfoLevel, fmt.Sprintf("restore exited %d", code)))
	text := rlog.Text()
	if m := restoringAs.FindSubmatch(text); m != nil {
		if c, err := remote.SSHWith(t, remote.Bounded, remote.LogSaveScript(t, string(m[1]), false), bytes.NewReader(text), io.Discard, false, nil); err != nil || c != 0 {
			report.Warn("could not save the restore's log on the server")
		}
	}
	return code
}
