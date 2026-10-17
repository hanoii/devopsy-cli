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
	"math/rand/v2"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"golang.org/x/term"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// A sync, operator side: devopsy @<child> --sync, and the first release
// of a child. See remote/sync.go for the sessions and scripts.

// syncJob is a sync about to run.
type syncJob struct {
	child, parent *remote.Target
	// projectName and parentName are the compose projects of child and
	// parent.
	projectName, parentName string
	// sameHost: no SSH between the servers, one container copies.
	sameHost bool
	// to and port are where the parent's server reaches the child's
	// (user@hostname), when they differ.
	to, port string
	// args go to rsync (after --sync's --).
	args []string
	// first: the child's first release, with no data of its own yet. Its
	// inherited variables are written before the upload, so the variables
	// check sees them, and stay if the sync fails: the next release is a
	// first one again. A re-sync inherits before the holder too, into a
	// pending file the sync reads and moves into place on go, so a failed
	// one leaves the child's own values with its own data.
	first bool
}

// syncPreflight checks a sync of p into c can start, before anything
// changes: c is a child with a run step, p another environment of the same
// project, released; across servers, an SSH agent to forward and a child
// the parent's server can reach.
func syncPreflight(c, p *remote.Target, projectName string, args []string) (*syncJob, error) {
	if c.Parent == "" {
		return nil, fmt.Errorf("@%s is not a child: environment %s has no parent: in %s", c.Address, c.Name, cmp.Or(c.File, "the project's config"))
	}
	if p == nil {
		return nil, fmt.Errorf("environment %s: its parent %s is not resolved", c.Name, c.Parent)
	}
	if p.Project == nil || c.Project == nil || p.Project.Name != c.Project.Name {
		return nil, fmt.Errorf("@%s is not an environment of %s: a sync copies between environments of one project", p.Address, c.Project.Name)
	}
	if c.Release == nil || len(remote.StepArgs(c.Release.Run)) == 0 {
		return nil, errors.New(missingSteps(c, "release"))
	}
	job := &syncJob{child: c, parent: p, projectName: projectName, parentName: p.ComposeName(), args: args}
	job.sameHost = sameHost(c.Host, p.Host)
	if p.Address == c.Address || (job.sameHost && p.Path == c.Path) {
		return nil, fmt.Errorf("@%s and its parent are the same environment (%s:%s)", c.Address, p.Host, p.Path)
	}
	if !job.sameHost {
		if os.Getenv("SSH_AUTH_SOCK") == "" {
			return nil, fmt.Errorf("the parent's server (%s) reaches the child's (%s) with your SSH agent, forwarded for the sync, and none is running locally: eval $(ssh-agent); ssh-add (in CI, add the key to an agent)", p.Host, c.Host)
		}
		if err := agentHasKeys(p.Host, c.Host); err != nil {
			return nil, err
		}
		user, hostname, port, err := remote.ChildAddress(c.Host)
		if err != nil {
			return nil, err
		}
		job.to, job.port = hostname, port
		if user != "" {
			job.to = user + "@" + hostname
		}
	}
	var out bytes.Buffer
	code, err := remote.SSH(p, remote.CurrentExistsScript(p), bytes.NewReader(nil), &out, false)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("could not check the parent @%s (%d)", p.Address, code)
	}
	if strings.TrimSpace(out.String()) != "current" {
		return nil, fmt.Errorf("the parent @%s (%s:%s) has no current release: release it first", p.Address, p.Host, p.Path)
	}
	return job, nil
}

// sameHost reports whether two SSH destinations are one server: the same
// string, or the same user, hostname and port for ssh -G.
func sameHost(a, b string) bool {
	if a == b {
		return true
	}
	ea, err := remote.SSHEffective(a)
	if err != nil {
		return false
	}
	eb, err := remote.SSHEffective(b)
	if err != nil {
		return false
	}
	for _, k := range []string{"user", "hostname", "port"} {
		if ea[k] != eb[k] {
			return false
		}
	}
	return ea["hostname"] != ""
}

// runSyncSession runs script, a holder (HolderScript, or ActivateScript
// with a sync) on the child's server; once it prints the marker, the push
// session on the parent's, then go or abort to the holder. The holder's
// exit code is the result.
func runSyncSession(job *syncJob, script string, rlog *releaseLog, color bool) int {
	say := func(c, msg string) {
		cli.Fprint(os.Stderr, c, msg, color)
		rlog.Line(msg)
	}
	// An *os.File, so the holder's ssh reads it directly: devopsy writes go
	// or abort, and nothing waits on a copy if the holder ends first.
	r, w, err := os.Pipe()
	if err != nil {
		say(red, err.Error())
		return 1
	}
	defer r.Close()
	defer w.Close()
	// The marker is taken out of stderr (holders print on one stream)
	// before the lines are colored for the terminal: the holder's
	// streams are read here, not in SSHContext.
	var out, errOut io.Writer = os.Stdout, os.Stderr
	var colored []*remote.ColorWriter
	if color {
		o, e := remote.Colorize(os.Stdout), remote.Colorize(os.Stderr)
		colored = append(colored, o, e)
		out, errOut = o, e
	}
	// The marker comes on stderr: holders print on one stream.
	marker := make(chan string, 1)
	lm := &remote.LineMarker{Prefix: remote.SyncMarker, Found: func(base string) { marker <- base }}
	mwErr := lm.Writer(errOut)
	flush := func() {
		mwErr.Flush()
		for _, c := range colored {
			c.Flush()
		}
	}
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	// Ended by devopsy only: before the marker on an interrupt, and when
	// devopsy returns while it still runs (a second Ctrl-C).
	holderCtx, endHolder := context.WithCancel(context.Background())
	defer endHolder()
	go func() {
		code, err := remote.SSHStreams(remote.WithoutInterrupt(holderCtx), job.child, remote.KeepAlive, script, r, out, mwErr, false, rlog)
		done <- result{code, err}
	}()
	// Interrupted before the marker, devopsy ends the holder's session
	// and waits for it: before a release's switch nothing changes, and a
	// holder restores on its own. After it, the push ends first, so
	// nothing copies into the child once its services are back, then the
	// holder gets abort.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	var res result
	select {
	case res = <-done:
	case <-sigs:
		say(red, "devopsy: interrupted: ending the session on @"+job.child.Address+", which leaves it as it was")
		endHolder()
		<-done
		flush()
		return 130
	case base := <-marker:
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		pushed := make(chan int, 1)
		go func() { pushed <- runPush(ctx, job, base, rlog, color) }()
		var code int
		interrupted := false
		select {
		case code = <-pushed:
		case <-sigs:
			interrupted = true
			say(red, "devopsy: interrupted: stopping the copy from @"+job.parent.Address+", then aborting the sync")
			cancel()
			<-pushed
		case res = <-done:
			// The holder is gone (its server's connection dropped, its
			// session killed): it restored the child's services on its
			// way out, so nothing may copy into them any more, and
			// nobody is left to answer.
			why := fmt.Sprint(res.code)
			if res.err != nil {
				why = res.err.Error()
			}
			say(red, "devopsy: the session on @"+job.child.Address+" ended during the copy ("+why+"): stopping the copy from @"+job.parent.Address)
			cancel()
			<-pushed
			flush()
			return cmp.Or(res.code, 1)
		}
		answer := "go\n"
		if code == 0 && !interrupted {
			// What --status counts as a sync (remote.SyncCopied).
			rlog.Line(remote.SyncCopied + job.parent.Address)
		}
		if interrupted {
			answer = "abort\n"
		} else if code != 0 {
			answer = "abort\n"
			say(red, fmt.Sprintf("devopsy: the copy from @%s failed (%d): aborting the sync", job.parent.Address, code))
		}
		_, _ = w.WriteString(answer)
		w.Close()
		if interrupted {
			// The holder's restoring is worth seeing, unless interrupted
			// again.
			select {
			case <-done:
			case <-sigs:
			}
			flush()
			return 130
		}
		// The run step is the server's now: an interrupt here only cuts
		// the session, and the step goes on there (remote's followOutput).
		// Say so once; a second one cuts it.
		select {
		case res = <-done:
		case <-sigs:
			say(yellow, "devopsy: interrupted: the run step goes on on @"+job.child.Address+" (devopsy @"+targetRef(job.child)+" --log shows how it ends). Ctrl-C again ends the session, not the step")
			select {
			case res = <-done:
			case <-sigs:
				flush()
				return 130
			}
		}
	}
	flush()
	if res.err != nil {
		say(red, res.err.Error())
		return 1
	}
	return res.code
}

