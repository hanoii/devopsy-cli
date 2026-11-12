package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/uids"
)

func init() { cli.ClaimUIDs = claimUIDs }

// doorUIDsWait bounds the door's answer to "uids": a claim waits for the claims' lock at most
// as long as any lock wait.
const doorUIDsWait = waitBound + time.Minute

// claimUIDs looks up a release's two uids: a project user asks the door, which holds the
// claims; the deploy user looks them up in root's claims directory itself.
func claimUIDs(root string, env uids.Owner) (uint32, uint32, error) {
	if behindDoor() {
		return askDoorUIDs(filepath.Join(root, env.Project, env.Instance, env.Env))
	}
	return claimsStore(root).LookupPair(env)
}

// claimsStore is the claims directory under root, its waits announced and bounded and the
// host's own ids never taken.
func claimsStore(root string) uids.Store {
	return uids.Store{
		Dir:   filepath.Join(root, cli.UIDsDir),
		Taken: uids.HostTaken("/"),
		Wait: func(what string, try func() (bool, error)) error {
			return boundedWait{what: what}.wait(context.Background(), report.Default(), try)
		},
	}
}

// askDoorUIDs asks the door's "uids" request for the environment at base, through
// doorAnswer, its cap and its deadline.
func askDoorUIDs(base string) (uint32, uint32, error) {
	c, err := doorCommand("uids", base)
	if err != nil {
		return 0, 0, err
	}
	c.Stderr = errOut
	out, err := doorAnswer(c, 64, doorUIDsWait)
	if err != nil {
		return 0, 0, fmt.Errorf("the door gave no uids: %w", err)
	}
	return parseUIDPair(string(out))
}

func parseUIDPair(s string) (uint32, uint32, error) {
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("%q is no pair of uids", strings.TrimSpace(s))
	}
	var got [2]uint32
	for i, f := range fields {
		n, err := strconv.ParseUint(f, 10, 32)
		if err != nil {
			return 0, 0, fmt.Errorf("%q is no uid", f)
		}
		got[i] = uint32(n)
	}
	return got[0], got[1], nil
}

// uids answers "uids <environment directory>": the environment's uid and its project's, the
// owners read from where the environment is in the caller's tree (never from the caller's
// words or files), claimed in the deploy user's claims directory. Nothing else is said.
func (s *doorSide) uids(base string) int {
	envOwner, err := s.ownerAt(base)
	if err != nil {
		return doorRefused(err)
	}
	r, err := remote.ReleaseSettings()
	if err != nil {
		return doorRefused(err)
	}
	e, p, err := claimsStore(r.Root).LookupPair(envOwner)
	if err != nil {
		return doorRefused(fmt.Errorf("refused: the uids of %s: %w", envOwner, err))
	}
	fmt.Println(e, p)
	return 0
}

// ownerAt is the owner of the environment at base in the caller's tree: <tree>/<environment>
// or <tree>/<instance>/<environment>, the project the policy's.
func (s *doorSide) ownerAt(base string) (uids.Owner, error) {
	real, err := s.Real(base)
	if err != nil {
		return uids.Owner{}, err
	}
	if _, err := s.policy.NameOf(real); err != nil {
		return uids.Owner{}, err
	}
	rel, _ := strings.CutPrefix(real, strings.TrimRight(s.policy.Tree, "/")+"/")
	parts := strings.Split(rel, "/")
	instance := ""
	if len(parts) == 2 {
		instance = parts[0]
	}
	o, err := uids.EnvOwner(s.policy.Project, instance, parts[len(parts)-1])
	if err != nil {
		return uids.Owner{}, errors.New("refused: " + err.Error())
	}
	return o, nil
}
