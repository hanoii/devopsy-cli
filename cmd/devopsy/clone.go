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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/compose-spec/compose-go/v2/dotenv"
	"golang.org/x/term"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// A clone, operator side: devopsy @<child> --clone, and the first release
// of a child. See remote/clone.go for the sessions and scripts.

// cloneJob is a clone about to run.
type cloneJob struct {
	child, parent *remote.Target
	// projectName and parentName are the compose projects of child and
	// parent.
	projectName, parentName string
	// sameHost: no SSH between the servers, one container copies.
	sameHost bool
	// to and port are where the parent's server reaches the child's
	// (user@hostname), when they differ.
	to, port string
	// args go to rsync (after --clone's --).
	args []string
	// first: the child's first release, with no data of its own yet. Its
	// inherited variables are written before the upload, so the variables
	// check sees them, and stay if the clone fails: the next release is a
	// first one again. A re-clone inherits after the copy succeeded, so a
	// failed one leaves the child's own values with its own data.
	first bool
}

// clonePreflight checks a clone of p into c can start, before anything
// changes: c is a child with a run step, p another environment of the same
// project, released; across servers, an SSH agent to forward and a child
// the parent's server can reach.
func clonePreflight(c, p *remote.Target, projectName string, args []string) (*cloneJob, error) {
	if c.Parent == "" {
		return nil, fmt.Errorf("@%s is not a child: environment %s has no parent: in %s", c.Name, c.Name, cmp.Or(c.File, "the project's config"))
	}
	if p == nil {
		return nil, fmt.Errorf("environment %s: its parent %s is not resolved", c.Name, c.Parent)
	}
	if p.Project == nil || c.Project == nil || p.Project.Name != c.Project.Name {
		return nil, fmt.Errorf("@%s is not an environment of %s: a clone copies between environments of one project", p.Address, c.Project.Name)
	}
	if c.Release == nil || len(remote.StepArgs(c.Release.Run)) == 0 {
		return nil, errors.New(missingSteps(c, "release"))
	}
	job := &cloneJob{child: c, parent: p, projectName: projectName, parentName: p.ComposeName(), args: args}
	job.sameHost = sameHost(c.Host, p.Host)
	if p.Address == c.Address || (job.sameHost && p.Path == c.Path) {
		return nil, fmt.Errorf("@%s and its parent are the same environment (%s:%s)", c.Address, p.Host, p.Path)
	}
	if !job.sameHost {
		if os.Getenv("SSH_AUTH_SOCK") == "" {
			return nil, fmt.Errorf("the parent's server (%s) reaches the child's (%s) with your SSH agent, forwarded for the clone, and none is running here: eval $(ssh-agent); ssh-add (in CI, add the key to an agent)", p.Host, c.Host)
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

// markerWriter passes a session's stdout through to w, but swallows the
// line starting with remote.CloneMarker and hands over the directory it
// names.
type markerWriter struct {
	w      io.Writer
	marker chan string
	mu     sync.Mutex
	line   []byte
	found  bool
}

func newMarkerWriter(w io.Writer) *markerWriter {
	return &markerWriter{w: w, marker: make(chan string, 1)}
}

func (m *markerWriter) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.found {
		return m.w.Write(p)
	}
	n := len(p)
	for len(p) > 0 && !m.found {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			m.line = append(m.line, p...)
			p = nil
			break
		}
		line := append(m.line, p[:i]...)
		m.line = nil
		p = p[i+1:]
		if base, ok := strings.CutPrefix(strings.TrimRight(string(line), "\r"), remote.CloneMarker); ok {
			m.found = true
			m.marker <- base
			continue
		}
		if _, err := m.w.Write(append(line, '\n')); err != nil {
			return n, err
		}
	}
	// A partial line that cannot become the marker goes through now
	// (progress output); the rest after the marker as it comes.
	if len(m.line) > 0 && !strings.HasPrefix(remote.CloneMarker, string(m.line)) && !bytes.HasPrefix(m.line, []byte(remote.CloneMarker)) {
		if _, err := m.w.Write(m.line); err != nil {
			return n, err
		}
		m.line = nil
	}
	if m.found && len(p) > 0 {
		if _, err := m.w.Write(p); err != nil {
			return n, err
		}
	}
	return n, nil
}

// flush writes what is left of a last line without a newline.
func (m *markerWriter) flush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.line) > 0 {
		_, _ = m.w.Write(m.line)
		m.line = nil
	}
}