// runPush is the push session: devopsy --sync-push on the parent's
// server, into the child's directory base. Across servers it forwards the
// SSH agent, so the parent's server reaches the child's for this session
// only.
func runPush(ctx context.Context, job *syncJob, base string, rlog *releaseLog, color bool) int {
	p := job.parent
	env := map[string]string{
		remote.SyncToEnv:         job.to,
		remote.SyncSSHPortEnv:    job.port,
		remote.SyncToBaseEnv:     base,
		remote.SyncToDirEnv:      base + "/current/.devopsy",
		remote.SyncToProjectEnv:  job.projectName,
		remote.SyncToTargetEnv:   job.child.Address,
		remote.SyncArgsEnv:       strings.Join(job.args, " "),
		remote.SyncFromTargetEnv: p.Address,
		remote.SyncWatchEnv:      "1",
	}
	if color {
		env[remote.ColorEnv] = "1"
	}
	extra := remote.KeepAlive
	where := "same server"
	if !job.sameHost {
		extra = append([]string{"-o", "ForwardAgent=yes"}, remote.KeepAlive...)
		where = "from " + p.Host + " to " + job.to
	}
	msg := fmt.Sprintf("Copying @%s (%s:%s) into @%s, %s...", p.Address, p.Host, p.Path, job.child.Address, where)
	cli.Fprint(os.Stderr, cyan, msg, color)
	rlog.Line(msg)
	// The session's stdin stays open until the push ends: the parent's
	// server sees EOF when the session is gone (interrupted, connection
	// lost) and stops copying at once, instead of at its next write.
	r, w, err := os.Pipe()
	if err != nil {
		return 1
	}
	defer r.Close()
	defer w.Close()
	// Cancelled, the session's stdin closes at once, as sshd closes the
	// remote command's when the connection goes: the push sees EOF
	// whether or not the signal to ssh reached it (through a shell that
	// did not exec, it does not).
	stop := context.AfterFunc(ctx, func() { w.Close() })
	defer stop()
	code, err := remote.SSHContext(remote.WithoutInterrupt(ctx), p, extra, remote.PushScript(p, job.parentName, env), r, nil, false, rlog)
	if err != nil {
		cli.Fprint(os.Stderr, red, err.Error(), color)
		rlog.Line(err.Error())
		return 1
	}
	return code
}

// inheritVars writes the variables the parent's inherit: names, read from
// its server's files (its shared/.env, then its instance's and project's
// .env, nearest first), as the child's shared/inherit.env: the whole file,
// so a resync replaces it. With pending (a re-sync), into the child's
// remote.SyncInheritPending instead, before the holder prepares: the sync
// reads its labels and everything else from it, and moves it into place
// on go, so a failed copy changes nothing. The child's own shared/.env
// wins over it, and a key set there too is named. A key the parent does
// not set is reported, never written. Values travel over SSH's stdin, and
// only names are printed.
func inheritVars(ctx context.Context, job *syncJob, rlog *releaseLog, color bool, pending bool) error {
	c, p := job.child, job.parent
	if len(p.Inherit) == 0 {
		return nil
	}
	say := func(colr, msg string) {
		cli.Fprint(os.Stderr, colr, msg, color)
		rlog.Line(msg)
	}
	var out bytes.Buffer
	code, err := remote.SSHContext(ctx, p, nil, remote.InheritReadScript(p), bytes.NewReader(nil), &out, false, nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("could not read the variables of @%s (%d)", p.Address, code)
	}
	files := map[string]map[string]string{}
	level := ""
	var content strings.Builder
	flush := func() error {
		if level == "" {
			return nil
		}
		vars, err := dotenv.UnmarshalWithLookup(content.String(), func(string) (string, bool) { return "", false })
		if err != nil {
			return fmt.Errorf("the %s .env of @%s: %w", level, p.Address, err)
		}
		files[level] = vars
		content.Reset()
		return nil
	}
	for _, line := range strings.SplitAfter(out.String(), "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), remote.InheritSeparator); ok {
			if err := flush(); err != nil {
				return err
			}
			level = name
			continue
		}
		content.WriteString(line)
	}
	if err := flush(); err != nil {
		return err
	}

	// Nearest first, as the server reads them.
	var lines strings.Builder
	var keys, missing []string
	for _, k := range p.Inherit {
		found := false
		for _, l := range []string{"shared", "instance", "project"} {
			if v, ok := files[l][k]; ok {
				lines.WriteString(cli.DotenvLine(k, v) + "\n")
				keys = append(keys, k)
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, k)
		}
	}
	var res bytes.Buffer
	code, err = remote.SSHContext(ctx, c, nil, remote.InheritWriteScript(c, keys, pending), strings.NewReader(lines.String()), &res, false, nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("could not write @%s's inherited variables (%d)", c.Address, code)
	}
	switch {
	case len(keys) > 0 && pending:
		say(cyan, fmt.Sprintf("Inherited %s from @%s for @%s's sync (its shared/%s once the copy is in).", strings.Join(keys, ", "), p.Address, c.Address, remote.InheritFile))
	case len(keys) > 0:
		say(cyan, fmt.Sprintf("Inherited %s from @%s into @%s's shared/%s.", strings.Join(keys, ", "), p.Address, c.Address, remote.InheritFile))
	}
	if shadowed := strings.Fields(res.String()); len(shadowed) > 0 {
		say(yellow, fmt.Sprintf("@%s's own shared .env also sets %s, which wins there (devopsy @%s --vars unset to inherit it).", c.Address, strings.Join(shadowed, ", "), targetRef(c)))
	}
	if len(missing) > 0 {
		say(yellow, fmt.Sprintf("@%s does not set %s (its inherit: in %s): not inherited.", p.Address, strings.Join(missing, ", "), cmp.Or(p.File, "the project's config")))
	}
	return nil
}

// runInherit implements devopsy @<child> --inherit: the parent's inherited
// variables into the child, nothing copied, no run step.
func runInherit(t *remote.Target, projectName string, args []string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(os.Stderr, red, msg, color)
		return 1
	}
	if len(args) > 0 {
		return fail("usage: devopsy @" + targetRef(t) + " --inherit")
	}
	if t.Parent == "" {
		return fail(fmt.Sprintf("@%s is not a child: no parent: in its config", t.Address))
	}
	if t.ParentErr != nil {
		report.Fail(os.Stderr, color, t.ParentErr)
		return 1
	}
	p := t.ParentTarget
	if len(p.Inherit) == 0 {
		cli.Fprint(os.Stderr, yellow, fmt.Sprintf("@%s inherits nothing: no inherit: on %s", t.Address, p.Name), color)
		return 0
	}
	for _, x := range []*remote.Target{p, t} {
		var out bytes.Buffer
		code, err := remote.SSH(x, remote.CurrentExistsScript(x), bytes.NewReader(nil), &out, false)
		if err != nil {
			return fail(err.Error())
		}
		if code != 0 {
			return fail(fmt.Sprintf("could not check @%s (%d)", x.Address, code))
		}
		if strings.TrimSpace(out.String()) != "current" {
			return fail(fmt.Sprintf("@%s (%s:%s) has no current release: release it first", x.Address, x.Host, x.Path))
		}
	}
	job := &syncJob{child: t, parent: p, projectName: projectName, parentName: p.ComposeName()}
	if err := inheritVars(context.Background(), job, newReleaseLog("inherit", t), color, false); err != nil {
		return fail("inherit: " + err.Error())
	}
	return 0
}

