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
		code, err := remote.SSHLog(job.child, script, r, mw, false, rlog)
		done <- result{code, err}
	}()
	var res result
	select {
	case res = <-done:
	case base := <-mw.marker:
		code := runPush(job, base, rlog, color)
		answer := "go\n"
		if code != 0 {
			answer = "abort\n"
			say(red, fmt.Sprintf("devopsy: the copy from @%s failed (%d): aborting the clone", job.parent.Address, code))
		}
		_, _ = w.WriteString(answer)
		w.Close()
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
func runPush(job *cloneJob, base string, rlog *releaseLog, color bool) int {
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
	}
	var extra []string
	where := "same server"
	if !job.sameHost {
		extra = []string{"-o", "ForwardAgent=yes"}
		where = "from " + p.Host + " to " + job.to
	}
	msg := fmt.Sprintf("Copying @%s (%s:%s) into @%s, %s...", p.Address, p.Host, p.Path, job.child.Address, where)
	cli.Fprint(os.Stderr, cyan, msg, color)
	rlog.Line(msg)
	null, err := os.Open(os.DevNull)
	if err != nil {
		return 1
	}
	defer null.Close()
	code, err := remote.SSHWith(p, extra, remote.PushScript(p, job.parentName, env), null, nil, false, rlog)
	if err != nil {
		cli.Fprint(os.Stderr, red, err.Error(), color)
		rlog.Line(err.Error())
		return 1
	}
	return code
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
		where := t.Host + ":" + t.Path
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fail("--clone replaces the data of " + where + " with its parent's: add --yes when not at a terminal")
		}
		fmt.Fprintf(os.Stderr, "This replaces the data of @%s (%s, shared/mnt) with @%s's (%s:%s), then runs 'devopsy %s' there. Type %s to go on: ",
			t.Address, where, p.Address, p.Host, p.Path, strings.Join(run, " "), t.Name)
		line, _ := stdinReader.ReadString('\n')
		if strings.TrimSpace(line) != t.Name {
			return fail("Nothing cloned.")
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

// Server side: the hidden flags a clone runs.

// errOut is stderr for what clones run on servers: never an *os.File, so
// commands write to a pipe devopsy drains and never die of a broken pipe
// when the session that called devopsy is gone (they would leave services
// paused or stopped).
var errOut io.Writer = quietWriter{os.Stderr}

type quietWriter struct{ w io.Writer }

func (q quietWriter) Write(p []byte) (int, error) {
	_, _ = q.w.Write(p)
	return len(p), nil
}

// sayErr prints a server-side notice.
func sayErr(msg string) { fmt.Fprintln(errOut, msg) }

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
	cmd.Stdout, cmd.Stderr = errOut, errOut
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
		return fail(fmt.Sprintf("no SSH agent was forwarded to %s: a restricted key refuses it; authorize it with `restrict,agent-forwarding`, or use a key without restrictions", host))
	}
	projectDir, err := cli.FindProjectDir(cwd)
	if err != nil {
		return fail(err.Error())
	}
	ctx, stop := interruptible()
	defer stop()

	if push := filepath.Join(projectDir, "capabilities", "clone", "push"); isExecutable(push) {
		sayErr("devopsy: running 'devopsy --capability clone push'")
		self, err := os.Executable()
		if err != nil {
			self = "devopsy"
		}
		cmd := exec.Command(self, "--capability", "clone", "push")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, errOut, errOut
		return exitCode(cmd.Run())
	}

	_, services, err := cli.CloneServices(cwd, os.Environ())
	if err != nil {
		return fail(err.Error())
	}
	dirs := cli.CloneDirs(services)
	if len(dirs) == 0 {
		sayErr("devopsy: nothing to clone: no bind mounts under .devopsy/mnt")
		return 0
	}
	if err := ensureRsyncImage(ctx); err != nil {
		return fail(err.Error())
	}
	if err := prepareServices(cwd, services); err != nil {
		return fail(err.Error())
	}
	code := 0
	if err := cloneRsync(ctx, projectDir, dirs, strings.Fields(os.Getenv(remote.CloneArgsEnv))); err != nil {
		code = fail(err.Error())
	}
	if err := finishServices(cwd, services); err != nil {
		code = fail(err.Error())
	}
	return code
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