// runCloneSession runs script, a holder (HolderScript, or ActivateScript
// with a clone) on the child's server; once it prints the marker, the push
// session on the parent's, then go or abort to the holder. The holder's
// exit code is the result.
func runCloneSession(job *cloneJob, script string, rlog *releaseLog, color bool) int {
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
	mw := newMarkerWriter(os.Stdout)
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := remote.SSHWith(job.child, remote.KeepAlive, script, r, mw, false, rlog)
		done <- result{code, err}
	}()
	// Interrupted before the marker, devopsy just exits: the holder gets
	// no answer and restores on its own. After it, the push ends first, so
	// nothing copies into the child once its services are back, then the
	// holder gets abort.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	var res result
	select {
	case res = <-done:
	case <-sigs:
		say(red, "devopsy: interrupted: the child's server restores what it paused or stopped")
		return 130
	case base := <-mw.marker:
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
			say(red, "devopsy: interrupted: stopping the copy from @"+job.parent.Address+", then aborting the clone")
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
			mw.flush()
			return cmp.Or(res.code, 1)
		}
		answer := "go\n"
		if interrupted {
			answer = "abort\n"
		} else if code != 0 {
			answer = "abort\n"
			say(red, fmt.Sprintf("devopsy: the copy from @%s failed (%d): aborting the clone", job.parent.Address, code))
		} else if !job.first {
			// The copy is in: now the parent's variables, which the run
			// step uses. A failure here aborts, and nothing was written.
			// The holder holds the child's lock: the write takes none.
			inherited := make(chan error, 1)
			go func() { inherited <- inheritVars(ctx, job, rlog, color, true) }()
			select {
			case err := <-inherited:
				if err != nil {
					answer = "abort\n"
					say(red, "devopsy: inherit: "+err.Error()+": aborting the clone")
				}
			case <-sigs:
				interrupted = true
				answer = "abort\n"
				say(red, "devopsy: interrupted: aborting the clone")
				cancel()
				<-inherited
			}
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
			mw.flush()
			return 130
		}
		res = <-done
	}
	mw.flush()
	if res.err != nil {
		say(red, res.err.Error())
		return 1
	}
	return res.code
}

// runPush is the push session: devopsy --clone-push on the parent's
// server, into the child's directory base. Across servers it forwards the
// SSH agent, so the parent's server reaches the child's for this session
// only.
func runPush(ctx context.Context, job *cloneJob, base string, rlog *releaseLog, color bool) int {
	p := job.parent
	env := map[string]string{
		remote.CloneToEnv:         job.to,
		remote.CloneSSHPortEnv:    job.port,
		remote.CloneToBaseEnv:     base,
		remote.CloneToDirEnv:      base + "/current/.devopsy",
		remote.CloneToProjectEnv:  job.projectName,
		remote.CloneToTargetEnv:   job.child.Address,
		remote.CloneArgsEnv:       strings.Join(job.args, " "),
		remote.CloneFromTargetEnv: p.Address,
		remote.CloneWatchEnv:      "1",
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
	code, err := remote.SSHContext(ctx, p, extra, remote.PushScript(p, job.parentName, env), r, nil, false, rlog)
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
// so a resync replaces it. The child's own shared/.env wins over it, and
// a key set there too is named. A key the parent does not set is reported,
// never written. Values travel over SSH's stdin, and only names are printed.
func inheritVars(ctx context.Context, job *cloneJob, rlog *releaseLog, color bool, locked bool) error {
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
	code, err = remote.SSHContext(ctx, c, nil, remote.InheritWriteScript(c, keys, locked), strings.NewReader(lines.String()), &res, false, nil)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("could not write @%s's inherited variables (%d)", c.Address, code)
	}
	if len(keys) > 0 {
		say(cyan, fmt.Sprintf("Inherited %s from @%s into @%s's shared/%s.", strings.Join(keys, ", "), p.Address, c.Address, remote.InheritFile))
	}
	if shadowed := strings.Fields(res.String()); len(shadowed) > 0 {
		say(yellow, fmt.Sprintf("@%s's own shared .env also sets %s, which wins there (devopsy @%s --vars unset to inherit it).", c.Address, strings.Join(shadowed, ", "), c.Name))
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
		return fail("usage: devopsy @" + t.Name + " --inherit")
	}
	if t.Parent == "" {
		return fail(fmt.Sprintf("@%s is not a child: no parent: in its config", t.Address))
	}
	if t.ParentErr != nil {
		return fail(t.ParentErr.Error())
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
	job := &cloneJob{child: t, parent: p, projectName: projectName, parentName: p.ComposeName()}
	if err := inheritVars(context.Background(), job, newReleaseLog("inherit", t), color, false); err != nil {
		return fail("inherit: " + err.Error())
	}
	return 0
}

// runClone implements devopsy @<child> --clone [--from <address>] [--yes]
// [-- args...]: the parent's data into the child's current release, then
// its run step.
func runClone(t *remote.Target, projectDir string, projectEnv func(string) (string, bool), projectName string, args []string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(os.Stderr, red, msg, color)
		return 1
	}
	usage := "usage: devopsy @" + t.Name + " --clone [--from <address>] [--yes] [-- <rsync args>...]"
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
		return fail(t.ParentErr.Error())
	}
	if from != "" {
		var err error
		if p, err = remote.LoadTarget(projectDir, from, projectEnv); err != nil {
			return fail("--from: " + err.Error())
		}
	}
	job, err := clonePreflight(t, p, projectName, rsyncArgs)
	if err != nil {
		return fail(err.Error())
	}
	run := remote.StepArgs(t.Release.Run)
	if !yes {
		if err := confirmClone(t, p, "runs 'devopsy "+strings.Join(run, " ")+"' there"); err != nil {
			return fail(cloneRefused(err, "Nothing cloned."))
		}
	}

	rlog := newReleaseLog("clone", t)
	msg := fmt.Sprintf("Cloning @%s into @%s (%s:%s)...", p.Address, t.Address, t.Host, t.Path)
	cli.Fprint(os.Stderr, cyan, msg, color)
	rlog.Line(msg)
	code := runCloneSession(job, remote.HolderScript(t, projectName, p.Address, rsyncArgs, run), rlog, color)

	// The log goes with the release cloned into, after its own.
	rlog.Line(fmt.Sprintf("devopsy: clone exited %d", code))
	text := rlog.Text()
	if m := remote.CloningInto.FindSubmatch(text); m != nil {
		text = append([]byte("\n"), text...)
		if c, err := remote.SSH(t, remote.LogSaveScript(t, string(m[1]), true), bytes.NewReader(text), io.Discard, false); err != nil || c != 0 {
			cli.Fprint(os.Stderr, yellow, fmt.Sprintf("devopsy: could not save the clone log on the server (%v, %d)", err, c), color)
		}
	}
	return code
}