// runSync implements devopsy @<child> --sync [--from @<address>] [--yes]
// [-- args...]: the parent's data into the child's current release, then
// its run step.
func runSync(t *remote.Target, projectDir string, projectEnv func(string) (string, bool), projectName string, args []string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(os.Stderr, red, msg, color)
		return 1
	}
	usage := "usage: devopsy @" + targetRef(t) + " --sync [--from @<address>] [--yes] [-- <rsync args>...]"
	yes, from := false, ""
	var rsyncArgs []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--yes", "-y":
			yes = true
		case "--from":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return fail(usage)
			}
			from = strings.TrimPrefix(args[i+1], "@")
			i++
		case "--":
			rsyncArgs = args[i+1:]
			i = len(args)
		default:
			return fail(usage)
		}
	}
	p := t.ParentTarget
	if from == "" && t.ParentErr != nil {
		report.Fail(os.Stderr, color, t.ParentErr, "Sync from an address instead:\ndevopsy @"+targetRef(t)+" --sync --from "+parentGuess(t))
		return 1
	}
	if from != "" {
		var err error
		if p, err = remote.LoadTarget(projectDir, from, projectEnv); err != nil {
			return fail("--from: " + err.Error())
		}
	}
	job, err := syncPreflight(t, p, projectName, rsyncArgs)
	if err != nil {
		return fail(err.Error())
	}
	run := remote.StepArgs(t.Release.Run)
	if !yes {
		if err := confirmSync(t, p, "runs 'devopsy "+strings.Join(run, " ")+"' there"); err != nil {
			return fail(syncRefused(err, "Nothing syncd."))
		}
	}

	rlog := newReleaseLog("sync", t)
	msg := fmt.Sprintf("Syncing @%s into @%s (%s:%s)...", p.Address, t.Address, t.Host, t.Path)
	cli.Fprint(os.Stderr, cyan, msg, color)
	rlog.Line(msg)
	// The parent's variables first, into the child's pending file: the
	// holder reads its labels from them; on go they move into place.
	if err := inheritVars(context.Background(), job, rlog, color, true); err != nil {
		return fail("inherit: " + err.Error() + "\nNothing syncd.")
	}
	o := &remote.SyncOptions{From: p.Address, Args: rsyncArgs, Inherit: len(p.Inherit) > 0}
	code := runSyncSession(job, remote.HolderScript(t, projectName, o, run), rlog, color)

	// The log goes with the release syncd into, after its own.
	rlog.Line(fmt.Sprintf("devopsy: sync exited %d", code))
	text := rlog.Text()
	if m := remote.SyncingInto.FindSubmatch(text); m != nil {
		text = append([]byte("\n"), text...)
		if c, err := remote.SSH(t, remote.LogSaveScript(t, string(m[1]), true), bytes.NewReader(text), io.Discard, false); err != nil || c != 0 {
			cli.Fprint(os.Stderr, yellow, fmt.Sprintf("devopsy: could not save the sync log on the server (%v, %d)", err, c), color)
		}
	}
	return code
}

// errNotConfirmed: the name typed was not the one asked for.
var errNotConfirmed = errors.New("")

// typeToGoOn asks to type name to go on, after what (what follows, a
// sentence): errNotConfirmed when something else is typed, noTerminal
// as the error without a terminal (--yes is the way there). tty reads
// and asks on the terminal itself, when stdin carries data.
func typeToGoOn(what, name, noTerminal string, tty bool) error {
	var in interface {
		ReadString(byte) (string, error)
	} = stdinReader
	var out io.Writer = os.Stderr
	if tty {
		f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return errors.New(noTerminal)
		}
		defer f.Close()
		in, out = bufio.NewReader(f), f
	} else if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New(noTerminal)
	}
	fmt.Fprintf(out, "%s Type %s to go on: ", what, name)
	line, _ := in.ReadString('\n')
	if strings.TrimSpace(line) != name {
		return errNotConfirmed
	}
	return nil
}

// syncRefused is what a refused confirmation prints, then nothing.
func syncRefused(err error, nothing string) string {
	if err == errNotConfirmed {
		return nothing
	}
	return err.Error() + "\n" + nothing
}

// confirmSync asks for the child's environment name before its data is
// replaced, like --destroy; then says what follows the copy. Not at a
// terminal it fails: --yes is the way there.
func confirmSync(t, p *remote.Target, then string) error {
	where := t.Host + ":" + t.Path
	return typeToGoOn(fmt.Sprintf("This replaces the data of @%s (%s, shared/mnt) with @%s's (%s:%s), then %s.", t.Address, where, p.Address, p.Host, p.Path, then),
		t.Name, "--sync replaces the data of "+where+" with its parent's: add --yes when not at a terminal", false)
}

// Server side: the hidden flags a sync runs.

// errOut is stderr for what syncs, --export and --import run: never an
// *os.File, so commands write to a pipe devopsy drains and never die of a
// broken pipe when the session that called devopsy is gone (they would
// leave services stopped).
var errOut = &quietWriter{w: os.Stderr}

// quietWriter never fails, and also appends to a file once the session
// is gone (keep): what restores the parent's services then has a reader.
type quietWriter struct {
	w    io.Writer
	mu   sync.Mutex
	file *os.File
}

func (q *quietWriter) Write(p []byte) (int, error) {
	_, _ = q.w.Write(p)
	q.mu.Lock()
	if q.file != nil {
		_, _ = q.file.Write(p)
	}
	q.mu.Unlock()
	return len(p), nil
}

// keep sends what follows to $base/.devopsy-sync.log too, like the
// holder does on the child when nobody reads the session any more.
func (q *quietWriter) keep(base string) {
	f, err := os.OpenFile(filepath.Join(base, syncLog), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	q.mu.Lock()
	q.file = f
	q.mu.Unlock()
	sayErr("devopsy: " + time.Now().UTC().Format("2006-01-02 15:04:05") + ": the sync's session is gone: restoring what was stopped")
}

// syncState records, next to syncLog, the services a sync stopped ("stop
// db" lines), so restoring brings back those and leaves a service that was
// down before the sync down (a maintenance stop). Written before the
// first stop, removed by a restore that brought everything back; absent,
// every service labeled devopsy.sync.stop=true is restored.
const syncState = ".devopsy-sync.services"

// syncLog is the holder's and the push's log for what happened after the
// operator's devopsy was gone, in the environment's directory.
const syncLog = ".devopsy-sync.log"

// sayErr prints a server-side notice.
func sayErr(msg string) { fmt.Fprintln(errOut, msg) }

// sayWarn is a notice worth a color on the operator's terminal
// (ColorEnv): the release log strips it.
func sayWarn(msg string) { cli.Fprint(errOut, yellow, msg, os.Getenv(remote.ColorEnv) != "") }

// trace prints a command devopsy runs on a server, with DEVOPSY_VERBOSE.
func trace(args []string) {
	if cli.VerboseLevel(os.Getenv(cli.VerboseEnv)) > 0 {
		sayErr("devopsy: running '" + strings.Join(args, " ") + "'")
	}
}

// interruptible cancels its context on SIGINT, SIGTERM, SIGHUP and
// SIGPIPE (the session gone), so a sync stops copying and restores its
// services instead of dying.
func interruptible() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() { signal.Stop(sigs); cancel() }
}

// watchSession cancels ctx when stdin reaches EOF: devopsy keeps the push
// session's stdin open (SyncWatchEnv), so EOF means the session is gone.
// Without it, a silent copy would only notice at its next write.
func watchSession(ctx context.Context) context.Context {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		cancel()
	}()
	return ctx
}

// dockerRun runs docker run args as a container named name, removed when
// ctx ends: killing the docker client alone leaves its container running.
func dockerRun(ctx context.Context, name string, args ...string) error {
	return dockerRunIO(ctx, name, nil, nil, args...)
}

// docker runs docker args, its output to errOut.
func docker(ctx context.Context, args ...string) error {
	trace(append([]string{"docker"}, args...))
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout, cmd.Stderr = errOut, errOut
	return cmd.Run()
}

// dockerOut runs docker args and returns its output.
func dockerOut(ctx context.Context, args ...string) (string, error) {
	trace(append([]string{"docker"}, args...))
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stderr = errOut
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// ensureUtilImage builds devopsy's util image on this host when missing.
func ensureUtilImage(ctx context.Context) error {
	if exec.CommandContext(ctx, "docker", "image", "inspect", remote.UtilImage).Run() == nil {
		return nil
	}
	sayErr("devopsy: building " + remote.UtilImage)
	cmd := exec.CommandContext(ctx, "docker", remote.BuildUtilImageArgs()...)
	cmd.Stdin = strings.NewReader(strings.Join(remote.UtilDockerfile, "\n") + "\n")
	cmd.Stdout, cmd.Stderr = io.Discard, errOut // stdout: the image's id
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("building %s: %w", remote.UtilImage, err)
	}
	return nil
}

// syncEnv reads the sync's variables in a push: the child's directory and
// project are required.
func syncEnv(flag string) (toDir, toProject string, err error) {
	toDir, toProject = os.Getenv(remote.SyncToDirEnv), os.Getenv(remote.SyncToProjectEnv)
	if toDir == "" || toProject == "" {
		return "", "", fmt.Errorf("%s only runs in a sync's push, on the parent's server (devopsy @<child> --sync)", flag)
	}
	return toDir, toProject, nil
}

