package main

import (
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
	return snapshotPaths{base: base, snaps: snaps, mirror: filepath.Join(base, "snapshots", remote.SnapshotMirror)}
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
		code, err := remote.SSHWith(t, remote.Bounded, remote.SnapshotExportScript(t, words[1]), bytes.NewReader(nil), os.Stdout, false, nil)
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
		switch name := f.Name(); {
		case f.IsDir(), name == remote.SnapshotManifest, name == images.ListFile, name == remote.PendingFile:
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
		default:
			if err := copyFile(filepath.Join(snap, name), filepath.Join(rel, name)); err != nil {
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
		r, wait, err := unzstd(ctx, snap, t.file, "devopsy-snapshot-"+filepath.Base(snap)+"-check")
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
		}
		if err != nil {
			return fmt.Errorf("snapshot %s's %s: %v: nothing restored", filepath.Base(snap), t.file, err)
		}
	}
	data, _ := os.ReadFile(filepath.Join(snap, remote.SnapshotData, "images"))
	d := images.CLI{}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 5 || f[0] != "image" {
			continue
		}
		if _, err := d.Inspect(f[3]); err == nil {
			continue
		}
		repo := f[2]
		if i := strings.LastIndexAny(repo, ":@"); i > strings.LastIndex(repo, "/") {
			repo = repo[:i]
		}
		pulled := false
		for _, dg := range strings.Fields(f[4]) {
			if _, digest, ok := strings.Cut(dg, "@"); ok && d.Pull(repo+"@"+digest) == nil {
				pulled = true
				break
			}
		}
		if !pulled {
			return fmt.Errorf("the image %s's data ran when snapshot %s was taken (%s) is gone and could not be pulled by its digest: nothing restored", f[1], filepath.Base(snap), f[2])
		}
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
	report.Info("imported snapshot " + old + " as " + id + " (taken " + m.taken + " in release " + m.release + "): devopsy --snapshots restore " + id)
	return nil
}

