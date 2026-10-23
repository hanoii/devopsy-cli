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
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// --export and --import: one service's data, the way its devopsy.data
// label says (stream through the data capability's export and import,
// mnt as a tar of its entries of mnt), locally or in a target's current
// release (ExportScript, ImportScript run the same code there), with what
// devopsy.data.stop says stopped meanwhile (cli.DataStops).

// dataService finds name among services, with data: a service without
// any has nothing to move.
func dataService(services []cli.SyncService, name string) (cli.SyncService, error) {
	s, ok := cli.SyncFind(services, name)
	switch {
	case !ok:
		var names []string
		for _, s := range services {
			if s.HasData {
				names = append(names, s.Name)
			}
		}
		msg := "no service " + name + " in the project"
		if len(names) > 0 {
			msg += " (with " + cli.DataLabel + ": " + strings.Join(names, ", ") + ")"
		}
		return s, errors.New(msg)
	case !s.HasData:
		return s, fmt.Errorf("%s has no data: label it %s=%s or %s to say how its data moves", name, cli.DataLabel, cli.DataMnt, cli.DataStream)
	}
	return s, nil
}

// stopSet stops what of set runs, in one docker compose stop, so Compose
// orders it (dependents first), saying what and why, and returns what it
// stopped and what brings those back (started, then waited for until
// healthy). With record (on a target), each gets a stop line in the
// record (syncState) before anything stops, and the restore puts the
// record back as it was: a devopsy killed after the stop leaves the
// record, which the next complete import, --resolved or sync restores.
func stopSet(cwd string, set []cli.Stopped, record bool) (stopped []string, restore func() error, err error) {
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
	if record {
		rec := prior
		rec.stops = slices.Clone(prior.stops)
		for _, name := range stopped {
			if !slices.Contains(rec.stops, name) {
				rec.stops = append(rec.stops, name)
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
			rec.stops = slices.DeleteFunc(rec.stops, func(s string) bool {
				return slices.Contains(stopped, s) && !slices.Contains(prior.stops, s)
			})
			if !hadRecord && !rec.first && len(rec.stops) == 0 && len(rec.writers) == 0 {
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
	if data, err := os.ReadFile(filepath.Join(filepath.Dir(cwd), remote.SyncIncompleteFile)); err == nil {
		if m := remote.ParseMark(string(data)); m.IsImport() {
			report.Warn("the import of " + m.From + " into this environment did not complete: exporting the data as it is")
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
	var mnt string
	switch s.Transfer {
	case cli.DataStream:
		if !isExecutable(dataHookFile(projectDir, "export")) {
			return fail(fmt.Sprintf("%s is labeled %s=%s: the project needs an executable .devopsy/capabilities/%s/export", name, cli.DataLabel, cli.DataStream, cli.DataCapability))
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
		if len(entries) == 0 {
			return fail("nothing to export: " + name + " bind-mounts nothing that exists under .devopsy/mnt")
		}
		if e := symlinkEntry(mnt, entries); e != "" {
			return fail("mnt/" + e + " is a symlink: a tar would carry only the link: make it a directory, mounting what it points to there, to export it")
		}
		if err := ensureUtilImage(ctx); err != nil {
			return fail(err.Error())
		}
	}

	set := cli.DataStops(services, cli.ExportOne, name)
	_, restore, err := stopSet(cwd, set, false)
	if err != nil {
		code := fail(err.Error())
		if err := restore(); err != nil {
			fail(err.Error())
		}
		return code
	}
	code := 0
	if s.Transfer == cli.DataStream {
		report.Info("exporting " + name)
		err = runDataHook(ctx, cwd, projectDir, dataHookEnv(services, cli.StoppedNames(set)), nil, os.Stdout, nil, "export", name)
	} else {
		report.Info("exporting " + name + ": a tar of mnt/" + strings.Join(entries, ", mnt/"))
		run := append([]string{"-v", mnt + ":/mnt:ro", remote.UtilImage, "tar", "-C", "/mnt", "-cf", "-", "--numeric-owner"}, entries...)
		err = dockerRunIO(ctx, fmt.Sprintf("devopsy-export-%d", os.Getpid()), nil, os.Stdout, run...)
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
	// archive/tar ends at its end-of-archive blocks and at a plain end
	// of file alike; only the first leaves a read that returned data
	// last.
	r := &endReader{r: f}
	tr := tar.NewReader(r)
	seen := map[string]*tarEntry{}
	dirs := map[string]bool{}
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
		// A pax global header (git archive writes one) is no member: tar
		// itself skips it.
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
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
		e := seen[top]
		if e == nil {
			e = &tarEntry{name: top}
			seen[top] = e
		}
		if form := strings.TrimSuffix(h.Name, "/")[:len(h.Name)-len(p)+len(top)]; !slices.Contains(e.forms, form) {
			e.forms = append(e.forms, form)
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
	var entries []tarEntry
	for _, e := range seen {
		entries = append(entries, *e)
	}
	slices.SortFunc(entries, func(a, b tarEntry) int { return strings.Compare(a.name, b.name) })
	return entries, nil
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
// top-level entry of mnt, and returns that entry. A path or a link out
// of its entry (into another entry too), and a top-level entry that is
// no directory or file, are errOutside; a member of another type than
// tarTypes (a device node, a FIFO) is errMember.
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
	var target string
	switch h.Typeflag {
	case tar.TypeSymlink:
		if path.IsAbs(h.Linkname) {
			return outside()
		}
		target = path.Clean(path.Join(path.Dir(clean), h.Linkname))
	case tar.TypeLink:
		if path.IsAbs(h.Linkname) || slices.Contains(strings.Split(h.Linkname, "/"), "..") {
			return outside()
		}
		target = path.Clean(strings.TrimPrefix(h.Linkname, "./"))
	default:
		return top, nil
	}
	if target == ".." || strings.HasPrefix(target, "../") {
		return outside()
	}
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

// emptyAndExtract empties the entry of mnt $1 (a sync's --delete: what
// the source does not have goes) and extracts the tar's members named
// by the rest ($1 as the archive names it: storage, ./storage), as root
// in the util image so owners and modes come as they were. A directory
// is emptied, never removed, and a file overwritten in place: a running
// container bind-mounts their inodes. It runs in a container that
// mounts that entry alone (extractEntries).
const emptyAndExtract = `set -eu
e=$1; shift
if [ -d "/mnt/$e" ] && [ ! -L "/mnt/$e" ]; then
  find "/mnt/$e" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
fi
exec tar -C /mnt -xf - --numeric-owner --overwrite --no-wildcards -- "$@"
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
// empty directory or file as the tar has it, for the container to mount.
func extractEntries(ctx context.Context, mnt, spool string, entries []tarEntry, name string) error {
	for i, e := range entries {
		if err := readyEntry(mnt, e); err != nil {
			return err
		}
		mount, err := bindMount(filepath.Join(mnt, e.name), "/mnt/"+e.name)
		if err != nil {
			return err
		}
		in, err := os.Open(spool)
		if err != nil {
			return err
		}
		run := append([]string{"-i", "--network", "none", "--mount", mount, remote.UtilImage, "sh", "-c", emptyAndExtract, "sh", e.name}, e.forms...)
		err = dockerRunIO(ctx, fmt.Sprintf("%s-extract-%d", name, i), in, nil, run...)
		in.Close()
		if err != nil {
			return fmt.Errorf("extracting mnt/%s: %w", e.name, err)
		}
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
	name, _, resolved, err := importArgs(args)
	if err != nil {
		return fail(err.Error())
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
	if s.Transfer == cli.DataStream && !isExecutable(dataHookFile(projectDir, "import")) {
		return fail(fmt.Sprintf("%s is labeled %s=%s: the project needs an executable .devopsy/capabilities/%s/import", name, cli.DataLabel, cli.DataStream, cli.DataCapability))
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
	if s.Transfer == cli.DataMnt {
		if entries, err = tarEntries(spool); errors.Is(err, errOutside) || errors.Is(err, errMember) {
			return fail("the tar has " + err.Error() + ": nothing imported")
		} else if err != nil {
			return fail("stdin is not a complete tar (" + err.Error() + "): nothing imported")
		}
		// The image extracts the tar.
		if err := ensureUtilImage(ctx); err != nil {
			return fail(err.Error())
		}
		for _, t := range entries {
			e := t.name
			names = append(names, e)
			switch {
			case slices.Contains(s.Excluded, e):
				return fail(fmt.Sprintf("the tar carries mnt/%s, which %s's %s leaves out here: nothing imported", e, name, cli.DataExcludeLabel))
			case !slices.Contains(s.Dirs, e):
				return fail(fmt.Sprintf("the tar carries mnt/%s, which %s does not bind-mount (%s): nothing imported", e, name, cmp.Or(strings.Join(s.Dirs, ", "), "none")))
			}
		}
		if mnt, err = dataMnt(projectDir); err != nil {
			return fail("the project's mnt: " + err.Error())
		}
		if e := symlinkEntry(mnt, names); e != "" {
			return fail("mnt/" + e + " is a symlink: the import would not reach what it points to: make it a directory, mounting that there, to import into it: nothing imported")
		}
		for _, e := range entries {
			if fi, err := os.Stat(filepath.Join(mnt, e.name)); err == nil && fi.IsDir() == e.file {
				as, is := "a directory", "a file"
				if e.file {
					as, is = is, as
				}
				return fail(fmt.Sprintf("the tar carries mnt/%s as %s, and it is %s here: nothing imported", e.name, as, is))
			}
		}
	}

	set := cli.DataStops(services, cli.ImportOne, name)
	dependentNotices(cwd, services, []string{name}, set)
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
	if s.Transfer == cli.DataStream && !slices.Contains(cli.StoppedNames(set), name) {
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
		if s.Transfer == cli.DataStream {
			report.Info("importing " + name)
			started := func(pid int) {
				if onTarget {
					_ = recordWriter(cwd, pid, true, !wasUp)
				}
			}
			err = runDataHook(ctx, cwd, projectDir, dataHookEnv(services, cli.StoppedNames(set)), in2, nil, started, "import", name)
			if onTarget {
				_ = recordWriter(cwd, 0, false, false)
			}
		} else {
			report.Info("importing " + name + ": mnt/" + strings.Join(names, ", mnt/") + " replaced")
			cname := fmt.Sprintf("%s%s-%d", remote.ImportContainerPrefix, composeName(cwd), os.Getpid())
			err = extractEntries(ctx, mnt, spool, entries, cname)
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
	if err := restoreServices(cwd, services, false, nil); err != nil {
		return fail(err.Error() + ": the import completed, and what it stopped did not all come back: the mark stays until it does (devopsy @" + targetAddress(cwd) + " --import " + name + " --resolved starts them)")
	}
	clearImportMark(base, name)
	return code
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
	if err := restoreServices(cwd, services, false, nil); err != nil {
		report.Fail(fmt.Errorf("%w: the mark stays", err), again)
		return 1
	}
	clearImportMark(base, name)
	report.Info("resolved: " + name + "'s data is accepted as it is, and what the import kept down runs again")
	return 0
}

// resolvedOwners gives the owned entries of name, when it is an mnt
// service, to their owners (ownMnt), in containers named as an import's
// owners' are (importContainers), for --resolved.
func resolvedOwners(cwd, projectDir string, services []cli.SyncService, name string) error {
	s, ok := cli.SyncFind(services, name)
	if !ok || s.Transfer != cli.DataMnt {
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
func importArgs(args []string) (name string, yes, resolved bool, err error) {
	usage := errors.New("usage: devopsy [@<target>] " + cli.ImportFlag + " <service> [--yes] [--resolved]")
	for _, a := range args {
		switch {
		case a == "--yes" || a == "-y":
			yes = true
		case a == "--resolved":
			resolved = true
		case name == "" && !strings.HasPrefix(a, "-"):
			name = a
		default:
			return "", false, false, usage
		}
	}
	if name == "" {
		return "", false, false, usage
	}
	return name, yes, resolved, nil
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
	name, yes, resolved, err := importArgs(args)
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
		code, err := remote.SSHStreams(remote.WithGate(remote.WithoutInterrupt(ctx)), t, remote.Bounded, remote.HeartbeatLine()+remote.ImportScript(t, projectName, name, end), r, nil, mw, false, nil)
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