// runSyncPush implements the hidden --sync-push, on the parent's server
// in its current release: the sync capability's prepare parent, then the
// parent's services labeled devopsy.sync.stop=true stopped, the entries of
// mnt its rsync services bind-mount rsynced, each stream service exported
// into a spool in the child's directory and imported there, the stopped
// services started again, and finish parent go|abort.
func runSyncPush(cwd string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(errOut, red, "devopsy: "+msg, color)
		return 1
	}
	if _, _, err := syncEnv(cli.SyncPushFlag); err != nil {
		return fail(err.Error())
	}
	if to := os.Getenv(remote.SyncToEnv); to != "" && os.Getenv("SSH_AUTH_SOCK") == "" {
		host, _ := os.Hostname()
		return fail(fmt.Sprintf("no SSH agent was forwarded to %s: a restricted key refuses it (authorize it with `restrict,agent-forwarding,port-forwarding`, or use a key without restrictions), and so does sshd with AllowAgentForwarding no", host))
	}
	projectDir, err := cli.FindProjectDir(cwd)
	if err != nil {
		return fail(err.Error())
	}
	ctx, stop := interruptible()
	defer stop()
	if os.Getenv(remote.SyncWatchEnv) != "" {
		ctx = watchSession(ctx)
	}
	// Cancelled before the end (a signal, the session's EOF): nobody
	// reads this session any more, so the rest goes to the log too. cwd
	// is $base/current, as the push script's cd left it.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			errOut.keep(filepath.Dir(cwd))
		case <-done:
		}
	}()

	_, services, err := cli.SyncServices(cwd, os.Environ())
	if err != nil {
		return fail(err.Error())
	}
	if !slices.ContainsFunc(services, func(s cli.SyncService) bool { return s.Labeled }) {
		sayErr("devopsy: nothing to sync: no service labeled " + cli.SyncLabel)
		return 0
	}
	streams, err := cli.SyncStreamServices(services)
	if err != nil {
		return fail(err.Error())
	}
	dirs := cli.SyncDirs(services)
	if len(dirs) == 0 && len(streams) == 0 {
		sayErr("devopsy: nothing to sync: no bind mounts under .devopsy/mnt for the services labeled " + cli.SyncLabel + "=" + cli.SyncRsync)
		return 0
	}
	// Everything that can refuse, before anything changes on this side.
	if len(streams) > 0 && !isExecutable(syncHookFile(projectDir, "export")) {
		return fail(fmt.Sprintf("%s labeled %s=%s: the parent's release needs an executable .devopsy/capabilities/sync/export", strings.Join(streams, ", "), cli.SyncLabel, cli.SyncStream))
	}
	if msg := cli.SyncWholeNotice(services); msg != "" {
		sayWarn(msg)
	}
	// What the rsync would change under a running service of the child,
	// left running by its own labels, and a stream the child would not
	// import, are mismatches, not a sync: refused before anything stops.
	child, err := childServices(ctx)
	if err != nil {
		return fail(err.Error())
	}
	if conflicts := append(syncConflicts(services, child), streamMismatches(services, streams, child)...); len(conflicts) > 0 {
		return fail(strings.Join(conflicts, "\ndevopsy: "))
	}
	if len(dirs) > 0 {
		if err := ensureUtilImage(ctx); err != nil {
			return fail(err.Error())
		}
	}

	env := syncHookEnv(services)
	// finish parent runs whatever happened, after the services are back,
	// and never with the sync's context: it restores too.
	finish := func(code int) int {
		answer := "go"
		if code != 0 {
			answer = "abort"
		}
		if err := runSyncHook(context.Background(), cwd, projectDir, env, nil, nil, "finish", "parent", answer); err != nil && code == 0 {
			code = fail("the sync capability's finish parent go: " + err.Error())
		}
		return code
	}
	if err := runSyncHook(ctx, cwd, projectDir, env, nil, nil, "prepare", "parent"); err != nil {
		return finish(fail("the sync capability's prepare parent: " + err.Error()))
	}
	if err := stopServices(cwd, services); err != nil {
		// What it stopped before failing comes back: restoring reads the
		// containers' state, not what stopping did.
		code := fail(err.Error())
		if err := restoreServices(cwd, services); err != nil {
			fail(err.Error())
		}
		return finish(code)
	}
	code := 0
	if len(dirs) > 0 {
		if err := syncRsync(ctx, projectDir, dirs, strings.Fields(os.Getenv(remote.SyncArgsEnv))); err != nil {
			code = fail(err.Error())
		}
	}
	for _, name := range streams {
		if code != 0 {
			break
		}
		if ctx.Err() != nil {
			code = fail("interrupted: " + name + " not exported")
			break
		}
		if err := syncStream(ctx, cwd, projectDir, name, env); err != nil {
			code = fail(err.Error())
		}
	}
	if err := restoreServices(cwd, services); err != nil {
		code = fail(err.Error())
	}
	return finish(code)
}

// syncHookFile is the sync capability's action in the project at
// projectDir (its .devopsy/).
func syncHookFile(projectDir, action string) string {
	return filepath.Join(projectDir, "capabilities", "sync", action)
}

// syncHookEnv are the variables every sync capability action gets about
// its own side: the services by label (cli.SyncLists).
func syncHookEnv(services []cli.SyncService) []string {
	rsync, stream, stop := cli.SyncLists(services)
	return []string{remote.SyncRsyncEnv + "=" + rsync, remote.SyncStreamEnv + "=" + stream, remote.SyncStopEnv + "=" + stop}
}

// runSyncHook runs the sync capability's action (args[0]) with args, in
// the project at cwd, with env over this process's: only when its file is
// there, except export and import, which a stream service needs (their
// callers check first). stdin nil is empty, stdout nil is errOut. Its own
// process group, all of it ended when ctx is (a pipeline's commands too,
// a dump and what compresses it).
func runSyncHook(ctx context.Context, cwd, projectDir string, env []string, stdin io.Reader, stdout io.Writer, args ...string) error {
	if a := args[0]; a != "export" && a != "import" && !isExecutable(syncHookFile(projectDir, a)) {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		self = "devopsy"
	}
	sayErr("devopsy: running 'devopsy --capability sync " + strings.Join(args, " ") + "'")
	cmd := exec.CommandContext(ctx, self, append([]string{"--capability", "sync"}, args...)...)
	cmd.Dir = cwd
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	if stdout == nil {
		cmd.Stdout = errOut
	}
	cmd.Stderr = errOut
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	err = cmd.Run()
	if ctx.Err() != nil {
		return errors.New("interrupted")
	}
	return err
}

// syncStream copies a stream service: the parent's export writes its
// data into a spool file in the child's directory (on the same server
// directly, across servers through an SSH session to the child), and only
// once the export exited 0 does the child's --sync-services import read
// it. A cut export never reaches the import; its partial spool is removed
// here on the same server, by the child's abort across servers.
func syncStream(ctx context.Context, cwd, projectDir, name string, env []string) error {
	spool := remote.StreamSpool(os.Getenv(remote.SyncToBaseEnv), name)
	to := os.Getenv(remote.SyncToEnv)
	sayErr("devopsy: exporting " + name)
	var err error
	if to == "" {
		var f *os.File
		if f, err = os.OpenFile(spool, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600); err != nil {
			return fmt.Errorf("the spool for %s: %w", name, err)
		}
		err = runSyncHook(ctx, cwd, projectDir, env, nil, f, "export", name)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(spool)
		}
	} else {
		err = exportInto(ctx, cwd, projectDir, name, env, to, spool)
	}
	if err != nil {
		return fmt.Errorf("export %s: %w: nothing imported", name, err)
	}
	if ctx.Err() != nil {
		return errors.New("interrupted")
	}
	self, err := os.Executable()
	if err != nil {
		self = "devopsy"
	}
	trace([]string{"devopsy", cli.SyncExecFlag, cli.SyncServicesFlag, "import", name})
	cmd := exec.CommandContext(ctx, self, cli.SyncExecFlag, cli.SyncServicesFlag, "import", name)
	cmd.Stdout, cmd.Stderr = errOut, errOut
	// The import watches its stdin (SyncWatchEnv, passed on by
	// --sync-exec): a pipe kept open until the import ends or the sync is
	// cancelled, so an import on another server stops as soon as this
	// side is gone, as the push does, instead of writing on while the
	// holder restores. On this server it gets SIGTERM too. Either way it
	// ends its own and removes the spool.
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	var closeOnce sync.Once
	closeW := func() { closeOnce.Do(func() { w.Close() }) }
	defer closeW()
	stopWatch := context.AfterFunc(ctx, closeW)
	defer stopWatch()
	cmd.Stdin = r
	cmd.Env = append(os.Environ(), remote.SyncWatchEnv+"=1")
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	err = cmd.Start()
	r.Close()
	if err == nil {
		err = cmd.Wait()
	}
	if err != nil {
		if ctx.Err() != nil {
			return errors.New("interrupted")
		}
		return fmt.Errorf("import %s: %w", name, err)
	}
	return nil
}