// errNotConfirmed: the name typed was not the environment's.
var errNotConfirmed = errors.New("")

// cloneRefused is what a refused confirmation prints, then nothing.
func cloneRefused(err error, nothing string) string {
	if err == errNotConfirmed {
		return nothing
	}
	return err.Error() + "\n" + nothing
}

// confirmClone asks for the child's environment name before its data is
// replaced, like --destroy; then says what follows the copy. Not at a
// terminal it fails: --yes is the way there.
func confirmClone(t, p *remote.Target, then string) error {
	where := t.Host + ":" + t.Path
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return errors.New("--clone replaces the data of " + where + " with its parent's: add --yes when not at a terminal")
	}
	fmt.Fprintf(os.Stderr, "This replaces the data of @%s (%s, shared/mnt) with @%s's (%s:%s), then %s. Type %s to go on: ",
		t.Address, where, p.Address, p.Host, p.Path, then, t.Name)
	line, _ := stdinReader.ReadString('\n')
	if strings.TrimSpace(line) != t.Name {
		return errNotConfirmed
	}
	return nil
}

// Server side: the hidden flags a clone runs.

// errOut is stderr for what clones run on servers: never an *os.File, so
// commands write to a pipe devopsy drains and never die of a broken pipe
// when the session that called devopsy is gone (they would leave services
// paused or stopped).
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

// keep sends what follows to $base/.devopsy-clone.log too, like the
// holder does on the child when nobody reads the session any more.
func (q *quietWriter) keep(base string) {
	f, err := os.OpenFile(filepath.Join(base, cloneLog), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return
	}
	q.mu.Lock()
	q.file = f
	q.mu.Unlock()
	sayErr("devopsy: " + time.Now().UTC().Format("2006-01-02 15:04:05") + ": the clone's session is gone: restoring what was paused or stopped")
}

// cloneState records, next to cloneLog, the services prepare paused or
// stopped ("pause app", "stop db" lines), so finish restores those and
// leaves a service that was down before the clone down (a maintenance
// stop). Written before the first pause, removed by a finish that
// restored everything; absent, finish restores every labeled service.
const cloneState = ".devopsy-clone.services"

// cloneLog is the holder's and the push's log for what happened after the
// operator's devopsy was gone, in the environment's directory.
const cloneLog = ".devopsy-clone.log"

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
// SIGPIPE (the session gone), so a clone stops copying and restores its
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
// session's stdin open (CloneWatchEnv), so EOF means the session is gone.
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
	full := append([]string{"run", "--rm", "--name", name}, args...)
	trace(append([]string{"docker"}, full...))
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Stdout, cmd.Stderr = errOut, errOut
	cmd.Cancel = func() error {
		_ = exec.Command("docker", "rm", "-f", name).Run()
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
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

// ensureRsyncImage builds devopsy's rsync image on this host when missing.
func ensureRsyncImage(ctx context.Context) error {
	if exec.CommandContext(ctx, "docker", "image", "inspect", remote.RsyncImage).Run() == nil {
		return nil
	}
	sayErr("devopsy: building " + remote.RsyncImage)
	cmd := exec.CommandContext(ctx, "docker", remote.BuildRsyncImageArgs()...)
	cmd.Stdin = strings.NewReader(strings.Join(remote.RsyncDockerfile, "\n") + "\n")
	cmd.Stdout, cmd.Stderr = io.Discard, errOut // stdout: the image's id
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("building %s: %w", remote.RsyncImage, err)
	}
	return nil
}

