package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// endProjectUser ends the project user who, under its sync keys' lock (lockedKeys), in one
// stretch: its sync keys go first, so sshd takes none for a login while the end runs; then
// what it runs or would run with no session (end: its processes and its linger); then its keys
// file is made anew (renewKeys), so a door request begun any time before, whose check of
// that file (keysAt) a sync key's add waits on this lock for, finds it changed and adds
// none. A key's removal (remote.EndProjectUserFlag) and --project-users remove both end a
// user this way.
func endProjectUser(who string, end func(string) error) error {
	file := remote.DoorSyncKeysFile(who)
	var ended error
	err := lockedKeys(file, func([]string) ([]string, error) {
		if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		ended = end(who)
		return nil, renewKeys(remote.DoorKeysFile(who))
	})
	if err != nil {
		return err
	}
	if ended != nil {
		return fmt.Errorf("its processes could not be ended: %w", ended)
	}
	return nil
}

// renewKeys writes file again, as it is, under a new inode, which a door request's keysAt is
// then never the same file as; none when it is gone. Its caller holds what edits it (a key's
// removal holds its lock), or it is gone (--project-users remove).
func renewKeys(file string) error {
	data, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	tmp := file + ".renew"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// runEndProjectUser is the hidden remote.EndProjectUserFlag, as the deploy user once a key of
// the project user's is gone (remote.KeysRemoveScript). A Unix user gone already runs nothing,
// and its sync keys still go.
func runEndProjectUser(args []string) int {
	if len(args) != 1 || !projectUserName(args[0]) {
		return fail("usage: devopsy " + remote.EndProjectUserFlag + " <project user>")
	}
	// A Unix user of that name that is no project user is someone's own account: nothing of
	// its ends here.
	if _, err := user.Lookup(args[0]); err == nil && !inDoorGroup(args[0]) {
		return fail(args[0] + " is no project user here: it is not in " + remote.DoorGroup)
	}
	if err := endProjectUser(args[0], endUser); err != nil {
		return fail("the key is removed, but " + args[0] + ": " + err.Error())
	}
	if err := finishPendingRemoval(args[0]); err != nil {
		return failErr(err)
	}
	report.Info("the processes of " + args[0] + " are ended")
	return 0
}

// finishPendingRemoval does what a removal of who's left (remote.DoorRemovingFile) once its
// end is done, as the end flag runs for keysSettle, whose script emptied the keys under their
// lock: what the door kept for the project goes, then both marks. Nothing with no such mark.
func finishPendingRemoval(who string) error {
	data, err := os.ReadFile(remote.DoorRemovingFile(who))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	project := strings.TrimSpace(string(data))
	d, err := remote.ServerDoor()
	if err != nil {
		return err
	}
	if d != nil && d.Space != "" && remote.ValidName(project) {
		if err := removeKept(d.Space, project); err != nil {
			return err
		}
	}
	for _, f := range []string{remote.DoorEndingFile(who), remote.DoorRemovingFile(who)} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// endUser ends every process of the user who and its linger (remote.EndUserArgs), as root
// through devopsy's util image, then its project's builder (endBuilder). A user deleted
// already runs none: userdel refuses one with processes.
func endUser(who string) error {
	u, err := user.Lookup(who)
	if err != nil && !errors.As(err, new(user.UnknownUserError)) {
		return err
	}
	if err == nil {
		if err := ensureUtilImage(context.Background()); err != nil {
			return err
		}
		if out, err := exec.Command("docker", remote.EndUserArgs(u.Uid, who, devopsyPath())...).CombinedOutput(); err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	return endBuilder(who)
}

// endBuilder removes the project user who's builder and its network, which the door makes anew
// at the next build (doorSide.builder, both named from its project, door.BuilderOf): what a removed key built is
// in its caches (cache mounts, layers), which a later build of the owner's would trust.
func endBuilder(who string) error {
	d, err := remote.ServerDoor()
	if err != nil || d == nil || d.Space == "" {
		return err
	}
	env := append(os.Environ(), "BUILDX_CONFIG="+filepath.Join(d.Space, "buildx"))
	docker := func(args ...string) *exec.Cmd {
		c := exec.Command("docker", args...)
		c.Env = env
		return c
	}
	builder, network := door.BuilderOf(door.ProjectOfUser(who)), door.BuilderNetworkOf(door.ProjectOfUser(who))
	if docker("buildx", "inspect", builder).Run() == nil {
		if out, err := docker("buildx", "rm", builder).CombinedOutput(); err != nil {
			return fmt.Errorf("its builder %s: %w: %s", builder, err, strings.TrimSpace(string(out)))
		}
	}
	if docker("network", "inspect", network).Run() == nil {
		if out, err := docker("network", "rm", network).CombinedOutput(); err != nil {
			return fmt.Errorf("its builder's network: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// noProcessUID is the real and saved uid endUIDKill takes (door.EndUID), none of the user's,
// so that none of the user's processes may signal it, while its effective uid, the user's,
// signals all of theirs. No container of the door's runs as it; one a build's step runs as it
// (a USER the door never reads) is the project's own and goes with the user's processes.
const noProcessUID = door.EndUID

// runEndUID is the hidden remote.EndUIDFlag, as root in remote.EndUserArgs' container under the
// host's pids: it clears the user's linger (/linger/<user>), then, round after round, has a
// child of its own kill every process the user's uid may signal (endUIDKill: kill(-1) is
// atomic against a fork, which a scan and a kill by pid race) and kills by pid, as root,
// whatever the scan still finds with the uid among its real, effective or saved ones (a
// zombie aside, no signal reaching one), until a scan finds none; it fails after 50.
func runEndUID(args []string) int {
	if len(args) == 2 && args[0] == "--kill" {
		return endUIDKill(args[1])
	}
	if len(args) != 2 {
		return fail("usage: devopsy " + remote.EndUIDFlag + " <uid> <user>")
	}
	uid, err := strconv.Atoi(args[0])
	if err != nil || uid <= 0 || uid == noProcessUID || !projectUserName(args[1]) {
		return fail("usage: devopsy " + remote.EndUIDFlag + " <uid> <user>")
	}
	if err := os.Remove(filepath.Join("/linger", args[1])); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return failErr(err)
	}
	for range 50 {
		kill := exec.Command("/proc/self/exe", remote.EndUIDFlag, "--kill", args[0])
		kill.Stderr = os.Stderr
		_ = kill.Run()
		left := processesOf(uid)
		if len(left) == 0 {
			return 0
		}
		for _, pid := range left {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fail("processes of uid " + args[0] + " are still running")
}

// processesOf are the pids under /proc, this one's aside, whose real, effective or saved uid is
// uid, a zombie aside.
func processesOf(uid int) []int {
	want := strconv.Itoa(uid)
	var pids []int
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "status"))
		if err != nil {
			continue
		}
		hit, zombie := false, false
		for _, line := range strings.Split(string(data), "\n") {
			f := strings.Fields(line)
			switch {
			case len(f) >= 2 && f[0] == "State:":
				zombie = f[1] == "Z"
			case len(f) >= 4 && f[0] == "Uid:":
				hit = f[1] == want || f[2] == want || f[3] == want
			}
		}
		if hit && !zombie {
			pids = append(pids, pid)
		}
	}
	return pids
}

// projectUserName says whether name is a project user's, devopsy-<project>.
func projectUserName(name string) bool {
	project := door.ProjectOfUser(name)
	return project != "" && remote.ValidName(project)
}