// childSSHArgs are ssh's arguments for a session from the parent's
// server to the child's (to) running script: -o option, remote.ChildSSH,
// remote.KeepAlive, -T, extra, the sync's port; traced, script aside.
func childSSHArgs(to, script, option string, extra ...string) []string {
	args := append(append([]string{"-o", option}, remote.ChildSSH...), remote.KeepAlive...)
	args = append(append(args, "-T"), extra...)
	if p := os.Getenv(remote.SyncSSHPortEnv); p != "" {
		args = append(args, "-p", p)
	}
	trace(append([]string{"ssh"}, append(args, to)...))
	return append(args, to, "sh -c "+remote.Quote(script))
}

// exportInto runs the export of name with its stdout written to spool on
// the child's server (to), by cat in an SSH session the forwarded agent
// opens. Both must succeed.
func exportInto(ctx context.Context, cwd, projectDir, name string, env []string, to, spool string) error {
	sshArgs := childSSHArgs(to, "umask 077; cat > "+remote.Quote(spool), "ClearAllForwardings=yes")
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	cat := remote.SSHCommandContext(ctx, sshArgs...)
	cat.Stdin, cat.Stdout, cat.Stderr = r, errOut, errOut
	if err := cat.Start(); err != nil {
		r.Close()
		w.Close()
		return err
	}
	r.Close()
	exported := runSyncHook(ctx, cwd, projectDir, env, nil, w, "export", name)
	w.Close()
	written := cat.Wait()
	// A session that failed first makes the export fail too (a broken
	// pipe): its own error says why.
	if written != nil && ctx.Err() == nil {
		return fmt.Errorf("writing it into %s: %w", to, written)
	}
	return exported
}

// childService is a line of the child's --sync-services list: a service,
// its label there (rsync, stream or skip), whether it is stopped during
// a sync, its containers' state (running, exited..., none without one, ""
// from a devopsy that does not say) and its entries of mnt.
type childService struct {
	Name, Transfer, State string
	Stop                  bool
	Dirs                  []string
}

// childServices asks the child's release for its services, through
// --sync-exec, after it prepared them.
func childServices(ctx context.Context) ([]childService, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, self, cli.SyncExecFlag, cli.SyncServicesFlag, "list")
	cmd.Stdout, cmd.Stderr = &out, errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("listing the child's services: %w", err)
	}
	return parseChildServices(out.String()), nil
}

// parseChildServices reads --sync-services list: name, label (with +stop),
// state, entries; or, from an older devopsy, name, label, entries.
func parseChildServices(out string) []childService {
	var services []childService
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 3 {
			continue
		}
		transfer, stop := strings.CutSuffix(f[1], "+stop")
		cs := childService{Name: f[0], Transfer: transfer, Stop: stop}
		if len(f) >= 4 {
			cs.State, cs.Dirs = f[2], strings.Fields(f[3])
		} else {
			cs.Dirs = strings.Fields(f[2])
		}
		services = append(services, cs)
	}
	return services
}

// streamMismatches names the services one side streams and the other
// does not: a parent stream the child does not label stream (its import
// would refuse it, after the rsync and the streams before it), and a
// child stream the parent rsyncs (its import would never run). Refused
// before anything stops, with the way out: a label from a variable the
// parent's inherit: names reaches the child with --inherit, which a
// re-sync only runs after the copy.
func streamMismatches(parent []cli.SyncService, streams []string, child []childService) []string {
	wayOut := " Label it alike on both sides (a variable the parent's inherit: names keeps them so; the child's own shared/.env wins over it)."
	var out []string
	for _, name := range streams {
		i := slices.IndexFunc(child, func(c childService) bool { return c.Name == name })
		switch {
		case i < 0:
			out = append(out, fmt.Sprintf("the parent streams %s (%s=%s) and the child has no service %s", name, cli.SyncLabel, cli.SyncStream, name))
		case child[i].Transfer != cli.SyncStream:
			label := "labels it " + cli.SyncLabel + "=" + child[i].Transfer
			if child[i].Transfer == cli.SyncSkip {
				label = "has no " + cli.SyncLabel + " label on it"
			}
			out = append(out, fmt.Sprintf("the parent streams %s (%s=%s) and the child %s: its import would refuse it.", name, cli.SyncLabel, cli.SyncStream, label)+wayOut)
		}
	}
	for _, c := range child {
		if p, ok := cli.SyncFind(parent, c.Name); ok && c.Transfer == cli.SyncStream && p.Transfer == cli.SyncRsync {
			out = append(out, fmt.Sprintf("the child streams %s (%s=%s) and the parent rsyncs it (%s=%s): its import would never run.", c.Name, cli.SyncLabel, cli.SyncStream, cli.SyncLabel, cli.SyncRsync)+wayOut)
		}
	}
	return out
}

// syncListLabel is a service's label column in --sync-services list:
// rsync, stream or skip, with +stop when it is stopped during a sync.
func syncListLabel(s cli.SyncService) string {
	if s.Stop {
		return s.Transfer + "+stop"
	}
	return s.Transfer
}

// syncConflicts names the child's services the rsync would change under:
// running, with an entry the parent copies among their mounts, and not
// labeled rsync there (which says so on purpose). One stopped by its
// label (devopsy.sync.stop=true) is not running any more; one labeled
// stream or not at all is left running, and the two sides disagree.
func syncConflicts(parent []cli.SyncService, child []childService) []string {
	dirs := cli.SyncDirs(parent)
	if len(dirs) == 0 {
		return nil
	}
	copied := func(d string) bool { return d == "." || dirs[0] == "." || slices.Contains(dirs, d) }
	from := func(d string) string {
		for _, s := range parent {
			if s.Transfer == cli.SyncRsync && (d == "." || slices.Contains(s.Dirs, d) || slices.Contains(s.Dirs, ".")) {
				return s.Name + " (" + cli.SyncLabel + "=" + s.Transfer + ")"
			}
		}
		return "another service"
	}
	var conflicts []string
	for _, c := range child {
		if c.State != "running" && c.State != "restarting" || c.Transfer == cli.SyncRsync {
			continue
		}
		for _, d := range c.Dirs {
			if !copied(d) {
				continue
			}
			label := "labeled " + cli.SyncLabel + "=" + c.Transfer
			if c.Transfer == cli.SyncSkip {
				label = "without a " + cli.SyncLabel + " label"
			}
			conflicts = append(conflicts, fmt.Sprintf("the child's %s is running, %s, and mnt/%s would change under it: the parent copies it for %s. Label it alike on both sides (an inherited variable keeps them so), %s=true to stop it, or %s=%s to copy under a running service.", c.Name, label, d, from(d), cli.SyncStopLabel, cli.SyncLabel, cli.SyncRsync))
			break
		}
	}
	return conflicts
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0
}

func exitCode(err error) int {
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.ExitCode()
	}
	sayErr("devopsy: " + err.Error())
	return 1
}

// rsyncdConf serves the entries of mnt being copied, each as a module of
// its name (`.`, all of mnt, as `mnt`), read-only, as root, so every
// owner's files are read; nothing else under mnt can be asked for (a
// stream database's datadir, an unlabeled service's directory). The daemon
// lives for one copy and publishes no port.
func rsyncdConf(entries, files []string) string {
	conf := "uid = 0\ngid = 0\nuse chroot = yes\nread only = yes\n"
	for _, e := range entries {
		if slices.Contains(files, e) {
			// A module is a directory: mnt, filtered down to the file.
			conf += "[" + e + "]\npath = /mnt\ninclude = /" + e + "\nexclude = *\n"
			continue
		}
		conf += "[" + remote.RsyncModule(e) + "]\npath = " + path.Join("/mnt", e) + "\n"
	}
	return conf
}

