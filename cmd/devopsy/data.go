package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// --export and --import: one service's data, the way its devopsy.data
// copy label says (custom through the data capability's export and
// import, volumes as a tar of its entries of mnt), locally or in a target's current
// release (ExportScript, ImportScript run the same code there), with what
// devopsy.data.stop says stopped meanwhile (cli.DataStops).

// dataService finds name among services, with data a copy carries: a
// service without any, or whose copy is none, has nothing to move.
func dataService(services []cli.SyncService, name string) (cli.SyncService, error) {
	s, ok := cli.SyncFind(services, name)
	switch {
	case !ok:
		var names []string
		for _, s := range services {
			if s.Copies() {
				names = append(names, s.Name)
			}
		}
		msg := "no service " + name + " in the project"
		if len(names) > 0 {
			msg += " (with data a copy carries: " + strings.Join(names, ", ") + ")"
		}
		return s, errors.New(msg)
	case !s.HasData:
		return s, fmt.Errorf("%s has no data: label it %s=true (its entries of mnt are copied; %s=%s for the data capability's scripts)", name, cli.DataLabel, cli.DataCopyLabel, cli.CopyCustom)
	case !s.Copies():
		return s, fmt.Errorf("%s has data no copy carries (%s=%s): %s and %s leave it out", name, cli.DataCopyLabel, cli.CopyNone, cli.ExportFlag, cli.ImportFlag)
	}
	return s, nil
}

// gateBeforeStop is the start gate (checkStartBinds) of the code at judged
// for what of set runs at cwd, before a flow stops it to start it again:
// nothing when none of it runs, which it neither stops nor starts
// (review: a read of the data was refused for a start that never came).
func gateBeforeStop(cwd, judged string, set []cli.Stopped) error {
	up, err := upServices(cwd)
	if err != nil || len(intersect(cli.StoppedNames(set), up)) == 0 {
		return nil
	}
	return checkStartBinds(judged)
}

// stopSet stops what of set runs, in one docker compose stop, so Compose
// orders it (dependents first), saying what and why, and returns what it
// stopped and what brings those back (started, then waited for until
// healthy). With record (on a target), each gets a stop line in the
// record (syncState) before anything stops, and the restore puts the
// record back as it was: a devopsy killed after the stop leaves the
// record, which the next complete import, --resolved or sync restores.
func stopSet(cwd string, set []cli.Stopped, record bool) (stopped []string, restore func() error, err error) {
	return stopSetFor(cwd, set, record, "")
}

// stopSetFor is stopSet, recording each as a snapshot's line (snapshot
// <id> <name>) with snapshot: what a take killed outright leaves for the
// next settle to start (settleSnapshots), where a stop line would be kept
// down.
func stopSetFor(cwd string, set []cli.Stopped, record bool, snapshot string) (stopped []string, restore func() error, err error) {
	restore = func() error { return nil }
	if len(set) == 0 {
		return nil, restore, nil
	}
	up, err := upServices(cwd)
	if err != nil {
		return nil, restore, err
	}
	stopped = intersect(cli.StoppedNames(set), up)
	if len(stopped) == 0 {
		return nil, restore, nil
	}
	prior, hadRecord := readSyncRecord(cwd)
	lines := func(rec *syncRecord) *[]string {
		if snapshot != "" {
			rec.snapID = snapshot
			return &rec.snaps
		}
		return &rec.stops
	}
	if record {
		rec := prior
		rec.stops, rec.snaps = slices.Clone(prior.stops), slices.Clone(prior.snaps)
		l := lines(&rec)
		for _, name := range stopped {
			if !slices.Contains(*l, name) {
				*l = append(*l, name)
			}
		}
		if err := rec.write(cwd); err != nil {
			return nil, restore, err
		}
	}
	// A stop that fails may have stopped them all the same (Ctrl-C
	// reaching compose while the daemon carries it out): restored either
	// way.
	restore = func() error {
		report.Info("starting " + strings.Join(stopped, " "))
		if err := compose(cwd, errOut, append([]string{"start"}, stopped...)...); err != nil {
			return fmt.Errorf("docker compose start: %w", err)
		}
		if err := waitHealthy(cwd, stopped); err != nil {
			return err
		}
		if record {
			rec, _ := readSyncRecord(cwd)
			before := prior.stops
			if snapshot != "" {
				before = prior.snaps
			}
			l := lines(&rec)
			*l = slices.DeleteFunc(*l, func(s string) bool {
				return slices.Contains(stopped, s) && !slices.Contains(before, s)
			})
			if !hadRecord && !rec.first && len(rec.stops) == 0 && len(rec.snaps) == 0 && len(rec.writers) == 0 {
				_ = os.Remove(filepath.Join(filepath.Dir(cwd), syncState))
				return nil
			}
			return rec.write(cwd)
		}
		return nil
	}
	report.Info(cli.StoppingLine(set, stopped))
	if err := compose(cwd, errOut, append([]string{"stop"}, stopped...)...); err != nil {
		return stopped, restore, fmt.Errorf("docker compose stop: %w", err)
	}
	return stopped, restore, nil
}

// dependentNotices names the running services that depend on name and
// carry no stop label, left running while it is imported.
func dependentNotices(cwd string, services []cli.SyncService, imported []string, set []cli.Stopped) {
	deps := cli.UnlabeledDependents(services, imported, set)
	if len(deps) == 0 {
		return
	}
	up, err := upServices(cwd)
	if err != nil {
		return
	}
	for _, d := range deps {
		if slices.Contains(up, d.Name) {
			report.Warn(cli.DependentNotice(d))
		}
	}
}

// restartAlwaysNotice names the services kept down that Docker starts
// again by itself when its daemon starts (restart: always), on data
// nothing should run on.
func restartAlwaysNotice(services []cli.SyncService, down []string) {
	for _, name := range down {
		if s, ok := cli.SyncFind(services, name); ok && s.Restart == "always" {
			report.Warn(name+" has restart: always: Docker starts it again when its daemon starts (a reboot), on the data as it is", "restart: unless-stopped keeps it down until devopsy starts it")
		}
	}
}

// symlinkEntry is the first of entries of mnt that is a symlink: tar
// carries only the link, and the util image mounts mnt alone, so what
// it points to is out of reach.
func symlinkEntry(mnt string, entries []string) string {
	for _, e := range entries {
		if fi, err := os.Lstat(filepath.Join(mnt, e)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return e
		}
	}
	return ""
}

// dataMnt is the project's mnt, its link resolved (on servers it is
// shared/mnt): what the util image mounts.
func dataMnt(projectDir string) (string, error) {
	mnt := filepath.Join(projectDir, "mnt")
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(mnt)
}