// snapshotCheckFit refuses a --no-code restore whose snapshot the current
// release's labels do not fit, naming each mismatch (devopsy would have
// to guess which service to stop, hold on the snapshot's image and give
// the files to): a data service of the snapshot gone or without data
// now, an entry or a volume it held that no service of that name mounts
// now, or one another service with data mounts now; and on a marked
// environment one that leaves a service with data of the current labels
// out (the run step would start it on its half-written data).
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
	covered := map[string]bool{}
	for _, ms := range m.services {
		cur, ok := cli.SyncFind(services, ms.name)
		if !ok || !cur.HasData {
			bad = append(bad, "the snapshot has data for "+ms.name+", which the current release has no service with data of")
			continue
		}
		covered[ms.name] = true
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
		for _, s := range services {
			if s.HasData && !covered[s.Name] {
				bad = append(bad, "the environment is marked, and the snapshot has no data for "+s.Name+", which would run on what is there")
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
	// The restored code's volumes, for what makes one missing.
	_, restored, _ := cli.SyncServices(filepath.Join(rel, remote.CodeDir), os.Environ())
	var names []string
	for _, s := range services {
		if s.HasData {
			names = append(names, s.Name)
		}
	}
	set := cli.WithDependents(services, names, "restore")
	stopped, restore, err := stopSet(cwd, set, true)
	if err != nil {
		if rerr := restore(); rerr != nil {
			report.Error(rerr.Error())
		}
		return fail(err.Error() + ": nothing restored")
	}
	if err := markIncomplete(p.base, id, remote.IncompleteRestore, "the restore of snapshot "+id+" did not complete", false); err != nil {
		if rerr := restore(); rerr != nil {
			report.Error(rerr.Error())
		}
		return fail(err.Error() + ": nothing restored")
	}
	// From the mark on, the restore rule: whatever fails keeps the set
	// down under the mark, its ways out a restore again or another.
	cname := fmt.Sprintf("%s%s-%d", remote.ImportContainerPrefix, composeName(cwd), os.Getpid())
	for i, t := range snapshotTars(m) {
		e := tarEntry{name: t.name, forms: []string{t.name}, file: t.isFile}
		if t.volume != "" {
			v := cli.Volume{Key: t.name, Name: t.volume}
			for _, s := range restored {
				if j := slices.IndexFunc(s.Volumes, func(x cli.Volume) bool { return x.Key == t.name }); j >= 0 {
					v = s.Volumes[j]
				}
			}
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
			return fail(err.Error() + ": the services stay down: restore it again (devopsy --snapshots restore " + id + ")")
		}
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
	} else if keys := envKeysDiffer(filepath.Join(snap, remote.SnapshotData, "shared.env"), filepath.Join(p.base, "shared", ".env")); len(keys) > 0 {
		report.Warn("the snapshot's shared/.env differs from the environment's in "+strings.Join(keys, ", ")+": kept as it is", "devopsy --snapshots restore "+id+" --with-env brings it back too")
	}
	_ = os.Remove(filepath.Join(p.base, remote.SyncIncompleteFile))
	rec, _ := readSyncRecord(cwd)
	rec.stops = slices.DeleteFunc(rec.stops, func(s string) bool { return slices.Contains(stopped, s) })
	if !rec.first && len(rec.stops) == 0 && len(rec.own) == 0 && len(rec.snaps) == 0 && len(rec.writers) == 0 {
		_ = os.Remove(filepath.Join(filepath.Dir(cwd), syncState))
	} else if err := rec.write(cwd); err != nil {
		return fail(err.Error())
	}
	report.Info("snapshot " + id + "'s data is in")
	return 0
}

// envKeysDiffer are the keys whose values differ between two .env files,
// sorted (never the values).
func envKeysDiffer(a, b string) []string {
	read := func(p string) map[string]string {
		m := map[string]string{}
		data, err := os.ReadFile(p)
		if err != nil {
			return m
		}
		for _, line := range strings.Split(string(data), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(strings.TrimSpace(k), "#") {
				m[strings.TrimSpace(k)] = v
			}
		}
		return m
	}
	ma, mb := read(a), read(b)
	var keys []string
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
	return keys
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
	var out bytes.Buffer
	code, err := remote.SSH(t, remote.SnapshotsScript(t, projectName), bytes.NewReader(nil), &out, false)
	if err != nil || code != 0 {
		if err != nil {
			report.Fail(err)
		}
		return max(code, 1)
	}
	var ids, match []string
	for _, line := range strings.Split(out.String(), "\n") {
		if f := strings.Split(line, "\t"); len(f) >= 6 && f[5] == "" {
			ids = append(ids, f[0])
		}
	}
	for _, id := range ids {
		if id == words[1] {
			match = []string{id}
			break
		}
		if strings.HasPrefix(id, words[1]) {
			match = append(match, id)
		}
	}
	switch {
	case len(match) == 0:
		report.Fail(&report.Failure{Msg: fmt.Sprintf("%s has no snapshot %s: nothing restored", t.Address, words[1]), Details: []string{"its snapshots: " + cmp.Or(strings.Join(ids, " "), "none")}})
		return 1
	case len(match) > 1:
		report.Fail(&report.Failure{Msg: fmt.Sprintf("%s starts several snapshots (%s): nothing restored", words[1], strings.Join(match, " "))})
		return 1
	}
	id := match[0]
	what := "snapshot " + id + "'s data, code and the images that ran on them"
	if noCode {
		what = "snapshot " + id + "'s data and the images that ran on it, under its current release"
	}
	if withEnv {
		what += ", and its shared/.env"
	}
	if !yes {
		if err := typeToGoOn(fmt.Sprintf("this replaces @%s's data with %s, as a new release (%s:%s): what it holds now goes; devopsy @%s --snapshot first keeps it", t.Address, what, t.Host, t.Path, targetRef(t)), confirmWord(t), "nothing restored", false); err != nil {
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
