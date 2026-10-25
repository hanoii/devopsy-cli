package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
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
// directory, where its snapshots go and the mirror (on the data's own
// filesystem, under $base/snapshots/ whatever the snapshot root).
type snapshotPaths struct{ base, snaps, mirror string }

func snapshotPathsOf(cwd string) snapshotPaths {
	base := cmp.Or(os.Getenv(images.BaseEnv), filepath.Dir(cwd))
	return snapshotPaths{
		base:   base,
		snaps:  cmp.Or(os.Getenv(images.SnapshotsEnv), filepath.Join(base, "snapshots")),
		mirror: filepath.Join(base, "snapshots", remote.SnapshotMirror),
	}
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
		if err := snapshotCheck(ctx, p, d); err != nil {
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
		_ = os.Remove(filepath.Join(p.mirror, remote.SnapshotComplete))
		if err := copyMirror(ctx, p, d, "devopsy-snapshot-"+composeName(cwd)+"-mirror"); err != nil {
			return fail(err.Error())
		}
		return 0
	case "stopped":
		if len(args) != 3 {
			return fail("usage: devopsy " + remote.SnapshotStepFlag + " stopped <id> <release>")
		}
		return snapshotStopped(cwd, p, args[1], args[2])
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
				report.Error(err.Error())
				code = 1
			}
		}
		return code
	case "tar":
		if len(args) != 2 {
			return fail("usage: devopsy " + remote.SnapshotStepFlag + " tar <id>")
		}
		// Past the stopped pass nothing interrupts it: a signal, a session
		// gone (its writes go to the server's log).
		signal.Ignore(syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)
		if err := snapshotTar(p, args[1]); err != nil {
			return fail(err.Error())
		}
		pruneSnapshots(cwd, p)
		return 0
	}
	return fail("unknown " + remote.SnapshotStepFlag + " word: " + args[0])
}

// snapshotCheck refuses, before anything stops: nothing with data, a
// service with data writing a bind from outside the project (no copy or
// snapshot carries it), and too little space for the mirror and the
// tars, the data measured as root in the util image (volumes are root's).
func snapshotCheck(ctx context.Context, p snapshotPaths, d *snapshotData) error {
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
	mirrored, err := duDirs(ctx, p.mirror)
	if err != nil {
		return err
	}
	// The mirror takes what it lacks, the tars at most the data again
	// (compressed, usually far less); a tenth more each, a margin.
	needMirror := max(data-mirrored, 0) * 11 / 10
	needTars := data * 11 / 10
	if sameDevice(p.mirror, p.snaps) {
		if free := freeBytes(p.mirror); free < needMirror+needTars {
			return fmt.Errorf("not enough space for a snapshot at %s: %s needed (the data is %s), %s free: nothing stopped", p.mirror, size(needMirror+needTars), size(data), size(free))
		}
		return nil
	}
	if free := freeBytes(p.mirror); free < needMirror {
		return fmt.Errorf("not enough space for the snapshot's mirror at %s: %s needed (the data is %s), %s free: nothing stopped", p.mirror, size(needMirror), size(data), size(free))
	}
	if free := freeBytes(p.snaps); free < needTars {
		return fmt.Errorf("not enough space for a snapshot at %s: %s needed (the data is %s), %s free: nothing stopped", p.snaps, size(needTars), size(data), size(free))
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

func sameDevice(a, b string) bool {
	dev := func(p string) (uint64, bool) {
		for d := p; ; d = filepath.Dir(d) {
			if fi, err := os.Stat(d); err == nil {
				if st, ok := fi.Sys().(*syscall.Stat_t); ok {
					return uint64(st.Dev), true
				}
				return 0, false
			}
			if d == filepath.Dir(d) {
				return 0, false
			}
		}
	}
	da, oka := dev(a)
	db, okb := dev(b)
	return oka && okb && da == db
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
func snapshotStopped(cwd string, p snapshotPaths, id, rel string) int {
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
	_, restore, err := stopSetFor(cwd, set, true, id)
	if err == nil {
		err = copyMirror(ctx, p, d, "devopsy-snapshot-"+composeName(cwd)+"-stopped")
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(p.mirror, remote.SnapshotComplete), []byte(id+"\n"), 0o600)
	}
	if rerr := restore(); rerr != nil {
		_ = os.Remove(filepath.Join(p.mirror, remote.SnapshotComplete))
		if err != nil {
			report.Error(err.Error())
		}
		return fail(rerr.Error() + ": no snapshot taken")
	}
	if err != nil {
		_ = os.Remove(filepath.Join(p.mirror, remote.SnapshotComplete))
		if ctx.Err() != nil {
			return fail("interrupted: no snapshot taken, the services started again")
		}
		return fail(err.Error() + ": no snapshot taken")
	}
	// The mirror holds the snapshot: from here on nothing interrupts.
	stopSig()
	signal.Ignore(syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)
	tmp := filepath.Join(p.snaps, id+".tmp")
	if err := writeSnapshotDir(cwd, p, d, tmp, id, rel); err != nil {
		return fail(err.Error())
	}
	return 0
}

// writeSnapshotDir makes the snapshot's directory tmp but its tars.
func writeSnapshotDir(cwd string, p snapshotPaths, d *snapshotData, tmp, id, rel string) error {
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
	fmt.Fprintf(&m, "format\t%s\nkind\tmanual\nrelease\t%s\ntaken\t%s\n", remote.SnapshotFormat, filepath.Base(rel), time.Now().UTC().Format(time.RFC3339))
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
	if err := os.WriteFile(filepath.Join(tmp, remote.SnapshotManifest), []byte(m.String()), 0o600); err != nil {
		return err
	}
	unlock, err := imagesShared(context.Background())
	if err != nil {
		return err
	}
	defer unlock()
	if err := set.BookmarkFrom(tmp, rel); err != nil {
		return fmt.Errorf("bookmarking the release's images: %w", err)
	}
	return nil
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
	format, kind, release, taken string
	entries, files               []string
	volumes                      map[string]string
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
		}
	}
	return m, sc.Err()
}