// dockerRunIO is dockerRun with stdin and stdout of its own (stdout nil
// is errOut): docker run args as a container named name, removed when ctx
// ends, since killing the docker client alone leaves its container
// running.
func dockerRunIO(ctx context.Context, name string, stdin io.Reader, stdout io.Writer, args ...string) error {
	full := append([]string{"run", "--rm", "--name", name}, args...)
	trace(append([]string{"docker"}, full...))
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, errOut
	if stdout == nil {
		cmd.Stdout = errOut
	}
	// The client first: one stuck writing to a dead session's full pipe
	// keeps dockerd from finishing the attach, and rm -f waits on it
	// (seen: an export's container "removing" for minutes, the
	// services it stopped down meanwhile).
	cmd.Cancel = func() error {
		err := cmd.Process.Kill()
		rm, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = exec.CommandContext(rm, "docker", "rm", "-f", name).Run()
		return err
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

// runExport implements devopsy --export <service>: its data on stdout,
// everything said on stderr.
func runExport(cwd string, args []string, color bool) int {
	fail := func(msg string) int {
		report.Error(msg)
		return 1
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp[cli.ExportFlag]))
		return 0
	}
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return fail("usage: devopsy [@<target>] " + cli.ExportFlag + " <service>")
	}
	name := args[0]
	if term.IsTerminal(int(os.Stdout.Fd())) {
		return fail(cli.ExportFlag + " writes " + name + "'s data to stdout: redirect it (> " + name + ".data) or pipe it (| devopsy " + cli.ImportFlag + " " + name + ")")
	}
	projectDir, services, err := cli.SyncServices(cwd, os.Environ())
	if err != nil {
		return fail(err.Error())
	}
	s, err := dataService(services, name)
	if err != nil {
		return fail(err.Error())
	}
	// On a target, what a snapshot's take killed outright left stopped
	// starts again first (ExportScript holds the release lock).
	if cli.InRelease(projectDir) {
		if err := settleSnapshots(cwd, filepath.Dir(cwd)); err != nil {
			return fail(err.Error())
		}
	}
	if data, err := os.ReadFile(filepath.Join(filepath.Dir(cwd), remote.SyncIncompleteFile)); err == nil {
		if m := remote.ParseMark(string(data)); m.IsImport() {
			report.Warn("the import of " + m.From + " into this environment did not complete: exporting the data as it is")
		} else if m.IsRestore() {
			report.Warn("the restore of snapshot " + m.From + " into this environment did not complete: exporting the data as it is")
		} else {
			report.Warn("this environment's data is from a sync that did not complete: exporting it anyway")
		}
	}
	ctx, stop := interruptible()
	defer stop()
	// From devopsy @<target> --export: its stdin is a pipe devopsy keeps
	// open, with a heartbeat, so a session gone, even silently, stops
	// the export and restores what it stopped.
	if os.Getenv(remote.SyncWatchEnv) != "" {
		ctx = watchSession(ctx)
	}

	var entries []string
	var vols []cli.Volume
	var mnt string
	switch s.Copy {
	case cli.CopyCustom:
		if !isExecutable(dataHookFile(projectDir, "export")) {
			return fail(fmt.Sprintf("%s is labeled %s=%s: the project needs an executable .devopsy/capabilities/%s/export", name, cli.DataCopyLabel, cli.CopyCustom, cli.DataCapability))
		}
	default:
		if mnt, err = dataMnt(projectDir); err != nil {
			return fail("the project's mnt: " + err.Error())
		}
		for _, d := range s.Dirs {
			if _, err := os.Lstat(filepath.Join(mnt, d)); err != nil {
				report.Info("mnt/" + d + " does not exist: skipped")
				continue
			}
			entries = append(entries, d)
		}
		for _, v := range s.Volumes {
			ok, err := volumeExists(ctx, v.Name)
			if err != nil {
				return fail(err.Error())
			}
			if !ok {
				report.Info("the volume " + v.Key + " does not exist: skipped")
				continue
			}
			vols = append(vols, v)
		}
		if len(entries) == 0 && len(vols) == 0 {
			return fail("nothing to export: " + name + " mounts nothing that exists under .devopsy/mnt, nor a named volume")
		}
		if e := symlinkEntry(mnt, entries); e != "" {
			return fail("mnt/" + e + " is a symlink: a tar would carry only the link: make it a directory, mounting what it points to there, to export it")
		}
		if err := ensureUtilImage(ctx); err != nil {
			return fail(err.Error())
		}
	}
	// What ran on the data goes with it (images.ExportData), for an
	// import to check as a sync checks a parent's.
	var record string
	if s.Copy == cli.CopyVolumes {
		record = exportRecord(cwd, projectDir, name)
	}

	set := cli.DataStops(services, cli.ExportOne, name)
	// On a target what it stops it starts again on the same data: refused
	// before anything stops when that start would be (checkStartBinds).
	if cli.InRelease(projectDir) {
		if err := gateBeforeStop(cwd, cwd, set); err != nil {
			report.Fail(err)
			return 1
		}
	}
	_, restore, err := stopSet(cwd, set, false)
	if err != nil {
		code := fail(err.Error())
		if err := restore(); err != nil {
			fail(err.Error())
		}
		return code
	}
	code := 0
	if s.Copy == cli.CopyCustom {
		report.Info("exporting " + name)
		err = runDataHook(ctx, cwd, projectDir, dataHookEnv(services, cli.StoppedNames(set)), nil, os.Stdout, nil, "export", name)
	} else {
		var what []string
		for _, e := range entries {
			what = append(what, "mnt/"+e)
		}
		for _, v := range vols {
			what = append(what, "the volume "+v.Key)
		}
		report.Info("exporting " + name + ": a tar of " + strings.Join(what, ", "))
		if record != "" {
			err = exportHeader(os.Stdout, record)
		}
		if err == nil {
			// Each entry and volume mounted at /x by its name, so the tar
			// has volumes at its top level by their key, beside the
			// entries (cli.SyncServices refuses a key named like one).
			// Only files, directories and links, as a sync (--no-D) and a
			// snapshot's tars: an import refuses anything else (tarTypes),
			// so a FIFO in the data made an export no import took
			// (review). Hard links as files of their own and holes kept,
			// as a snapshot's tars (an import refuses a hard link to a
			// link, and a database's holes came back as zeros: review).
			var run []string
			for _, e := range entries {
				m, merr := bindMount(filepath.Join(mnt, e), "/x/"+e)
				if merr != nil {
					return fail(merr.Error())
				}
				run = append(run, "--mount", m+",readonly")
			}
			for _, v := range vols {
				run = append(run, "-v", v.Name+":/x/"+v.Key+":ro")
			}
			run = append(append(run, remote.UtilImage, "sh", "-c", `set -o pipefail; cd /x && find "$@" \( -type f -o -type d -o -type l \) -print0 | tar -cf - --numeric-owner --hard-dereference --sparse --null --no-recursion -T -`, "sh"), append(entries, cli.VolumeKeys(vols)...)...)
			err = dockerRunIO(ctx, fmt.Sprintf("devopsy-export-%d", os.Getpid()), nil, os.Stdout, run...)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			err = errors.New("interrupted")
		}
		code = fail("export " + name + ": " + err.Error() + ": what was written is incomplete")
	}
	if err := restore(); err != nil {
		code = fail(err.Error())
	}
	return code
}

// exportRecord is what an mnt export of name records of the images that
// ran on its data: its release's on a target, its container's locally.
func exportRecord(cwd, projectDir, name string) string {
	set := &images.Set{Docker: images.CLI{}, Name: composeName(cwd), Out: errOut, List: images.ListCompose}
	if !cli.InRelease(projectDir) {
		set.List = func(string) ([]images.Service, error) { return localServices(cwd) }
		return set.ExportData("", "", name)
	}
	return set.ExportData(filepath.Dir(cwd), remote.ReleaseFile(filepath.Join(cwd, cli.ProjectDirName), ""), name)
}

// localServices are the local project's services as compose resolves
// them, for ExportData to tell a pulled data service from a built one.
func localServices(cwd string) ([]images.Service, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	cmd := exec.Command(self, "--", "config", "--format", "json")
	cmd.Dir, cmd.Stdout, cmd.Stderr = cwd, &out, errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("compose config: %w", err)
	}
	return images.ParseComposeConfig(out.Bytes())
}

