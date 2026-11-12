package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
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

// projectClaims are the claims of project in s, none when it has no directory yet.
func projectClaims(s uids.Store, project string) ([]uids.Claim, error) {
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	var mine []uids.Claim
	for _, c := range all {
		if c.Bad == "" && c.Owner.Project == project {
			mine = append(mine, c)
		}
	}
	return mine, nil
}

// claimedUIDs are every uid project's claims hold: what an owner of its data may be.
func claimedUIDs(claims []uids.Claim) []uint32 {
	var out []uint32
	for _, c := range claims {
		out = append(out, c.UID)
	}
	return out
}

// uidsOf are what a container of the environment named name may run as: the uid of each of
// the project's environments whose compose name it is (two owners can share one, both the
// same tenant's), and each one's project uid. None when no load of its release claimed one.
func uidsOf(claims []uids.Claim, name string) []uint32 {
	var out []uint32
	for _, c := range claims {
		if c.Owner.Env == "" || c.Owner.ComposeName() != name {
			continue
		}
		out = append(out, c.UID)
		for _, p := range claims {
			if p.Owner == c.Owner.ProjectOf() {
				out = append(out, p.UID)
			}
		}
	}
	return out
}

// freeUIDs frees env's claim, last in its --destroy: under env's start lock, exclusive, it
// takes down once more whatever a start made after the destroy's own down (down, by the
// environment's compose name), then removes the claim; then its project's, when no other
// environment of it holds a claim. Its waits are announced and bounded.
func freeUIDs(s uids.Store, env uids.Owner, down func(name string) error) error {
	lock, err := s.LockStart(env, syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := down(env.ComposeName()); err != nil {
		return fmt.Errorf("taking %s down before its uid goes: %w", env.ComposeName(), err)
	}
	if err := s.Free(env, lock); err != nil {
		return err
	}
	project := env.ProjectOf()
	plock, err := s.LockStart(project, syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer plock.Close()
	_, err = s.FreeIfUnused(project, plock)
	return err
}

// downArgs take a compose project down by its name alone, whatever is left of it.
func downArgs(name string) []string {
	return []string{"compose", "-p", name, "down", "--volumes", "--remove-orphans"}
}

// runFreeUIDs is the hidden --free-uids <owner>: a --destroy's last step, which frees the
// environment's uid claim, the project user's through the door.
func runFreeUIDs(args []string) int {
	if len(args) != 1 {
		report.Error("usage: devopsy " + remote.FreeUIDsFlag + " <owner>")
		return 2
	}
	env, err := uids.ParseOwner(args[0])
	if err != nil || env.Env == "" {
		report.Error(fmt.Sprintf("%s: %q is no environment's owner", remote.FreeUIDsFlag, args[0]))
		return 2
	}
	if behindDoor() {
		if _, err := doorAsk(true, "free", env.String()); err != nil {
			return 1
		}
		return 0
	}
	r, err := remote.ReleaseSettings()
	if err != nil {
		return failErr(err)
	}
	down := func(name string) error {
		c := exec.Command(realDocker(), downArgs(name)...)
		c.Stdout, c.Stderr = errOut, errOut
		return c.Run()
	}
	if err := freeUIDs(claimsStore(r.Root), env, down); err != nil {
		return failErr(err)
	}
	return 0
}

// free answers "free <owner>": a project user's --destroy's last step, for an environment of
// its own project whose data is gone, through the same step as the deploy user's.
func (s *doorSide) free(owner string) int {
	env, err := uids.ParseOwner(owner)
	if err != nil || env.Env == "" || env.Project != s.policy.Project {
		return doorRefused(fmt.Errorf("refused: not an environment of this project's: %q", owner))
	}
	name := env.ComposeName()
	if _, err := os.Stat(s.policy.Mnt(name)); err == nil {
		return doorRefused(fmt.Errorf("refused: %s still has its data (%s): its --destroy removes it first", name, s.policy.Mnt(name)))
	}
	r, err := remote.ReleaseSettings()
	if err != nil {
		return doorRefused(err)
	}
	down := func(name string) error {
		env, err := s.composeEnv("", false)
		if err != nil {
			return err
		}
		c := s.docker(downArgs(name)...)
		c.Env, c.Stdout, c.Stderr = env, os.Stderr, os.Stderr
		return c.Run()
	}
	if err := freeUIDs(claimsStore(r.Root), env, down); err != nil {
		return doorRefused(fmt.Errorf("refused: freeing the uid of %s: %w", name, err))
	}
	return 0
}

// holdStartLocks takes, shared, the start locks of the environment plan starts containers of
// and of its project, then reads their claims again under them: a --destroy frees a claim only
// with its lock exclusive, so what the load set either holds until compose ends or the start
// fails here. The locks are close-on-exec until keepAcrossExec.
func holdStartLocks(plan *cli.Plan) ([]*uids.Lock, error) {
	s := claimsStore(plan.UIDRoot)
	var locks []*uids.Lock
	for _, o := range []uids.Owner{plan.UIDOwner, plan.UIDOwner.ProjectOf()} {
		l, err := s.LockStart(o, syscall.LOCK_SH)
		if err != nil {
			closeLocks(locks)
			return nil, err
		}
		locks = append(locks, l)
	}
	for k, o := range map[string]uids.Owner{cli.UIDEnv: plan.UIDOwner, cli.ProjectUIDEnv: plan.UIDOwner.ProjectOf()} {
		uid, ok, err := s.Read(o)
		if err != nil || !ok || envValue(plan.Env, k) != strconv.FormatUint(uint64(uid), 10) {
			closeLocks(locks)
			return nil, fmt.Errorf("the uid claim of %s changed as it started (a --destroy meanwhile?): run it again", o)
		}
	}
	return locks, nil
}

func closeLocks(locks []*uids.Lock) {
	for _, l := range locks {
		l.Close()
	}
}

// keepAcrossExec hands the locks to what the exec runs, right before it.
func keepAcrossExec(locks []*uids.Lock) error {
	for _, l := range locks {
		if err := l.KeepAcrossExec(); err != nil {
			return err
		}
	}
	return nil
}

// startLocks takes, shared, the start locks of the owners whose compose name is name and of
// their projects, then reads the project's claims again under them, for the check and the
// exec that follow: a --destroy frees a claim only with its lock exclusive. A side with no
// claims directory (a test's) takes none.
func (s *doorSide) startLocks(name string) ([]*uids.Lock, error) {
	if s.uidRoot == "" {
		return nil, nil
	}
	store := claimsStore(s.uidRoot)
	var owners []uids.Owner
	for _, c := range s.claims {
		if c.Owner.Env != "" && c.Owner.ComposeName() == name {
			owners = append(owners, c.Owner)
			if !slices.Contains(owners, c.Owner.ProjectOf()) {
				owners = append(owners, c.Owner.ProjectOf())
			}
		}
	}
	var locks []*uids.Lock
	for _, o := range owners {
		l, err := store.LockStart(o, syscall.LOCK_SH)
		if err != nil {
			closeLocks(locks)
			return nil, err
		}
		locks = append(locks, l)
	}
	claims, err := projectClaims(store, s.policy.Project)
	if err != nil {
		closeLocks(locks)
		return nil, err
	}
	s.claims = claims
	return locks, nil
}

// uidFacts are the claims under root, for --server-info: the directory, and each entry.
func uidFacts(root string) (string, []remote.UIDClaim) {
	s := claimsStore(root)
	claims, err := s.List()
	if err != nil {
		return s.Dir, []remote.UIDClaim{{Name: s.Dir, Bad: err.Error()}}
	}
	var out []remote.UIDClaim
	for _, c := range claims {
		if c.Bad != "" {
			out = append(out, remote.UIDClaim{Name: c.Name, Bad: c.Bad})
			continue
		}
		out = append(out, remote.UIDClaim{Owner: c.Owner.String(), UID: c.UID, Name: c.Name})
	}
	return s.Dir, out
}

// staleUID says why claim c is left over, or nothing: an environment's whose environment is
// on the server no more (removed without --destroy), a project's with no environment claim of
// it left.
func staleUID(c remote.UIDClaim, all []remote.UIDClaim, envs []remote.Environment) string {
	o, err := uids.ParseOwner(c.Owner)
	if err != nil {
		return err.Error()
	}
	if o.Env != "" {
		path := filepath.ToSlash(filepath.Join(o.Project, o.Instance, o.Env))
		for _, e := range envs {
			if e.Path == path {
				return ""
			}
		}
		return "no environment " + path + " on the server"
	}
	for _, other := range all {
		if e, err := uids.ParseOwner(other.Owner); err == nil && e.Env != "" && e.ProjectOf() == o {
			return ""
		}
	}
	return "no environment of it holds a claim"
}