// syncRsync copies dirs (entries of the parent's mnt) into the child's,
// replacing what is there (--delete), as root in devopsy's util image.
// Same server: one container with both mounts. Across servers: an rsync
// daemon on this server, reached by the child's server through a reverse forward of
// an SSH session the forwarded agent opens.
func syncRsync(ctx context.Context, projectDir string, dirs, args []string) error {
	toDir, _, err := syncEnv(cli.SyncPushFlag)
	if err != nil {
		return err
	}
	parentMnt, err := filepath.EvalSymlinks(filepath.Join(projectDir, "mnt"))
	if err != nil {
		return fmt.Errorf("the parent's mnt: %w", err)
	}
	// Entries are directories, or files a service bind-mounts alone
	// (./mnt/app.ini): copied into mnt, not as a directory.
	var present, files []string
	for _, d := range dirs {
		info, err := os.Stat(filepath.Join(parentMnt, d))
		if err != nil {
			sayErr("devopsy: mnt/" + d + " does not exist on this server: skipped")
			continue
		}
		present = append(present, d)
		if !info.IsDir() {
			files = append(files, d)
		}
	}
	if len(present) == 0 {
		return nil
	}
	rsync := []string{"rsync", "-a", "--delete", "--numeric-ids"}

	to := os.Getenv(remote.SyncToEnv)
	if to == "" {
		childMnt, err := filepath.EvalSymlinks(path.Join(toDir, "mnt"))
		if err != nil {
			return fmt.Errorf("the child's mnt: %w", err)
		}
		for i, d := range present {
			sayErr("devopsy: rsync mnt/" + d)
			run := append(append([]string{"-v", parentMnt + ":/src:ro", "-v", childMnt + ":/dst", remote.UtilImage}, rsync...), args...)
			name := fmt.Sprintf("devopsy-sync-%d-%d", os.Getpid(), i)
			src, dst := "/src/"+d+"/", "/dst/"+d+"/"
			if slices.Contains(files, d) {
				// In place: a container bind-mounts the file's inode,
				// and a replaced file would leave it reading the old one
				// until recreated.
				src, dst = "/src/"+d, "/dst/"
				run = append(run, "--inplace")
			}
			if err := dockerRun(ctx, name, append(run, src, dst)...); err != nil {
				if ctx.Err() == nil && exitCode(err) == remote.RsyncVanished {
					sayErr("devopsy: rsync mnt/" + d + ": " + remote.RsyncVanishedNotice)
					continue
				}
				sayErr("devopsy: mnt/" + d + " " + remote.HalfCopiedNotice)
				if ctx.Err() != nil {
					return errors.New("interrupted")
				}
				return fmt.Errorf("rsync mnt/%s: %w", d, err)
			}
		}
		return nil
	}

	id, err := dockerOut(ctx, "run", "--rm", "-d", "-v", parentMnt+":/mnt:ro", "-e", "RSYNCD_CONF="+rsyncdConf(present, files), remote.UtilImage,
		"sh", "-c", `printf "%s\n" "$RSYNCD_CONF" > /etc/rsyncd.conf && exec rsync --daemon --no-detach --config=/etc/rsyncd.conf`)
	if err != nil || id == "" {
		return fmt.Errorf("starting the rsync daemon: %v", err)
	}
	// Removed whatever happens, an interrupt included (ctx is cancelled
	// then: not this).
	defer func() {
		trace([]string{"docker", "rm", "-f", id})
		cmd := exec.Command("docker", "rm", "-f", id)
		cmd.Stderr = errOut // stdout: the id again
		_ = cmd.Run()
	}()
	ip, err := dockerOut(ctx, "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", id)
	if err != nil || ip == "" {
		return fmt.Errorf("the rsync daemon's address: %v", err)
	}
	ready := false
	for i := 0; i < 50 && !ready; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if ready = exec.CommandContext(ctx, "docker", "exec", id, "rsync", "rsync://127.0.0.1/").Run() == nil; !ready {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !ready {
		return fmt.Errorf("the rsync daemon did not answer in 5 seconds")
	}

	// A port free on the child's server: ssh fails at once when the
	// forward cannot listen (ExitOnForwardFailure), then another. Never
	// ClearAllForwardings here: it clears command-line forwardings too.
	for attempt := 0; ; attempt++ {
		port := fmt.Sprint(20000 + rand.IntN(40000))
		sshArgs := childSSHArgs(to, remote.SyncReceiveScript(toDir, port, present, files, args), "ExitOnForwardFailure=yes", "-R", "127.0.0.1:"+port+":"+ip+":873")
		cmd := remote.SSHCommandContext(ctx, sshArgs...)
		var stderr bytes.Buffer
		cmd.Stdin = nil
		cmd.Stdout = errOut
		cmd.Stderr = io.MultiWriter(errOut, &stderr)
		err := cmd.Run()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			// The child's script says which entry when rsync fails; cut
			// by the session's end, it says nothing.
			sayErr("devopsy: the copy into " + to + " was cut: an entry " + remote.HalfCopiedNotice)
			return errors.New("interrupted")
		}
		if strings.Contains(stderr.String(), "remote port forwarding failed") {
			if attempt < 2 {
				continue
			}
			// sshd answers a refused forward as it does a taken port.
			return fmt.Errorf("copying into %s: its server refused the reverse forward %d times: a key with `restrict` needs `port-forwarding` too (`restrict,agent-forwarding,port-forwarding`), and sshd's AllowTcpForwarding must allow remote forwards", to, attempt+1)
		}
		return fmt.Errorf("copying into %s: %w", to, err)
	}
}

// cleanEnvKeys are what the child's devopsy gets of the parent's
// environment on the same server: the machine's, and the sync's addresses
// and arguments and SyncWatchEnv (for the child's import), never the
// project's. The
// caller's environment wins over a project's .env, so the parent's
// secrets and compose project must not reach the child's commands.
var cleanEnvKeys = regexp.MustCompile(`^(PATH|HOME|USER|LOGNAME|LANG|LC_[A-Z]+|TERM|TZ|DOCKER_[A-Z_]+|DEVOPSY_VERBOSE|DEVOPSY_HOME|XDG_CONFIG_HOME|` + remote.SyncFromTargetEnv + `|` + remote.SyncToTargetEnv + `|` + remote.SyncArgsEnv + `|` + remote.SyncWatchEnv + `)=`)

// syncExecEnv are the sync's variables the child's devopsy gets across
// servers, as cleanEnvKeys lets them through on the same one.
func syncExecEnv() map[string]string {
	env := map[string]string{}
	for _, k := range []string{remote.SyncFromTargetEnv, remote.SyncToTargetEnv, remote.SyncArgsEnv, remote.SyncWatchEnv} {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	if k, v := childInheritEnv(); k != "" {
		env[k] = v
	}
	return env
}

// childInheritEnv is the child's pending inherited variables' file
// (remote.InheritPendingEnv), from the child's directory the push knows:
// what the holder sets for everything it runs on the child, set for
// --sync-exec too, never for the parent's own reads.
func childInheritEnv() (key, value string) {
	base := os.Getenv(remote.SyncToBaseEnv)
	if base == "" {
		return "", ""
	}
	return remote.InheritPendingEnv, remote.SyncInheritPath(base)
}

func cleanEnv(projectName string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if cleanEnvKeys.MatchString(kv) {
			env = append(env, kv)
		}
	}
	if k, v := childInheritEnv(); k != "" {
		env = append(env, k+"="+v)
	}
	return append(env, "COMPOSE_PROJECT_NAME="+projectName)
}

// runSyncExec implements the hidden --sync-exec <args>...: devopsy args
// in the child's current release, as its project, stdin, stdout and stderr
// through: the push's look at the child's services, and its imports.
// Across servers over SSH (the forwarded agent); on the same one with a
// clean environment.
func runSyncExec(args []string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(os.Stderr, red, "devopsy: "+msg, color)
		return 1
	}
	toDir, toProject, err := syncEnv(cli.SyncExecFlag)
	if err != nil {
		return fail(err.Error())
	}
	if len(args) == 0 {
		return fail("usage: devopsy " + cli.SyncExecFlag + " <devopsy arguments>...")
	}
	if to := os.Getenv(remote.SyncToEnv); to != "" {
		remote.Verbose = cli.VerboseLevel(os.Getenv(cli.VerboseEnv)) > 0
		sshArgs := childSSHArgs(to, remote.SyncExecScript(toDir, toProject, syncExecEnv(), args), "ClearAllForwardings=yes")
		cmd := remote.SSHCommand(sshArgs...)
		err := syscall.Exec(cmd.Path, cmd.Args, os.Environ())
		return fail(err.Error())
	}
	self, err := os.Executable()
	if err != nil {
		return fail(err.Error())
	}
	if err := os.Chdir(path.Dir(toDir)); err != nil {
		return fail(err.Error())
	}
	env := cleanEnv(toProject)
	// The working directory as the shell names it, current and not the
	// release it points to: compose's bind paths go through current.
	env = append(env, "PWD="+path.Dir(toDir))
	trace(append([]string{"devopsy"}, args...))
	err = syscall.Exec(self, append([]string{"devopsy"}, args...), env)
	return fail(err.Error())
}