// imagesShared takes the server's image lock shared (remote.ImagesLockEnv),
// as a sync's img sync-check and sync-done do, for an import's check and
// take-over of the export's image: a removal elsewhere, which takes it
// exclusively, then never frees the image between its pull and its
// bookmark. Waits up to 10 minutes, saying so; without the variable (a
// local import) nothing to take.
func imagesShared(ctx context.Context) (unlock func(), err error) {
	path := os.Getenv(remote.ImagesLockEnv)
	if path == "" {
		return func() {}, nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("the server's image lock: %w", err)
	}
	unlock = func() { f.Close() }
	deadline := time.Now().Add(10 * time.Minute)
	for said := false; ; said = true {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if err == nil {
			return unlock, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			unlock()
			return nil, fmt.Errorf("the server's image lock: %w", err)
		}
		if !said {
			report.Info("waiting for the server's images, held by a removal, up to 10 minutes")
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			unlock()
			return nil, errors.New("the server's images are still held after 10 minutes")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// exportHeader writes an export's record as the tar's first header, a
// pax global one (images.ExportKey), which tar itself skips: GNU tar's
// members follow, and the whole stays one archive any tar reads.
func exportHeader(w io.Writer, record string) error {
	tw := tar.NewWriter(w)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{images.ExportKey: record}}); err != nil {
		return err
	}
	return tw.Flush()
}

// tarRecord is the export's record in the tar at spool: its first
// header's, when that is a pax global one; "" otherwise.
func tarRecord(spool string) string {
	f, err := os.Open(spool)
	if err != nil {
		return ""
	}
	defer f.Close()
	h, err := tar.NewReader(f).Next()
	if err != nil || h.Typeflag != tar.TypeXGlobalHeader {
		return ""
	}
	return h.PAXRecords[images.ExportKey]
}

// importSpool is where --import keeps stdin until it is complete: in a
// release, the environment directory's StreamSpool (what a sync uses);
// locally a temporary file under .devopsy/, which releases never upload.
func importSpool(projectDir, name string) (*os.File, error) {
	if cli.InRelease(projectDir) {
		base := filepath.Dir(filepath.Dir(projectDir))
		removeSpools(base, name)
		return os.CreateTemp(base, remote.StreamSpoolPrefix+name+".*")
	}
	return os.CreateTemp(projectDir, remote.StreamSpoolPrefix+name+".*")
}

// removeSpools removes the service's spools in the environment at base:
// one an import or a sync killed outright left (a whole dump), which
// nothing else ever removes. Under the release lock, where none is in
// use.
func removeSpools(base, name string) {
	spools, _ := filepath.Glob(filepath.Join(base, remote.StreamSpoolPrefix+name+".*"))
	for _, f := range spools {
		_ = os.Remove(f)
	}
}

// errNoTarEnd is a tar without its end-of-archive blocks: cut short,
// maybe right after a member.
var errNoTarEnd = errors.New("no end-of-archive blocks: cut short?")

// tarEntry is a top-level entry of mnt an import's tar carries: its
// name, the names the archive gives it (storage, ./storage), which its
// extraction selects its members by (GNU tar matches ./storage/x only
// by ./storage), and whether it is a file.
type tarEntry struct {
	name  string
	forms []string
	file  bool
	// volume is the named volume the entry is, by the service's labels
	// (its key is name): extracted into it, never into mnt.
	volume *cli.Volume
	// links are its symbolic links' names, clean (storage/public), and
	// tar the tar's links, for a walk through them (linkAtBind).
	links []string
	tar   *tarLinks
	// size is its files' bytes, sparse ones whole (a restore's space
	// warning).
	size int64
}

// tarEntries lists the top-level entries of mnt the tar in spool carries,
// sorted: a truncated archive fails here, before anything is emptied, and
// so does one carrying mnt itself (./), whose owner and mode are no
// entry's. Read here, not by tar -t, which takes a tar cut at a member
// boundary for complete, its last member's zeros (a sparse file, a
// database's pages) passing for the end-of-archive blocks. A member's
// name must be a plain path (storage/x, ./storage/x: no ././, //, or
// /./), so that each entry's extraction selects its members by name
// (tarEntry.forms), and an entry is a file or a directory, never both.
func tarEntries(spool string) ([]tarEntry, error) {
	f, err := os.Open(spool)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return tarEntriesIn(f)
}

// tarEntriesIn is tarEntries of the tar f streams (a snapshot's,
// decompressed on the way). A symbolic link may lead anywhere: it is the
// data, an app's absolute links included (review: a snapshot's made
// every restore refuse, and an export's its import); what a bind source
// makes of them is linkAtBind's.
func tarEntriesIn(f io.Reader) ([]tarEntry, error) {
	// archive/tar ends at its end-of-archive blocks and at a plain end
	// of file alike; only the first leaves a read that returned data
	// last.
	r := &endReader{r: f}
	tr := tar.NewReader(r)
	seen := map[string]*tarEntry{}
	dirs := map[string]bool{}
	var links tarLinks
	first := true
	for {
		h, err := tr.Next()
		if err == io.EOF {
			if r.atEOF {
				return nil, errNoTarEnd
			}
			break
		}
		if err != nil {
			return nil, err
		}
		// A pax global header (git archive writes one) is no member, but
		// GNU tar applies its records to every member after it: a path or
		// a linkpath there renames members and retargets links these
		// checks never saw (storage/b -> a extracted as storage/evil ->
		// /etc/shadow, checked in devopsy-util:2). Only the first, and
		// only with what tar applies to nothing: the export's record
		// (images.ExportKey) and git's comment.
		if h.Typeflag == tar.TypeXGlobalHeader {
			if !first {
				return nil, fmt.Errorf("%w: a pax global header after the first member (tar applies it to every member after it)", errMember)
			}
			for k := range h.PAXRecords {
				if k != images.ExportKey && k != "comment" {
					return nil, fmt.Errorf("%w: a pax global header with %s (tar applies it to every member)", errMember, k)
				}
			}
			first = false
			continue
		}
		first = false
		p := strings.TrimPrefix(h.Name, "./")
		if p == "" || p == "." || p == "./" {
			return nil, fmt.Errorf("%w: %s is mnt itself: a tar of its entries carries them alone (tar -C mnt -cf - <entry>...)", errOutside, h.Name)
		}
		top, err := tarMember(h, p)
		if err != nil {
			return nil, err
		}
		if strings.TrimSuffix(p, "/") != path.Clean(p) {
			return nil, fmt.Errorf("%w: %s is no plain path (tar -C mnt -cf - <entry>... writes them)", errMember, h.Name)
		}
		links.add(h, path.Clean(p))
		e := seen[top]
		if e == nil {
			e = &tarEntry{name: top}
			seen[top] = e
		}
		if form := strings.TrimSuffix(h.Name, "/")[:len(h.Name)-len(p)+len(top)]; !slices.Contains(e.forms, form) {
			e.forms = append(e.forms, form)
		}
		if h.Typeflag == tar.TypeSymlink {
			e.links = append(e.links, path.Clean(p))
		}
		if h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeRegA || h.Typeflag == tar.TypeGNUSparse {
			e.size += h.Size
		}
		if strings.TrimSuffix(p, "/") == top && h.Typeflag != tar.TypeDir {
			e.file = true
		} else {
			dirs[top] = true
		}
		if e.file && dirs[top] {
			return nil, fmt.Errorf("%w: mnt/%s is a file and a directory in the tar", errMember, top)
		}
	}
	if err := links.check(); err != nil {
		return nil, err
	}
	var entries []tarEntry
	for _, e := range seen {
		e.tar = &links
		entries = append(entries, *e)
	}
	slices.SortFunc(entries, func(a, b tarEntry) int { return strings.Compare(a.name, b.name) })
	return entries, nil
}

// tarLinks are the members of an import's tar as links see them, for a
// check of the whole tar once it is read (check) and a walk through its
// links (walkMnt): a target judged alone, lexically, is right only while
// no component of the way is a link. storage/x -> . plus storage/w ->
// x/x/../../etc/shadow is storage/etc/shadow to path.Clean, and
// /etc/shadow on the host (review of 2026-10-23, round 4).
type tarLinks struct {
	// names counts the members of each name (clean, without ./).
	names map[string]int
	// symlinks are the symbolic links, name to target; hard the hard
	// links, name to target.
	symlinks, hard map[string]string
	// header is a member's name as the tar writes it, for messages.
	header map[string]string
}

// add records the member h, its name clean.
func (l *tarLinks) add(h *tar.Header, clean string) {
	if l.names == nil {
		l.names, l.symlinks, l.hard, l.header = map[string]int{}, map[string]string{}, map[string]string{}, map[string]string{}
	}
	l.names[clean]++
	switch h.Typeflag {
	case tar.TypeSymlink:
		l.symlinks[clean] = h.Linkname
	case tar.TypeLink:
		l.hard[clean] = path.Clean(strings.TrimPrefix(h.Linkname, "./"))
	default:
		return
	}
	l.header[clean] = h.Name
}

// maxLinks is how many links a walk follows (walkMnt): Linux's own
// limit for one path, so a longer chain is one the kernel would not
// follow either.
const maxLinks = 40

// check refuses the members a link would put elsewhere than their names
// say (errOutside):
//
//   - a member below a symbolic link of the tar (storage/x/f with
//     storage/x a link), and a link with another member of its name: a
//     tar writes neither (tar never descends into a link), and then
//     every member's name is where it lands, so nothing is written
//     through a link;
//   - a hard link whose target is below a link, or is a symbolic link
//     itself: GNU tar links the symbolic link, whose relative target
//     then leads elsewhere from the hard link's directory.
//
// A symbolic link's target is not judged: it is the data, never
// followed by the extraction, which mounts its entry alone, so nothing
// of the host is in the way of a link; what the app makes of its links
// is its own, as a sync copies them (decided after review: an export's
// absolute links, storage/latest -> /app/storage/f, made its import
// refuse). A bind source through one is linkAtBind's.
func (l *tarLinks) check() error {
	under := func(name string) string {
		for i := range len(name) {
			if name[i] != '/' {
				continue
			}
			if _, ok := l.symlinks[name[:i]]; ok {
				return name[:i]
			}
		}
		return ""
	}
	for _, name := range slices.Sorted(maps.Keys(l.names)) {
		if link := under(name); link != "" {
			return fmt.Errorf("%w: %s is below the link %s", errOutside, name, l.header[link])
		}
		if _, ok := l.symlinks[name]; ok && l.names[name] > 1 {
			return fmt.Errorf("%w: %s is a link and another member of the same name", errOutside, l.header[name])
		}
	}
	for _, name := range slices.Sorted(maps.Keys(l.hard)) {
		target := l.hard[name]
		if link := under(target); link != "" {
			return fmt.Errorf("%w: %s is a hard link below the link %s", errOutside, l.header[name], l.header[link])
		}
		if _, ok := l.symlinks[target]; ok {
			return fmt.Errorf("%w: %s is a hard link to the link %s", errOutside, l.header[name], l.header[target])
		}
	}
	return nil
}

// walkMnt follows target from entry as the host resolves it in mnt,
// through the tar's symbolic links, its last component's too, and
// returns where it leads: the way may pass through mnt, a .. above the
// entry, but only back into entry (storage/public -> ../storage/v1),
// never into another entry or above mnt, nor follow more than maxLinks
// links, hops counting them (linkAtBind).
func (l *tarLinks) walkMnt(entry, target string, hops *int) ([]string, error) {
	return l.walkIn([]string{entry}, target, hops, entry)
}

func (l *tarLinks) walkIn(dir []string, target string, hops *int, entry string) ([]string, error) {
	if path.IsAbs(target) {
		return nil, errors.New("is absolute")
	}
	at := slices.Clone(dir)
	for _, c := range strings.Split(target, "/") {
		switch c {
		case "", ".":
			continue
		case "..":
			if entry != "" && len(at) <= 1 {
				if len(at) == 0 {
					return nil, fmt.Errorf("leads out of mnt/%s, above mnt", entry)
				}
				at = at[:0]
				continue
			}
			if len(at) <= 1 {
				return nil, fmt.Errorf("leads out of mnt/%s", at[0])
			}
			at = at[:len(at)-1]
			continue
		}
		if entry != "" && len(at) == 0 && c != entry {
			return nil, fmt.Errorf("leads out of mnt/%s, into mnt/%s", entry, c)
		}
		at = append(at, c)
		link, ok := l.symlinks[strings.Join(at, "/")]
		if !ok {
			continue
		}
		if *hops++; *hops > maxLinks {
			return nil, fmt.Errorf("follows more than %d links", maxLinks)
		}
		var err error
		if at, err = l.walkIn(at[:len(at)-1], link, hops, entry); err != nil {
			return nil, err
		}
	}
	if entry != "" && len(at) == 0 {
		return nil, fmt.Errorf("leads out of mnt/%s, to mnt itself", entry)
	}
	return at, nil
}

// errOutside is a tar whose members reach out of the entries of mnt
// they are in: GNU tar extracts some through to another entry or out of
// mnt, refuses others only after the entries were emptied, and a link
// in place of an entry removes the directory a container bind-mounts.
var errOutside = errors.New("a member reaching out of its entry")

// errMember is a tar member devopsy never extracts, wherever it is: a
// device node, which GNU tar as root makes on the host (bind mounts are
// not nodev) for the owners' step to give to the entry's owner, a block
// device of the server's disk the raw disk for that UID; a FIFO, which
// a tar of data never needs either. Only files, directories and links
// pass (tarTypes).
var errMember = errors.New("a member devopsy does not extract")

// tarTypes are the member types an mnt import extracts.
var tarTypes = []byte{tar.TypeReg, tar.TypeRegA, tar.TypeGNUSparse, tar.TypeCont, tar.TypeDir, tar.TypeSymlink, tar.TypeLink}

// tarMember checks a member (p, its name without ./) stays in its
// top-level entry of mnt, and returns that entry. A path or a hard link
// out of its entry (into another entry too), and a top-level entry that
// is no directory or file, are errOutside; a member of another type than
// tarTypes (a device node, a FIFO) is errMember. A symbolic link's
// target is not judged (tarLinks.check).
func tarMember(h *tar.Header, p string) (top string, err error) {
	outside := func() (string, error) { return "", fmt.Errorf("%w: %s", errOutside, h.Name) }
	if !slices.Contains(tarTypes, h.Typeflag) {
		kind := map[byte]string{tar.TypeBlock: "a block device", tar.TypeChar: "a character device", tar.TypeFifo: "a FIFO"}[h.Typeflag]
		if kind == "" {
			kind = fmt.Sprintf("of type %q", h.Typeflag)
		}
		return "", fmt.Errorf("%w: %s is %s, which a tar of data never needs", errMember, h.Name, kind)
	}
	if path.IsAbs(p) || slices.Contains(strings.Split(p, "/"), "..") {
		return outside()
	}
	clean := path.Clean(p)
	top, _, nested := strings.Cut(clean, "/")
	if !nested && h.Typeflag != tar.TypeDir && h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeGNUSparse {
		return outside()
	}
	if h.Typeflag != tar.TypeLink {
		return top, nil
	}
	if path.IsAbs(h.Linkname) || slices.Contains(strings.Split(h.Linkname, "/"), "..") {
		return outside()
	}
	target := path.Clean(strings.TrimPrefix(h.Linkname, "./"))
	if t, _, _ := strings.Cut(target, "/"); t != top {
		return "", fmt.Errorf("%w: %s links into %s", errOutside, h.Name, t)
	}
	return top, nil
}

// endReader reads r and says whether its last read found the end of
// file with nothing read.
type endReader struct {
	r     io.Reader
	atEOF bool
}

func (e *endReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	e.atEOF = n == 0 && err == io.EOF
	return n, err
}

// emptyAndExtract empties the entry of mnt $2 (a sync's --delete: what
// the source does not have goes) and extracts the members named by the
// rest ($2 as the archive names it: storage, ./storage) of the tar $1, a
// file mounted read-only (decompressed when it is a .zst: a snapshot's),
// as root in the util image so owners and modes come as they were. A
// directory is emptied, never removed, and a file overwritten in place:
// a running container bind-mounts their inodes. It runs in a container
// that mounts that entry alone and the tar (extractEntries). The tar is
// read there, never sent on the container's stdin: Docker's stream ran
// at 8 MB/s on a small DigitalOcean Droplet, a restore's downtime for 1
// GiB 131 s against some 9 for the same tar read in the container
// (measured 2026-10-26).
const emptyAndExtract = `set -eu
set -o pipefail
src=$1 e=$2; shift 2
if [ -d "/mnt/$e" ] && [ ! -L "/mnt/$e" ]; then
  find "/mnt/$e" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
fi
case $src in
  *.zst)
    # What tar leaves unread past its end blocks (a tar of large records)
    # is drained: zstd killed by SIGPIPE would fail the pipe.
    zstd -dcq -- "$src" | { s=0; tar -C /mnt -xf - --numeric-owner --overwrite --no-wildcards -- "$@" || s=$?; cat >/dev/null; exit "$s"; } ;;
  *) exec tar -C /mnt -xf "$src" --numeric-owner --overwrite --no-wildcards -- "$@" ;;
esac
`

// extractEntries empties and extracts each entry of mnt the tar in spool
// carries (emptyAndExtract), in a util container of its own, named
// name-extract-<n>, that mounts that entry alone, without a network, and
// reads the whole spool for that entry's members. Why one per entry: the
// service whose entry it is may run meanwhile (its stop label says, and
// never is the default), and its owner can swap a directory the
// extraction just made for a link to ../<another entry>, which GNU tar
// follows as root; here it leads only into the entry itself or the
// throwaway container, never another entry or the host (review of
// 2026-10-23, round 3: one container mounting all of mnt let the app
// plant files in another entry). A missing entry is made first, an
// empty directory or file as the tar has it, for the container to mount;
// a named volume (project's, by its key) is made when missing as compose
// would, and mounted by name.
func extractEntries(ctx context.Context, mnt, spool string, entries []tarEntry, name, project string) error {
	for i, e := range entries {
		if err := extractEntry(ctx, mnt, spool, e, fmt.Sprintf("%s-extract-%d", name, i), project); err != nil {
			return err
		}
	}
	return nil
}

// extractEntry empties and extracts the entry or volume e from the tar
// file tarFile (plain, or a snapshot's .tar.zst), as extractEntries does
// each, in the util container name, which mounts the file read-only.
func extractEntry(ctx context.Context, mnt, tarFile string, e tarEntry, name, project string) error {
	var mount string
	var err error
	if e.volume != nil {
		// A volume: made when missing, as compose would, and mounted
		// by name.
		if err := ensureVolumes(ctx, project, []cli.Volume{*e.volume}); err != nil {
			return err
		}
		mount = "type=volume,source=" + e.volume.Name + ",target=/mnt/" + e.name
	} else {
		if err := readyEntry(mnt, e); err != nil {
			return err
		}
		if mount, err = bindMount(filepath.Join(mnt, e.name), "/mnt/"+e.name); err != nil {
			return err
		}
	}
	// The tar under its own name: a .zst is decompressed (emptyAndExtract).
	in := "/in/" + filepath.Base(tarFile)
	src, err := bindMount(tarFile, in)
	if err != nil {
		return err
	}
	run := append([]string{"--network", "none", "--mount", mount, "--mount", src + ",readonly", remote.UtilImage, "sh", "-c", emptyAndExtract, "sh", in, e.name}, e.forms...)
	if err := dockerRunFresh(ctx, name, nil, run...); err != nil {
		what := "mnt/" + e.name
		if e.volume != nil {
			what = "the volume " + e.name
		}
		return fmt.Errorf("extracting %s: %w", what, err)
	}
	return nil
}

// readyEntry makes the entry e of mnt when missing, as the tar has it: a
// directory (makeEntry) or an empty file.
func readyEntry(mnt string, e tarEntry) error {
	if !e.file {
		return makeEntry(mnt, e.name)
	}
	f, err := os.OpenFile(filepath.Join(mnt, e.name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return f.Close()
}

// runImport implements devopsy --import <service> [--yes] [--resolved]:
// stdin, spooled to a file until complete, into the service. --yes is
// what a target's session passes after asking: locally nothing asks.
//
// Nothing is replaced until the data has fully arrived. Before the first
// write a failure restores what was stopped; from it (devopsy asks Docker
// to start the writer: the import hook, or the first container that empties
// and extracts), on a target the environment is marked (an import's
// mark, IncompleteImport) and what was stopped is recorded, both before
// the write, and a failure keeps it all down, the imported service and
// what depends on it stopped too, until a complete import of that
// service or --resolved. Locally there is no mark: the error says what
// stays down.
func runImport(cwd string, args []string, color bool) int {
	fail := func(msg string) int {
		report.Error(msg)
		return 1
	}
	if len(args) >= 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp[cli.ImportFlag]))
		return 0
	}
	name, _, resolved, accept, err := importArgs(args)
	if err != nil {
		return fail(err.Error())
	}
	// On a server the operator's --accept-image comes as
	// remote.ImagesAcceptEnv; locally nothing is held, so nothing to
	// accept.
	if len(accept) > 0 {
		return fail(cli.ImportFlag + " --accept-image is for a target, whose data services are held on their data's images: locally nothing is checked")
	}
	if resolved {
		return runResolved(cwd, name)
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return fail(cli.ImportFlag + " reads " + name + "'s data from stdin: devopsy @<target> " + cli.ExportFlag + " " + name + " | devopsy " + cli.ImportFlag + " " + name + ", or < a file")
	}
	projectDir, services, err := cli.SyncServices(cwd, os.Environ())
	if err != nil {
		return fail(err.Error())
	}
	s, err := dataService(services, name)
	if err != nil {
		return fail(err.Error())
	}
	if s.Copy == cli.CopyCustom && !isExecutable(dataHookFile(projectDir, "import")) {
		return fail(fmt.Sprintf("%s is labeled %s=%s: the project needs an executable .devopsy/capabilities/%s/import", name, cli.DataCopyLabel, cli.CopyCustom, cli.DataCapability))
	}
	// What an mnt import extracts goes to its owner.
	owners, err := cli.MntOwners(services)
	if err != nil {
		return fail(err.Error() + ": nothing imported")
	}
	// On a target the import keeps a mark and a record next to the
	// release (none locally: a release in place has nothing to guard).
	onTarget := cli.InRelease(projectDir)
	base := filepath.Dir(cwd)
	var prior syncRecord
	if onTarget {
		// The writer never holds the release lock (fds 9 and 8, from
		// ImportScript): one a killed devopsy left would block every
		// command for its whole load. This devopsy keeps them to its end.
		syscall.CloseOnExec(8)
		syscall.CloseOnExec(9)
		if m := readMark(base); m != nil {
			if m.IsRestore() {
				return fail("the restore of snapshot " + m.From + " did not complete, and only a restore completes it: nothing imported")
			}
			if !m.IsImport() {
				return fail("this environment's data is from a sync that did not complete, and only a sync completes it: nothing imported")
			}
			if m.From != name {
				return fail("the import of " + m.From + " did not complete, and only " + m.From + " can be imported until one does: nothing imported")
			}
			// Holding the lock, no import runs: what a killed one left
			// may still load.
			if err := endLeftoverImport(cwd, base, services, name); err != nil {
				return fail(err.Error() + ": nothing imported")
			}
		}
		prior, _ = readSyncRecord(cwd)
	}
	ctx, stop := interruptible()
	defer stop()

	// stdin first, whole, into the spool: nothing changes before it is
	// all here. From devopsy @<target> --import (its end line set), the
	// data ends with the end line, a pause longer than dataQuiet is the
	// session gone, silently (the release lock is held meanwhile), and
	// stdin stays open after the end line, for the byte the first write
	// waits for.
	f, err := importSpool(projectDir, name)
	if err != nil {
		return fail("the spool for " + name + ": " + err.Error())
	}
	spool := f.Name()
	defer os.Remove(spool)
	end := os.Getenv(remote.ImportEndEnv)
	_ = os.Unsetenv(remote.ImportEndEnv)
	quiet := time.Duration(0)
	if end != "" {
		quiet = dataQuiet()
	}
	in := newChunks(os.Stdin)
	type spooled struct {
		n    int64
		rest []byte
		err  error
	}
	done := make(chan spooled, 1)
	go func() {
		n, rest, err := spoolUntil(in, f, end, quiet)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		done <- spooled{n, rest, err}
	}()
	var c spooled
	select {
	case c = <-done:
	case <-ctx.Done():
		return fail("interrupted while reading stdin: nothing imported")
	}
	if c.err != nil {
		return fail(c.err.Error() + ": nothing imported")
	}
	if c.n == 0 {
		return fail("nothing on stdin: nothing imported")
	}

	var entries []tarEntry
	var names []string
	var mnt string
	if s.Copy == cli.CopyVolumes {
		if entries, err = tarEntries(spool); errors.Is(err, errOutside) || errors.Is(err, errMember) {
			return fail("the tar has " + err.Error() + ": nothing imported")
		} else if err != nil {
			return fail("stdin is not a complete tar (" + err.Error() + "): nothing imported")
		}
		// The image extracts the tar.
		if err := ensureUtilImage(ctx); err != nil {
			return fail(err.Error())
		}
		for i, t := range entries {
			e := t.name
			v := slices.IndexFunc(s.Volumes, func(v cli.Volume) bool { return v.Key == e })
			switch {
			case slices.Contains(s.Excluded, e):
				return fail(fmt.Sprintf("the tar carries %s, which %s's %s leaves out here: nothing imported", e, name, cli.DataExcludeLabel))
			case v >= 0 && t.file:
				return fail(fmt.Sprintf("the tar carries %s as a file, and it is the volume %s here: nothing imported", e, e))
			case v >= 0:
				entries[i].volume = &s.Volumes[v]
			case !slices.Contains(s.Dirs, e):
				return fail(fmt.Sprintf("the tar carries %s, which %s neither bind-mounts from mnt (%s) nor mounts as a volume (%s): nothing imported", e, name, cmp.Or(strings.Join(s.Dirs, ", "), "none"), cmp.Or(strings.Join(cli.VolumeKeys(s.Volumes), ", "), "none")))
			default:
				names = append(names, e)
			}
			// Its links may lead anywhere, but none may make a bind
			// source lead out of its entry: Docker follows it on the host
			// (an import's own check as a restore's, locally too, where
			// no mounts check runs).
			if entries[i].volume == nil {
				if err := linkAtBind(t, services); err != nil {
					return fail("the tar has " + err.Error() + ": nothing imported")
				}
			}
		}
		if mnt, err = dataMnt(projectDir); err != nil {
			return fail("the project's mnt: " + err.Error())
		}
		if e := symlinkEntry(mnt, names); e != "" {
			return fail("mnt/" + e + " is a symlink: the import would not reach what it points to: make it a directory, mounting that there, to import into it: nothing imported")
		}
		for _, e := range entries {
			if e.volume != nil {
				continue
			}
			if fi, err := os.Stat(filepath.Join(mnt, e.name)); err == nil && fi.IsDir() == e.file {
				as, is := "a directory", "a file"
				if e.file {
					as, is = is, as
				}
				return fail(fmt.Sprintf("the tar carries mnt/%s as %s, and it is %s here: nothing imported", e.name, as, is))
			}
		}
	}
	// The images that ran on the data, when its export recorded them,
	// are checked before anything stops, as a sync checks its parent's
	// (images.ImportCheck), and the service is held on the export's once
	// the data is in: on a target, where data services are held. A tar
	// without them (by hand, an older devopsy) starts the history over.
	var imgs *images.Set
	var record string
	rel := remote.ReleaseFile(filepath.Join(cwd, cli.ProjectDirName), "")
	if s.Copy == cli.CopyVolumes && onTarget {
		if record = images.DataFor(tarRecord(spool), name); record != "" {
			imgs = &images.Set{Docker: images.CLI{}, Name: composeName(cwd), Out: errOut, List: images.ListCompose, Address: targetAddress(cwd), Accept: acceptedImages(),
				Left: os.Getenv(remote.ImagesLeftEnv), Mode: os.Getenv(remote.ImagesModeEnv)}
			unlock, err := imagesShared(ctx)
			if err != nil {
				return fail(err.Error() + ": nothing imported")
			}
			err = imgs.ImportCheck(rel, record, name)
			unlock()
			if errors.Is(err, images.ErrRefused) {
				return 1
			} else if err != nil {
				return fail(err.Error() + ": nothing imported")
			}
		}
	}

	set := cli.DataStops(services, cli.ImportOne, name)
	dependentNotices(cwd, services, []string{name}, set)
	// What it stops it starts again: refused before anything stops when a
	// bind source leads out of its entry through a link in what the
	// import keeps (the start gate would refuse that start, the set left
	// down under the mark: review); the entries its tar replaces are no
	// matter.
	if onTarget {
		var carried []string
		for _, e := range entries {
			if e.volume == nil {
				carried = append(carried, e.name)
			}
		}
		if err := cli.CheckLeavingBinds(services, carried, "the import: nothing imported"); err != nil {
			report.Fail(err)
			return 1
		}
	}
	stopped, restore, err := stopSet(cwd, set, onTarget)
	// Up to the first write, a failure brings back what was stopped (and
	// a stream's up undone).
	down := func() error { return nil }
	before := func(code int) int {
		if err := down(); err != nil {
			code = fail(err.Error())
		}
		if err := restore(); err != nil {
			code = fail(err.Error())
		}
		return code
	}
	if err != nil {
		return before(fail(err.Error()))
	}
	// A stream not stopped is up for its import, never its dependencies
	// (one the set stopped, or a failed import left down), and down again
	// after it if it was down; one a failed import kept down (in the
	// record) stays up once its import completes. Whether it ran before
	// says whether a failure records it (one down on purpose stays so).
	wasUp := true
	if s.Copy == cli.CopyCustom && !slices.Contains(cli.StoppedNames(set), name) {
		var was string
		if down, was, err = upFor(ctx, cwd, name, false, true); err != nil {
			return before(fail(err.Error()))
		}
		wasUp = was == "running"
	}
	// The first write, from here: on a target only once the session
	// answers the marker with a byte, so a session gone since the data
	// came imports nothing; then nothing from the session interrupts.
	if end != "" {
		// A marker for the operator's devopsy, never a message.
		_, _ = io.WriteString(errOut, remote.ImportMarker+"\n")
		if !awaitByte(ctx, in, c.rest, quiet) {
			return before(fail("the session ended before the import started: nothing imported"))
		}
	}
	if ctx.Err() != nil {
		return before(fail("interrupted: nothing imported"))
	}
	if onTarget {
		signal.Ignore(syscall.SIGPIPE, syscall.SIGHUP, syscall.SIGINT)
		errOut.keep(base)
		report.Info("from here the import of " + name + " goes on whatever happens to the session (its output also goes to " + filepath.Join(base, syncLog) + ")")
		if err := markIncomplete(base, name, remote.IncompleteImport, "it failed or was cut after its first write: its data may be partial", false); err != nil {
			return before(fail(err.Error()))
		}
	}
	in2, err := os.Open(spool)
	if err == nil {
		defer in2.Close()
		if s.Copy == cli.CopyCustom {
			report.Info("importing " + name)
			// On a target its group is recorded before it runs, so
			// the next lock-taker can end it (endLeftoverImport).
			var started func(pid int) error
			if onTarget {
				started = func(pid int) error { return recordWriter(cwd, pid, true, !wasUp) }
			}
			err = runDataHook(ctx, cwd, projectDir, dataHookEnv(services, cli.StoppedNames(set)), in2, nil, started, "import", name)
			if onTarget {
				_ = recordWriter(cwd, 0, false, false)
			}
		} else {
			report.Info("importing " + name + ": mnt/" + strings.Join(names, ", mnt/") + " replaced")
			cname := fmt.Sprintf("%s%s-%d", remote.ImportContainerPrefix, composeName(cwd), os.Getpid())
			err = extractEntries(ctx, mnt, spool, entries, cname, composeName(cwd))
			// What it extracted goes to its owner before the import counts
			// as complete: a tar made anywhere carries its maker's owners.
			// Not locally on macOS, as localMnt: Docker Desktop's file
			// sharing may refuse the chown, and both it and OrbStack let
			// any container user write the Mac's files anyway.
			owned := map[string]string{}
			for _, e := range names {
				if o := owners[e]; o != "" && (onTarget || runtime.GOOS != "darwin") {
					owned[e] = o
				}
			}
			if err == nil {
				err = ownMnt(ctx, mnt, owned, cname+"-owners")
			}
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			err = errors.New("interrupted")
		}
		return importFailed(cwd, base, name, services, stopped, onTarget, wasUp, err)
	}
	code := 0
	if !onTarget {
		if err := down(); err != nil {
			code = fail(err.Error())
		}
		if err := restore(); err != nil {
			code = fail(err.Error())
		}
		return code
	}
	// Complete: what the record holds comes back (a retry's too, kept
	// down by an earlier failure), and only once all of it is healthy do
	// the mark and the record go. A service in the record that was down
	// before stays up: the earlier import stopped it.
	if !slices.Contains(prior.stops, name) {
		if err := down(); err != nil {
			code = fail(err.Error())
		}
	}
	// Held on the export's image before anything starts on the data:
	// started as it was, the service would run the image it had on data
	// another wrote.
	if imgs != nil {
		unlock, err := imagesShared(context.Background())
		if err == nil {
			err = imgs.ImportDone(base, rel, record)
			unlock()
		}
		if err != nil {
			return fail(err.Error() + ": the import completed, and " + name + " is not held on the image that wrote its data: what it stopped stays down, and the mark until an import of " + name + " completes")
		}
	}
	// Running through the import (devopsy.data.stop never): made again on
	// the image that wrote its data before what it stopped starts again
	// on it (an app, on the old image's restart).
	if imgs != nil && !slices.Contains(stopped, name) {
		if err := recreateRunning(cwd, rel, name); err != nil {
			return fail(err.Error() + ": the import completed, and " + name + " still runs the image it had: the mark stays until an import of " + name + " completes")
		}
	}
	if err := restoreServices(cwd, services, false, imgs != nil, nil); err != nil {
		return fail(err.Error() + ": the import completed, and what it stopped did not all come back: the mark stays until it does (devopsy @" + targetAddress(cwd) + " --import " + name + " --resolved starts them)")
	}
	if imgs == nil {
		importedImages(cwd, base, name, services, time.Time{})
	}
	clearImportMark(base, name)
	return code
}