// cloneEnv reads the clone's variables in a push: the child's directory and
// project are required.
func cloneEnv(flag string) (toDir, toProject string, err error) {
	toDir, toProject = os.Getenv(remote.CloneToDirEnv), os.Getenv(remote.CloneToProjectEnv)
	if toDir == "" || toProject == "" {
		return "", "", fmt.Errorf("%s only runs in a clone's push, on the parent's server (devopsy @<child> --clone)", flag)
	}
	return toDir, toProject, nil
}

// runClonePush implements the hidden --clone-push, on the parent's server
// in its current release: the clone capability's push, else the generic
// path (pause and stop the parent's labeled services, rsync the entries of
// mnt their binds hold, start and unpause them).
func runClonePush(cwd string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(errOut, red, "devopsy: "+msg, color)
		return 1
	}
	if _, _, err := cloneEnv(cli.ClonePushFlag); err != nil {
		return fail(err.Error())
	}
	if to := os.Getenv(remote.CloneToEnv); to != "" && os.Getenv("SSH_AUTH_SOCK") == "" {
		host, _ := os.Hostname()
		return fail(fmt.Sprintf("no SSH agent was forwarded to %s: a restricted key refuses it (authorize it with `restrict,agent-forwarding,port-forwarding`, or use a key without restrictions), and so does sshd with AllowAgentForwarding no", host))
	}
	projectDir, err := cli.FindProjectDir(cwd)
	if err != nil {
		return fail(err.Error())
	}
	ctx, stop := interruptible()
	defer stop()
	if os.Getenv(remote.CloneWatchEnv) != "" {
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

	_, services, err := cli.CloneServices(cwd, os.Environ())
	if err != nil {
		return fail(err.Error())
	}
	labeled := slices.ContainsFunc(services, func(s cli.CloneService) bool { return s.Labeled })
	custom := cli.ClonePolicy(services, cli.CloneCustom)
	push := filepath.Join(projectDir, "capabilities", "clone", "push")
	hasPush := isExecutable(push)
	switch {
	case len(custom) > 0 && !hasPush:
		return fail(fmt.Sprintf("%s labeled %s=%s: the parent's release needs an executable .devopsy/capabilities/clone/push to copy it", strings.Join(custom, ", "), cli.CloneLabel, cli.CloneCustom))
	case len(custom) == 0 && hasPush:
		sayErr("devopsy: capabilities/clone/push present but no service labeled " + cli.CloneLabel + "=" + cli.CloneCustom + ": not run")
	}
	if !labeled {
		sayErr("devopsy: nothing to clone: no service labeled " + cli.CloneLabel)
		return 0
	}

	// The generic path first: flush, pause and stop, rsync, start and
	// unpause the services labeled live, pause and stop.
	if dirs := cli.CloneDirs(services); len(dirs) > 0 {
		if msg := cli.CloneWholeNotice(services); msg != "" {
			sayWarn(msg)
		}
		// Before the parent pauses anything: what the copy would change
		// under a running service of the child, left running by its own
		// labels, is a mismatch, not a clone.
		child, err := childServices(ctx)
		if err != nil {
			return fail(err.Error())
		}
		if conflicts := cloneConflicts(services, child); len(conflicts) > 0 {
			return fail(strings.Join(conflicts, "\ndevopsy: "))
		}
		if err := ensureRsyncImage(ctx); err != nil {
			return fail(err.Error())
		}
		if err := prepareServices(cwd, projectDir, services); err != nil {
			// What it paused or stopped before failing comes back: finish
			// reads the containers' state, not what prepare did.
			code := fail(err.Error())
			if err := finishServices(cwd, services); err != nil {
				fail(err.Error())
			}
			return code
		}
		code := 0
		if err := cloneRsync(ctx, projectDir, dirs, strings.Fields(os.Getenv(remote.CloneArgsEnv))); err != nil {
			code = fail(err.Error())
		}
		if err := finishServices(cwd, services); err != nil {
			code = fail(err.Error())
		}
		if code != 0 {
			return code
		}
	} else if len(custom) == 0 {
		sayErr("devopsy: nothing to clone: no bind mounts under .devopsy/mnt for the services labeled " + cli.CloneLabel)
		return 0
	}

	// Then the custom services, the capability's, unless the clone was
	// interrupted meanwhile: the child is restoring its services already.
	if len(custom) == 0 {
		return 0
	}
	if ctx.Err() != nil {
		return fail("interrupted: 'devopsy --capability clone push' not run")
	}
	sayErr("devopsy: running 'devopsy --capability clone push' (" + strings.Join(custom, " ") + ")")
	self, err := os.Executable()
	if err != nil {
		self = "devopsy"
	}
	// Its own process group, all of it ended when the clone is
	// interrupted: a pipeline's commands too (a dump and its import).
	cmd := exec.CommandContext(ctx, self, "--capability", "clone", "push")
	cmd.Env = append(os.Environ(), remote.CloneCustomEnv+"="+strings.Join(custom, " "))
	cmd.Stdout, cmd.Stderr = errOut, errOut
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	err = cmd.Run()
	if ctx.Err() != nil {
		return fail("interrupted: 'devopsy --capability clone push' ended")
	}
	return exitCode(err)
}

