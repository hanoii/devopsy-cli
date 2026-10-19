package main

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// --export and --import: one service's data, the way its devopsy.sync
// label says (stream through the sync capability's export and import,
// rsync as a tar of its entries of mnt), locally or in a target's current
// release (ExportScript, ImportScript run the same code there).

// dataService finds name among services, labeled: an unlabeled service
// has no way of moving its data.
func dataService(services []cli.SyncService, name string) (cli.SyncService, error) {
	s, ok := cli.SyncFind(services, name)
	switch {
	case !ok:
		var names []string
		for _, s := range services {
			if s.Labeled {
				names = append(names, s.Name)
			}
		}
		msg := "no service " + name + " in the project"
		if len(names) > 0 {
			msg += " (labeled " + cli.SyncLabel + ": " + strings.Join(names, ", ") + ")"
		}
		return s, errors.New(msg)
	case !s.Labeled:
		return s, fmt.Errorf("%s has no %s label: label it %s or %s to say how its data moves", name, cli.SyncLabel, cli.SyncRsync, cli.SyncStream)
	}
	return s, nil
}

// stopFor stops s while its data moves, when it is labeled
// devopsy.sync.stop=true and running, and returns what brings it back
// (started, then waited for until healthy): the label is the contract, so
// every caller obeys it.
func stopFor(cwd string, s cli.SyncService) (restore func() error, err error) {
	restore = func() error { return nil }
	if !s.Stop {
		return restore, nil
	}
	up, err := upServices(cwd)
	if err != nil {
		return restore, err
	}
	if !slices.Contains(up, s.Name) {
		return restore, nil
	}
	sayErr("devopsy: stopping " + s.Name)
	if err := compose(cwd, errOut, "stop", s.Name); err != nil {
		return restore, fmt.Errorf("docker compose stop: %w", err)
	}
	return func() error {
		sayErr("devopsy: starting " + s.Name)
		if err := compose(cwd, errOut, "start", s.Name); err != nil {
			return fmt.Errorf("docker compose start: %w", err)
		}
		return waitHealthy(cwd, []string{s.Name})
	}, nil
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
		cli.Fprint(errOut, red, "devopsy: "+msg, color)
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
	if fileExists(filepath.Join(filepath.Dir(cwd), remote.SyncIncompleteFile)) {
		cli.Fprint(errOut, yellow, "devopsy: this environment's data is from a sync that did not complete: exporting it anyway", color)
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
	case cli.SyncStream:
		if !isExecutable(syncHookFile(projectDir, "export")) {
			return fail(fmt.Sprintf("%s is labeled %s=%s: the project needs an executable .devopsy/capabilities/sync/export", name, cli.SyncLabel, cli.SyncStream))
		}
	default:
		if mnt, err = dataMnt(projectDir); err != nil {
			return fail("the project's mnt: " + err.Error())
		}
		for _, d := range s.Dirs {
			if d == "." {
				entries = []string{"."}
				if msg := wholeMntNotice(services, s, mnt, "carries"); msg != "" {
					cli.Fprint(errOut, yellow, msg, color)
				}
				break
			}
			if _, err := os.Lstat(filepath.Join(mnt, d)); err != nil {
				sayErr("devopsy: mnt/" + d + " does not exist: skipped")
				continue
			}
			entries = append(entries, d)
		}
		if len(entries) == 0 {
			return fail("nothing to export: " + name + " bind-mounts nothing that exists under .devopsy/mnt")
		}
		if err := ensureUtilImage(ctx); err != nil {
			return fail(err.Error())
		}
	}

	restore, err := stopFor(cwd, s)
	if err != nil {
		code := fail(err.Error())
		if err := restore(); err != nil {
			fail(err.Error())
		}
		return code
	}
	code := 0
	if s.Transfer == cli.SyncStream {
		sayErr("devopsy: exporting " + name)
		err = runSyncHook(ctx, cwd, projectDir, syncHookEnv(services), nil, os.Stdout, "export", name)
	} else {
		sayErr("devopsy: exporting " + name + ": a tar of mnt/" + strings.Join(entries, ", mnt/"))
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

// wholeMntNotice says what a tar of a service mounting mnt itself
// carries (verb) beyond its label: every entry of mnt, other services'
// and its excluded ones included, as SyncWholeNotice does for syncs.
// Empty when mnt holds nothing.
func wholeMntNotice(services []cli.SyncService, s cli.SyncService, mnt, verb string) string {
	var entries []string
	if list, err := os.ReadDir(mnt); err == nil {
		for _, e := range list {
			entries = append(entries, e.Name())
		}
	}
	if len(entries) == 0 {
		return ""
	}
	var others []string
	for _, o := range services {
		if o.Name == s.Name {
			continue
		}
		for _, d := range o.Dirs {
			if d != "." {
				others = append(others, o.Name+"'s "+d+" ("+o.Transfer+")")
			}
		}
	}
	for _, d := range s.Excluded {
		others = append(others, d+" (its "+cli.SyncExcludeLabel+")")
	}
	msg := fmt.Sprintf("devopsy: %s mounts mnt whole, so the tar %s all of it (%s)", s.Name, verb, strings.Join(entries, ", "))
	if len(others) > 0 {
		msg += ": " + strings.Join(others, ", ") + " included"
	}
	return msg
}

// spoolEndsWithZeros reports whether the file at p ends with n zero
// bytes: a tar's end-of-archive blocks, which tar -t does not require,
// so a tar cut at a member boundary lists fine without them.
func spoolEndsWithZeros(p string, n int64) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() < n {
		return false
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, fi.Size()-n); err != nil {
		return false
	}
	for _, b := range buf {
		if b != 0 {
			return false
		}
	}
	return true
}

// importSpool is where --import keeps stdin until it is complete: in a
// release, the environment directory's StreamSpool (what a sync uses);
// locally a temporary file under .devopsy/, which releases never upload.
func importSpool(projectDir, name string) (*os.File, error) {
	if cli.InRelease(projectDir) {
		return os.OpenFile(remote.StreamSpool(filepath.Dir(filepath.Dir(projectDir)), name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	}
	return os.CreateTemp(projectDir, remote.StreamSpoolPrefix+name+".*")
}

// tarEntries lists the top-level entries of mnt the tar in spool carries,
// "." for mnt itself, as tar reads it in the util image: a truncated
// archive fails here, before anything is emptied.
func tarEntries(ctx context.Context, spool string) ([]string, error) {
	f, err := os.Open(spool)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	name := fmt.Sprintf("devopsy-import-%d-list", os.Getpid())
	full := []string{"run", "--rm", "-i", "--name", name, remote.UtilImage, "tar", "-tf", "-"}
	trace(append([]string{"docker"}, full...))
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Stdin, cmd.Stderr = f, errOut
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
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	scan := bufio.NewScanner(out)
	scan.Buffer(make([]byte, 64*1024), 1024*1024)
	for scan.Scan() {
		p := strings.TrimPrefix(scan.Text(), "./")
		if p == "" || p == "." {
			seen["."] = true
			continue
		}
		seen[strings.SplitN(p, "/", 2)[0]] = true
	}
	if err := cmd.Wait(); err != nil {
		return nil, err
	}
	if seen["."] {
		return []string{"."}, nil
	}
	var entries []string
	for e := range seen {
		entries = append(entries, e)
	}
	sort.Strings(entries)
	return entries, nil
}

// emptyAndExtract empties each entry the tar carries (a sync's --delete:
// what the source does not have goes) and extracts it, as root in the
// rsync image so owners and modes come as they were. Directories are
// emptied, never removed, and files overwritten in place: a running
// container bind-mounts their inodes.
const emptyAndExtract = `set -eu
for e; do
  if [ "$e" = . ]; then
    find /mnt -mindepth 2 -maxdepth 2 -exec rm -rf {} +
    find /mnt -mindepth 1 -maxdepth 1 ! -type d -exec rm -f {} +
  elif [ -d "/mnt/$e" ] && [ ! -L "/mnt/$e" ]; then
    find "/mnt/$e" -mindepth 1 -maxdepth 1 -exec rm -rf {} +
  fi
done
exec tar -C /mnt -xf - --numeric-owner --overwrite
`

// runImport implements devopsy --import <service> [--yes]: stdin, spooled
// to a file until complete, into the service. --yes is what a target's
// session passes after asking: locally nothing asks.
func runImport(cwd string, args []string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(errOut, red, "devopsy: "+msg, color)
		return 1
	}
	if len(args) >= 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Print(styleFor(os.Stdout).help(cli.RemoteCommandHelp[cli.ImportFlag]))
		return 0
	}
	name, _, err := importArgs(args)
	if err != nil {
		return fail(err.Error())
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
	if s.Transfer == cli.SyncStream && !isExecutable(syncHookFile(projectDir, "import")) {
		return fail(fmt.Sprintf("%s is labeled %s=%s: the project needs an executable .devopsy/capabilities/sync/import", name, cli.SyncLabel, cli.SyncStream))
	}
	ctx, stop := interruptible()
	defer stop()

	// stdin first, whole, into the spool: nothing changes before it is
	// all here.
	f, err := importSpool(projectDir, name)
	if err != nil {
		return fail("the spool for " + name + ": " + err.Error())
	}
	spool := f.Name()
	defer os.Remove(spool)
	type copied struct {
		n   int64
		err error
	}
	done := make(chan copied, 1)
	// From devopsy @<target> --import (its end line set), a pause longer
	// than dataQuiet is the session gone, silently: the release
	// lock is held meanwhile.
	quiet := time.Duration(0)
	if os.Getenv(remote.ImportEndEnv) != "" {
		quiet = dataQuiet()
	}
	go func() {
		var n int64
		silent, err := readQuiet(os.Stdin, quiet, func(b []byte) error {
			m, err := f.Write(b)
			n += int64(m)
			return err
		})
		if silent {
			err = fmt.Errorf("nothing came for %s, the session is gone", quiet)
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		done <- copied{n, err}
	}()
	var c copied
	select {
	case c = <-done:
	case <-ctx.Done():
		return fail("interrupted while reading stdin: nothing imported")
	}
	if c.err != nil {
		return fail("reading stdin: " + c.err.Error() + ": nothing imported")
	}
	// From devopsy @<target> --import: the data ends with the end line,
	// or the session was cut before it did.
	if end := os.Getenv(remote.ImportEndEnv); end != "" {
		_ = os.Unsetenv(remote.ImportEndEnv)
		if c.n < int64(len(end)) || !spoolEndsWith(spool, end) {
			return fail("the data stopped before its end (the session was cut): nothing imported")
		}
		c.n -= int64(len(end))
		if err := os.Truncate(spool, c.n); err != nil {
			return fail("the spool for " + name + ": " + err.Error())
		}
	}
	if c.n == 0 {
		return fail("nothing on stdin: nothing imported")
	}

	var entries []string
	var mnt string
	if s.Transfer == cli.SyncRsync {
		// The image reads the tar too: it has to be there first.
		// Every tar writer ends with two zero blocks: without them the
		// archive was cut, maybe right after a member, which tar -t takes.
		if !spoolEndsWithZeros(spool, 1024) {
			return fail("stdin is not a complete tar (no end-of-archive blocks: cut short?): nothing imported")
		}
		if err := ensureUtilImage(ctx); err != nil {
			return fail(err.Error())
		}
		if entries, err = tarEntries(ctx, spool); err != nil {
			if ctx.Err() != nil {
				return fail("interrupted: nothing imported")
			}
			return fail("stdin is not a complete tar (" + err.Error() + "): nothing imported")
		}
		if !slices.Contains(s.Dirs, ".") {
			for _, e := range entries {
				switch {
				case slices.Contains(s.Excluded, e):
					return fail(fmt.Sprintf("the tar carries mnt/%s, which %s's %s leaves out here: nothing imported", e, name, cli.SyncExcludeLabel))
				case !slices.Contains(s.Dirs, e):
					return fail(fmt.Sprintf("the tar carries mnt/%s, which %s does not bind-mount (%s): nothing imported", e, name, cmp.Or(strings.Join(s.Dirs, ", "), "none")))
				}
			}
		}
		if mnt, err = dataMnt(projectDir); err != nil {
			return fail("the project's mnt: " + err.Error())
		}
		if slices.Contains(entries, ".") {
			if msg := wholeMntNotice(services, s, mnt, "replaces"); msg != "" {
				cli.Fprint(errOut, yellow, msg, color)
			}
		}
	}

	restore, err := stopFor(cwd, s)
	if err != nil {
		code := fail(err.Error())
		if err := restore(); err != nil {
			fail(err.Error())
		}
		return code
	}
	code := 0
	in, err := os.Open(spool)
	if err == nil {
		defer in.Close()
		if s.Transfer == cli.SyncStream {
			// Up for its import, and down again after it if it was down.
			down := func() error { return nil }
			if !s.Stop {
				down, err = upFor(ctx, cwd, name, false)
			}
			if err == nil {
				sayErr("devopsy: importing " + name)
				err = runSyncHook(ctx, cwd, projectDir, syncHookEnv(services), in, nil, "import", name)
				if derr := down(); derr != nil {
					code = fail(derr.Error())
				}
			}
		} else {
			sayErr("devopsy: importing " + name + ": mnt/" + strings.Join(entries, ", mnt/") + " replaced")
			run := append([]string{"-i", "-v", mnt + ":/mnt", remote.UtilImage, "sh", "-c", emptyAndExtract, "sh"}, entries...)
			err = dockerRunIO(ctx, fmt.Sprintf("devopsy-import-%d", os.Getpid()), in, nil, run...)
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			err = errors.New("interrupted")
		}
		code = fail("import " + name + ": " + err.Error())
	}
	if err := restore(); err != nil {
		code = fail(err.Error())
	}
	return code
}

// spoolEndsWith reports whether the file at p ends with end.
func spoolEndsWith(p, end string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, len(end))
	fi, err := f.Stat()
	if err != nil || fi.Size() < int64(len(end)) {
		return false
	}
	if _, err := f.ReadAt(buf, fi.Size()-int64(len(end))); err != nil {
		return false
	}
	return string(buf) == end
}

// importArgs parses --import's arguments: <service> [--yes].
func importArgs(args []string) (name string, yes bool, err error) {
	usage := errors.New("usage: devopsy [@<target>] " + cli.ImportFlag + " <service> [--yes]")
	for _, a := range args {
		switch {
		case a == "--yes" || a == "-y":
			yes = true
		case name == "" && !strings.HasPrefix(a, "-"):
			name = a
		default:
			return "", false, usage
		}
	}
	if name == "" {
		return "", false, usage
	}
	return name, yes, nil
}

// runRemoteExport implements devopsy @<target> --export <service>: one
// session running devopsy --export in the current release, its stdout
// through as it is (never colored).
func runRemoteExport(t *remote.Target, projectName string, args []string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(os.Stderr, red, msg, color)
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
// stdin through.
func runRemoteImport(t *remote.Target, projectName string, args []string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(os.Stderr, red, msg, color)
		return 1
	}
	name, yes, err := importArgs(args)
	if err != nil {
		return fail(strings.Replace(err.Error(), "[@<target>]", "@"+targetRef(t), 1))
	}
	if term.IsTerminal(int(os.Stdin.Fd())) {
		return fail(cli.ImportFlag + " reads " + name + "'s data from stdin: devopsy @<target> " + cli.ExportFlag + " " + name + " | devopsy @" + targetRef(t) + " " + cli.ImportFlag + " " + name + ", or < a file")
	}
	if !yes {
		if err := confirmImport(t, name); err != nil {
			return fail(syncRefused(err, "Nothing imported."))
		}
	}
	// stdin through, then the end line only once it ended cleanly: the
	// server tells a complete stream from a cut session by it.
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return fail(err.Error())
	}
	end := "\n-- devopsy import end " + hex.EncodeToString(nonce) + " --\n"
	r, w, err := os.Pipe()
	if err != nil {
		return fail(err.Error())
	}
	go func() {
		if _, err := io.Copy(w, os.Stdin); err == nil {
			_, _ = io.WriteString(w, end)
		}
		w.Close()
	}()
	code, err := remote.SSHWith(t, remote.Bounded, remote.HeartbeatLine()+remote.ImportScript(t, projectName, name, end), r, nil, false, nil)
	r.Close()
	if err != nil {
		return fail(err.Error())
	}
	return code
}

// confirmImport asks for the environment's name before its data is
// replaced, like --sync, on the terminal itself: stdin carries the data.
// Without a terminal it fails: --yes is the way there.
func confirmImport(t *remote.Target, name string) error {
	where := t.Host + ":" + t.Path
	return typeToGoOn(fmt.Sprintf("This replaces %s's data on @%s (%s) with what stdin carries.", name, t.Address, where),
		t.Name, cli.ImportFlag+" replaces "+name+"'s data on "+where+": add --yes when not at a terminal", true)
}
