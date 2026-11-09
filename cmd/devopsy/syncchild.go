package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// syncRequest is what the parent's push asks of the child across servers, one per
// session, as JSON in the session's command (SSH_ORIGINAL_COMMAND under the sync key's
// forced command, --sync-child): exactly one of Exec, Receive and Spool.
type syncRequest struct {
	// Exec is devopsy's arguments in the child's release: --sync-services list, wipe
	// (a reset's only) or import <service> [last], or --util-image.
	Exec []string `json:"exec,omitempty"`
	// Receive is the rsync of one entry or volume from the parent's daemon, over the
	// session's own stdin and stdout.
	Receive *syncReceive `json:"receive,omitempty"`
	// Spool is a custom service whose export the session's stdin carries into the
	// child's spool for it.
	Spool string `json:"spool,omitempty"`
	// Beat is the heartbeat's seconds (remote.HeartbeatEnv), the push's own.
	Beat int `json:"beat,omitempty"`
}

// syncReceive is a Receive request: one entry of mnt or one named volume by its key, which
// the child's labels name; whether the entry is a file on the parent, which the child's own
// entry decides where it has one; whether it is the copy's last write.
type syncReceive struct {
	Entry  string `json:"entry,omitempty"`
	Volume string `json:"volume,omitempty"`
	File   bool   `json:"file,omitempty"`
	Last   bool   `json:"last,omitempty"`
}

// syncName is what a request may name of the child's: an entry, a volume's key, a service.
var syncName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// runSyncChild is --sync-child <id> <base> <to> [reset], a sync key's forced command on
// the child's server (remote.SyncKeyLine): while sync id is open in the environment at
// base, it does one of that sync's requests (syncRequest), building the script itself
// from the child's own facts (its release, its labels, its spool's name), never running
// one the parent sent. to is the child's address, for hints; reset says the sync is one
// (its wipe allowed). The gate it checks is in the environment's tree, its user's, so a
// key does nothing that user cannot do already.
func runSyncChild(args []string) int {
	if len(args) < 3 || len(args) > 4 || (len(args) == 4 && args[3] != "reset") {
		return fail("usage: devopsy " + remote.SyncChildFlag + " <id> <base> <to> [reset]")
	}
	id, base, to, reset := args[0], args[1], args[2], len(args) == 4
	if err := remote.CheckSyncChild(id, base, to); err != nil {
		return fail(err.Error())
	}
	from, ok := syncOpen(base, id)
	if !ok {
		return fail("sync " + id + " is not open in " + base + ": its key does nothing now")
	}
	var req syncRequest
	dec := json.NewDecoder(strings.NewReader(os.Getenv("SSH_ORIGINAL_COMMAND")))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return fail("not a sync's request: " + err.Error())
	}
	toDir := path.Join(base, "current", ".devopsy")
	// The child's own release says its compose project, never the
	// session's environment.
	toProject := projectVar(path.Join(base, "current"), "COMPOSE_PROJECT_NAME")
	if toProject == "" {
		return fail(base + " has no current release")
	}
	env := map[string]string{
		remote.SyncIDEnv:         id,
		remote.SyncFromTargetEnv: from,
		remote.SyncToTargetEnv:   to,
		remote.SyncWatchEnv:      "1",
		remote.InheritPendingEnv: remote.SyncInheritPath(base, id),
	}
	if reset {
		env[remote.SyncResetEnv] = "1"
	}
	if req.Beat > 0 && req.Beat <= 300 {
		env[remote.HeartbeatEnv] = strconv.Itoa(req.Beat)
	}
	if req.Receive != nil && len(req.Exec) == 0 && req.Spool == "" {
		return receiveOne(req.Receive, id, base, toDir, toProject, env, reset)
	}
	script, err := syncChildScript(req, id, base, toDir, toProject, env, reset)
	if err != nil {
		return fail(err.Error())
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		return fail(err.Error())
	}
	return fail(syscall.Exec(sh, []string{"sh", "-c", script}, os.Environ()).Error())
}