// acceptedImages are the services the operator's --accept-image named
// (remote.ImagesAcceptEnv).
func acceptedImages() map[string]bool {
	m := map[string]bool{}
	for _, svc := range strings.Fields(os.Getenv(remote.ImagesAcceptEnv)) {
		m[svc] = true
	}
	return m
}

// recreateRunning makes name's container again when it runs and its
// activation gave it another image than rel's ComposeFile now holds it
// on (images.Recreate).
func recreateRunning(cwd, rel, name string) error {
	if !slices.Contains(images.Recreate(images.CLI{}, composeName(cwd), rel), name) {
		return nil
	}
	running, err := servicesIn(cwd, "running")
	if err != nil || !slices.Contains(running, name) {
		return err
	}
	report.Info("recreating " + name + " on the image its data now needs")
	if err := compose(cwd, errOut, "up", "-d", "--no-deps", "--wait", name); err != nil {
		return fmt.Errorf("docker compose up: %w", err)
	}
	return nil
}

// importedImages starts the history of the images that ran on a pulled
// data service's data over (images.Set.Imported) once its files were
// imported (mnt), or accepted (--resolved): they came from elsewhere. A
// stream's import goes through the service's own image, which keeps its
// history.
func importedImages(cwd, base, name string, services []cli.SyncService, resolvedSince time.Time) {
	for _, s := range services {
		if s.Name != name || s.Copy != cli.CopyVolumes {
			continue
		}
		set := &images.Set{Docker: images.CLI{}, Name: composeName(cwd), Out: errOut}
		rel := remote.ReleaseFile(filepath.Join(cwd, cli.ProjectDirName), "")
		record := set.Imported
		if !resolvedSince.IsZero() {
			record = func(base, rel, name string) error { return set.Resolved(base, rel, name, resolvedSince) }
		}
		if err := record(base, rel, name); err != nil {
			report.Warn("could not record the image that runs " + name + "'s imported data: " + err.Error())
		}
	}
}

