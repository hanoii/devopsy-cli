package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// syncRequest is what the parent's push asks of the child across servers, one per
// session, as JSON in the session's command (SSH_ORIGINAL_COMMAND under the sync key's
// forced command, --sync-child): exactly one of Exec, Receive and Spool.
type syncRequest struct {
	// Exec is devopsy's arguments in the child's release: --sync-services list, wipe
	// (a reset's only) or import <service> [last], or --util-image.
	Exec []string `json:"exec,omitempty"`
	// Receive is the rsync of entries and volumes from the parent's daemon, behind the
	// session's reverse forward of Port.
	Receive *syncReceive `json:"receive,omitempty"`
	// Spool is a custom service whose export the session's stdin carries into the
	// child's spool for it.
	Spool string `json:"spool,omitempty"`
	// Beat is the heartbeat's seconds (remote.HeartbeatEnv), the push's own.
	Beat int `json:"beat,omitempty"`
}

// syncReceive is a Receive request: the entries of mnt, those that are files, and the
// named volumes by key, each one the child's labels name; whether it is the copy's last
// write; whether stdin's first line is the daemon's password.
type syncReceive struct {
	Port    int      `json:"port"`
	Dirs    []string `json:"dirs,omitempty"`
	Files   []string `json:"files,omitempty"`
	Volumes []string `json:"volumes,omitempty"`
	Last    bool     `json:"last,omitempty"`
	Secret  bool     `json:"secret,omitempty"`
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
	r := req.Receive
	if r.Port < 1 || r.Port > 65535 {
		return "", fmt.Errorf("not a port: %d", r.Port)
	}
	child, err := childListAt(toDir, toProject, env)
	if err != nil {
		return "", err
	}
	var dirs, vols []string
	for _, c := range child {
		dirs = append(dirs, c.Dirs...)
		vols = append(vols, c.Volumes...)
	}
	for _, d := range r.Dirs {
		if !syncName.MatchString(d) || !slices.Contains(dirs, d) {
			return "", fmt.Errorf("the child's labels name no entry %q", d)
		}
	}
	for _, f := range r.Files {
		if !slices.Contains(r.Dirs, f) {
			return "", fmt.Errorf("not an entry of this copy: %q", f)
		}
	}
	for _, v := range r.Volumes {
		if !syncName.MatchString(v) || !slices.Contains(vols, v) {
			return "", fmt.Errorf("the child's labels name no volume %q", v)
		}
	}
	wipe := ""
	if reset {
		wipe = remote.SyncExecScript(toDir, toProject, env, []string{cli.SyncServicesFlag, "wipe"})
	}
	return remote.SyncReceiveScript(toDir, base, id, strconv.Itoa(r.Port), r.Dirs, r.Files, r.Volumes, toProject, childOwners(child), r.Last, wipe, r.Secret), nil
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
