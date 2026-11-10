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

// endProjectUser ends what the project user who runs or would run with no session (end: its
// processes and its linger), then drops its sync keys under their lock (lockedKeys), whatever
// the end did: a door request already past its sudo, which no end reaches, adds a sync key
// under that lock only while the user's keys file is still the one it found (keysAt), so one
// that got in first is dropped here. A key's removal (remote.EndProjectUserFlag) and
// --project-users remove both end a user this way.
func endProjectUser(who string, end func(string) error) error {
	ended := end(who)
	if err := lockedKeys(remote.DoorSyncKeysFile(who), func([]string) ([]string, error) { return nil, nil }); err != nil {
		return err
	}
	if ended != nil {
		return fmt.Errorf("its processes could not be ended: %w", ended)
	}
	return nil
}

// runEndProjectUser is the hidden remote.EndProjectUserFlag, as the deploy user once a key of
// the project user's is gone (remote.KeysRemoveScript). A Unix user gone already runs nothing,
// and its sync keys still go.
func runEndProjectUser(args []string) int {
	if len(args) != 1 || !projectUserName(args[0]) {
		return fail("usage: devopsy " + remote.EndProjectUserFlag + " <project user>")
	}
	if err := endProjectUser(args[0], endUser); err != nil {
		return fail("the key is removed, but " + args[0] + ": " + err.Error())
	}
	report.Info("the processes of " + args[0] + " are ended")
	return 0
}

// endUser ends every process of the user who and its linger (remote.EndUserArgs), as root
// through devopsy's util image. A user deleted already runs none: userdel refuses one with
// processes.
func endUser(who string) error {
	u, err := user.Lookup(who)
	if errors.As(err, new(user.UnknownUserError)) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := ensureUtilImage(context.Background()); err != nil {
		return err
	}
	if out, err := exec.Command("docker", remote.EndUserArgs(u.Uid, who, devopsyPath())...).CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
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
	project, ok := strings.CutPrefix(name, "devopsy-")
	return ok && remote.ValidName(project)
}