// importFailed is a failure from the first write: whatever may still
// write ends (endImport), and what was stopped stays down, the imported
// service and what depends on it stopped too; on a target recorded and
// marked, with the ways out.
func importFailed(cwd, base, name string, services []cli.SyncService, stopped []string, onTarget, wasUp bool, cause error) int {
	report.Error("import " + name + ": " + cause.Error())
	more, err := endImport(cwd, name, services, onTarget, wasUp)
	if err != nil {
		report.Error(err.Error())
	}
	down := slices.Clone(stopped)
	for _, s := range more {
		if !slices.Contains(down, s) && (s != name || wasUp) {
			down = append(down, s)
		}
	}
	if onTarget {
		if rec, ok := readSyncRecord(cwd); ok {
			down = rec.stops
		}
	}
	restartAlwaysNotice(services, down)
	what := "nothing was stopped"
	if len(down) > 0 {
		what = strings.Join(down, ", ") + " stay down"
	}
	if !onTarget {
		fixes := []report.Remedy{report.Fix("import it again, from a complete file:", "devopsy "+cli.ImportFlag+" "+name+" < <file>")}
		if len(down) > 0 {
			fixes = append(fixes, report.Fix("or start them on the data as it is:", "devopsy start "+strings.Join(down, " ")))
		}
		report.Fail(&report.Failure{Msg: "the import of " + name + " did not complete: its data may be partial, and " + what, Fixes: fixes})
		return 1
	}
	report.Error("the import of "+name+" did not complete: its data may be partial, and "+what+": nothing runs on it until an import of "+name+" completes", "devopsy @"+targetAddress(cwd)+" --status shows it, and the ways out")
	return 1
}