// syncChildScript is the script for req, built from the child's facts.
func syncChildScript(req syncRequest, id, base, toDir, toProject string, env map[string]string, reset bool) (string, error) {
	n := 0
	for _, set := range []bool{len(req.Exec) > 0, req.Receive != nil, req.Spool != ""} {
		if set {
			n++
		}
	}
	if n != 1 {
		return "", errors.New("not a sync's request: one of exec, receive and spool")
	}
	switch {
	case len(req.Exec) > 0:
		if !syncExecAllowed(req.Exec, reset) {
			return "", fmt.Errorf("not a sync's request: %q", req.Exec)
		}
		if len(req.Exec) >= 3 && req.Exec[1] == "import" {
			if err := childHas(toDir, toProject, env, func(c childService) bool { return c.Name == req.Exec[2] && c.Copy == cli.CopyCustom }); err != nil {
				return "", err
			}
		}
		return remote.SyncExecScript(toDir, toProject, env, req.Exec), nil
	case req.Spool != "":
		if !syncName.MatchString(req.Spool) {
			return "", fmt.Errorf("not a service: %q", req.Spool)
		}
		if err := childHas(toDir, toProject, env, func(c childService) bool { return c.Name == req.Spool && c.Copy == cli.CopyCustom }); err != nil {
			return "", err
		}
		return "umask 077; exec cat > " + remote.Quote(remote.StreamSpool(base, req.Spool, id)) + "\n", nil
	}
	return "", errors.New("not a sync's request: one of exec, receive and spool")
}

// receivePlan is what a receive writes and how, from the child's own list: the entry or
// volume, its mount, and rsync's words after RsyncCopy (owners, in place, from, into). An
// entry's kind is the child's where it has the entry (file says the parent's, for one it
// has not yet), so the parent's word never turns the child's directory into a file.
type receivePlan struct {
	what, entry, mount string
	file               bool
	args               []string
}

func planReceive(r *syncReceive, child []childService, mnt, toProject string) (receivePlan, error) {
	var dirs, vols []string
	for _, c := range child {
		dirs = append(dirs, c.Dirs...)
		vols = append(vols, c.Volumes...)
	}
	switch {
	case (r.Entry == "") == (r.Volume == ""):
		return receivePlan{}, errors.New("not a sync's request: one entry or one volume")
	case r.Volume != "":
		if !syncName.MatchString(r.Volume) || !slices.Contains(vols, r.Volume) || r.File {
			return receivePlan{}, fmt.Errorf("the child's labels name no volume %q", r.Volume)
		}
		return receivePlan{what: "the volume " + r.Volume, mount: toProject + "_" + r.Volume + ":/vol/" + r.Volume,
			args: []string{remote.ReceiveFrom + r.Volume + "/", "/vol/" + r.Volume + "/"}}, nil
	}
	e := r.Entry
	if !syncName.MatchString(e) || !slices.Contains(dirs, e) {
		return receivePlan{}, fmt.Errorf("the child's labels name no entry %q", e)
	}
	file := r.File
	if fi, err := os.Lstat(filepath.Join(mnt, e)); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return receivePlan{}, fmt.Errorf("mnt/%s is a symbolic link: nothing copied", e)
		}
		if file != !fi.IsDir() {
			return receivePlan{}, fmt.Errorf("mnt/%s is a %s here and a %s on the parent: nothing copied", e, kindWord(!fi.IsDir()), kindWord(file))
		}
	}
	p := receivePlan{what: "mnt/" + e, entry: e, file: file, mount: filepath.Join(mnt, e) + ":/mnt/" + e}
	if o := childOwners(child)[e]; o != "" {
		// The child's owner for every file written (its labels, from its list).
		p.args = append(p.args, "--chown="+o)
	}
	if file {
		// A file: its module serves it alone, into the file mounted, in place (a
		// container bind-mounts the file's inode).
		p.args = append(p.args, "--inplace", remote.ReceiveFrom+e+"/"+e, "/mnt/"+e)
	} else {
		p.args = append(p.args, remote.ReceiveFrom+e+"/", "/mnt/"+e+"/")
	}
	return p, nil
}

// receiveRun is docker's words for a receive as the container c, labeled for its holder,
// which removes it by them.
func receiveRun(c container, plan receivePlan) []string {
	return slices.Concat([]string{"run", "--rm", "-i"}, c.runArgs(), []string{"--network", "none", "-v", plan.mount, remote.UtilImage, "sh", "-c", receiveScript},
		remote.RsyncCopy, plan.args)
}

func kindWord(file bool) string {
	if file {
		return "file"
	}
	return "directory"
}