// runSyncServices implements the hidden --sync-services, in the child's
// current release (its holder, and the push through --sync-exec):
//   - prepare: checks a stream service has the import to receive it, runs
//     the sync capability's prepare child, then stops the services
//     labeled devopsy.sync.stop=true;
//   - finish go|abort: starts those again and waits until they are
//     healthy, removes what the streams left on abort, then runs finish
//     child go|abort;
//   - import <service>: the stream's spool into the service (the
//     capability's import), the spool removed whatever happens;
//   - list: name, label, state and entries of mnt, one service per line.
func runSyncServices(cwd string, args []string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(errOut, red, "devopsy: "+msg, color)
		return 1
	}
	usage := "usage: devopsy " + cli.SyncServicesFlag + " prepare | finish go|abort | import <service> | list"
	switch {
	case len(args) == 1 && (args[0] == "prepare" || args[0] == "list"):
	case len(args) == 2 && args[0] == "finish" && (args[1] == "go" || args[1] == "abort"):
	case len(args) == 2 && args[0] == "import":
	default:
		return fail(usage)
	}
	projectDir, services, err := cli.SyncServices(cwd, os.Environ())
	if err != nil {
		return fail(err.Error())
	}
	env := syncHookEnv(services)
	base := filepath.Dir(cwd)
	switch args[0] {
	case "list":
		states, err := serviceStates(cwd)
		if err != nil {
			return fail(err.Error())
		}
		for _, s := range services {
			fmt.Printf("%s\t%s\t%s\t%s\n", s.Name, syncListLabel(s), cmp.Or(states[s.Name], "none"), strings.Join(s.Dirs, " "))
		}
		return 0
	case "import":
		return syncImport(cwd, projectDir, base, args[1], services, env, fail)
	}
	// A session gone is no reason to die halfway: the holder restores
	// what this stopped.
	signal.Ignore(syscall.SIGPIPE, syscall.SIGHUP)
	if args[0] == "prepare" {
		// The child refuses before anything stops anywhere: its holder
		// runs this before the parent's push starts.
		// The record first: whatever fails from here, finish abort then
		// restores only what this sync stopped. A child without any
		// container is on its first release, said in the record now:
		// imports create containers before finish looks.
		states, err := serviceStates(cwd)
		if err != nil {
			return fail(err.Error())
		}
		if err := startSyncState(cwd, len(states) == 0); err != nil {
			return fail(err.Error())
		}
		if streams, _ := cli.SyncStreamServices(services); len(streams) > 0 && !isExecutable(syncHookFile(projectDir, "import")) {
			return fail(fmt.Sprintf("%s labeled %s=%s: the child's release needs an executable .devopsy/capabilities/sync/import", strings.Join(streams, ", "), cli.SyncLabel, cli.SyncStream))
		}
		if err := runSyncHook(context.Background(), cwd, projectDir, env, nil, nil, "prepare", "child"); err != nil {
			return fail("the sync capability's prepare child: " + err.Error())
		}
		if err := stopServices(cwd, services); err != nil {
			return fail(err.Error())
		}
		return 0
	}
	code := 0
	if err := restoreServices(cwd, services); err != nil {
		code = fail(err.Error())
	}
	if args[1] == "abort" {
		spools, _ := filepath.Glob(filepath.Join(base, remote.StreamSpoolPrefix+"*"))
		for _, f := range spools {
			_ = os.Remove(f)
		}
	}
	if err := runSyncHook(context.Background(), cwd, projectDir, env, nil, nil, "finish", "child", args[1]); err != nil {
		code = fail("the sync capability's finish child " + args[1] + ": " + err.Error())
	}
	return code
}

// syncImport is --sync-services import <name>: the spool the parent's
// export wrote (complete: the push only calls this after the export
// exited 0) into the child's service, through the sync capability's
// import. The service is brought up unless it is stopped for the sync.
// The spool goes whatever happens, an interrupt included.
func syncImport(cwd, projectDir, base, name string, services []cli.SyncService, env []string, fail func(string) int) (code int) {
	spool := remote.StreamSpool(base, name)
	defer os.Remove(spool)
	ctx, stop := interruptible()
	defer stop()
	if os.Getenv(remote.SyncWatchEnv) != "" {
		ctx = watchSession(ctx)
	}
	s, ok := cli.SyncFind(services, name)
	if !ok || s.Transfer != cli.SyncStream {
		return fail(fmt.Sprintf("the child's %s is not labeled %s=%s: the parent's export of it has nowhere to go", name, cli.SyncLabel, cli.SyncStream))
	}
	f, err := os.Open(spool)
	if err != nil {
		return fail(fmt.Sprintf("the spool for %s: %v", name, err))
	}
	defer f.Close()
	if !s.Stop {
		// A first release (the record says so) keeps it up, for the
		// finish hook and the run step; otherwise one down before the
		// sync goes down again.
		down, err := upFor(ctx, cwd, name, recordedFirst(cwd))
		if err != nil {
			return fail(err.Error())
		}
		defer func() {
			if err := down(); err != nil {
				code = fail(err.Error())
			}
		}()
	}
	if err := runSyncHook(ctx, cwd, projectDir, env, f, nil, "import", name); err != nil {
		code = fail("the sync capability's import " + name + ": " + err.Error())
	}
	return code
}

// upFor brings name up (up -d --wait) for an import and returns what puts
// it back as it was: stopped again when it was not running before (a
// service down before stays down). keep leaves it up whatever it was (a
// child's first release).
func upFor(ctx context.Context, cwd, name string, keep bool) (down func() error, err error) {
	down = func() error { return nil }
	states, err := serviceStates(cwd)
	if err != nil {
		return down, err
	}
	if err := composeContext(ctx, cwd, errOut, "up", "-d", "--wait", name); err != nil {
		return down, fmt.Errorf("docker compose up %s: %w", name, err)
	}
	if states[name] == "running" || keep {
		return down, nil
	}
	return func() error {
		sayErr("devopsy: stopping " + name + " again: it was down before")
		if err := compose(cwd, errOut, "stop", name); err != nil {
			return fmt.Errorf("docker compose stop: %w", err)
		}
		return nil
	}, nil
}

// compose runs docker compose args in the project at cwd, with its
// environment.
func compose(cwd string, stdout io.Writer, args ...string) error {
	return composeContext(context.Background(), cwd, stdout, args...)
}

// composeContext is compose, ended with ctx.
func composeContext(ctx context.Context, cwd string, stdout io.Writer, args ...string) error {
	plan, err := cli.Build(cwd, append([]string{"--"}, args...), os.Environ())
	if err != nil {
		return err
	}
	trace(plan.Args)
	cmd := exec.CommandContext(ctx, "docker", plan.Args[1:]...)
	cmd.Env = plan.Env
	cmd.Stdout, cmd.Stderr = stdout, errOut
	return cmd.Run()
}