// snapshotTar makes a snapshot's tars from the mirror, one .tar.zst per
// entry and volume, under nice, as root in the util image (the mirror is
// root's) writing to stdout, which devopsy writes as the deploy user's,
// then renames <id>.tmp into place: only then is it a snapshot.
func snapshotTar(p snapshotPaths, id string) error {
	tmp := filepath.Join(p.snaps, id+".tmp")
	m, err := readManifest(tmp)
	if err != nil {
		return fmt.Errorf("snapshot %s's manifest: %w", id, err)
	}
	ctx := context.Background()
	if err := ensureUtilImage(ctx); err != nil {
		return err
	}
	tar := func(dir, name, file string) error {
		out, err := os.OpenFile(filepath.Join(tmp, remote.SnapshotData, file), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		defer out.Close()
		err = dockerRunIO(ctx, "devopsy-snapshot-"+id+"-tar", nil, out, "--network", "none", "-v", p.mirror+":/m:ro", remote.UtilImage,
			"sh", "-c", `set -o pipefail; cd "/m/$1" && tar -cf - --numeric-owner -- "$2" | nice zstd -q -T0`, "sh", dir, name)
		if err != nil {
			return fmt.Errorf("the tar of %s/%s: %w", dir, name, err)
		}
		return out.Close()
	}
	for _, e := range m.entries {
		report.Info("snapshot " + id + ": mnt/" + e)
		if err := tar("mnt", e, "mnt."+e+".tar.zst"); err != nil {
			return err
		}
	}
	for _, k := range slices.Sorted(maps.Keys(m.volumes)) {
		report.Info("snapshot " + id + ": the volume " + k)
		if err := tar("volumes", k, "volume."+k+".tar.zst"); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp, filepath.Join(p.snaps, id)); err != nil {
		return err
	}
	report.Info("snapshot " + id + " taken")
	return nil
}

// snapshotLeftovers is what a snapshot lock's holder does first: a take
// cut after its stopped pass (the mirror's marker names it) is finished
// from the mirror, any other .tmp removed with its bookmarks.
func snapshotLeftovers(cwd string, p snapshotPaths) error {
	tmps, _ := filepath.Glob(filepath.Join(p.snaps, "*.tmp"))
	if len(tmps) == 0 {
		return nil
	}
	marker, _ := os.ReadFile(filepath.Join(p.mirror, remote.SnapshotComplete))
	for _, tmp := range tmps {
		id := strings.TrimSuffix(filepath.Base(tmp), ".tmp")
		if fileExists(filepath.Join(tmp, remote.SnapshotManifest)) && strings.TrimSpace(string(marker)) == id {
			report.Info("finishing snapshot " + id + ", cut off after its stopped pass")
			if err := snapshotTar(p, id); err != nil {
				return err
			}
			continue
		}
		report.Info("removing snapshot " + id + ", cut off before it was complete")
		if err := removeSnapshotDir(cwd, p, tmp); err != nil {
			report.Warn(err.Error())
		}
	}
	return nil
}

// removeSnapshotDir removes a snapshot's directory, its bookmarks first,
// under the exclusive image lock when it is free (else it stays, for the
// next holder of the snapshot lock).
func removeSnapshotDir(cwd string, p snapshotPaths, dir string) error {
	unlock, ok, err := imagesExclusiveNow()
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("the server's images are in use: " + filepath.Base(dir) + " stays until the next snapshot")
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
		return errors.New("the restore of snapshot " + id + " did not complete: it stays until a restore completes (devopsy --snapshots restore " + id + " again)")
	}
	for start := time.Now(); ; time.Sleep(250 * time.Millisecond) {
		unlock, ok, err := imagesExclusiveNow()
		if err != nil {
			return err
		}
		if ok {
			unlock()
			break
		}
		if time.Since(start) > 10*time.Minute {
			return errors.New("the server's images are still in use after 10 minutes: snapshot " + id + " stays")
		}
	}
	report.Info("removing snapshot " + id)
	return removeSnapshotDir(cwd, p, dir)
}