// clearImportMark removes the environment's mark at base while it is
// still the import of name's: never a sync's that replaced it.
func clearImportMark(base, name string) {
	if m := readMark(base); m.IsImport() && m.From == name {
		_ = os.Remove(filepath.Join(base, remote.SyncIncompleteFile))
	}
}

// readMark is the environment's mark at base, nil without one.
func readMark(base string) *remote.Incomplete {
	data, err := os.ReadFile(filepath.Join(base, remote.SyncIncompleteFile))
	if err != nil {
		return nil
	}
	return remote.ParseMark(string(data))
}

// runResolved implements devopsy --import <service> --resolved on a
// server, under the release lock (ResolvedScript): the import of service
// did not complete, and the data as it is now is accepted. It checks the
// mark again, ends a writer a killed devopsy left, starts what the
// record holds and only once all of it is healthy removes the mark and
// the record. Locally there is neither: refused.
func runResolved(cwd, name string) int {
	fail := func(msg string) int {
		report.Error(msg)
		return 1
	}
	projectDir, err := cli.FindProjectDir(cwd)
	if err != nil {
		return fail(err.Error())
	}
	if !cli.InRelease(projectDir) {
		report.Fail(&report.Failure{Msg: "--resolved works on a target only: locally nothing records what a failed import kept down", Fixes: []report.Remedy{report.Fix("start what it kept down (the import's error names it):", "devopsy start <service>...")}})
		return 1
	}
	base := filepath.Dir(cwd)
	m := readMark(base)
	switch {
	case m == nil:
		return fail("no import of " + name + " to resolve: the environment has no mark")
	case m.IsRestore():
		return fail("the restore of snapshot " + m.From + " did not complete: --resolved is for imports; restore it again (--snapshots restore " + m.From + ")")
	case !m.IsImport():
		return fail("this environment's data is from a sync that did not complete, and only a sync completes it: nothing resolved")
	case m.From != name:
		return fail("the import that did not complete is " + m.From + "'s, not " + name + "'s: nothing resolved")
	}
	_, services, err := cli.SyncServices(cwd, os.Environ())
	if err != nil {
		return fail(err.Error())
	}
	signal.Ignore(syscall.SIGPIPE, syscall.SIGHUP)
	if err := endLeftoverImport(cwd, base, services, name); err != nil {
		return fail(err.Error())
	}
	removeSpools(base, name)
	again := report.Fix("fix what does not start, then run it again:", "devopsy @"+targetAddress(cwd)+" "+cli.ImportFlag+" "+name+" --resolved")
	// What an mnt import may have extracted goes to its owners first, as
	// a complete import does: a tar made anywhere carries its maker's
	// owners, and the services would start on them.
	if err := resolvedOwners(cwd, projectDir, services, name); err != nil {
		report.Fail(fmt.Errorf("%w: the mark stays", err), again)
		return 1
	}
	// Made again where devopsy's compose file holds a service on another
	// image than its container's (an import that held it on its export's
	// failed after): started, the old container would run the old image
	// on the imported data.
	if err := restoreServices(cwd, services, false, true, nil); err != nil {
		report.Fail(fmt.Errorf("%w: the mark stays", err), again)
		return 1
	}
	since := time.Time{}
	if m := readMark(base); m != nil {
		since, _ = time.Parse(time.DateTime, m.Since)
	}
	importedImages(cwd, base, name, services, since)
	clearImportMark(base, name)
	report.Info("resolved: " + name + "'s data is accepted as it is, and what the import kept down runs again")
	return 0
}