// rsyncdConf serves mnt read-only, as root, so every owner's files are
// read; the daemon lives for one clone and publishes no port.
const rsyncdConf = "uid = 0\ngid = 0\nuse chroot = yes\nread only = yes\n[mnt]\npath = /mnt\n"

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
	var present []string
	for _, d := range dirs {
		if _, err := os.Lstat(filepath.Join(parentMnt, d)); err != nil {
			sayErr("devopsy: mnt/" + d + " does not exist here: skipped")
			continue
		}
		present = append(present, d)
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
		for _, d := range present {
			sayErr("devopsy: rsync mnt/" + d)
			run := append(append([]string{"run", "--rm", "-v", parentMnt + ":/src:ro", "-v", childMnt + ":/dst", remote.RsyncImage}, rsync...), args...)
			if err := docker(ctx, append(run, "/src/"+d+"/", "/dst/"+d+"/")...); err != nil {
				return fmt.Errorf("rsync mnt/%s: %w", d, err)
			}
		}
		return nil
	}

	id, err := dockerOut(ctx, "run", "--rm", "-d", "-v", parentMnt+":/mnt:ro", "-e", "RSYNCD_CONF="+rsyncdConf, remote.RsyncImage,
		"sh", "-c", `printf "%s\n" "$RSYNCD_CONF" > /etc/rsyncd.conf && exec rsync --daemon --no-detach --config=/etc/rsyncd.conf`)
	if err != nil || id == "" {
		return fmt.Errorf("starting the rsync daemon: %v", err)
	}
	// Removed whatever happens, an interrupt included (ctx is cancelled
	// then: not this).
	defer func() { _ = docker(context.Background(), "rm", "-f", id) }()
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
		sshArgs := []string{"-o", "StrictHostKeyChecking=accept-new", "-o", "ExitOnForwardFailure=yes", "-T", "-R", "127.0.0.1:" + port + ":" + ip + ":873"}
		if p := os.Getenv(remote.CloneSSHPortEnv); p != "" {
			sshArgs = append(sshArgs, "-p", p)
		}
		sshArgs = append(sshArgs, to, "sh -c "+remote.Quote(remote.CloneReceiveScript(toDir, port, present, args)))
		trace(append([]string{"ssh"}, sshArgs[:len(sshArgs)-1]...))
		cmd := remote.SSHCommand(sshArgs...)
		var stderr bytes.Buffer
		cmd.Stdin = nil
		cmd.Stdout = errOut
		cmd.Stderr = io.MultiWriter(errOut, &stderr)
		err := cmd.Run()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return errors.New("interrupted")
		}
		if attempt < 2 && strings.Contains(stderr.String(), "remote port forwarding failed") {
			continue
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
		sshArgs := []string{"-o", "ClearAllForwardings=yes", "-o", "StrictHostKeyChecking=accept-new", "-T"}
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
// are healthy, unpause; or list them, name, policy and entries of mnt, one
// per line.
func runCloneServices(cwd string, args []string, color bool) int {
	fail := func(msg string) int {
		cli.Fprint(errOut, red, "devopsy: "+msg, color)
		return 1
	}
	if len(args) != 1 || !slices.Contains([]string{"prepare", "finish", "list"}, args[0]) {
		return fail("usage: devopsy " + cli.CloneServicesFlag + " prepare|finish|list")
	}
	_, services, err := cli.CloneServices(cwd, os.Environ())
	if err != nil {
		return fail(err.Error())
	}
	switch args[0] {
	case "list":
		for _, s := range services {
			fmt.Printf("%s\t%s\t%s\n", s.Name, s.Policy, strings.Join(s.Dirs, " "))
		}
		return 0
	case "prepare":
		err = prepareServices(cwd, services)
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

// prepareServices pauses the running services labeled pause, then stops
// those labeled stop. When stopping fails, it unpauses what it paused.
func prepareServices(cwd string, services []cli.CloneService) error {
	pause, stop := cli.ClonePolicy(services, cli.ClonePause), cli.ClonePolicy(services, cli.CloneStop)
	if len(pause)+len(stop) == 0 {
		return nil
	}
	running, err := composeList(cwd, "--status", "running")
	if err != nil {
		return fmt.Errorf("docker compose ps: %w", err)
	}
	pause, stop = intersect(pause, running), intersect(stop, running)
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
// whatever happened before. It reads the containers' state, not what
// prepare did, so it also recovers after an interrupted clone.
func finishServices(cwd string, services []cli.CloneService) error {
	pause, stop := cli.ClonePolicy(services, cli.ClonePause), cli.ClonePolicy(services, cli.CloneStop)
	if len(pause)+len(stop) == 0 {
		return nil
	}
	var errs []error
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