// snapshotList prints p's snapshots for SnapshotsScript, newest first:
// id, kind, release, taken, size in bytes, state.
func snapshotList(p snapshotPaths) {
	ents, _ := os.ReadDir(p.snaps)
	busy := false
	if f, err := os.OpenFile(filepath.Join(p.base, remote.SnapshotLockFile), os.O_RDONLY, 0); err == nil {
		busy = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) != nil
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
		lines = append(lines, strings.Join([]string{id, m.kind, m.release, m.taken, strconv.FormatInt(bytes, 10), state}, "\t"))
	}
	sort.Sort(sort.Reverse(sort.StringSlice(lines)))
	for _, l := range lines {
		fmt.Println(l)
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

// pruneSnapshots removes the oldest manual snapshots past the take's
// keep (remote.SnapshotKeepEnv), at most the server's snapshots:
// max_keep.
func pruneSnapshots(cwd string, p snapshotPaths) {
	keep, _ := strconv.Atoi(os.Getenv(remote.SnapshotKeepEnv))
	if keep < 1 {
		keep = remote.DefaultManualSnapshots.Keep
	}
	if r, err := remote.ReleaseSettings(); err == nil && keep > r.Snapshots.MaxKeep {
		report.Warn(fmt.Sprintf("keeping %d snapshots, this server's snapshots: max_keep, not %d", r.Snapshots.MaxKeep, keep))
		keep = r.Snapshots.MaxKeep
	}
	ids := manualSnapshots(p)
	for len(ids) > keep {
		report.Info("removing snapshot " + ids[0] + " (keep " + strconv.Itoa(keep) + ")")
		if err := removeSnapshotDir(cwd, p, filepath.Join(p.snaps, ids[0])); err != nil {
			report.Warn(err.Error())
			return
		}
		ids = ids[1:]
	}
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
	usage := "usage: devopsy @" + targetRef(t) + " --snapshots [remove <id>... [--yes]]"
	yes := false
	var words []string
	for _, a := range args {
		switch a {
		case "--help", "-h":
			fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp["--snapshots"]))
			return 0
		case "--yes", "-y":
			yes = true
		default:
			if strings.HasPrefix(a, "-") {
				report.Error(usage)
				return 2
			}
			words = append(words, a)
		}
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
	type snap struct{ id, kind, release, taken, size, state string }
	var snaps []snap
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
		snaps = append(snaps, snap{f[0], f[1], f[2], taken, size(n), f[5]})
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
