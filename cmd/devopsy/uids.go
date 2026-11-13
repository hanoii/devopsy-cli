package main

import (
	"bytes"
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
	s := uids.StoreAt(root)
	s.Wait = func(what string, try func() (bool, error)) error {
		return boundedWait{what: what}.wait(context.Background(), report.Default(), try)
	}
	return s
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
		n, err := uids.ParseUID(f)
		if err != nil {
			return 0, 0, err
		}
		got[i] = n
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
	if s.uidRoot == "" {
		return doorRefused(errors.New("refused: the server has no uid claims"))
	}
	e, p, err := claimsStore(s.uidRoot).LookupPair(envOwner)
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
func projectClaims(s uids.Store, project string) ([]uids.Claim, error) { return s.ListOf(project) }

// claimedUIDs are every uid project's claims hold: what an owner of its data may be.
func claimedUIDs(claims []uids.Claim) []uint32 {
	var out []uint32
	for _, c := range claims {
		out = append(out, c.UID)
	}
	return out
}

// dirOwners are the environments of project a request's directory real, in its tree tree,
// is of, those claims holds whose compose name is name, the file's: <tree>/<environment>/...
// or <tree>/<instance>/<environment>/..., read from the path, never what a file's name picks
// alone, and never a name the directory is not: a --destroy takes an environment's
// containers down by its name, so a container on its uid under another would outlive its free.
func dirOwners(project, tree, real, name string, claims []uids.Claim) []uids.Owner {
	rel, ok := strings.CutPrefix(real, strings.TrimRight(tree, "/")+"/")
	if !ok {
		return nil
	}
	parts := strings.Split(rel, "/")
	var candidates []uids.Owner
	if o, err := uids.EnvOwner(project, "", parts[0]); err == nil {
		candidates = append(candidates, o)
	}
	if len(parts) > 1 {
		if o, err := uids.EnvOwner(project, parts[0], parts[1]); err == nil {
			candidates = append(candidates, o)
		}
	}
	var owners []uids.Owner
	for _, o := range candidates {
		if o.ComposeName() == name && slices.ContainsFunc(claims, func(c uids.Claim) bool { return c.Owner == o }) {
			owners = append(owners, o)
		}
	}
	return owners
}

// uidsOfOwners are what a container of those environments may run as: each one's uid and its
// project's, from claims.
func uidsOfOwners(claims []uids.Claim, owners []uids.Owner) []uint32 {
	var out []uint32
	for _, o := range owners {
		for _, c := range claims {
			if c.Owner == o || c.Owner == o.ProjectOf() {
				out = append(out, c.UID)
			}
		}
	}
	return out
}

// freeUIDs frees env's claim, last in its --destroy: under env's start lock, exclusive, it
// takes down once more whatever a start made after the destroy's own down (down, by the
// environment's compose name), then removes the claim. An environment that holds no claim
// (never released, or freed by a cut destroy) takes nothing down: its flat compose name may
// be another project's. Then its project's claim, taking the project's start lock only when no
// environment of it holds one, so a sibling's start never holds up this free. Its waits are
// announced and bounded.
func freeUIDs(s uids.Store, env uids.Owner, gone func() error, down func(name string) error) error {
	lock, err := s.LockStart(env, syscall.LOCK_EX)
	if err != nil {
		return err
	}
	defer lock.Close()
	// Again under the lock: a release of it while the free waited for a start made it anew.
	if err := gone(); err != nil {
		return err
	}
	_, claimed, err := s.Read(env)
	if err != nil {
		return err
	}
	if claimed {
		if err := down(env.ComposeName()); err != nil {
			return fmt.Errorf("taking %s down before its uid goes: %w", env.ComposeName(), err)
		}
	}
	if err := s.Free(env, lock); err != nil {
		return err
	}
	project := env.ProjectOf()
	if left, err := s.EnvClaimsLeft(project); err != nil || left {
		return err
	}
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

// runFreeUIDs is the hidden --free-uids <environment directory>: a --destroy's last step,
// once the environment is gone, which frees its uid claim, the project user's through the door.
func runFreeUIDs(args []string) int {
	if len(args) != 1 {
		report.Error("usage: devopsy " + remote.FreeUIDsFlag + " <environment directory>")
		return 2
	}
	if behindDoor() {
		if _, err := doorAsk(true, "free", args[0]); err != nil {
			return 1
		}
		return 0
	}
	r, err := remote.ReleaseSettings()
	if err != nil {
		return failErr(err)
	}
	env, err := ownerUnder(r.Root, args[0])
	if err != nil {
		return failErr(err)
	}
	gone := func() error {
		if _, err := os.Lstat(args[0]); err == nil {
			return fmt.Errorf("%s is still there: its --destroy removes it first", args[0])
		}
		return nil
	}
	if err := gone(); err != nil {
		return failErr(err)
	}
	docker := func(args ...string) ([]byte, error) { return exec.Command(realDocker(), args...).Output() }
	down := downIfLeft(docker, func(name string) error {
		c := exec.Command(realDocker(), downArgs(name)...)
		// From /, which holds no compose file of anyone's: compose looks for one in its
		// directory and every parent.
		c.Dir, c.Stdout, c.Stderr = "/", errOut, errOut
		return c.Run()
	})
	if err := freeUIDs(claimsStore(r.Root), env, gone, down); err != nil {
		return failErr(err)
	}
	return 0
}

// downIfLeft is a free's down that runs only while a container, volume or network still
// carries compose's project label of the name: the destroy's own down took the rest, and
// compose's down by name with nothing left says "No resource found to remove" in its own
// words. A listing that fails reads as something left, so the down runs.
func downIfLeft(docker func(args ...string) ([]byte, error), down func(name string) error) func(name string) error {
	return func(name string) error {
		f := "label=com.docker.compose.project=" + name
		for _, args := range [][]string{{"ps", "-aq", "--filter", f}, {"volume", "ls", "-q", "--filter", f}, {"network", "ls", "-q", "--filter", f}} {
			if out, err := docker(args...); err != nil || len(bytes.TrimSpace(out)) > 0 {
				return down(name)
			}
		}
		return nil
	}
}

// ownerUnder is the owner of the environment directory base under the release root root:
// <root>/<project>/[<instance>/]<environment>.
func ownerUnder(root, base string) (uids.Owner, error) {
	rel, err := filepath.Rel(root, filepath.Clean(base))
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if err != nil || !filepath.IsAbs(base) || len(parts) < 2 || len(parts) > 3 {
		return uids.Owner{}, fmt.Errorf("%s is no environment directory under the release root %s", base, root)
	}
	instance := ""
	if len(parts) == 3 {
		instance = parts[1]
	}
	return uids.EnvOwner(parts[0], instance, parts[len(parts)-1])
}

// free answers "free <environment directory>": a project user's --destroy's last step, for an
// environment of its tree that is gone, its owner read from where it was (ownerAt), never from
// the caller's words, and its compose name taken down through the door's own checked down.
func (s *doorSide) free(base string) int {
	env, err := s.ownerAt(base)
	if err != nil {
		return doorRefused(err)
	}
	real, _ := s.Real(base)
	gone := func() error {
		if s.IsDir(real) {
			return fmt.Errorf("refused: %s is still there: its --destroy removes it first", base)
		}
		return nil
	}
	if err := gone(); err != nil {
		return doorRefused(err)
	}
	name := env.ComposeName()
	if left, err := dataLeft(s.policy.Mnt(name)); err != nil || left {
		return doorRefused(fmt.Errorf("refused: %s still has its data (%s): its --destroy removes it first", name, s.policy.Mnt(name)))
	}
	if s.uidRoot == "" {
		return doorRefused(errors.New("refused: the server has no uid claims"))
	}
	docker := func(args ...string) ([]byte, error) { return s.docker(args...).Output() }
	down := downIfLeft(docker, func(name string) error {
		argv, env, err := s.checkedDown(name, []string{"--volumes", "--remove-orphans"})
		if err != nil {
			return err
		}
		c := exec.Command(s.real, argv[1:]...)
		c.Env, c.Dir, c.Stdout, c.Stderr = env, "/", os.Stderr, os.Stderr
		return c.Run()
	})
	if err := freeUIDs(claimsStore(s.uidRoot), env, gone, down); err != nil {
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
	locks, err := s.LockShared(plan.UIDOwner)
	if err != nil {
		return nil, err
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

func closeLocks(locks []*uids.Lock) { uids.CloseAll(locks) }

// keepAcrossExec hands the locks to what the exec runs, right before it.
func keepAcrossExec(locks []*uids.Lock) error {
	for _, l := range locks {
		if err := l.KeepAcrossExec(); err != nil {
			return err
		}
	}
	return nil
}

// startLocks takes, shared, the start locks of the environments a request's directory real
// and its file's name are of (dirOwners) and of their projects, then reads the project's claims again under them,
// for the check and the exec that follow, and the owners then: a --destroy frees a claim only
// with its lock exclusive. When the owners changed in between (one claimed or freed
// meanwhile), it takes them again. A side with no claims directory (a test's) takes none.
func (s *doorSide) startLocks(real, name string) ([]uids.Owner, []*uids.Lock, error) {
	owners := dirOwners(s.policy.Project, s.policy.Tree, real, name, s.claims)
	if s.uidRoot == "" {
		return owners, nil, nil
	}
	store := claimsStore(s.uidRoot)
	for range 3 {
		locks, err := store.LockShared(owners...)
		if err != nil {
			return nil, nil, err
		}
		claims, err := projectClaims(store, s.policy.Project)
		if err != nil {
			closeLocks(locks)
			return nil, nil, err
		}
		s.claims = claims
		now := dirOwners(s.policy.Project, s.policy.Tree, real, name, claims)
		if slices.Equal(now, owners) {
			return owners, locks, nil
		}
		closeLocks(locks)
		owners = now
	}
	return nil, nil, errors.New("its claims kept changing: run it again")
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
// it left. A door project's tree is read as its user, which may answer nothing in time: with
// none of its environments listed, its claims are taken for live.
func staleUID(c remote.UIDClaim, all []remote.UIDClaim, envs []remote.Environment, door []string) string {
	o, err := uids.ParseOwner(c.Owner)
	if err != nil {
		return err.Error()
	}
	if o.Env != "" {
		path := filepath.ToSlash(filepath.Join(o.Project, o.Instance, o.Env))
		listed := false
		for _, e := range envs {
			if e.Path == path {
				return ""
			}
			listed = listed || strings.HasPrefix(e.Path, o.Project+"/")
		}
		if slices.Contains(door, o.Project) && !listed {
			return ""
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

// dataLeft says whether dir, an environment's mnt in the door's space, still holds an entry:
// a --destroy empties it and leaves the directory.
func dataLeft(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return len(entries) > 0, err
}