// composeServices lists the services docker compose ps prints for args.
// servicesIn are the services whose container is in one of states
// (serviceStates: one docker compose ps), sorted.
func servicesIn(cwd string, states ...string) ([]string, error) {
	all, err := serviceStates(cwd)
	if err != nil {
		return nil, err
	}
	var names []string
	for name, state := range all {
		if slices.Contains(states, state) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// upServices are the services running or restarting: a container in a
// restart loop (a database on a datadir an aborted copy left half
// written) is up as far as a copy is concerned.
func upServices(cwd string) ([]string, error) {
	return servicesIn(cwd, "running", "restarting")
}

func intersect(a, b []string) []string {
	var out []string
	for _, s := range a {
		if slices.Contains(b, s) {
			out = append(out, s)
		}
	}
	return out
}

// stopServices stops the running services labeled
// devopsy.sync.stop=true, in one docker compose stop, so Compose orders
// them by depends_on (dependents first), and records them first
// (syncState): restoreServices brings back only those.
func stopServices(cwd string, services []cli.SyncService) error {
	stop := cli.SyncStopped(services)
	if len(stop) == 0 {
		return nil
	}
	// The record exists before anything can fail (startSyncState).
	rec, _ := readSyncRecord(cwd)
	if err := rec.write(cwd); err != nil {
		return err
	}
	// The guard on the parent's side counts a restarting one as running:
	// stop it too.
	up, err := upServices(cwd)
	if err != nil {
		return err
	}
	stop = intersect(stop, up)
	for _, name := range stop {
		if !slices.Contains(rec.stops, name) {
			rec.stops = append(rec.stops, name)
		}
	}
	if err := rec.write(cwd); err != nil {
		return err
	}
	if len(stop) == 0 {
		return nil
	}
	sayErr("devopsy: stopping " + strings.Join(stop, " "))
	if err := compose(cwd, errOut, append([]string{"stop"}, stop...)...); err != nil {
		return fmt.Errorf("docker compose stop: %w", err)
	}
	return nil
}

// syncFirst is the record's line for a child's first release: it had no
// container at all when the sync started.
const syncFirst = "first"

// syncRecord is the record a sync keeps (syncState): whether it started
// on a child's first release (no container at all: syncFirst) and the
// services it stopped ("stop <name>" lines).
type syncRecord struct {
	first bool
	stops []string
}

// readSyncRecord reads the record; ok is false without one.
func readSyncRecord(cwd string) (rec syncRecord, ok bool) {
	state, err := os.ReadFile(filepath.Join(filepath.Dir(cwd), syncState))
	if err != nil {
		return rec, false
	}
	for _, line := range strings.Split(string(state), "\n") {
		if line == syncFirst {
			rec.first = true
		} else if action, name, ok := strings.Cut(line, " "); ok && action == "stop" && !slices.Contains(rec.stops, name) {
			rec.stops = append(rec.stops, name)
		}
	}
	return rec, true
}

// write replaces the record whole.
func (rec syncRecord) write(cwd string) error {
	var b strings.Builder
	if rec.first {
		b.WriteString(syncFirst + "\n")
	}
	for _, name := range rec.stops {
		b.WriteString("stop " + name + "\n")
	}
	path := filepath.Join(filepath.Dir(cwd), syncState)
	if err := os.WriteFile(path+".tmp", []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("recording the services to restore: %w", err)
	}
	if err := os.Rename(path+".tmp", path); err != nil {
		return fmt.Errorf("recording the services to restore: %w", err)
	}
	return nil
}

// startSyncState makes sure a record exists (syncState) before a sync's
// prepare changes or asks anything: without one, restoring brings back
// every service labeled to stop, a service down on purpose included. The
// stops of one left by an earlier sync that could not restore stay: their
// services come back with this one's. first is what the sync finds now:
// an earlier first release's mark goes once containers exist.
func startSyncState(cwd string, first bool) error {
	rec, _ := readSyncRecord(cwd)
	rec.first = first
	return rec.write(cwd)
}

// recordedFirst reports whether the record says the sync started on a
// first release (syncFirst).
func recordedFirst(cwd string) bool {
	rec, _ := readSyncRecord(cwd)
	return rec.first
}

// healthyTimeout is how long restoreServices waits for a started service.
const healthyTimeout = 5 * time.Minute

// restoreServices starts the stopped services labeled
// devopsy.sync.stop=true and waits until they run and are healthy,
// whatever happened before: those stopServices recorded (syncState), else
// every one. It reads the containers' state, so it also recovers after an
// interrupted sync.
func restoreServices(cwd string, services []cli.SyncService) error {
	stop := cli.SyncStopped(services)
	var errs []error
	// A child's first release had no containers: nothing was stopped,
	// and the finish hook expects the labeled services up (an app to
	// sanitize through). Create every one, with one up, as the run step's
	// up would a moment later; only then: a service without a container
	// in a project that has others was removed on purpose. The record
	// says so (imports created some since); without one, no containers.
	rec, recorded := readSyncRecord(cwd)
	first := rec.first
	if !recorded {
		states, err := serviceStates(cwd)
		if err != nil {
			return err
		}
		first = len(states) == 0
	}
	var create []string
	if first {
		for _, s := range services {
			if s.Labeled {
				create = append(create, s.Name)
			}
		}
	}
	if len(create) > 0 {
		sayErr("devopsy: creating " + strings.Join(create, " "))
		if err := compose(cwd, errOut, append([]string{"up", "-d", "--wait"}, create...)...); err != nil {
			errs = append(errs, fmt.Errorf("docker compose up: %w", err))
		}
	}
	stateFile := filepath.Join(filepath.Dir(cwd), syncState)
	if recorded {
		stop = intersect(stop, rec.stops)
	}
	if len(stop) > 0 {
		exited, err := servicesIn(cwd, "exited")
		if err != nil {
			errs = append(errs, err)
		}
		if start := intersect(stop, exited); len(start) > 0 {
			sayErr("devopsy: starting " + strings.Join(start, " "))
			if err := compose(cwd, errOut, append([]string{"start"}, start...)...); err != nil {
				errs = append(errs, fmt.Errorf("docker compose start: %w", err))
			} else if err := waitHealthy(cwd, start); err != nil {
				errs = append(errs, err)
			}
		}
	}
	if len(errs) == 0 {
		_ = os.Remove(stateFile)
	}
	return errors.Join(errs...)
}

// psEntry is a container in docker compose ps --format json.
type psEntry struct {
	Service, State, Health, Status string
}

// parsePS reads docker compose ps --format json: one object per line, or
// an array (older compose).
func parsePS(out []byte) ([]psEntry, error) {
	out = bytes.TrimSpace(out)
	var entries []psEntry
	if len(out) == 0 {
		return nil, nil
	}
	if out[0] == '[' {
		err := json.Unmarshal(out, &entries)
		return entries, err
	}
	for _, line := range bytes.Split(out, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e psEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// serviceStates is the state of each service's container (docker compose
// ps: running, paused, exited...), a running one winning when a service
// has several.
func serviceStates(cwd string) (map[string]string, error) {
	var out bytes.Buffer
	if err := compose(cwd, &out, "ps", "--all", "--format", "json"); err != nil {
		return nil, fmt.Errorf("docker compose ps: %w", err)
	}
	entries, err := parsePS(out.Bytes())
	if err != nil {
		return nil, fmt.Errorf("docker compose ps: %w", err)
	}
	states := map[string]string{}
	for _, e := range entries {
		if states[e.Service] != "running" {
			states[e.Service] = e.State
		}
	}
	return states, nil
}

// waitHealthy waits until every container of services runs and, when it
// has a healthcheck, is healthy.
func waitHealthy(cwd string, services []string) error {
	deadline := time.Now().Add(healthyTimeout)
	for {
		var out bytes.Buffer
		if err := compose(cwd, &out, append([]string{"ps", "--all", "--format", "json"}, services...)...); err != nil {
			return fmt.Errorf("docker compose ps: %w", err)
		}
		entries, err := parsePS(out.Bytes())
		if err != nil {
			return fmt.Errorf("docker compose ps: %w", err)
		}
		// Every service must have a container: one gone cannot be started.
		ready := true
		for _, name := range services {
			if !slices.ContainsFunc(entries, func(e psEntry) bool { return e.Service == name }) {
				ready = false
			}
		}
		for _, e := range entries {
			switch {
			case e.State == "exited" || e.State == "dead" || e.Health == "unhealthy":
				return fmt.Errorf("%s is %s after starting", e.Service, cmp.Or(e.Health, e.State))
			case e.State != "running" || (e.Health != "" && e.Health != "healthy"):
				ready = false
			}
		}
		if ready {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not healthy after %s", strings.Join(services, " "), healthyTimeout)
		}
		time.Sleep(time.Second)
	}
}

// agentHasKeys asks the local agent for its keys before anything starts:
// SSH_AUTH_SOCK set is not an agent (macOS always sets it, CI may leave a
// stale one), and a parent with a forwarded agent holding no key would
// only find out after the child's services were stopped. A missing
// ssh-add is not an error: ssh itself reports later.
func agentHasKeys(parent, child string) error {
	err := exec.Command("ssh-add", "-l").Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return nil
	}
	switch exit.ExitCode() {
	case 1:
		return fmt.Errorf("the parent's server (%s) reaches the child's (%s) with your SSH agent, forwarded for the sync, and it holds no key: ssh-add the key %s accepts (in CI, each server's CI key)", parent, child, child)
	case 2:
		return fmt.Errorf("the parent's server (%s) reaches the child's (%s) with your SSH agent, forwarded for the sync, and none answers at SSH_AUTH_SOCK (%s): eval $(ssh-agent); ssh-add (in CI, add the key to an agent)", parent, child, os.Getenv("SSH_AUTH_SOCK"))
	}
	return nil
}