// resolvedOwners gives the owned entries of name, when it is an mnt
// service, to their owners (ownMnt), in containers named as an import's
// owners' are (importContainers), for --resolved.
func resolvedOwners(cwd, projectDir string, services []cli.SyncService, name string) error {
	s, ok := cli.SyncFind(services, name)
	if !ok || s.Copy != cli.CopyVolumes {
		return nil
	}
	owners, err := cli.MntOwners(services)
	if err != nil {
		return err
	}
	owned := map[string]string{}
	for _, e := range s.Dirs {
		if o := owners[e]; o != "" {
			owned[e] = o
		}
	}
	if len(owned) == 0 {
		return nil
	}
	mnt, err := dataMnt(projectDir)
	if err != nil {
		return err
	}
	return ownMnt(context.Background(), mnt, owned, fmt.Sprintf("%s%s-%d-owners", remote.ImportContainerPrefix, composeName(cwd), os.Getpid()))
}

// chunk is a read of a stream: data, or its error.
type chunk struct {
	b   []byte
	err error
}

// newChunks reads r in the background, for spoolUntil and awaitByte:
// one reader for the data and the byte after it.
func newChunks(r io.Reader) <-chan chunk {
	ch := make(chan chunk, 1)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			ch <- chunk{append([]byte(nil), buf[:n]...), err}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

// spoolUntil writes what in carries to w: to its end, or with end set to
// the end line, found in the stream, which it takes off; rest is what
// came after it. With quiet, a pause that long is the session gone. An
// end of the stream before the end line is a session cut.
func spoolUntil(in <-chan chunk, w io.Writer, end string, quiet time.Duration) (n int64, rest []byte, err error) {
	var timer *time.Timer
	var fired <-chan time.Time
	if quiet > 0 {
		timer = time.NewTimer(quiet)
		defer timer.Stop()
		fired = timer.C
	}
	var window []byte
	write := func(b []byte) error {
		m, err := w.Write(b)
		n += int64(m)
		return err
	}
	for {
		select {
		case c := <-in:
			window = append(window, c.b...)
			if end != "" {
				if i := bytes.Index(window, []byte(end)); i >= 0 {
					return n, window[i+len(end):], write(window[:i])
				}
				// What cannot be the start of the end line goes.
				if keep := len(end) - 1; len(window) > keep {
					if err := write(window[:len(window)-keep]); err != nil {
						return n, nil, err
					}
					window = window[len(window)-keep:]
				}
			}
			if c.err == io.EOF {
				if end != "" {
					return n, nil, errors.New("the data stopped before its end (the session was cut)")
				}
				return n, nil, write(window)
			}
			if c.err != nil {
				return n, nil, fmt.Errorf("reading stdin: %w", c.err)
			}
			if end == "" {
				if err := write(window); err != nil {
					return n, nil, err
				}
				window = window[:0]
			}
			if timer != nil {
				timer.Reset(quiet)
			}
		case <-fired:
			return n, nil, fmt.Errorf("nothing came for %s, the session is gone", quiet)
		}
	}
}

// awaitByte waits for a byte from the session after ImportMarker (rest
// is what came after the end line already), up to quiet: none, the
// session's end or an interrupt is no go.
func awaitByte(ctx context.Context, in <-chan chunk, rest []byte, quiet time.Duration) bool {
	if len(rest) > 0 {
		return true
	}
	timer := time.NewTimer(quiet)
	defer timer.Stop()
	select {
	case c := <-in:
		return len(c.b) > 0
	case <-timer.C:
	case <-ctx.Done():
	}
	return false
}

// composeName is the compose project at cwd: COMPOSE_PROJECT_NAME, which
// devopsy sets on a server, else what devopsy computes there.
func composeName(cwd string) string {
	if n := os.Getenv("COMPOSE_PROJECT_NAME"); n != "" {
		return n
	}
	return cmp.Or(projectVar(cwd, "COMPOSE_PROJECT_NAME"), "local")
}

// targetAddress is the address of the release at cwd, for a hint on a
// server (its devopsy-target.env's DEVOPSY_TARGET), else <target>.
func targetAddress(cwd string) string {
	return cmp.Or(projectVar(cwd, "DEVOPSY_TARGET"), "<target>")
}

// projectVar is key in the environment devopsy computes for the project
// at cwd, "" when unset.
func projectVar(cwd, key string) string {
	if plan, err := cli.Build(cwd, []string{"--", "ps"}, os.Environ()); err == nil {
		for _, kv := range plan.Env {
			if v, ok := strings.CutPrefix(kv, key+"="); ok {
				return v
			}
		}
	}
	return ""
}

// importArgs parses --import's arguments: <service> [--yes] [--resolved].
func importArgs(args []string) (name string, yes, resolved bool, accept []string, err error) {
	usage := errors.New("usage: devopsy [@<target>] " + cli.ImportFlag + " <service> [--accept-image <service>] [--yes] [--resolved]")
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--yes" || a == "-y":
			yes = true
		case a == "--resolved":
			resolved = true
		case a == "--accept-image" && i+1 < len(args):
			i++
			accept = acceptImage(accept, args[i])
		case name == "" && !strings.HasPrefix(a, "-"):
			name = a
		default:
			return "", false, false, nil, usage
		}
	}
	if name == "" || (resolved && len(accept) > 0) {
		return "", false, false, nil, usage
	}
	return name, yes, resolved, accept, nil
}

// runRemoteExport implements devopsy @<target> --export <service>: one
// session running devopsy --export in the current release, its stdout
// through as it is (never colored).
func runRemoteExport(t *remote.Target, projectName string, args []string, color bool) int {
	fail := func(msg string) int {
		report.Error(msg)
		return 1
	}
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return fail("usage: devopsy @" + targetRef(t) + " " + cli.ExportFlag + " <service>")
	}
	if term.IsTerminal(int(os.Stdout.Fd())) {
		return fail(cli.ExportFlag + " writes " + args[0] + "'s data to stdout: redirect it (> " + args[0] + ".data) or pipe it (| devopsy " + cli.ImportFlag + " " + args[0] + ")")
	}
	// stdin is a pipe kept open with a heartbeat: the server stops the
	// export when the session goes, even silently.
	r, w, err := os.Pipe()
	if err != nil {
		return fail(err.Error())
	}
	defer r.Close()
	defer w.Close()
	defer heartbeat(w)()
	code, err := remote.SSHWith(t, remote.Bounded, "export "+remote.SyncWatchEnv+"=1\n"+remote.HeartbeatLine()+remote.ExportScript(t, projectName, args[0]), r, os.Stdout, false, nil)
	if err != nil {
		return fail(err.Error())
	}
	return code
}

