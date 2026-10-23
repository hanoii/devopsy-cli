package main

import (
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/compose-spec/compose-go/v2/dotenv"

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
	// id is the sync's (remote.SyncIDEnv), chosen here: its pending
	// inherited variables are written before the holder runs.
	id string
	// reset wipes the child at the copy's first write (--reset).
	reset bool
}

// staleInherit is the age past which another sync's pending inherited
// variables are a leftover: longer than any wait for the release lock.
const staleInherit = time.Hour

// newSyncID is a sync's id: the time, then random hex, so two syncs never
// share one, from one operator or two.
func newSyncID() string {
	return fmt.Sprintf("%d.%06x", time.Now().Unix(), rand.IntN(1<<24))
}

// syncPreflight checks a sync of p into c can start, before anything
// changes: c is a child with a run step, p another environment of the same
// project, released; across servers, an SSH agent to forward and a child
// the parent's server can reach.
func syncPreflight(c, p *remote.Target, projectName string, args []string, reset bool) (*syncJob, error) {
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
	job := &syncJob{child: c, parent: p, projectName: projectName, parentName: p.ComposeName(), args: args, id: newSyncID(), reset: reset}
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
	// The parent's devopsy must have what the push needs, checked in this
	// session already open, before the holder stops anything on the
	// child or a first release uploads.
	var out bytes.Buffer
	var pushEnv map[string]string
	if reset {
		pushEnv = map[string]string{remote.SyncResetEnv: "1"}
	}
	code, err := remote.SSH(p, remote.CurrentExistsScript(p, remote.Needs(remote.PushScript(p, job.parentName, pushEnv))), bytes.NewReader(nil), &out, false)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("could not check the parent @%s (%d)", p.Address, code)
	}
	cur := remote.ParseCurrent(out.String())
	switch {
	case !cur.Current:
		return nil, fmt.Errorf("the parent @%s (%s:%s) has no current release: release it first", p.Address, p.Host, p.Path)
	case cur.Importing:
		return nil, fmt.Errorf("an import into the parent @%s (%s:%s) is running: sync once it is done (devopsy @%s --status shows it)", p.Address, p.Host, p.Path, targetRef(p))
	case cur.Mark.IsImport():
		return nil, fmt.Errorf("the import of %s into the parent @%s (%s:%s) did not complete: import it again, or devopsy @%s --import %s --resolved, then sync", cur.Mark.From, p.Address, p.Host, p.Path, targetRef(p), cur.Mark.From)
	case cur.Mark != nil:
		return nil, fmt.Errorf("the parent @%s (%s:%s) holds data from a sync that did not complete: sync it first (devopsy @%s --sync)", p.Address, p.Host, p.Path, targetRef(p))
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
// with a sync when release) on the child's server; once it prints the
// marker, the push session on the parent's, then go or abort to the
// holder. The holder's exit code is the result.
func runSyncSession(job *syncJob, script string, release bool, rlog *releaseLog, color bool) int {
	log := report.Default().Tee(rlog.Line)
	partial := func() {
		log.Warn("@"+job.child.Address+" may hold a partial copy now", "devopsy @"+targetRef(job.child)+" --status says, and --sync completes it")
	}
	// An *os.File, so the holder's ssh reads it directly: devopsy writes go
	// or abort, and nothing waits on a copy if the holder ends first.
	r, w, err := os.Pipe()
	if err != nil {
		log.Fail(err)
		return 1
	}
	defer r.Close()
	defer w.Close()
	// The marker is taken out of stderr (holders print on one stream)
	// before the lines are colored for the terminal: the holder's
	// streams are read here, not in SSHContext.
	var out, errOut io.Writer = os.Stdout, os.Stderr
	var colored []*report.ColorWriter
	if color {
		o, e := report.Colorize(os.Stdout), report.Colorize(os.Stderr)
		colored = append(colored, o, e)
		out, errOut = o, e
	}
	// The marker comes on stderr: holders print on one stream.
	marker := make(chan string, 1)
	lm := &remote.LineMarker{Prefix: remote.SyncMarker, Found: func(rest string) { marker <- rest }}
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
	// Beats until the answer: the holder's watcher passes on none of
	// them.
	stopBeat := heartbeat(w)
	defer stopBeat()
	// Ended by devopsy only: before the marker on an interrupt, and when
	// devopsy returns while it still runs (a second Ctrl-C).
	holderCtx, endHolder := context.WithCancel(context.Background())
	defer endHolder()
	go func() {
		code, err := remote.SSHStreams(remote.WithBeat(remote.WithoutInterrupt(holderCtx), w), job.child, remote.KeepAlive, remote.HeartbeatLine()+script, r, out, mwErr, false, rlog)
		done <- result{code, err}
	}()
	// Interrupted before the marker, devopsy answers abort at once and
	// closes the holder's stdin, then waits for it, as for a release:
	// waiting for the lock, its watcher ends it; past it, it reads the
	// abort after its prepare and restores in this session; a release
	// before its switch is cancelled by its watcher. A second interrupt
	// ends the session. After the marker, the push ends first, so
	// nothing copies into the child once its services are back, then
	// the holder gets abort.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	// What a lost connection leaves depends on how far the holder got:
	// before the marker nothing copied (a release that syncs may have
	// switched, and goes back), after the answer the run step is the
	// server's.
	marked, answered := false, false
	// The push's exit when it failed after a copy that completed (the
	// parent's services not back): the child goes on, the command fails.
	pushFailed := 0
	var res result
	select {
	case res = <-done:
	case <-sigs:
		if release {
			log.Error("interrupted: cancelling the release on @" + job.child.Address + ": it does not go live, and past its switch the previous release, if any, comes back")
		} else {
			log.Error("interrupted: aborting the sync into @" + job.child.Address + ", which comes back as it was")
		}
		stopBeat()
		_, _ = w.WriteString("abort\n")
		w.Close()
		slow := time.After(5 * time.Second)
		for {
			select {
			case <-done:
				flush()
				return 130
			case <-slow:
				// The holder reads the abort once its prepare ends, and
				// then restores what it stopped.
				log.Warn("the server is cancelling it, after the step it is in, and restores what it stopped", "Ctrl-C again leaves now")
			case <-sigs:
				endHolder()
				<-done
				flush()
				return 130
			}
		}
	case found := <-marker:
		marked = true
		base, id, _ := strings.Cut(found, "\t")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		pushed := make(chan int, 1)
		go func() { pushed <- runPush(ctx, job, base, id, rlog, color) }()
		var code int
		interrupted := false
		select {
		case code = <-pushed:
		case <-sigs:
			interrupted = true
			log.Error("interrupted: stopping the copy from @" + job.parent.Address + ", then aborting the sync")
			cancel()
			<-pushed
		case res = <-done:
			// The holder is gone (its server's connection dropped, its
			// session killed): it decided on its way out (or whatever
			// takes the child's lock next does), so nothing may copy
			// into the child any more, and nobody is left to answer.
			why := fmt.Sprint(res.code)
			if res.err != nil {
				why = res.err.Error()
			}
			log.Error("the session on @" + job.child.Address + " ended during the copy (" + why + "): stopping the copy from @" + job.parent.Address)
			cancel()
			<-pushed
			flush()
			partial()
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
			log.Error(fmt.Sprintf("the copy from @%s failed (%d): aborting the sync", job.parent.Address, code))
		}
		stopBeat()
		_, _ = w.WriteString(answer)
		w.Close()
		answered = !interrupted && code == 0
		if !interrupted {
			pushFailed = code
		}
		if interrupted {
			// The holder's ending is worth seeing, unless interrupted
			// again: a copy that had completed goes on there.
			select {
			case res = <-done:
				flush()
				if res.err == nil && res.code == 0 {
					// The copy had completed: the sync went on.
					rlog.Line(remote.SyncCopied + job.parent.Address)
					return 0
				}
			case <-sigs:
				flush()
			}
			return 130
		}
		// The run step is the server's now: an interrupt here only cuts
		// the session, and the step goes on there (remote's followOutput).
		// Say so once; a second one cuts it.
		select {
		case res = <-done:
		case <-sigs:
			log.Warn("interrupted: the run step goes on on @"+job.child.Address, "devopsy @"+targetRef(job.child)+" --log shows how it ends; Ctrl-C again ends the session, not the step")
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
		log.Fail(res.err)
		partial()
		return 1
	}
	if res.code == 0 && pushFailed != 0 {
		log.Error(fmt.Sprintf("the copy into @%s had completed and it went on, but the push on @%s failed (%d): check its services", job.child.Address, job.parent.Address, pushFailed), "devopsy @"+targetRef(job.parent)+" --status shows them")
		return 1
	}
	if res.code == sshLost {
		lost := "the connection to @" + job.child.Address + " was lost"
		switch {
		case !marked && release:
			log.Error(lost+" before the copy: the server cancels the release once it notices, within two minutes, and past its switch the previous release, if any, comes back", "devopsy @"+targetRef(job.child)+" --releases shows it")
		case !marked:
			log.Error(lost+" before the copy: the server leaves it as it was once it notices", "devopsy @"+targetRef(job.child)+" --status shows it")
		case answered:
			log.Error(lost+" after the copy: the server goes on as far as the copy went, the run step too if it went through", "devopsy @"+targetRef(job.child)+" --log shows how it ends")
		default:
			log.Error(lost+": the server finishes the sync as far as its copy went", "devopsy @"+targetRef(job.child)+" --status says how it ended")
		}
	}
	return res.code
}

// sshLost is ssh's exit code when the connection failed or was lost.
const sshLost = 255

// runPush is the push session: devopsy --sync-push on the parent's
// server, into the child's directory base. Across servers it forwards the
// SSH agent, so the parent's server reaches the child's for this session
// only.
func runPush(ctx context.Context, job *syncJob, base, id string, rlog *releaseLog, color bool) int {
	p := job.parent
	env := map[string]string{
		remote.SyncToEnv:         job.to,
		remote.SyncSSHPortEnv:    job.port,
		remote.SyncToBaseEnv:     base,
		remote.SyncIDEnv:         id,
		remote.SyncToDirEnv:      base + "/current/.devopsy",
		remote.SyncToProjectEnv:  job.projectName,
		remote.SyncToTargetEnv:   job.child.Address,
		remote.SyncArgsEnv:       strings.Join(job.args, " "),
		remote.SyncFromTargetEnv: p.Address,
		remote.SyncWatchEnv:      "1",
		remote.HeartbeatEnv:      fmt.Sprint(int(remote.HeartbeatEvery().Seconds())),
	}
	if job.reset {
		env[remote.SyncResetEnv] = "1"
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
	log := report.Default().Tee(rlog.Line)
	log.Info(fmt.Sprintf("copying @%s (%s:%s) into @%s, %s", p.Address, p.Host, p.Path, job.child.Address, where))
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
	defer heartbeat(w)()
	code, err := remote.SSHContext(remote.WithoutInterrupt(ctx), p, extra, remote.PushScript(p, job.parentName, env), r, nil, false, rlog)
	if err != nil {
		log.Fail(err)
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
	log := report.Default().Tee(rlog.Line)
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
	// Gated: the variables go once the child holds its release lock.
	pendingID := ""
	if pending {
		pendingID = job.id
	}
	code, err = remote.SSHContext(remote.WithGate(ctx), c, nil, remote.InheritWriteScript(c, keys, pendingID), strings.NewReader(remote.GatedInput(lines.String())), &res, false, nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("could not write @%s's inherited variables (%d)", c.Address, code)
	}
	switch {
	case len(keys) > 0 && pending:
		log.Info(fmt.Sprintf("inherited %s from @%s for @%s's sync (its shared/%s once the copy is in)", strings.Join(keys, ", "), p.Address, c.Address, remote.InheritFile))
	case len(keys) > 0:
		log.Info(fmt.Sprintf("inherited %s from @%s into @%s's shared/%s", strings.Join(keys, ", "), p.Address, c.Address, remote.InheritFile))
	}
	if shadowed := strings.Fields(res.String()); len(shadowed) > 0 {
		log.Warn(fmt.Sprintf("@%s's own shared .env also sets %s, which wins there", c.Address, strings.Join(shadowed, ", ")), "to inherit it:", report.CmdMark+"devopsy @"+targetRef(c)+" --vars unset "+strings.Join(shadowed, " "))
	}
	if len(missing) > 0 {
		log.Warn(fmt.Sprintf("@%s does not set %s (its inherit: in %s): not inherited", p.Address, strings.Join(missing, ", "), cmp.Or(p.File, "the project's config")))
	}
	return nil
}

// runInherit implements devopsy @<child> --inherit: the parent's inherited
// variables into the child, nothing copied, no run step.
func runInherit(t *remote.Target, projectName string, args []string, color bool) int {
	fail := func(msg string) int {
		report.Error(msg)
		return 1
	}
	if len(args) > 0 {
		return fail("usage: devopsy @" + targetRef(t) + " --inherit")
	}
	if t.Parent == "" {
		return fail(fmt.Sprintf("@%s is not a child: no parent: in its config", t.Address))
	}
	if t.ParentErr != nil {
		report.Fail(t.ParentErr)
		return 1
	}
	p := t.ParentTarget
	if len(p.Inherit) == 0 {
		report.Warn(fmt.Sprintf("@%s inherits nothing: no inherit: on %s", t.Address, p.Name))
		return 0
	}
	for _, x := range []*remote.Target{p, t} {
		var out bytes.Buffer
		code, err := remote.SSH(x, remote.CurrentExistsScript(x, nil), bytes.NewReader(nil), &out, false)
		if err != nil {
			return fail(err.Error())
		}
		if code != 0 {
			return fail(fmt.Sprintf("could not check @%s (%d)", x.Address, code))
		}
		if !remote.ParseCurrent(out.String()).Current {
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
func runSync(t *remote.Target, projectDir string, projectEnv func(string) (string, bool), projectName string, reset bool, args []string, color bool) int {
	fail := func(msg string) int {
		report.Error(msg)
		return 1
	}
	flag := "--sync"
	if reset {
		flag = "--reset"
	}
	usage := "usage: devopsy @" + targetRef(t) + " " + flag + " [--from @<address>] [--yes] [-- <rsync args>...]"
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
		report.Fail(t.ParentErr, report.Fix("sync from an address instead:", "devopsy @"+targetRef(t)+" "+flag+" --from "+parentGuess(t)))
		return 1
	}
	if from != "" {
		var err error
		if p, err = remote.LoadTarget(projectDir, from, projectEnv); err != nil {
			return fail("--from: " + err.Error())
		}
	}
	job, err := syncPreflight(t, p, projectName, rsyncArgs, reset)
	if err != nil {
		return fail(err.Error())
	}
	if !yes {
		confirm := confirmSync(t, p, "reruns its release's run step there", "nothing synced")
		if reset {
			confirm = typeToGoOn(fmt.Sprintf("this throws away all of @%s's data (%s:%s: every container and volume, all of shared/mnt; shared/.env stays) and copies @%s's (%s:%s) into it, then reruns its release's run step there", t.Address, t.Host, t.Path, p.Address, p.Host, p.Path), confirmWord(t), "nothing reset", false)
		}
		if err := confirm; err != nil {
			report.Fail(err)
			return 1
		}
	}

	rlog := newReleaseLog("sync", t)
	log := report.Default().Tee(rlog.Line)
	verb := "syncing"
	if reset {
		verb = "resetting from"
	}
	log.Info(fmt.Sprintf("%s @%s into @%s (%s:%s)", verb, p.Address, t.Address, t.Host, t.Path))
	// The parent's variables first, into the child's pending file: the
	// holder reads its labels from them; on go they move into place.
	if err := inheritVars(context.Background(), job, rlog, color, true); err != nil {
		log.Fail(fmt.Errorf("inherit: %w: nothing synced", err))
		return 1
	}
	o := &remote.SyncOptions{From: p.Address, Args: rsyncArgs, Inherit: len(p.Inherit) > 0, ID: job.id}
	code := runSyncSession(job, remote.HolderScript(t, projectName, o, len(remote.StepArgs(t.Release.Run)) > 0), false, rlog, color)

	// The log goes with the release syncd into, after its own.
	rlog.Line(report.Line(report.InfoLevel, fmt.Sprintf("sync exited %d", code)))
	text := rlog.Text()
	if m := remote.SyncingInto.FindSubmatch(text); m != nil {
		text = append([]byte("\n"), text...)
		if c, err := remote.SSHWith(t, remote.Bounded, remote.LogSaveScript(t, string(m[1]), true), bytes.NewReader(text), io.Discard, false, nil); err != nil || c != 0 {
			report.Warn(fmt.Sprintf("could not save the sync log on the server (%v, %d)", err, c))
		}
	}
	return code
}

// confirmSync asks for the child's environment name before its data is
// replaced, like --destroy; then says what follows the copy. Not at a
// terminal it fails: --yes is the way there.
func confirmSync(t, p *remote.Target, then, nothing string) error {
	where := t.Host + ":" + t.Path
	return typeToGoOn(fmt.Sprintf("this replaces the data of @%s (%s, shared/mnt) with @%s's (%s:%s), then %s", t.Address, where, p.Address, p.Host, p.Path, then),
		confirmWord(t), nothing, false)
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
// holder does on the child when nobody reads the session any more: the
// caller says why, after it.
func (q *quietWriter) keep(base string) {
	trimSyncLog(filepath.Join(base, syncLog))
	f, err := os.OpenFile(filepath.Join(base, syncLog), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	q.mu.Lock()
	q.file = f
	q.mu.Unlock()
}

// trimSyncLog keeps the last remote.SyncLogKeep lines of the log at
// path once it grew past remote.SyncLogMax: every sync whose session
// went appends to it.
func trimSyncLog(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() < remote.SyncLogMax {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	lines := strings.SplitAfter(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) > remote.SyncLogKeep {
		lines = lines[len(lines)-remote.SyncLogKeep:]
	}
	if os.WriteFile(path+".tmp", []byte(strings.Join(lines, "")+"\n"), 0o644) == nil {
		_ = os.Rename(path+".tmp", path)
	}
}

// syncState records, next to syncLog, the services a sync or an import
// stopped ("stop db" lines), so restoring brings back those and leaves a
// service that was down before down (a maintenance stop). Written before
// the first stop, removed by a restore that brought everything back;
// absent, a sync restores its side's set (cli.DataStops), an import or
// --resolved nothing (its mark and record are written together).
const syncState = remote.SyncStateFile

// syncLog is the holder's and the push's log for what happened after the
// operator's devopsy was gone, in the environment's directory.
const syncLog = ".devopsy-sync.log"

// trace says a command devopsy runs on a server, at debug level.
func trace(args []string) { report.Debug("running '" + strings.Join(args, " ") + "'") }

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
// Without it, a silent copy would only notice at its next write. With
// devopsy's heartbeat, a silence (heartbeatQuiet) means it too: a
// connection that died silently.
func watchSession(ctx context.Context) context.Context {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		_, _ = readQuiet(os.Stdin, heartbeatQuiet(), func([]byte) error { return nil })
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

// ensureUtilImage gets devopsy's util image ready on this host: built
// when missing, which fails what needs it when the build does; rebuilt,
// its base pulled and nothing cached, once older than
// remote.UtilImageMaxAge, so what runs as root on the data gets alpine's
// fixes. A rebuild that fails only warns: the image there still works,
// and a registry out of reach must not stop a sync, an import or
// --destroy; the next use tries again. Two at once on a host are
// harmless: a build tags the image only once complete, and the image a
// rebuild replaced is removed without -f, so one a container still runs
// stays.
func ensureUtilImage(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "-f", "{{.Id}} {{.Created}}", remote.UtilImage).Output()
	if err != nil {
		report.Info("building " + remote.UtilImage)
		if err := buildUtilImage(ctx, false); err != nil {
			return fmt.Errorf("building %s: %w", remote.UtilImage, err)
		}
		return nil
	}
	id, created, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	// A date Docker writes otherwise counts as old: a rebuild too many
	// costs a minute, never a stale image.
	if t, err := time.Parse(time.RFC3339Nano, created); err == nil && time.Since(t) < remote.UtilImageMaxAge {
		return nil
	}
	report.Info("updating " + remote.UtilImage + ", built over a week ago")
	if err := buildUtilImage(ctx, true); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		report.Warn("updating "+remote.UtilImage+" failed: going on with the one there", err.Error())
		return nil
	}
	now, err := exec.CommandContext(ctx, "docker", "image", "inspect", "-f", "{{.Id}}", remote.UtilImage).Output()
	if now := strings.TrimSpace(string(now)); err == nil && id != "" && now != "" && now != id {
		// Untagged now; a container still running it keeps it.
		_ = exec.CommandContext(ctx, "docker", "image", "rm", id).Run()
	}
	return nil
}

// buildUtilImage builds remote.UtilImage from its Dockerfile, refresh as
// remote.BuildUtilImageArgs says.
func buildUtilImage(ctx context.Context, refresh bool) error {
	cmd := exec.CommandContext(ctx, "docker", remote.BuildUtilImageArgs(refresh)...)
	cmd.Stdin = strings.NewReader(strings.Join(remote.UtilDockerfile, "\n") + "\n")
	cmd.Stdout, cmd.Stderr = io.Discard, errOut // stdout: the image's id
	return cmd.Run()
}

// runUtilImage is the hidden remote.UtilImageFlag: ensureUtilImage, for
// a script about to use the image.
func runUtilImage() int {
	if err := ensureUtilImage(context.Background()); err != nil {
		report.Fail(err)
		return 1
	}
	return 0
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
// in its current release: the data capability's prepare parent, then the
// parent side's set stopped (cli.DataStops), the entries of mnt its mnt
// services bind-mount rsynced, each stream service exported into a spool
// in the child's directory and imported there, the stopped services
// started again, and finish parent go|abort.
func runSyncPush(cwd string, args []string, color bool) int {
	fail := func(msg string) int {
		report.Error(msg)
		return 1
	}
	if _, _, err := syncEnv(cli.SyncPushFlag); err != nil {
		return fail(err.Error())
	}
	switch {
	case len(args) == 1 && args[0] == "reset":
		// The child's first write wipes it first: this side's own, and
		// the child's devopsy's (its imports) through --sync-exec.
		os.Setenv(remote.SyncResetEnv, "1")
	case len(args) > 0:
		return fail("usage: devopsy " + cli.SyncPushFlag + " [reset]")
	}
	reset := os.Getenv(remote.SyncResetEnv) != ""
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
			report.Warn(time.Now().UTC().Format("2006-01-02 15:04:05") + ": the sync's session is gone: restoring what was stopped")
		case <-done:
		}
	}()

	_, services, err := cli.SyncServices(cwd, os.Environ())
	if err != nil {
		return fail(err.Error())
	}
	streams, err := cli.SyncStreamServices(services)
	if err != nil {
		return fail(err.Error())
	}
	dirs := cli.SyncDirs(services)
	// What the rsync would change under a running service of the child,
	// left running by its own labels, and a stream the child would not
	// import, are mismatches, not a sync: refused before anything stops.
	// So is an import's mark on the child for a service this side does
	// not copy: only a sync that writes it may clear it. Asked even with
	// nothing to copy here, for that mark.
	child, mark, err := childServices(ctx, childNeeds(dirs, streams))
	if err != nil {
		return fail(err.Error())
	}
	// A reset wipes the child whatever its mark: completed, it starts
	// over from this side's data.
	if mark != "" && !reset {
		var childDirs []string
		for _, c := range child {
			if c.Name == mark {
				childDirs = c.Dirs
			}
		}
		if missing, ok := copiesService(projectDir, services, streams, mark, childDirs); !ok {
			ref := os.Getenv(remote.SyncToTargetEnv)
			var fixes []report.Remedy
			for _, f := range remote.ImportFixes(ref, mark, true) {
				fixes = append(fixes, report.Fix(f[0], f[1]))
			}
			msg := "the import of " + mark + " into the child did not complete, and the parent does not copy " + mark + ": a sync completes it only by copying it: nothing changed"
			if len(missing) > 0 {
				msg = "the import of " + mark + " into the child did not complete, and the parent does not copy ./mnt/" + strings.Join(missing, ", ./mnt/") + ", which the child's " + mark + " mounts: a sync completes it only by copying all of it: nothing changed"
			}
			report.Fail(&report.Failure{Msg: msg, Fixes: fixes})
			return 1
		}
	}
	nothing := !slices.ContainsFunc(services, func(s cli.SyncService) bool { return s.HasData }) || len(dirs) == 0 && len(streams) == 0
	if reset && !nothing && len(streams) == 0 {
		// The rsync skips an entry missing here: with none present it
		// would write nothing, and nothing wipes without a write.
		mnt, _ := filepath.EvalSymlinks(filepath.Join(projectDir, "mnt"))
		nothing = mnt == "" || !slices.ContainsFunc(dirs, func(d string) bool { return fileExists(filepath.Join(mnt, d)) })
	}
	if nothing && reset {
		// It would only empty the child: --destroy and a release do that.
		return fail("the parent has no data to start the child over from (no service labeled " + cli.DataLabel + " with entries of mnt or a stream): nothing reset")
	}
	if !slices.ContainsFunc(services, func(s cli.SyncService) bool { return s.HasData }) {
		report.Info("nothing to sync: no service has data (" + cli.DataLabel + ")")
		return 0
	}
	if len(dirs) == 0 && len(streams) == 0 {
		report.Info("nothing to sync: no bind mounts under .devopsy/mnt for the services labeled " + cli.DataLabel + "=" + cli.DataMnt)
		return 0
	}
	// Everything that can refuse, before anything changes on this side.
	if len(streams) > 0 && !isExecutable(dataHookFile(projectDir, "export")) {
		return fail(fmt.Sprintf("%s labeled %s=%s: the parent's release needs an executable .devopsy/capabilities/%s/export", strings.Join(streams, ", "), cli.DataLabel, cli.DataStream, cli.DataCapability))
	}
	// A reset takes the child's services down before it copies: none
	// runs under the copy.
	var conflicts []string
	if !reset {
		conflicts = syncConflicts(services, child)
	}
	mismatches := streamMismatches(services, streams, child)
	if len(conflicts)+len(mismatches) > 0 {
		// A label from a variable the parent's inherit: names follows in
		// the same sync (the child reads the pending inherited file); one
		// in the child's own shared/.env wins over it.
		fixes := []report.Remedy{report.Fix("label it alike on both sides (a variable the parent's inherit: names keeps them so; the child's own shared/.env wins over it)")}
		if len(conflicts) > 0 {
			fixes = append(fixes, report.Fix(cli.DataStopLabel+"="+cli.StopImport+" on the child's service, to stop it for the sync"), report.Fix(cli.DataLabel+"="+cli.DataMnt+" on the child's service, to copy under it running"))
		}
		report.Fail(&report.Failure{Msg: "the child's data labels do not match the parent's: nothing changed", Details: append(conflicts, mismatches...), Fixes: fixes})
		return 1
	}
	for _, msg := range stopMismatches(services, child) {
		report.Warn(msg)
	}
	if len(dirs) > 0 {
		if err := ensureUtilImage(ctx); err != nil {
			return fail(err.Error())
		}
	}

	set := cli.DataStops(services, cli.SyncParent, "")
	env := dataHookEnv(services, cli.StoppedNames(set))
	// finish parent runs whatever happened, after the services are back,
	// and never with the sync's context: it restores too. Its word is the
	// copy's, as the child's: go once the copy completed, even when a
	// service here did not come back (that fails the command, said).
	finish := func(code int, copied bool) int {
		answer := "go"
		if !copied {
			answer = "abort"
		}
		if err := runDataHook(context.Background(), cwd, projectDir, env, nil, nil, nil, "finish", "parent", answer); err != nil && code == 0 {
			code = fail("the data capability's finish parent go: " + err.Error())
		}
		return code
	}
	if err := runDataHook(ctx, cwd, projectDir, env, nil, nil, nil, "prepare", "parent"); err != nil {
		return finish(fail("the data capability's prepare parent: "+err.Error()), false)
	}
	// On the same server, a child that decided already (its gate closed)
	// is seen before anything stops here: the claim would refuse later.
	if os.Getenv(remote.SyncToEnv) == "" {
		if base := os.Getenv(remote.SyncToBaseEnv); !fileExists(filepath.Join(base, remote.SyncOpenFile(os.Getenv(remote.SyncIDEnv)))) && !fileExists(filepath.Join(base, remote.SyncStartedFile)) {
			return finish(fail("the child's side ended already ("+base+"): nothing copied"), false)
		}
	}
	if err := stopServices(cwd, set); err != nil {
		// What it stopped before failing comes back: restoring reads the
		// containers' state, not what stopping did.
		code := fail(err.Error())
		if err := restoreServices(cwd, services, false, cli.StoppedNames(set)); err != nil {
			fail(err.Error())
		}
		return finish(code, false)
	}
	// The child's side writes remote.SyncStartedFile before its first
	// change and SyncDoneFile after the last (the rsync's or the last
	// import's): what its holder decides from, answer or not.
	code := 0
	if len(dirs) > 0 {
		if err := syncRsync(ctx, projectDir, dirs, childOwners(child), strings.Fields(os.Getenv(remote.SyncArgsEnv)), len(streams) == 0); err != nil {
			code = fail(err.Error())
		}
	}
	for i, name := range streams {
		if code != 0 {
			break
		}
		if ctx.Err() != nil {
			code = fail("interrupted: " + name + " not exported")
			break
		}
		if err := syncStream(ctx, cwd, projectDir, name, env, i == len(streams)-1); err != nil {
			code = fail(err.Error())
		}
	}
	copied := code == 0
	if err := restoreServices(cwd, services, false, cli.StoppedNames(set)); err != nil {
		code = fail(err.Error())
	}
	return finish(code, copied)
}

// copiesService reports whether this side's copy writes all of service
// on the child: a stream among streams, or a mnt service whose every
// entry the child mounts (childDirs, from its list; this side's own
// entries without them) this side copies and has (syncRsync skips a
// missing one, and the copy of the others would complete, leaving what
// a cut import half wrote there). missing are the entries it lacks.
func copiesService(projectDir string, services []cli.SyncService, streams []string, service string, childDirs []string) (missing []string, ok bool) {
	s, found := cli.SyncFind(services, service)
	switch {
	case !found || !s.HasData:
		return nil, false
	case s.Transfer == cli.DataStream:
		return nil, slices.Contains(streams, service)
	}
	mnt, err := filepath.EvalSymlinks(filepath.Join(projectDir, "mnt"))
	if err != nil {
		return nil, false
	}
	copied := cli.SyncDirs(services)
	want := childDirs
	if len(want) == 0 {
		want = s.Dirs
	}
	for _, d := range want {
		if !slices.Contains(copied, d) || !fileExists(filepath.Join(mnt, d)) {
			missing = append(missing, d)
		}
	}
	return missing, len(want) > 0 && len(missing) == 0
}

// dataHookFile is the data capability's action in the project at
// projectDir (its .devopsy/).
func dataHookFile(projectDir, action string) string {
	return filepath.Join(projectDir, "capabilities", cli.DataCapability, action)
}

// dataHookEnv are the variables every data capability action gets about
// its own side: the services by label, and stop, what the operation
// stops there (cli.DataLists).
func dataHookEnv(services []cli.SyncService, stop []string) []string {
	mnt, stream, stops := cli.DataLists(services, stop)
	return []string{remote.DataMntEnv + "=" + mnt, remote.DataStreamEnv + "=" + stream, remote.DataStopEnv + "=" + stops}
}

// runDataHook runs the data capability's action (args[0]) with args, in
// the project at cwd, with env over this process's: only when its file is
// there, except export and import, which a stream service needs (their
// callers check first). stdin nil is empty, stdout nil is errOut;
// started, when not nil, gets its pid once it runs (its process group's).
// Its own process group, all of it ended when ctx is (a pipeline's
// commands too, a dump and what compresses it).
func runDataHook(ctx context.Context, cwd, projectDir string, env []string, stdin io.Reader, stdout io.Writer, started func(pid int), args ...string) error {
	if a := args[0]; a != "export" && a != "import" && !isExecutable(dataHookFile(projectDir, a)) {
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		self = "devopsy"
	}
	report.Info("running 'devopsy --capability " + cli.DataCapability + " " + strings.Join(args, " ") + "'")
	cmd := exec.CommandContext(ctx, self, append([]string{"--capability", cli.DataCapability}, args...)...)
	cmd.Dir = cwd
	// The session's watching is devopsy's, never the hook's: a hook
	// running devopsy --export would take its own /dev/null stdin for a
	// session gone.
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, remote.SyncWatchEnv+"=") || strings.HasPrefix(kv, remote.HeartbeatEnv+"=")
	}), env...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	if stdout == nil {
		cmd.Stdout = errOut
	}
	cmd.Stderr = errOut
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	if err = cmd.Start(); err == nil {
		if started != nil {
			started(cmd.Process.Pid)
		}
		err = cmd.Wait()
	}
	// What the hook left running in its group (a child it put in the
	// background) ends with it: hooks run in the foreground, and nothing
	// of one may write into a child past its sync. What it started
	// outside its group (setsid, docker exec -d) is its own.
	if cmd.Process != nil && syscall.Kill(-cmd.Process.Pid, 0) == nil {
		report.Warn("the data capability's " + strings.Join(args, " ") + " left processes running: ending them (hooks run in the foreground)")
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil && cmd.ProcessState.Success() {
			err = nil
		}
	}
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
func syncStream(ctx context.Context, cwd, projectDir, name string, env []string, last bool) error {
	spool := remote.StreamSpool(os.Getenv(remote.SyncToBaseEnv), name, os.Getenv(remote.SyncIDEnv))
	to := os.Getenv(remote.SyncToEnv)
	report.Info("exporting " + name)
	var err error
	if to == "" {
		var f *os.File
		if f, err = os.OpenFile(spool, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600); err != nil {
			return fmt.Errorf("the spool for %s: %w", name, err)
		}
		err = runDataHook(ctx, cwd, projectDir, env, nil, f, nil, "export", name)
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
	importArgs := []string{cli.SyncExecFlag, cli.SyncServicesFlag, "import", name}
	if last {
		importArgs = append(importArgs, "last")
	}
	trace(append([]string{"devopsy"}, importArgs...))
	cmd := exec.CommandContext(ctx, self, importArgs...)
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
	defer heartbeat(w)()
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
// server to the child's (to) running script behind the check of the
// child's devopsy (remote.ChildCheck, needs the later sessions'): -o
// option, remote.ChildSSH, remote.KeepAlive, -T, extra, the sync's port;
// traced, script aside.
func childSSHArgs(to, script string, needs []string, option string, extra ...string) []string {
	args := append(append([]string{"-o", option}, remote.ChildSSH...), remote.KeepAlive...)
	args = append(append(args, "-T"), extra...)
	if p := os.Getenv(remote.SyncSSHPortEnv); p != "" {
		args = append(args, "-p", p)
	}
	trace(append([]string{"ssh"}, append(args, to)...))
	script = remote.ChildCheck(os.Getenv(remote.SyncToTargetEnv), script, needs)
	return append(args, to, "sh -c "+remote.Quote(script))
}

// exportInto runs the export of name with its stdout written to spool on
// the child's server (to), by cat in an SSH session the forwarded agent
// opens. Both must succeed.
func exportInto(ctx context.Context, cwd, projectDir, name string, env []string, to, spool string) error {
	sshArgs := childSSHArgs(to, "umask 077; cat > "+remote.Quote(spool), nil, "ClearAllForwardings=yes")
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
	exported := runDataHook(ctx, cwd, projectDir, env, nil, w, nil, "export", name)
	w.Close()
	written := cat.Wait()
	// A session that failed first makes the export fail too (a broken
	// pipe): its own error says why.
	if written != nil && ctx.Err() == nil {
		return fmt.Errorf("writing it into %s: %w", to, written)
	}
	return exported
}

// childNeeds is what the sync's later sessions to the child's server
// need of its devopsy (remote.Needs): the imports' and, with entries to
// copy, the receive's, and the data labels always (every later session
// reads them), checked on the first session (childServices), so a
// child's devopsy older than this one refuses before anything stops.
func childNeeds(dirs, streams []string) []string {
	toDir, toProject := os.Getenv(remote.SyncToDirEnv), os.Getenv(remote.SyncToProjectEnv)
	script := ""
	for i, name := range streams {
		args := []string{cli.SyncServicesFlag, "import", name}
		if i == len(streams)-1 {
			args = append(args, "last")
		}
		script += remote.SyncExecScript(toDir, toProject, nil, args)
	}
	if len(dirs) > 0 {
		script += remote.SyncReceiveScript(toDir, os.Getenv(remote.SyncToBaseEnv), os.Getenv(remote.SyncIDEnv), "0", dirs, nil, nil, nil, len(streams) == 0, wipeScript())
	}
	if os.Getenv(remote.SyncResetEnv) != "" {
		script += remote.SyncExecScript(toDir, toProject, nil, []string{cli.SyncServicesFlag, "wipe"})
	}
	needs := remote.Needs(script)
	for _, f := range []string{remote.DataLabelsFeature, remote.MntOwnerFeature} {
		if !slices.Contains(needs, f) {
			needs = append(needs, f)
		}
	}
	slices.Sort(needs)
	return needs
}

// childService is a line of the child's --sync-services list: a service,
// its data label there (mnt, stream or none) and its stop word, its
// containers' state (running, exited..., none without one), its
// entries of mnt, and the owners of those it owns (entry to <uid>:<gid>,
// cli.MntOwnerLabel).
type childService struct {
	Name, Transfer, Stop, State string
	Dirs                        []string
	Owners                      map[string]string
}

// childOwners are the owners of the child's entries, from its list: what
// the rsync gives every file it writes there (--chown), so a parent's
// owners never land on the child. The child's own check refuses two
// owners on one entry.
func childOwners(child []childService) map[string]string {
	owners := map[string]string{}
	for _, c := range child {
		for e, o := range c.Owners {
			owners[e] = o
		}
	}
	return owners
}

// listOwners is a service's owners column in --sync-services list:
// <entry>=<uid>:<gid> for each entry it owns, space separated (entries
// have no whitespace, owners no =).
func listOwners(s cli.SyncService) string {
	var out []string
	if s.Owner != "" {
		for _, d := range s.Writable {
			out = append(out, d+"="+s.Owner)
		}
	}
	return strings.Join(out, " ")
}

// listMark starts the list's line for the child's import mark, followed
// by a tab and the service: no service is named so.
const listMark = "!import"

// childServices asks the child's release for its services, and the
// service of its import's mark if any, through --sync-exec, after it
// prepared them: across servers the sync's first session there, which
// checks the child's devopsy for needs too.
func childServices(ctx context.Context, needs []string) ([]childService, string, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, "", err
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, self, cli.SyncExecFlag, cli.SyncServicesFlag, "list")
	cmd.Stdout, cmd.Stderr = &out, errOut
	cmd.Env = append(os.Environ(), remote.SyncNeedsEnv+"="+strings.Join(needs, " "))
	if err := cmd.Run(); err != nil {
		return nil, "", fmt.Errorf("listing the child's services: %w", err)
	}
	services, mark := parseChildServices(out.String())
	return services, mark, nil
}

// parseChildServices reads --sync-services list: per service its name,
// label (<method>+<stop word>), state, entries and owners (listOwners),
// and the import mark's line (listMark), whose service it returns.
func parseChildServices(out string) (services []childService, mark string) {
	// Lines one by one: the last service's entries may be empty, its
	// line ending in a tab.
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(f) == 2 && f[0] == listMark {
			mark = f[1]
			continue
		}
		if len(f) < 4 {
			continue
		}
		transfer, stop, _ := strings.Cut(f[1], "+")
		c := childService{Name: f[0], Transfer: transfer, Stop: cmp.Or(stop, cli.StopNever), State: f[2], Dirs: strings.Fields(f[3]), Owners: map[string]string{}}
		if len(f) > 4 {
			for _, eo := range strings.Fields(f[4]) {
				if i := strings.LastIndex(eo, "="); i > 0 {
					c.Owners[eo[:i]] = eo[i+1:]
				}
			}
		}
		services = append(services, c)
	}
	return services, mark
}

// streamMismatches names the services one side streams and the other
// does not: a parent stream the child does not label stream (its import
// would refuse it, after the rsync and the streams before it), and a
// child stream the parent copies as mnt (its import would never run).
// Refused before anything stops, with the way out (labelsAlike).
func streamMismatches(parent []cli.SyncService, streams []string, child []childService) []string {
	var out []string
	for _, name := range streams {
		i := slices.IndexFunc(child, func(c childService) bool { return c.Name == name })
		switch {
		case i < 0:
			out = append(out, fmt.Sprintf("the parent streams %s (%s=%s) and the child has no service %s", name, cli.DataLabel, cli.DataStream, name))
		case child[i].Transfer != cli.DataStream:
			out = append(out, fmt.Sprintf("the parent streams %s (%s=%s) and the child labels it %s=%s: its import would refuse it", name, cli.DataLabel, cli.DataStream, cli.DataLabel, child[i].Transfer))
		}
	}
	for _, c := range child {
		if p, ok := cli.SyncFind(parent, c.Name); ok && c.Transfer == cli.DataStream && p.Transfer == cli.DataMnt {
			out = append(out, fmt.Sprintf("the child streams %s (%s=%s) and the parent copies it as %s (%s=%s): its import would never run", c.Name, cli.DataLabel, cli.DataStream, cli.DataMnt, cli.DataLabel, cli.DataMnt))
		}
	}
	return out
}

// stopMismatches names the services whose stop word differs between the
// sides (parent import, child never from its own shared/.env): the child
// then serves, or stops, otherwise than the parent's labels say. Only
// said.
func stopMismatches(parent []cli.SyncService, child []childService) []string {
	var out []string
	for _, c := range child {
		if p, ok := cli.SyncFind(parent, c.Name); ok && p.Stop != c.Stop {
			out = append(out, fmt.Sprintf("%s is %s=%s on the parent and %s on the child, which stops it by its own", c.Name, cli.DataStopLabel, p.Stop, c.Stop))
		}
	}
	return out
}

// syncListLabel is a service's label column in --sync-services list: its
// data label (mnt, stream or none), + and its stop word.
func syncListLabel(s cli.SyncService) string {
	return s.Transfer + "+" + s.Stop
}

// syncConflicts names the child's services the rsync would change under:
// running, with an entry the parent copies among their mounts, and not
// labeled mnt there (which says so on purpose). One the child's side
// stopped is not running any more; one labeled stream or without data is
// left running, and the two sides disagree.
func syncConflicts(parent []cli.SyncService, child []childService) []string {
	dirs := cli.SyncDirs(parent)
	if len(dirs) == 0 {
		return nil
	}
	copied := func(d string) bool { return slices.Contains(dirs, d) }
	from := func(d string) string {
		for _, s := range parent {
			if s.Transfer == cli.DataMnt && slices.Contains(s.Dirs, d) {
				return s.Name + " (" + cli.DataLabel + "=" + s.Transfer + ")"
			}
		}
		return "another service"
	}
	var conflicts []string
	for _, c := range child {
		if c.State != "running" && c.State != "restarting" || c.Transfer == cli.DataMnt {
			continue
		}
		for _, d := range c.Dirs {
			if !copied(d) {
				continue
			}
			label := "labeled " + cli.DataLabel + "=" + c.Transfer
			if c.Transfer == cli.DataNone {
				label = "without data (" + cli.DataLabel + "=" + cli.DataNone + " or no label)"
			}
			conflicts = append(conflicts, fmt.Sprintf("the child's %s is running, %s, and mnt/%s would change under it: the parent copies it for %s", c.Name, label, d, from(d)))
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
	report.Info(err.Error())
	return 1
}

// rsyncdConf serves the entries of mnt being copied, each as a module of
// its name, read-only, as root, so every
// owner's files are read; nothing else under mnt can be asked for (a
// stream database's datadir, the directory of a service without data). The daemon
// lives for one copy and publishes no port.
func rsyncdConf(entries, files []string) string {
	conf := "uid = 0\ngid = 0\nuse chroot = yes\nread only = yes\n"
	for _, e := range entries {
		if slices.Contains(files, e) {
			// A module is a directory: mnt, filtered down to the file.
			conf += "[" + e + "]\npath = /mnt\ninclude = /" + e + "\nexclude = *\n"
			continue
		}
		conf += "[" + e + "]\npath = " + path.Join("/mnt", e) + "\n"
	}
	return conf
}

// syncRsync copies dirs (entries of the parent's mnt) into the child's,
// replacing what is there (--delete), as root in devopsy's util image,
// owners kept (--numeric-ids) unless the child's labels own the entry
// (owners, from its list): then every file it writes is that owner's
// (--chown).
// Same server: one container with both mounts. Across servers: an rsync
// daemon on this server, reached by the child's server through a reverse forward of
// an SSH session the forwarded agent opens.
func syncRsync(ctx context.Context, projectDir string, dirs []string, owners map[string]string, args []string, last bool) error {
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
			report.Info("mnt/" + d + " does not exist on this server: skipped")
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
	rsync := slices.Clone(remote.RsyncCopy)

	to := os.Getenv(remote.SyncToEnv)
	if to == "" {
		childMnt, err := filepath.EvalSymlinks(path.Join(toDir, "mnt"))
		if err != nil {
			return fmt.Errorf("the child's mnt: %w", err)
		}
		toBase := os.Getenv(remote.SyncToBaseEnv)
		first, err := claimWrite(toBase)
		if err != nil {
			return err
		}
		if first && os.Getenv(remote.SyncResetEnv) != "" {
			if err := wipeChild(ctx); err != nil {
				return err
			}
		}
		for i, d := range present {
			report.Info("rsync mnt/" + d)
			run := append(append([]string{"-v", parentMnt + ":/src:ro", "-v", childMnt + ":/dst", remote.UtilImage}, rsync...), args...)
			name := fmt.Sprintf("%s%s-rsync-%d", remote.SyncContainerPrefix, os.Getenv(remote.SyncIDEnv), i)
			src, dst := "/src/"+d+"/", "/dst/"+d+"/"
			if slices.Contains(files, d) {
				// In place: a container bind-mounts the file's inode,
				// and a replaced file would leave it reading the old one
				// until recreated.
				src, dst = "/src/"+d, "/dst/"
				run = append(run, "--inplace")
			}
			if o := owners[d]; o != "" {
				run = append(run, "--chown="+o)
			}
			if err := dockerRun(ctx, name, append(run, src, dst)...); err != nil {
				if ctx.Err() == nil && exitCode(err) == remote.RsyncVanished {
					report.Info("rsync mnt/" + d + ": " + remote.RsyncVanishedNotice)
					continue
				}
				report.Info("mnt/" + d + " " + remote.HalfCopiedNotice)
				if ctx.Err() != nil {
					return errors.New("interrupted")
				}
				return fmt.Errorf("rsync mnt/%s: %w", d, err)
			}
		}
		if last {
			return recordWrite(toBase, remote.DoneLine(os.Getenv(remote.SyncIDEnv)))
		}
		return nil
	}

	// A daemon an earlier push left (its devopsy killed outright) goes:
	// this push holds the parent's sync lock, so no other is serving.
	parentLabel := "devopsy.data.parent=" + parentMnt
	if err := removeContainers("label=" + parentLabel); err != nil {
		return err
	}
	id, err := dockerOut(ctx, "run", "--rm", "-d", "--name", remote.SyncContainerPrefix+os.Getenv(remote.SyncIDEnv)+"-daemon", "--label", parentLabel, "-v", parentMnt+":/mnt:ro", "-e", "RSYNCD_CONF="+rsyncdConf(present, files), remote.UtilImage,
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
		sshArgs := childSSHArgs(to, remote.SyncReceiveScript(toDir, os.Getenv(remote.SyncToBaseEnv), os.Getenv(remote.SyncIDEnv), port, present, files, owners, args, last, wipeScript()), nil, "ExitOnForwardFailure=yes", "-R", "127.0.0.1:"+port+":"+ip+":873")
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
			report.Info("the copy into " + to + " was cut: an entry " + remote.HalfCopiedNotice)
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
var cleanEnvKeys = regexp.MustCompile(`^(PATH|HOME|USER|LOGNAME|LANG|LC_[A-Z]+|TERM|TZ|DOCKER_[A-Z_]+|DEVOPSY_VERBOSE|DEVOPSY_HOME|XDG_CONFIG_HOME|` + remote.SyncFromTargetEnv + `|` + remote.SyncToTargetEnv + `|` + remote.SyncArgsEnv + `|` + remote.SyncWatchEnv + `|` + remote.HeartbeatEnv + `|` + remote.SyncIDEnv + `|` + remote.SyncResetEnv + `)=`)

// syncExecEnv are the sync's variables the child's devopsy gets across
// servers, as cleanEnvKeys lets them through on the same one.
func syncExecEnv() map[string]string {
	env := map[string]string{}
	for _, k := range []string{remote.SyncFromTargetEnv, remote.SyncToTargetEnv, remote.SyncArgsEnv, remote.SyncWatchEnv, remote.HeartbeatEnv, remote.SyncIDEnv, remote.SyncResetEnv} {
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
	return remote.InheritPendingEnv, remote.SyncInheritPath(base, os.Getenv(remote.SyncIDEnv))
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
		report.Error(msg)
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
		sshArgs := childSSHArgs(to, remote.SyncExecScript(toDir, toProject, syncExecEnv(), args), strings.Fields(os.Getenv(remote.SyncNeedsEnv)), "ClearAllForwardings=yes")
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
//   - prepare: settles an earlier sync's files, ends what an import a
//     killed devopsy left may still write (its mark), checks a stream
//     service has the import to receive it, runs the data capability's
//     prepare child, then stops the child side's set (cli.DataStops);
//   - finish go|abort|partial: what the copy did to the data (the
//     holder decides from remote.SyncStartedFile and SyncDoneFile): go,
//     complete, and abort, nothing written, start what the record holds
//     again and wait until it is healthy; partial, written in part,
//     leaves it down and marks the environment (remote.SyncIncompleteFile),
//     as abort does on one marked already; then finish child with the word;
//   - settle: a sync's files no holder decided on (it was killed): the
//     environment marked, finish child partial. Labels it cannot read
//     only warn: every activation runs it;
//   - import <service> [last]: the stream's spool into the service (the
//     capability's import), the spool removed whatever happens; with
//     last, the copy's last write, remote.SyncDoneFile once it went;
//   - list: name, label (<method>+<stop word>), state and entries of mnt,
//     one service per line, then the service of an import's mark
//     (listMark).
func runSyncServices(cwd string, args []string, color bool) int {
	fail := func(msg string) int {
		report.Error(msg)
		return 1
	}
	usage := "usage: devopsy " + cli.SyncServicesFlag + " prepare [first] | finish go|abort|partial | settle | import <service> [last] | list | wipe"
	switch {
	case len(args) == 1 && (args[0] == "prepare" || args[0] == "list" || args[0] == "settle" || args[0] == "wipe"):
	case len(args) == 2 && args[0] == "prepare" && args[1] == "first":
	case len(args) == 2 && args[0] == "finish" && slices.Contains([]string{"go", "abort", "partial"}, args[1]):
	case len(args) == 2 && args[0] == "import":
	case len(args) == 3 && args[0] == "import" && args[2] == "last":
	default:
		return fail(usage)
	}
	projectDir, services, err := cli.SyncServices(cwd, os.Environ())
	if err != nil {
		if args[0] != "settle" {
			return fail(err.Error())
		}
		// What is down then is what the record says.
		report.Warn("settling with the record alone: " + err.Error())
		if projectDir, err = cli.FindProjectDir(cwd); err != nil {
			return fail(err.Error())
		}
		services = nil
	}
	base := filepath.Dir(cwd)
	set := cli.DataStops(services, cli.SyncChild, "")
	// The hooks' DEVOPSY_DATA_STOP: the labels' set, but what the record
	// says is down at finish child partial and settle.
	env := dataHookEnv(services, cli.StoppedNames(set))
	recordEnv := func() []string {
		rec, _ := readSyncRecord(cwd)
		return dataHookEnv(services, rec.stops)
	}
	switch args[0] {
	case "list":
		states, err := serviceStates(cwd)
		if err != nil {
			return fail(err.Error())
		}
		// Two owners on one entry would leave the parent to pick one.
		if _, err := cli.MntOwners(services); err != nil {
			return fail(err.Error())
		}
		for _, s := range services {
			fmt.Printf("%s\t%s\t%s\t%s\t%s\n", s.Name, syncListLabel(s), cmp.Or(states[s.Name], "none"), strings.Join(s.Dirs, " "), listOwners(s))
		}
		if m := readMark(base); m.IsImport() {
			fmt.Printf("%s\t%s\n", listMark, m.From)
		}
		return 0
	case "import":
		return syncImport(cwd, projectDir, base, args[1], len(args) == 3, services, set, env, fail)
	case "wipe":
		signal.Ignore(syscall.SIGPIPE, syscall.SIGHUP)
		if err := wipeData(cwd, projectDir, base); err != nil {
			return fail(err.Error())
		}
		return 0
	}
	// A session gone is no reason to die halfway: the holder restores
	// what this stopped.
	signal.Ignore(syscall.SIGPIPE, syscall.SIGHUP)
	// settle marks an environment whose sync's files no holder decided
	// on: its data may be anything from untouched to complete, never
	// finished by finish child go.
	settle := func() int {
		started, taken := filepath.Join(base, remote.SyncStartedFile), filepath.Join(base, remote.SyncTakenFile)
		if !fileExists(started) && !fileExists(taken) {
			return 0
		}
		report.Info("an earlier sync into " + base + " left its files (its holder was killed before it was done): its data counts as a partial copy")
		code := 0
		// Taken first, as a holder deciding would: nothing appends any
		// more, and what its record names is ended before anything else.
		if fileExists(started) {
			_ = os.Rename(started, taken)
		}
		if err := endWriters(cwd, base, services); err != nil {
			code = fail(err.Error())
		}
		// A holder killed after marking it said why already; an import's
		// mark is replaced: the data is partly a sync's now.
		if err := markIncomplete(base, "", remote.IncompleteSettled, "a sync's holder ended before it decided: the data may be anything from untouched to complete", true); err != nil {
			code = fail(err.Error())
		}
		_ = os.Remove(taken)
		gates, _ := filepath.Glob(filepath.Join(base, remote.SyncOpenPrefix+"*"))
		for _, g := range gates {
			_ = os.Remove(g)
		}
		if err := runDataHook(context.Background(), cwd, projectDir, recordEnv(), nil, nil, nil, "finish", "child", "partial"); err != nil {
			code = fail("the data capability's finish child partial: " + err.Error())
		}
		return code
	}
	if args[0] == "settle" {
		return settle()
	}
	if args[0] == "prepare" {
		if code := settle(); code != 0 {
			return code
		}
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
		// A release with nothing current says so: a first one that
		// failed may have left containers (an import's).
		if err := startSyncState(cwd, len(states) == 0 || len(args) == 2); err != nil {
			return fail(err.Error())
		}
		// An import a killed devopsy left may still load: ended, and what
		// it stops recorded, before anything streams into it.
		if m := readMark(base); m.IsImport() {
			if err := endLeftoverImport(cwd, base, services, m.From); err != nil {
				return fail(err.Error())
			}
		}
		if streams, _ := cli.SyncStreamServices(services); len(streams) > 0 && !isExecutable(dataHookFile(projectDir, "import")) {
			return fail(fmt.Sprintf("%s labeled %s=%s: the child's release needs an executable .devopsy/capabilities/%s/import", strings.Join(streams, ", "), cli.DataLabel, cli.DataStream, cli.DataCapability))
		}
		if err := runDataHook(context.Background(), cwd, projectDir, env, nil, nil, nil, "prepare", "child"); err != nil {
			return fail("the data capability's prepare child: " + err.Error())
		}
		// What depends on the child's data services and runs on during
		// the copy, unlabeled: named (the parent's labels say what is
		// copied, unknown here).
		var data []string
		for _, s := range services {
			if s.HasData {
				data = append(data, s.Name)
			}
		}
		dependentNotices(cwd, services, data, set)
		if err := stopServices(cwd, set); err != nil {
			return fail(err.Error())
		}
		return 0
	}
	word, code := args[1], 0
	incomplete := filepath.Join(base, remote.SyncIncompleteFile)
	if word == "abort" && fileExists(incomplete) {
		// Nothing written this time, but the data is an earlier sync's
		// partial copy still, or an import's.
		word = "partial"
	}
	hookEnv := env
	switch word {
	case "go":
		// Complete: whatever an earlier sync or import left is replaced.
		_ = os.Remove(incomplete)
		if err := restoreServices(cwd, services, true, cli.StoppedNames(set)); err != nil {
			code = fail(err.Error())
		}
	case "abort":
		// A first release's services are not created on empty data: it
		// does not go live.
		if err := restoreServices(cwd, services, false, cli.StoppedNames(set)); err != nil {
			code = fail(err.Error())
		}
	case "partial":
		// Whatever may still write into the data ends first.
		if err := endWriters(cwd, base, services); err != nil {
			code = fail(err.Error())
		}
		// The holder marked it already, saying why, when the copy wrote;
		// when it wrote nothing the mark stays as it was, an import's too,
		// and what this sync stopped comes back: only what was down
		// before stays down.
		if readMark(base) == nil {
			if err := markIncomplete(base, "", remote.IncompleteCopy, "the copy did not complete", false); err != nil {
				code = fail(err.Error())
			}
		}
		if !fileExists(filepath.Join(base, remote.SyncTakenFile)) {
			if err := restoreOwnStops(cwd); err != nil {
				code = fail(err.Error())
			}
		}
		if rec, _ := readSyncRecord(cwd); len(rec.stops) > 0 {
			report.Info(strings.Join(rec.stops, " ") + " stay down: the data is partial until a sync completes")
			restartAlwaysNotice(services, rec.stops)
		}
		hookEnv = recordEnv()
	}
	// Whatever the word: this sync's imports took their spools, and any
	// other is an earlier sync's, stale (another's push starts only once
	// its holder holds the lock).
	spools, _ := filepath.Glob(filepath.Join(base, remote.StreamSpoolPrefix+"*"))
	for _, f := range spools {
		_ = os.Remove(f)
	}
	// Another sync's pending inherited variables may be fresh (written
	// before its holder waits for the lock): only those older than any
	// wait go, left by a sync cut before its holder ran.
	pendings, _ := filepath.Glob(filepath.Join(base, remote.SyncInheritPrefix+"*"))
	for _, f := range pendings {
		if fi, err := os.Stat(f); err == nil && time.Since(fi.ModTime()) > staleInherit {
			_ = os.Remove(f)
		}
	}
	if err := runDataHook(context.Background(), cwd, projectDir, hookEnv, nil, nil, nil, "finish", "child", word); err != nil {
		code = fail("the data capability's finish child " + word + ": " + err.Error())
		// A sync completes with its finish: until one does, nothing runs
		// on what it left (the hook says what stays up).
		if word == "go" {
			why := "the copy completed, but the project's data capability refused it: finish child go failed (" + err.Error() + "); a sync completes only once its finish accepts the copy, and nothing runs on the data until one does"
			if err := markIncomplete(base, "", remote.IncompleteFinish, why, false); err != nil {
				fail(err.Error())
			}
		}
	}
	return code
}

// markIncomplete marks the environment at base as one no sync completed
// on, or whose import did not complete (remote.SyncIncompleteFile,
// remote.IncompleteLine): code and why say what happened, from the
// parent's address (a sync's: "" takes it from the sync's record or
// environment) or the imported service. An earlier mark gives its time
// (since when); keep leaves a sync's as it is, and replaces an import's:
// the data is partly a sync's now.
func markIncomplete(base, from, code, why string, keep bool) error {
	path := filepath.Join(base, remote.SyncIncompleteFile)
	since := time.Now().UTC().Format("2006-01-02 15:04:05")
	if old, err := os.ReadFile(path); err == nil {
		m := remote.ParseMark(string(old))
		if keep && !m.IsImport() {
			return nil
		}
		if m.Since != "" {
			since = m.Since
		}
	}
	if from == "" {
		// Who wrote it: the parent the gate named (remote.SyncOpenFile's
		// line, carried into the record), else this sync's.
		record, err := os.ReadFile(filepath.Join(base, remote.SyncTakenFile))
		if err != nil {
			record, _ = os.ReadFile(filepath.Join(base, remote.SyncStartedFile))
		}
		from, _, _ = strings.Cut(strings.TrimSpace(string(record)), " ")
		from = cmp.Or(from, os.Getenv(remote.SyncFromTargetEnv), "unknown")
	}
	line := remote.IncompleteLine(since, from, code, why)
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		return fmt.Errorf("marking %s as partial: %w", base, err)
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// claimWrite claims the right to write into the child at base before
// the first write: the holder's gate of this sync (remote.SyncOpenFile,
// named by remote.SyncIDEnv) renamed to remote.SyncStartedFile, one
// rename or the holder's unlink winning (first: this is the sync's
// first write, which a reset wipes before), or a write of this sync
// claimed it already. Neither: the holder decided, and nothing may be
// written into it.
func claimWrite(base string) (first bool, err error) {
	id := os.Getenv(remote.SyncIDEnv)
	open, started := filepath.Join(base, remote.SyncOpenFile(id)), filepath.Join(base, remote.SyncStartedFile)
	if id != "" && os.Rename(open, started) == nil {
		return true, nil
	}
	if recordOf(started, id) {
		return false, nil
	}
	return false, fmt.Errorf("the child's side ended already (%s): nothing copied", base)
}

// wipeScript is the receive script's wipe of the child for a reset
// (remote.SyncResetEnv), "" for a sync.
func wipeScript() string {
	if os.Getenv(remote.SyncResetEnv) == "" {
		return ""
	}
	return remote.SyncExecScript(os.Getenv(remote.SyncToDirEnv), os.Getenv(remote.SyncToProjectEnv), syncExecEnv(), []string{cli.SyncServicesFlag, "wipe"})
}

// wipeChild runs the child's --sync-services wipe on this server
// (--sync-exec), right after a reset's first claim.
func wipeChild(ctx context.Context) error {
	self, err := os.Executable()
	if err != nil {
		self = "devopsy"
	}
	cmd := exec.CommandContext(ctx, self, cli.SyncExecFlag, cli.SyncServicesFlag, "wipe")
	cmd.Stdout, cmd.Stderr = errOut, errOut
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("wiping the child: %w: it holds a partial copy now", err)
	}
	return nil
}

// recordOf reports whether the gate or record at path is sync id's: its
// first line is "<from> <id>".
func recordOf(path, id string) bool {
	data, err := os.ReadFile(path)
	first, _, _ := strings.Cut(string(data), "\n")
	f := strings.Fields(first)
	return err == nil && len(f) == 2 && f[1] == id
}

// errDecided is a write into a child whose holder decided already: its
// record (remote.SyncStartedFile) is taken, or another sync's.
var errDecided = errors.New("the child's side decided already: this write does not count")

// recordWrite appends line (remote.WriterLine, remote.DoneLine) to the
// sync's record in the child's directory base, never creating it: once
// the holder took it, the write is not this sync's to make.
func recordWrite(base, line string) error {
	path := filepath.Join(base, remote.SyncStartedFile)
	if !recordOf(path, os.Getenv(remote.SyncIDEnv)) {
		return errDecided
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if errors.Is(err, os.ErrNotExist) {
		return errDecided
	}
	if err != nil {
		return fmt.Errorf("the sync's record: %w", err)
	}
	_, err = f.WriteString(line + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("the sync's record: %w", err)
	}
	return nil
}

// syncImport is --sync-services import <name>: the spool the parent's
// export wrote (complete: the push only calls this after the export
// exited 0) into the child's service, through the data capability's
// import. The service is brought up unless the child's side stops it
// (set). The spool goes whatever happens, an interrupt included.
func syncImport(cwd, projectDir, base, name string, last bool, services []cli.SyncService, set []cli.Stopped, env []string, fail func(string) int) (code int) {
	spool := remote.StreamSpool(base, name, os.Getenv(remote.SyncIDEnv))
	defer os.Remove(spool)
	ctx, stop := interruptible()
	defer stop()
	if os.Getenv(remote.SyncWatchEnv) != "" {
		ctx = watchSession(ctx)
	}
	s, ok := cli.SyncFind(services, name)
	if !ok || s.Transfer != cli.DataStream {
		return fail(fmt.Sprintf("the child's %s is not labeled %s=%s: the parent's export of it has nowhere to go", name, cli.DataLabel, cli.DataStream))
	}
	f, err := os.Open(spool)
	if err != nil {
		return fail(fmt.Sprintf("the spool for %s: %v", name, err))
	}
	defer f.Close()
	// A reset's first write wipes the child before anything is brought up
	// for this import.
	if os.Getenv(remote.SyncResetEnv) != "" {
		first, err := claimWrite(base)
		if err != nil {
			return fail(err.Error())
		}
		if first {
			// Recorded first, so a holder deciding meanwhile ends this
			// devopsy, which wipes.
			if err := recordWrite(base, remote.WriterLine(name, os.Getpid(), "none")); err != nil {
				return fail(err.Error())
			}
			if err := wipeData(cwd, projectDir, base); err != nil {
				return fail(err.Error())
			}
		}
	}
	// What the service was before the sync brought it up: running, it is
	// recorded as the sync's to start again if a cut ends this import.
	was := ""
	if !slices.Contains(cli.StoppedNames(set), name) {
		// A first release (the record says so) keeps it up, for the
		// finish hook and the run step, unless it is in a profile that is
		// not up; otherwise one down before the sync goes down again.
		// A first release creates what the service needs, but what the
		// sync stopped (a retried one finds containers) stays down.
		rec, _ := readSyncRecord(cwd)
		down, state, err := upFor(ctx, cwd, name, rec.first && s.Active(composeProfiles(cwd)), !rec.first || len(rec.stops) > 0)
		if err != nil {
			code := fail(err.Error())
			if err := down(); err != nil {
				fail(err.Error())
			}
			return code
		}
		was = state
		defer func() {
			if err := down(); err != nil {
				code = fail(err.Error())
			}
		}()
	}
	// The service's data changes from here: claimed, and recorded, so a
	// holder that decides while this runs ends it (endWriters).
	if _, err := claimWrite(base); err != nil {
		return fail(err.Error())
	}
	if err := recordWrite(base, remote.WriterLine(name, os.Getpid(), was)); err != nil {
		return fail(err.Error())
	}
	if err := runDataHook(ctx, cwd, projectDir, env, f, nil, nil, "import", name); err != nil {
		code = fail("the data capability's import " + name + ": " + err.Error())
	} else if last {
		if err := recordWrite(base, remote.DoneLine(os.Getenv(remote.SyncIDEnv))); err != nil {
			code = fail(err.Error())
		}
	}
	return code
}

// upFor brings name up (up -d --wait) for an import and returns what puts
// it back as it was: stopped again when it was not running before (a
// service down before stays down). keep leaves it up whatever it was (a
// child's first release); noDeps starts none of its dependencies (a
// sync's: one it stopped stays down through the imports).
func upFor(ctx context.Context, cwd, name string, keep, noDeps bool) (down func() error, was string, err error) {
	down = func() error { return nil }
	states, err := serviceStates(cwd)
	if err != nil {
		return down, "", err
	}
	was = states[name]
	up := []string{"up", "-d", "--wait", name}
	if noDeps {
		up = []string{"up", "-d", "--wait", "--no-deps", name}
	}
	if states[name] != "running" && !keep {
		down = func() error {
			report.Info("stopping " + name + " again: it was down before")
			if err := compose(cwd, errOut, "stop", name); err != nil {
				return fmt.Errorf("docker compose stop: %w", err)
			}
			return nil
		}
	}
	// An up that fails may have started it all the same (unhealthy, or
	// late): down then too.
	if err := composeContext(ctx, cwd, errOut, up...); err != nil {
		return down, was, fmt.Errorf("docker compose up %s: %w", name, err)
	}
	return down, was, nil
}

// compose runs docker compose args in the project at cwd, with its
// environment, to its end: in a process group of its own, so a
// terminal's Ctrl-C, which devopsy handles, never stops it halfway (a
// restore leaving a stopped app down without a word).
func compose(cwd string, stdout io.Writer, args ...string) error {
	return composeRun(context.Background(), cwd, stdout, true, args...)
}

// composeContext is compose, ended with ctx, in devopsy's process group.
func composeContext(ctx context.Context, cwd string, stdout io.Writer, args ...string) error {
	return composeRun(ctx, cwd, stdout, false, args...)
}

func composeRun(ctx context.Context, cwd string, stdout io.Writer, detach bool, args ...string) error {
	plan, err := cli.Build(cwd, append([]string{"--"}, args...), os.Environ())
	if err != nil {
		return err
	}
	trace(plan.Args)
	cmd := exec.CommandContext(ctx, "docker", plan.Args[1:]...)
	cmd.Env = plan.Env
	cmd.Stdout, cmd.Stderr = stdout, errOut
	if detach {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	return cmd.Run()
}

// composeProfiles is COMPOSE_PROFILES as compose sees it in the project
// at cwd: the caller's, else the project's .env.
func composeProfiles(cwd string) string {
	plan, err := cli.Build(cwd, []string{"--", "ps"}, os.Environ())
	if err != nil {
		return ""
	}
	for _, kv := range plan.Env {
		if v, ok := strings.CutPrefix(kv, "COMPOSE_PROFILES="); ok {
			return v
		}
	}
	return ""
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

// stopServices stops what of a sync's side's set runs (cli.DataStops),
// in one docker compose stop, so Compose orders them by depends_on
// (dependents first), saying what and why, and records them first
// (syncState): restoreServices brings back only those.
func stopServices(cwd string, set []cli.Stopped) error {
	stop := cli.StoppedNames(set)
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
	// What this sync stops, by its id: a sync that writes nothing starts
	// it again, and only it (restoreOwnStops).
	if id := os.Getenv(remote.SyncIDEnv); rec.ownID != id {
		rec.own, rec.ownID = nil, id
	}
	for _, name := range stop {
		if !slices.Contains(rec.stops, name) {
			rec.stops = append(rec.stops, name)
			rec.own = append(rec.own, name)
		}
	}
	if err := rec.write(cwd); err != nil {
		return err
	}
	if len(stop) == 0 {
		return nil
	}
	report.Info(cli.StoppingLine(set, stop))
	if err := compose(cwd, errOut, append([]string{"stop"}, stop...)...); err != nil {
		return fmt.Errorf("docker compose stop: %w", err)
	}
	return nil
}

// syncFirst is the record's line for a child's first release: it had no
// container at all when the sync started.
const syncFirst = "first"

// syncRecord is the record a sync or an import keeps (syncState):
// whether a sync started on a child's first release (no container at
// all: syncFirst), the services it stopped ("stop <name>" lines), those
// of them a sync stopped itself, by its id ("own <sync id> <name>": what
// a sync that writes nothing starts again; the rest is an earlier sync's
// or import's, kept down) and an import's hook while it runs ("writer
// <process group>", then " down" when its service was not running
// before the import: writerDown).
type syncRecord struct {
	first      bool
	stops      []string
	own        []string
	ownID      string
	writers    []int
	writerDown bool
}

// readSyncRecord reads the record; ok is false without one.
func readSyncRecord(cwd string) (rec syncRecord, ok bool) {
	state, err := os.ReadFile(filepath.Join(filepath.Dir(cwd), syncState))
	if err != nil {
		return rec, false
	}
	for _, line := range strings.Split(string(state), "\n") {
		action, name, _ := strings.Cut(line, " ")
		switch {
		case line == syncFirst:
			rec.first = true
		case action == "stop" && name != "" && !slices.Contains(rec.stops, name):
			rec.stops = append(rec.stops, name)
		case action == "own":
			if id, svc, ok := strings.Cut(name, " "); ok && svc != "" {
				rec.ownID = id
				rec.own = append(rec.own, svc)
			}
		case action == "writer":
			pgid, down, _ := strings.Cut(name, " ")
			if n, err := strconv.Atoi(pgid); err == nil {
				rec.writers = append(rec.writers, n)
				rec.writerDown = rec.writerDown || down == "down"
			}
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
	for _, name := range rec.own {
		b.WriteString("own " + rec.ownID + " " + name + "\n")
	}
	for _, pgid := range rec.writers {
		b.WriteString("writer " + strconv.Itoa(pgid))
		if rec.writerDown {
			b.WriteString(" down")
		}
		b.WriteString("\n")
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
// the side's whole set, a service down on purpose included. The
// stops of one left by an earlier sync that could not restore stay: their
// services come back with this one's. first is what the sync finds now:
// an earlier first release's mark goes once containers exist.
func startSyncState(cwd string, first bool) error {
	rec, _ := readSyncRecord(cwd)
	rec.first = first
	return rec.write(cwd)
}

// wipeData takes the child at cwd (base its directory) down for a
// reset, right after the reset's first claim: what an import a killed
// devopsy left may still extract, every container of every profile and
// the named volumes, every entry of mnt emptied as root; its shared/.env
// and its release, images included, stay. The record then says first
// release, nothing stopped: finish go creates the services with data
// and the child side's set, and the run step the rest.
func wipeData(cwd, projectDir, base string) error {
	who := base
	if to := os.Getenv(remote.SyncToTargetEnv); to != "" {
		who = "@" + to
	}
	report.Info("resetting " + who + ": every container and volume goes and mnt is emptied; its shared/.env stays")
	if err := removeContainers(importContainers(cwd)); err != nil {
		return err
	}
	if err := compose(cwd, errOut, "--profile", "*", "down", "--volumes", "--remove-orphans"); err != nil {
		return fmt.Errorf("docker compose down: %w", err)
	}
	// Named by the sync, so a holder deciding meanwhile removes it
	// (endWriters): no write outlives its sync.
	if err := emptyMnt(context.Background(), projectDir, remote.SyncContainerPrefix+os.Getenv(remote.SyncIDEnv)+"-wipe"); err != nil {
		return err
	}
	// Its owned entries back, as a release left them: finish go creates
	// the services with data, and a stream's import brings its service
	// up, on entries Docker would otherwise make root's.
	owners, mnt, err := projectOwners(cwd)
	if err == nil {
		err = ownMnt(context.Background(), mnt, owners, remote.SyncContainerPrefix+os.Getenv(remote.SyncIDEnv)+"-owners")
	}
	if err != nil {
		return err
	}
	rec, _ := readSyncRecord(cwd)
	rec.first, rec.stops, rec.own = true, nil, nil
	return rec.write(cwd)
}

// restoreOwnStops starts what a sync that wrote nothing stopped itself
// (the record's own lines with its id: none when its prepare failed
// before stopping, or another sync's) and keeps the rest recorded.
func restoreOwnStops(cwd string) error {
	rec, ok := readSyncRecord(cwd)
	id := os.Getenv(remote.SyncIDEnv)
	if !ok || id == "" || rec.ownID != id {
		return nil
	}
	own := rec.own
	if len(own) > 0 {
		exited, err := servicesIn(cwd, "exited")
		if err != nil {
			return err
		}
		if start := intersect(own, exited); len(start) > 0 {
			report.Info("starting " + strings.Join(start, " ") + ": nothing was written into their data")
			if err := compose(cwd, errOut, append([]string{"start"}, start...)...); err != nil {
				return fmt.Errorf("docker compose start: %w", err)
			}
			if err := waitHealthy(cwd, start); err != nil {
				return err
			}
		}
	}
	rec.stops = slices.DeleteFunc(rec.stops, func(name string) bool { return slices.Contains(own, name) })
	rec.own, rec.ownID = nil, ""
	return rec.write(cwd)
}

// healthyTimeout is how long restoreServices waits for a started service.
const healthyTimeout = 5 * time.Minute

// restoreServices starts what a sync or an import stopped and waits
// until it runs and is healthy, whatever happened before: what the
// record holds (syncState), else fallback (a sync's side's set; nothing
// for an import, whose mark and record are written together). It reads
// the containers' state, so it also recovers after an interrupted sync.
// With create, a child's first release gets its data services and the
// child side's set created (finish go): never on an abort, which leaves
// it not live.
func restoreServices(cwd string, services []cli.SyncService, create bool, fallback []string) error {
	stop := fallback
	var errs []error
	// A child's first release had no containers: nothing was stopped,
	// and the finish hook expects the services up (an app to sanitize
	// through): those with data and the child side's set, by labels (an
	// unlabeled app following its own database, an import app without
	// data), the closure because up creates dependencies but never
	// dependents. Created with one up, as the run step's up would a
	// moment later, so not in a profile that is not up (an up naming one
	// starts it); only then: a service without a container in a
	// project that has others was removed on purpose. The record says so
	// (imports created some since); without one, no containers.
	rec, recorded := readSyncRecord(cwd)
	first := rec.first
	if !recorded {
		states, err := serviceStates(cwd)
		if err != nil {
			return err
		}
		first = len(states) == 0
	}
	var created []string
	if first && create {
		children := cli.StoppedNames(cli.DataStops(services, cli.SyncChild, ""))
		profiles := composeProfiles(cwd)
		for _, s := range services {
			if (s.HasData || slices.Contains(children, s.Name)) && s.Active(profiles) {
				created = append(created, s.Name)
			}
		}
	}
	if len(created) > 0 {
		report.Info("creating " + strings.Join(created, " "))
		if err := compose(cwd, errOut, append([]string{"up", "-d", "--wait"}, created...)...); err != nil {
			errs = append(errs, fmt.Errorf("docker compose up: %w", err))
		}
	}
	stateFile := filepath.Join(filepath.Dir(cwd), syncState)
	// What a sync stopped comes back whatever its labels say now (an
	// inherited variable may have changed them since a cut one), if the
	// project still has it: compose start refuses a service it lacks.
	if recorded {
		var names []string
		for _, s := range services {
			names = append(names, s.Name)
		}
		stop = intersect(rec.stops, names)
	}
	if len(stop) > 0 {
		exited, err := servicesIn(cwd, "exited")
		if err != nil {
			errs = append(errs, err)
		}
		if start := intersect(stop, exited); len(start) > 0 {
			report.Info("starting " + strings.Join(start, " "))
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
	Name, Service, State, Health, Status, Labels string
}

// oneOff reports a compose run container (a hook's run --rm): ps lists
// it under its service, but it is not the service's container.
func (e psEntry) oneOff() bool {
	return strings.Contains(","+e.Labels+",", ",com.docker.compose.oneoff=True,")
}

// serviceEntries is parsePS without one-off containers: a stray run --rm
// (an import's, still running) must not stand for its service, or a
// stopped service would read as running and never be started again.
func serviceEntries(out []byte) ([]psEntry, error) {
	entries, err := parsePS(out)
	return slices.DeleteFunc(entries, psEntry.oneOff), err
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
	entries, err := serviceEntries(out.Bytes())
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
		entries, err := serviceEntries(out.Bytes())
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