// receiveOne copies one entry or volume of sync id from the parent's daemon over this
// session's stdin and stdout (remote.ReceiveShim), into the child at base: claimed first,
// with a reset's wipe at its first write, the entry made as the child has it or the parent
// says, mounted alone so nothing the daemon serves can replace it, and the copy ended once
// the holder decided. With Last, it records the copy done.
func receiveOne(r *syncReceive, id, base, toDir, toProject string, env map[string]string, reset bool) int {
	// Signals of the session's end never stop the watchdog before the copy does.
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	mnt, err := filepath.EvalSymlinks(path.Join(toDir, "mnt"))
	if err != nil {
		return fail("the child's mnt: " + err.Error())
	}
	child, err := childListAt(toDir, toProject, env)
	if err != nil {
		return fail(err.Error())
	}
	plan, err := planReceive(r, child, mnt, toProject)
	if err != nil {
		return fail(err.Error())
	}
	if err := ensureUtilImage(context.Background()); err != nil {
		return failErr(err)
	}
	wipe := ""
	if reset {
		wipe = remote.SyncExecScript(toDir, toProject, env, []string{cli.SyncServicesFlag, "wipe"})
	}
	// Its stdout to stderr, and no stdin: the session's carry rsync's protocol alone.
	claim := exec.Command("sh", "-c", remote.SyncClaimScript(base, id, r.Volume, toProject, wipe))
	claim.Stdout, claim.Stderr = os.Stderr, os.Stderr
	if err := claim.Run(); err != nil {
		return 1
	}
	started := filepath.Join(base, remote.SyncStartedFile)
	mine := func() bool { _, ok := gateOf(started, id); return ok }
	if !mine() {
		return fail("the child's side decided already: " + plan.what + " not copied")
	}
	if plan.entry != "" {
		if err := readyEntry(mnt, tarEntry{name: plan.entry, file: plan.file}); err != nil {
			return fail(err.Error())
		}
	}
	report.Info("rsync " + plan.what)
	c := container{remote.SyncContainerPrefix + id + "-receive", toProject, remote.SyncTask(id)}
	run := receiveRun(c, plan)
	trace(append([]string{"docker"}, run...))
	cmd := exec.Command("docker", run...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// Once the holder decided (its record gone, or another sync's), nothing may be
	// written: the copy's container goes.
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-time.After(time.Second):
				if !mine() {
					_ = exec.Command("docker", "rm", "-f", c.name).Run()
					return
				}
			}
		}
	}()
	err = cmd.Run()
	close(done)
	if err != nil {
		if exitCode(err) != remote.RsyncVanished {
			return fail(plan.what + " " + remote.HalfCopiedNotice)
		}
		report.Warn("rsync " + plan.what + ": " + remote.RsyncVanishedNotice)
	}
	if r.Last {
		// Appended to the record, never created: once the holder took it, the copy is
		// not this sync's to complete.
		f, err := os.OpenFile(started, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil || !mine() {
			return fail("the child's side decided already: the copy does not count")
		}
		_, err = f.WriteString(remote.DoneLine(id) + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fail("recording the copy done: " + err.Error())
		}
	}
	return 0
}

// syncExecAllowed says whether args are one of the devopsy calls a sync makes in the
// child: --sync-services list, wipe on a reset, import <service> [last], --util-image.
func syncExecAllowed(args []string, reset bool) bool {
	switch {
	case len(args) == 1:
		return args[0] == remote.UtilImageFlag
	case args[0] != cli.SyncServicesFlag:
		return false
	case len(args) == 2:
		return args[1] == "list" || (args[1] == "wipe" && reset)
	case len(args) == 3 || (len(args) == 4 && args[3] == "last"):
		return args[1] == "import" && syncName.MatchString(args[2])
	}
	return false
}

// childListAt is the child's --sync-services list, run here as a sync's request would.
func childListAt(toDir, toProject string, env map[string]string) ([]childService, error) {
	var out bytes.Buffer
	cmd := exec.Command("sh", "-c", remote.SyncExecScript(toDir, toProject, env, []string{cli.SyncServicesFlag, "list"}))
	cmd.Stdout, cmd.Stderr = &out, errOut
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("listing the child's services: %w", err)
	}
	services, _, _ := parseChildServices(out.String())
	return services, nil
}

// childHas refuses unless a service of the child's list is one ok takes.
func childHas(toDir, toProject string, env map[string]string, ok func(childService) bool) error {
	child, err := childListAt(toDir, toProject, env)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(child, ok) {
		return errors.New("the child's labels name no such custom service")
	}
	return nil
}

// syncOpen says whether sync id is open in the environment at base, and from where:
// its gate is there (remote.SyncOpenFile), or its record (remote.SyncStartedFile), which
// the first write renamed the gate to; the record taken (the holder decided) or another
// sync's, it is not.
func syncOpen(base, id string) (from string, ok bool) {
	if from, ok := gateOf(filepath.Join(base, remote.SyncOpenFile(id)), id); ok {
		return from, true
	}
	return gateOf(filepath.Join(base, remote.SyncStartedFile), id)
}