// runRemoteImport implements devopsy @<target> --import <service> [--yes]:
// asks for the environment's name on the terminal (stdin is the data),
// then one session running devopsy --import in the current release, with
// stdin through and the end line after it. The server's first write waits
// for a byte (remote.ImportMarker), which devopsy sends unless
// interrupted: a Ctrl-C before it imports nothing, and after it the
// import goes on on the server, said; a second ends the session.
func runRemoteImport(t *remote.Target, projectName string, args []string, color bool) int {
	fail := func(msg string) int {
		report.Error(msg)
		return 1
	}
	name, yes, resolved, accept, err := importArgs(args)
	if err != nil {
		return fail(strings.Replace(err.Error(), "[@<target>]", "@"+targetRef(t), 1))
	}
	if resolved {
		return runRemoteResolved(t, projectName, name, yes)
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return fail(cli.ImportFlag + " reads " + name + "'s data from stdin: devopsy @<target> " + cli.ExportFlag + " " + name + " | devopsy @" + targetRef(t) + " " + cli.ImportFlag + " " + name + ", or < a file")
	}
	if !yes {
		if err := confirmImport(t, name); err != nil {
			report.Fail(err)
			return 1
		}
	}
	// stdin through, then the end line only once it ended cleanly: the
	// server tells a complete stream from a cut session by it.
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return fail(err.Error())
	}
	end := "\n-- devopsy import end " + hex.EncodeToString(nonce) + " --\n"
	// The session opens with the data's first byte: the server takes a
	// pause of remote.DataQuietBeats for a session gone, and a writer
	// may be slow to start (an export waiting for its environment's
	// sync lock, or dumping first). After that, data flows.
	in := bufio.NewReader(os.Stdin)
	_, _ = in.Peek(1)
	r, w, err := os.Pipe()
	if err != nil {
		return fail(err.Error())
	}
	// stdin stays open after the end line, for the byte; closed, the
	// server imports nothing.
	var mu sync.Mutex
	closed, started := false, false
	closeIn := func() {
		if !closed {
			closed = true
			w.Close()
		}
	}
	go func() {
		_, err := io.Copy(w, in)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			closeIn()
		} else if !closed {
			_, _ = io.WriteString(w, end)
		}
	}()
	var errW io.Writer = os.Stderr
	var colored *report.ColorWriter
	if remote.Color {
		colored = report.Colorize(os.Stderr)
		errW = colored
	}
	lm := &remote.LineMarker{Prefix: remote.ImportMarker, Found: func(string) {
		mu.Lock()
		defer mu.Unlock()
		if !closed {
			_, _ = w.Write([]byte{0})
			started = true
		}
	}}
	mw := lm.Writer(errW)
	ctx, kill := context.WithCancel(context.Background())
	defer kill()
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		// Gated: the data goes once the script holds the release lock,
		// so a session cut while it waits leaves an empty stdin there,
		// and nothing stops.
		code, err := remote.SSHStreams(remote.WithGate(remote.WithoutInterrupt(ctx)), t, remote.Bounded, remote.HeartbeatLine()+remote.ImportScript(t, projectName, name, end, strings.Join(accept, " ")), r, nil, mw, false, nil)
		done <- result{code, err}
	}()
	var res result
	interrupted := false
wait:
	for {
		select {
		case res = <-done:
			break wait
		case <-sigs:
			mu.Lock()
			first := !interrupted
			interrupted = true
			if first && started {
				report.Warn("interrupted: the import of "+name+" goes on on @"+t.Address, "devopsy @"+targetRef(t)+" --status shows it; Ctrl-C again ends the session, not the import")
			} else if first {
				report.Error("interrupted: nothing imported into @" + t.Address + ", and what was stopped there comes back")
				closeIn()
				// What the pipe holds already never goes either: a script
				// still waiting for the lock then reads an empty stdin.
				r.Close()
			} else {
				kill()
			}
			mu.Unlock()
		}
	}
	mw.Flush()
	if colored != nil {
		colored.Flush()
	}
	r.Close()
	mu.Lock()
	closeIn()
	wasStarted := started
	mu.Unlock()
	if res.err != nil {
		return fail(res.err.Error())
	}
	switch {
	case wasStarted && ctx.Err() != nil:
		report.Warn("the session is ended: the import of "+name+" goes on on @"+t.Address, "devopsy @"+targetRef(t)+" --status shows it")
		return 130
	case wasStarted && res.code == sshLost:
		report.Error("the connection to @"+t.Address+" was lost during the import: it goes on on the server", "devopsy @"+targetRef(t)+" --status shows it")
		return sshLost
	case interrupted && !wasStarted:
		return 130
	}
	return res.code
}

// runRemoteResolved implements devopsy @<target> --import <service>
// --resolved: a read-only session finds the mark and what the import
// kept down (refusing there: no mark, another service's, a sync's, or
// the import still running), asks for the environment's full name, then
// a session under the release lock checks again and starts them.
func runRemoteResolved(t *remote.Target, projectName, name string, yes bool) int {
	fail := func(msg string) int {
		report.Error(msg)
		return 1
	}
	var out bytes.Buffer
	code, err := remote.SSH(t, remote.ResolvedCheckScript(t), bytes.NewReader(nil), &out, false)
	if err != nil {
		return fail(err.Error())
	}
	if code != 0 {
		return code
	}
	var mark *remote.Incomplete
	var down []string
	importing, settle := false, false
	for _, line := range strings.Split(out.String(), "\n") {
		key, value, _ := strings.Cut(strings.TrimRight(line, "\r"), "\t")
		if k, v, ok := strings.Cut(key, " "); ok && k == "stop" {
			down = append(down, v)
			continue
		}
		switch key {
		case "importing":
			importing = true
		case "settle":
			settle = true
		case "mark":
			mark = remote.ParseMark(value)
		}
	}
	switch {
	case importing:
		return fail("an import into @" + t.Address + " still runs: nothing resolved (devopsy @" + targetRef(t) + " --status shows it)")
	case mark == nil:
		return fail("no import of " + name + " to resolve on @" + t.Address + ": its data has no mark")
	case mark.IsRestore():
		var fixes []report.Remedy
		for _, f := range remote.RestoreFixes(targetRef(t), mark.From, t.Parent != "") {
			fixes = append(fixes, report.Fix(f[0], f[1]))
		}
		report.Fail(&report.Failure{Msg: "the restore of snapshot " + mark.From + " into @" + t.Address + " did not complete: --resolved is for imports: nothing resolved", Fixes: fixes})
		return 1
	case !mark.IsImport() || settle:
		reset := remote.ResetFix(targetRef(t))
		report.Fail(&report.Failure{Msg: "@" + t.Address + "'s data is from a sync that did not complete, and only a sync completes it: nothing resolved", Fixes: []report.Remedy{report.Fix("sync it:", "devopsy @"+targetRef(t)+" --sync"), report.Fix(reset[0], reset[1])}})
		return 1
	case mark.From != name:
		report.Fail(&report.Failure{Msg: "the import that did not complete on @" + t.Address + " is " + mark.From + "'s, not " + name + "'s: nothing resolved", Fixes: []report.Remedy{report.Fix("resolve that one:", "devopsy @"+targetRef(t)+" "+cli.ImportFlag+" "+mark.From+" --resolved")}})
		return 1
	}
	if !yes {
		// What the record holds is what starts: the imported service only
		// when it ran before the import.
		left := "nothing was left stopped"
		others := slices.DeleteFunc(slices.Clone(down), func(s string) bool { return s == name })
		switch {
		case slices.Contains(down, name) && len(others) > 0:
			left = name + " was left stopped with " + joinAnd(others)
		case slices.Contains(down, name):
			left = name + " was left stopped"
		case len(others) == 1:
			left = others[0] + " was left stopped"
		case len(others) > 1:
			left = joinAnd(others) + " were left stopped"
		}
		what := fmt.Sprintf("the import of %s into %s did not complete; %s: this starts them on the data the import left", name, confirmWord(t), left)
		if err := typeToGoOn(what, confirmWord(t), "nothing resolved", false); err != nil {
			report.Fail(err)
			return 1
		}
	}
	code, err = remote.SSH(t, remote.ResolvedScript(t, projectName, name), bytes.NewReader(nil), nil, false)
	if err != nil {
		return fail(err.Error())
	}
	return code
}

// joinAnd joins names with commas and a final "and".
func joinAnd(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// confirmWord is what a prompt that replaces or removes an environment's
// data asks to type: its full name, <project>/[<instance>/]<environment>,
// the same as its directory under the release root.
func confirmWord(t *remote.Target) string {
	return t.Path
}

// confirmImport asks for the environment's name before its data is
// replaced, like --sync, on the terminal itself: stdin carries the data.
// Without a terminal it fails: --yes is the way there.
func confirmImport(t *remote.Target, name string) error {
	where := t.Host + ":" + t.Path
	return typeToGoOn(fmt.Sprintf("this replaces %s's data on @%s (%s) with what stdin carries", name, t.Address, where),
		confirmWord(t), "nothing imported", true)
}