// childService is a line of the child's --clone-services list: a service,
// its policy there, its containers' state (running, paused, exited...,
// none without one, "" from a devopsy that does not say) and its entries
// of mnt.
type childService struct {
	Name, Policy, State string
	Dirs                []string
}

// childServices asks the child's release for its services, through
// --clone-exec, after it prepared them.
func childServices(ctx context.Context) ([]childService, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, self, cli.CloneExecFlag, cli.CloneServicesFlag, "list")
	cmd.Stdout, cmd.Stderr = &out, errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("listing the child's services: %w", err)
	}
	var services []childService
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		f := strings.Split(line, "\t")
		if len(f) < 3 {
			continue
		}
		cs := childService{Name: f[0], Policy: f[1]}
		if len(f) >= 4 {
			cs.State, cs.Dirs = f[2], strings.Fields(f[3])
		} else {
			cs.Dirs = strings.Fields(f[2])
		}
		services = append(services, cs)
	}
	return services, nil
}

// cloneConflicts names the child's services the copy would change under:
// running, with an entry the parent copies among their mounts, and not
// labeled live there (which says so on purpose). A pause or stop label
// on the child has already paused or stopped it; custom or no label
// leaves it running, and the two sides disagree.
func cloneConflicts(parent []cli.CloneService, child []childService) []string {
	dirs := cli.CloneDirs(parent)
	if len(dirs) == 0 {
		return nil
	}
	copied := func(d string) bool { return d == "." || dirs[0] == "." || slices.Contains(dirs, d) }
	from := func(d string) string {
		for _, s := range parent {
			if s.Policy != cli.CloneSkip && s.Policy != cli.CloneCustom && (d == "." || slices.Contains(s.Dirs, d) || slices.Contains(s.Dirs, ".")) {
				return s.Name + " (" + cli.CloneLabel + "=" + s.Policy + ")"
			}
		}
		return "another service"
	}
	var conflicts []string
	for _, c := range child {
		if c.State != "running" && c.State != "restarting" || c.Policy == cli.CloneLive {
			continue
		}
		for _, d := range c.Dirs {
			if !copied(d) {
				continue
			}
			label := "labeled " + cli.CloneLabel + "=" + c.Policy
			if c.Policy == cli.CloneSkip {
				label = "without a " + cli.CloneLabel + " label"
			}
			conflicts = append(conflicts, fmt.Sprintf("the child's %s is running, %s, and mnt/%s would change under it: the parent copies it for %s. Label it alike on both sides (an inherited variable keeps them so), or live to copy under a running service.", c.Name, label, d, from(d)))
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
// custom database's datadir, an unlabeled service's directory). The daemon
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

// cloneRsync copies dirs (entries of the parent's mnt) into the child's,
// replacing what is there (--delete), as root in devopsy's rsync image.
// Same server: one container with both mounts. Across servers: an rsync
// daemon here, reached by the child's server through a reverse forward of
// an SSH session the forwarded agent opens.
func cloneRsync(ctx context.Context, projectDir string, dirs, args []string) error {
	toDir, _, err := cloneEnv(cli.CloneRsyncFlag)
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
			sayErr("devopsy: mnt/" + d + " does not exist here: skipped")
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

	to := os.Getenv(remote.CloneToEnv)
	if to == "" {
		childMnt, err := filepath.EvalSymlinks(path.Join(toDir, "mnt"))
		if err != nil {
			return fmt.Errorf("the child's mnt: %w", err)
		}
		for i, d := range present {
			sayErr("devopsy: rsync mnt/" + d)
			run := append(append([]string{"-v", parentMnt + ":/src:ro", "-v", childMnt + ":/dst", remote.RsyncImage}, rsync...), args...)
			name := fmt.Sprintf("devopsy-clone-%d-%d", os.Getpid(), i)
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

	id, err := dockerOut(ctx, "run", "--rm", "-d", "-v", parentMnt+":/mnt:ro", "-e", "RSYNCD_CONF="+rsyncdConf(present, files), remote.RsyncImage,
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
	for i := 0; i < 50; i++ {
		if exec.CommandContext(ctx, "docker", "exec", id, "rsync", "rsync://127.0.0.1/").Run() == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// A port free on the child's server: ssh fails at once when the
	// forward cannot listen (ExitOnForwardFailure), then another. Never
	// ClearAllForwardings here: it clears command-line forwardings too.
	for attempt := 0; ; attempt++ {
		port := fmt.Sprint(20000 + rand.IntN(40000))
		sshArgs := append(append([]string{"-o", "ExitOnForwardFailure=yes"}, remote.ChildSSH...), remote.KeepAlive...)
		sshArgs = append(sshArgs, "-T", "-R", "127.0.0.1:"+port+":"+ip+":873")
		if p := os.Getenv(remote.CloneSSHPortEnv); p != "" {
			sshArgs = append(sshArgs, "-p", p)
		}
		sshArgs = append(sshArgs, to, "sh -c "+remote.Quote(remote.CloneReceiveScript(toDir, port, present, files, args)))
		trace(append([]string{"ssh"}, sshArgs[:len(sshArgs)-1]...))
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

// mntEntryRe is an entry of mnt --clone-rsync takes: a name, or "." for
// all of it.
var mntEntryRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// runCloneRsync implements the hidden --clone-rsync <entry>... [--
// <rsync args>...] for a clone capability's push: cloneRsync with the
// arguments after --, only those. DEVOPSY_CLONE_ARGS are the push's to
// use (its own switches, or passed here explicitly after --).
func runCloneRsync(cwd string, args []string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(errOut, red, "devopsy: "+msg, color)
		return 1
	}
	if _, _, err := cloneEnv(cli.CloneRsyncFlag); err != nil {
		return fail(err.Error())
	}
	var entries, extra []string
	for i, a := range args {
		if a == "--" {
			extra = args[i+1:]
			break
		}
		entries = append(entries, a)
	}
	projectDir, err := cli.FindProjectDir(cwd)
	if err != nil {
		return fail(err.Error())
	}
	if len(entries) == 0 {
		return fail("usage: devopsy " + cli.CloneRsyncFlag + " <entry of .devopsy/mnt>... [-- <rsync args>...]")
	}
	for _, e := range entries {
		if e != "." && (!mntEntryRe.MatchString(e) || e == "..") {
			return fail(fmt.Sprintf("%s %s: an entry of .devopsy/mnt, a name (or . for all of it)", cli.CloneRsyncFlag, e))
		}
		if _, err := os.Lstat(filepath.Join(projectDir, "mnt", e)); err != nil {
			return fail(fmt.Sprintf("%s %s: .devopsy/mnt/%s does not exist here", cli.CloneRsyncFlag, e, e))
		}
	}
	ctx, stop := interruptible()
	defer stop()
	if err := ensureRsyncImage(ctx); err != nil {
		return fail(err.Error())
	}
	if err := cloneRsync(ctx, projectDir, entries, extra); err != nil {
		return fail(err.Error())
	}
	return 0
}

// cleanEnvKeys are what the child's devopsy gets of the parent's
// environment on the same server: the machine's, never the project's. The
// caller's environment wins over a project's .env, so the parent's
// secrets and compose project must not reach the child's commands.
var cleanEnvKeys = regexp.MustCompile(`^(PATH|HOME|USER|LOGNAME|LANG|LC_[A-Z]+|TERM|TZ|DOCKER_[A-Z_]+|DEVOPSY_VERBOSE|DEVOPSY_HOME|XDG_CONFIG_HOME)=`)

func cleanEnv(projectName string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if cleanEnvKeys.MatchString(kv) {
			env = append(env, kv)
		}
	}
	return append(env, "COMPOSE_PROJECT_NAME="+projectName)
}

// runCloneExec implements the hidden --clone-exec <args>...: devopsy args
// in the child's current release, as its project, stdin, stdout and stderr
// through, so a push can pipe a dump into it. Across servers over SSH (the
// forwarded agent); on the same one with a clean environment.
func runCloneExec(args []string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(os.Stderr, red, "devopsy: "+msg, color)
		return 1
	}
	toDir, toProject, err := cloneEnv(cli.CloneExecFlag)
	if err != nil {
		return fail(err.Error())
	}
	if len(args) == 0 {
		return fail("usage: devopsy " + cli.CloneExecFlag + " <devopsy arguments>...")
	}
	if to := os.Getenv(remote.CloneToEnv); to != "" {
		remote.Verbose = cli.VerboseLevel(os.Getenv(cli.VerboseEnv)) > 0
		sshArgs := append(append([]string{"-o", "ClearAllForwardings=yes"}, remote.ChildSSH...), remote.KeepAlive...)
		sshArgs = append(sshArgs, "-T")
		if p := os.Getenv(remote.CloneSSHPortEnv); p != "" {
			sshArgs = append(sshArgs, "-p", p)
		}
		sshArgs = append(sshArgs, to, "sh -c "+remote.Quote(remote.CloneExecScript(toDir, toProject, args)))
		trace(append([]string{"ssh"}, sshArgs[:len(sshArgs)-1]...))
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

// runCloneServices implements the hidden --clone-services
// prepare|finish|list, in the current project: pause the services labeled
// pause, then stop those labeled stop; start those again, wait until they
// are healthy, unpause; or list them, name, policy, state and entries of
// mnt, one per line.
func runCloneServices(cwd string, args []string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(errOut, red, "devopsy: "+msg, color)
		return 1
	}
	if len(args) != 1 || !slices.Contains([]string{"prepare", "finish", "list"}, args[0]) {
		return fail("usage: devopsy " + cli.CloneServicesFlag + " prepare|finish|list")
	}
	projectDir, services, err := cli.CloneServices(cwd, os.Environ())
	if err != nil {
		return fail(err.Error())
	}
	switch args[0] {
	case "list":
		states, err := serviceStates(cwd)
		if err != nil {
			return fail(err.Error())
		}
		for _, s := range services {
			fmt.Printf("%s\t%s\t%s\t%s\n", s.Name, s.Policy, cmp.Or(states[s.Name], "none"), strings.Join(s.Dirs, " "))
		}
		return 0
	case "prepare":
		// A session gone is no reason to die halfway: the holder
		// restores what this paused or stopped.
		signal.Ignore(syscall.SIGPIPE, syscall.SIGHUP)
		err = prepareServices(cwd, projectDir, services)
	default:
		signal.Ignore(syscall.SIGPIPE, syscall.SIGHUP)
		err = finishServices(cwd, services)
	}
	if err != nil {
		return fail(err.Error())
	}
	return 0
}

// compose runs docker compose args in the project at cwd, with its
// environment.
func compose(cwd string, stdout io.Writer, args ...string) error {
	plan, err := cli.Build(cwd, append([]string{"--"}, args...), os.Environ())
	if err != nil {
		return err
	}
	trace(plan.Args)
	cmd := exec.Command("docker", plan.Args[1:]...)
	cmd.Env = plan.Env
	cmd.Stdout, cmd.Stderr = stdout, errOut
	return cmd.Run()
}

// composeServices lists the services docker compose ps prints for args.
func composeList(cwd string, args ...string) ([]string, error) {
	var out bytes.Buffer
	if err := compose(cwd, &out, append([]string{"ps", "--services"}, args...)...); err != nil {
		return nil, err
	}
	return strings.Fields(out.String()), nil
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

// prepareServices runs the clone capability's flush, when the project has
// one, for each running service labeled live or pause, then pauses the
// running services labeled pause, then stops those labeled stop. When
// stopping fails, it unpauses what it paused; a failing flush changes
// nothing after it.
func prepareServices(cwd, projectDir string, services []cli.CloneService) error {
	pause, stop := cli.ClonePolicy(services, cli.ClonePause), cli.ClonePolicy(services, cli.CloneStop)
	var flush []string
	if isExecutable(filepath.Join(projectDir, "capabilities", "clone", "flush")) {
		for _, s := range services {
			if s.Policy == cli.CloneLive || s.Policy == cli.ClonePause {
				flush = append(flush, s.Name)
			}
		}
	}
	if len(flush)+len(pause)+len(stop) == 0 {
		return nil
	}
	running, err := composeList(cwd, "--status", "running")
	if err != nil {
		return fmt.Errorf("docker compose ps: %w", err)
	}
	if len(flush) > 0 {
		self, err := os.Executable()
		if err != nil {
			self = "devopsy"
		}
		for _, name := range intersect(flush, running) {
			sayErr("devopsy: running 'devopsy --capability clone flush " + name + "'")
			cmd := exec.Command(self, "--capability", "clone", "flush", name)
			cmd.Dir = cwd
			cmd.Stdout, cmd.Stderr = errOut, errOut
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("the clone capability's flush %s: %w", name, err)
			}
		}
	}
	// A container in a restart loop (a database on a datadir an aborted
	// copy left half written) is up as far as the copy is concerned, and
	// the guard on the parent's side counts it as running: stop it too.
	restarting, err := composeList(cwd, "--status", "restarting")
	if err != nil {
		return fmt.Errorf("docker compose ps: %w", err)
	}
	pause, stop = intersect(pause, running), intersect(stop, append(running, restarting...))
	if len(pause)+len(stop) == 0 {
		return nil
	}
	state := ""
	for _, name := range pause {
		state += "pause " + name + "\n"
	}
	for _, name := range stop {
		state += "stop " + name + "\n"
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(cwd), cloneState), []byte(state), 0o644); err != nil {
		return fmt.Errorf("recording the services to restore: %w", err)
	}
	if len(pause) > 0 {
		sayErr("devopsy: pausing " + strings.Join(pause, " "))
		if err := compose(cwd, errOut, append([]string{"pause"}, pause...)...); err != nil {
			return fmt.Errorf("docker compose pause: %w", err)
		}
	}
	if len(stop) > 0 {
		sayErr("devopsy: stopping " + strings.Join(stop, " "))
		if err := compose(cwd, errOut, append([]string{"stop"}, stop...)...); err != nil {
			if len(pause) > 0 {
				_ = compose(cwd, errOut, append([]string{"unpause"}, pause...)...)
			}
			return fmt.Errorf("docker compose stop: %w", err)
		}
	}
	return nil
}

// healthyTimeout is how long finishServices waits for a started service.
const healthyTimeout = 5 * time.Minute

// finishServices starts the stopped services labeled stop, waits until
// they run and are healthy, then unpauses the paused ones labeled pause,
// whatever happened before: those prepare recorded (cloneState), else
// every one. It reads the containers' state, so it also recovers after
// an interrupted clone.
func finishServices(cwd string, services []cli.CloneService) error {
	pause, stop := cli.ClonePolicy(services, cli.ClonePause), cli.ClonePolicy(services, cli.CloneStop)
	var errs []error
	// A child's first release has no containers yet: nothing was paused
	// or stopped, and the finish hook still expects the labeled services
	// up. Create those without a container, as the run step's up will.
	states, err := serviceStates(cwd)
	if err != nil {
		return err
	}
	var create []string
	for _, name := range append(append([]string{}, pause...), stop...) {
		if states[name] == "" {
			create = append(create, name)
		}
	}
	if len(create) > 0 {
		sayErr("devopsy: creating " + strings.Join(create, " "))
		if err := compose(cwd, errOut, append([]string{"up", "-d", "--wait"}, create...)...); err != nil {
			errs = append(errs, fmt.Errorf("docker compose up: %w", err))
		}
	}
	stateFile := filepath.Join(filepath.Dir(cwd), cloneState)
	if state, err := os.ReadFile(stateFile); err == nil {
		var recordedPause, recordedStop []string
		for _, line := range strings.Split(string(state), "\n") {
			if action, name, ok := strings.Cut(line, " "); ok {
				switch action {
				case "pause":
					recordedPause = append(recordedPause, name)
				case "stop":
					recordedStop = append(recordedStop, name)
				}
			}
		}
		pause, stop = intersect(pause, recordedPause), intersect(stop, recordedStop)
	}
	if len(pause)+len(stop) == 0 {
		if len(errs) == 0 {
			_ = os.Remove(stateFile)
		}
		return errors.Join(errs...)
	}
	if len(stop) > 0 {
		exited, err := composeList(cwd, "--all", "--status", "exited")
		if err != nil {
			errs = append(errs, fmt.Errorf("docker compose ps: %w", err))
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
	if len(pause) > 0 {
		paused, err := composeList(cwd, "--status", "paused")
		if err != nil {
			errs = append(errs, fmt.Errorf("docker compose ps: %w", err))
		}
		if unpause := intersect(pause, paused); len(unpause) > 0 {
			sayErr("devopsy: unpausing " + strings.Join(unpause, " "))
			if err := compose(cwd, errOut, append([]string{"unpause"}, unpause...)...); err != nil {
				errs = append(errs, fmt.Errorf("docker compose unpause: %w", err))
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
	Service, State, Health string
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
		ready := len(entries) > 0
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
// only find out after the child's services were paused. A missing
// ssh-add is not an error: ssh itself reports later.
func agentHasKeys(parent, child string) error {
	err := exec.Command("ssh-add", "-l").Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return nil
	}
	switch exit.ExitCode() {
	case 1:
		return fmt.Errorf("the parent's server (%s) reaches the child's (%s) with your SSH agent, forwarded for the clone, and it holds no key: ssh-add the key %s accepts (in CI, each server's CI key)", parent, child, child)
	case 2:
		return fmt.Errorf("the parent's server (%s) reaches the child's (%s) with your SSH agent, forwarded for the clone, and none answers at SSH_AUTH_SOCK (%s): eval $(ssh-agent); ssh-add (in CI, add the key to an agent)", parent, child, os.Getenv("SSH_AUTH_SOCK"))
	}
	return nil
}
