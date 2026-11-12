// Package uids allocates the uid an environment's containers run as, and its project's, on a
// server: a hash of the owner's name picks a slot in its domain's range, and a claim, one
// symlink per owner in the claims directory, holds it, the next free slot taken when the
// hashed one is another owner's. The gid is always the same number.
//
// Containers of different owners never share a uid: a process escaped from one reaches
// another's environment, root and signals through /proc only with the same uid (and gid).
//
// Rules: internals/uids.md.
package uids

import (
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// The variables devopsy passes an environment's compose files its uids in
// (`user: "${DEVOPSY_UID:?}:${DEVOPSY_UID:?}"`): its own, and its project's (or instance's),
// shared by the project's environments.
const (
	EnvVar     = "DEVOPSY_UID"
	ProjectVar = "DEVOPSY_PROJECT_UID"
)

// Reserved says why k is devopsy's to set, never a project's or a server's variable: a uid
// is the claims', and one set by hand could be another tenant's.
func Reserved(k string) error {
	if k == EnvVar || k == ProjectVar {
		return errors.New(k + " is devopsy's: the server gives each environment its uid")
	}
	return nil
}

// Domain is the kind of an owner, each with a range of its own, so the number says its kind
// and a tenant's own names (its environments, its instances) never reach a bare project's.
type Domain int

const (
	// ProjectDomain is a project's own uid, with no instance.
	ProjectDomain Domain = iota
	// InstanceDomain is the project uid of an instance of a project.
	InstanceDomain
	// EnvDomain is an environment's uid, with or without an instance.
	EnvDomain
)

// ranges are the domains' [start, start+size), inside the block systemd's table of uids
// (systemd.io/UIDS-GIDS) leaves unused, 1879048192 to 2147352575: above systemd-nspawn's
// container blocks, where useradd's subordinate ranges fall too, below the foreign OS image
// range and 2^31.
var ranges = [...]struct{ start, size uint32 }{
	ProjectDomain:  {1_900_000_000, 50_000_000},
	InstanceDomain: {1_950_000_000, 50_000_000},
	EnvDomain:      {2_000_000_000, 100_000_000},
}

// Range is d's first uid and how many it holds.
func Range(d Domain) (start, size uint32) { return ranges[d].start, ranges[d].size }

// DomainOf says which domain's range uid is in, if any.
func DomainOf(uid uint32) (Domain, bool) {
	for d, r := range ranges {
		if uid >= r.start && uid-r.start < r.size {
			return Domain(d), true
		}
	}
	return 0, false
}

var (
	// NameRe is a project's or an instance's name: a directory, and part of compose and
	// host names. The config takes names by it.
	NameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	// EnvRe is an environment's name, lowercase: the compose name lowercases it, so two
	// names differing in case would be one set of containers. The config takes names by it.
	EnvRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)
)

// nameMax is the longest file name a claim can have.
const nameMax = 255

// Owner is what a uid is for: an environment (Env set) or a project's own uid (Env empty),
// either with or without an instance.
type Owner struct {
	Project, Instance, Env string
}

// EnvOwner is an environment's owner, its parts checked.
func EnvOwner(project, instance, env string) (Owner, error) {
	o := Owner{project, instance, env}
	return o, o.check()
}

// ProjectOwner is a project's (or an instance's) owner, its parts checked.
func ProjectOwner(project, instance string) (Owner, error) {
	o := Owner{Project: project, Instance: instance}
	return o, o.check()
}

// ProjectOf is the project owner of o's environment: the same project and instance.
func (o Owner) ProjectOf() Owner { return Owner{Project: o.Project, Instance: o.Instance} }

func (o Owner) check() error {
	switch {
	case !NameRe.MatchString(o.Project):
		return fmt.Errorf("%q is no project name", o.Project)
	case o.Instance != "" && !NameRe.MatchString(o.Instance):
		return fmt.Errorf("%q is no instance name", o.Instance)
	case o.Env != "" && !EnvRe.MatchString(o.Env):
		return fmt.Errorf("%q is no environment name (lowercase letters, digits, '_', '.' and '-')", o.Env)
	case len(o.String()) > nameMax:
		return fmt.Errorf("%s: too long for a claim's name, %d bytes at most", o, nameMax)
	}
	return nil
}

// ComposeName is the compose name o's environment has: <project>[-<instance>]-<environment>,
// one name that two owners can share (an instance admin's prod and a project shop-admin's).
func (o Owner) ComposeName() string {
	parts := []string{o.Project}
	if o.Instance != "" {
		parts = append(parts, o.Instance)
	}
	return strings.Join(append(parts, o.Env), "-")
}

// Domain is o's domain.
func (o Owner) Domain() Domain {
	switch {
	case o.Env != "":
		return EnvDomain
	case o.Instance != "":
		return InstanceDomain
	}
	return ProjectDomain
}

// String is o by its parts joined with ':', which no name holds, so every owner is one even
// where compose names are flat (an instance admin's prod and a project shop-admin's prod are
// both shop-admin-prod): env:<project>[:<instance>]:<environment>, project:<project>[:<instance>].
// It is the hash's input and the claim's name.
func (o Owner) String() string {
	parts := []string{"project", o.Project}
	if o.Env != "" {
		parts[0] = "env"
	}
	if o.Instance != "" {
		parts = append(parts, o.Instance)
	}
	if o.Env != "" {
		parts = append(parts, o.Env)
	}
	return strings.Join(parts, ":")
}

// ParseOwner reads an owner back from its String, checked.
func ParseOwner(s string) (Owner, error) {
	parts := strings.Split(s, ":")
	var o Owner
	switch {
	case parts[0] == "env" && len(parts) == 3:
		o = Owner{Project: parts[1], Env: parts[2]}
	case parts[0] == "env" && len(parts) == 4:
		o = Owner{parts[1], parts[2], parts[3]}
	case parts[0] == "project" && len(parts) == 2:
		o = Owner{Project: parts[1]}
	case parts[0] == "project" && len(parts) == 3:
		o = Owner{Project: parts[1], Instance: parts[2]}
	default:
		return Owner{}, fmt.Errorf("%q is no owner", s)
	}
	return o, o.check()
}

// Plain is o's hashed slot, its uid wherever no other owner holds it: the start of its
// domain's range plus FNV-1a-64 of its name, modulo the range's size. Locally, with no
// claims, it is the uid.
func (o Owner) Plain() uint32 { return HashIn(o.Domain(), o.String()) }

// HashIn is name's slot in d's range: its start plus FNV-1a-64 of name, modulo its size.
func HashIn(d Domain, name string) uint32 {
	h := fnv.New64a()
	h.Write([]byte(name))
	r := ranges[d]
	return r.start + uint32(h.Sum64()%uint64(r.size))
}

// HostTaken is a Taken for the host whose files are under root ("/" on a server): the uids
// and gids its users and groups have, and its subordinate ranges, read once. A file missing
// holds none.
func HostTaken(root string) func(uint32) bool {
	ids := map[uint32]bool{}
	for _, f := range []string{"etc/passwd", "etc/group"} {
		data, _ := os.ReadFile(filepath.Join(root, f))
		for _, line := range strings.Split(string(data), "\n") {
			if fields := strings.Split(line, ":"); len(fields) > 2 {
				if id, err := parseUID(fields[2]); err == nil {
					ids[id] = true
				}
			}
		}
	}
	type span struct{ start, count uint64 }
	var spans []span
	for _, f := range []string{"etc/subuid", "etc/subgid"} {
		data, _ := os.ReadFile(filepath.Join(root, f))
		for _, line := range strings.Split(string(data), "\n") {
			if fields := strings.Split(line, ":"); len(fields) == 3 {
				start, err1 := strconv.ParseUint(fields[1], 10, 32)
				count, err2 := strconv.ParseUint(fields[2], 10, 32)
				if err1 == nil && err2 == nil {
					spans = append(spans, span{start, count})
				}
			}
		}
	}
	return func(uid uint32) bool {
		if ids[uid] {
			return true
		}
		for _, s := range spans {
			if uint64(uid) >= s.start && uint64(uid)-s.start < s.count {
				return true
			}
		}
		return false
	}
}

// MaxClaims is how many claims one project may hold in a domain: a tenant makes environments
// freely, and a first claim reads its domain's.
const MaxClaims = 10_000

// Store is a server's claims directory.
type Store struct {
	// Dir is the claims directory, the deploy user's, closed to others.
	Dir string
	// Taken says whether a uid is the host's (a user or group, a subordinate range): a slot
	// a claim never takes. Nil takes none.
	Taken func(uid uint32) bool
	// Wait waits, announced and bounded, until try takes what it waits for; what names it.
	Wait func(what string, try func() (bool, error)) error

	// span, when not zero, narrows every range to its first span slots: tests make owners
	// collide with it.
	span uint32
}

// Claim is a claim as listed: its owner and uid, or the name that is no owner's.
type Claim struct {
	Owner Owner
	UID   uint32
	// Bad is why the entry is no claim (a name no owner has, a target no uid), with Name.
	Bad, Name string
}

func (s Store) path(o Owner) string { return filepath.Join(s.Dir, o.String()) }

// Read is o's claimed uid, if it has one.
func (s Store) Read(o Owner) (uint32, bool, error) {
	target, err := os.Readlink(s.path(o))
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	uid, err := parseUID(target)
	if err != nil {
		return 0, false, fmt.Errorf("claim %s: %w", o, err)
	}
	if d, ok := DomainOf(uid); !ok || d != o.Domain() {
		return 0, false, fmt.Errorf("claim %s: %d is not in its range", o, uid)
	}
	return uid, true, nil
}

func parseUID(s string) (uint32, error) {
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || strconv.FormatUint(n, 10) != s {
		return 0, fmt.Errorf("%q is no uid", s)
	}
	return uint32(n), nil
}

// Lookup is o's uid, claiming one when it has none: its hashed slot, else the next free one
// in its range, going round. The claim is made under the directory's lock, so two lookups at
// once never take one uid, and a symlink is made whole or not at all.
func (s Store) Lookup(o Owner) (uint32, error) {
	if err := o.check(); err != nil {
		return 0, err
	}
	if uid, ok, err := s.Read(o); err != nil || ok {
		return uid, err
	}
	unlock, err := s.lockDir()
	if err != nil {
		return 0, err
	}
	defer unlock()
	if uid, ok, err := s.Read(o); err != nil || ok {
		return uid, err
	}
	claims, err := s.List()
	if err != nil {
		return 0, err
	}
	used := map[uint32]bool{}
	mine := 0
	for _, c := range claims {
		if c.Bad != "" {
			// A name no owner's still holds whatever it points at: never take its uid.
			if uid, err := parseUID(c.Name); err == nil {
				used[uid] = true
			}
			continue
		}
		if c.Owner.Domain() != o.Domain() {
			continue
		}
		used[c.UID] = true
		if c.Owner.Project == o.Project {
			mine++
		}
	}
	if mine >= MaxClaims {
		return 0, fmt.Errorf("%s: project %s holds %d claims in its range already, the most it may", o, o.Project, mine)
	}
	r := ranges[o.Domain()]
	size := r.size
	if s.span != 0 {
		size = s.span
	}
	start := (o.Plain() - r.start) % size
	for i := uint32(0); i < size; i++ {
		uid := r.start + (start+i)%size
		if used[uid] || (s.Taken != nil && s.Taken(uid)) {
			continue
		}
		if err := os.Symlink(strconv.FormatUint(uint64(uid), 10), s.path(o)); err != nil {
			return 0, err
		}
		return uid, nil
	}
	return 0, fmt.Errorf("%s: no free uid left in its range", o)
}

// LookupPair is env's uid and its project's (ProjectOf), each looked up as Lookup does.
func (s Store) LookupPair(env Owner) (uint32, uint32, error) {
	e, err := s.Lookup(env)
	if err != nil {
		return 0, 0, err
	}
	p, err := s.Lookup(env.ProjectOf())
	return e, p, err
}

// lockDir takes the directory's own lock, making the directory, for a claim to be made.
func (s Store) lockDir() (func(), error) {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := s.wait("the uid claims' lock", f, syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

func (s Store) wait(what string, f *os.File, how int) error {
	try := func() (bool, error) { return tryFlock(f, how) }
	if s.Wait == nil {
		return syscall.Flock(int(f.Fd()), how)
	}
	return s.Wait(what, try)
}

func tryFlock(f *os.File, how int) (bool, error) {
	err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

// List is every claim in the directory, sorted by name; an entry that is no claim is listed
// with why. A missing directory holds none.
func (s Store) List() ([]Claim, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var claims []Claim
	for _, e := range entries {
		name := e.Name()
		if name == ".lock" || name == locksDir {
			continue
		}
		o, err := ParseOwner(name)
		if err != nil {
			claims = append(claims, Claim{Name: name, Bad: err.Error()})
			continue
		}
		uid, ok, err := s.Read(o)
		switch {
		case err != nil:
			claims = append(claims, Claim{Name: name, Bad: err.Error()})
		case ok:
			claims = append(claims, Claim{Owner: o, UID: uid, Name: name})
		}
	}
	return claims, nil
}

// locksDir holds the start locks, apart from the claims: an environment may be named
// prod.lock, so a lock beside its claim could be another's claim.
const locksDir = "locks"

// Lock is an owner's start lock: shared by every start of containers on its uid, exclusive
// while its claim is freed, so no container is ever made on a uid just freed.
type Lock struct {
	f    *os.File
	path string
}

// LockStart takes o's start lock, shared (syscall.LOCK_SH) or exclusive (LOCK_EX), its wait
// announced and bounded. It is opened close-on-exec, so nothing the caller starts holds it,
// until KeepAcrossExec.
func (s Store) LockStart(o Owner, how int) (*Lock, error) {
	if err := o.check(); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.Dir, locksDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, o.String())
	for {
		// Read-write: flock is fcntl's lock on NFS, where an exclusive one needs it.
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_CLOEXEC, 0o600)
		if err != nil {
			return nil, err
		}
		if err := s.wait("the start lock of "+o.String(), f, how); err != nil {
			f.Close()
			return nil, err
		}
		// A free removes the file while it holds it: a lock taken on the file it removed is
		// on nothing, so open the one there now and lock again.
		if same, err := samePath(f, path); err != nil || !same {
			f.Close()
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			continue
		}
		return &Lock{f: f, path: path}, nil
	}
}

func samePath(f *os.File, path string) (bool, error) {
	held, err := f.Stat()
	if err != nil {
		return false, err
	}
	now, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return os.SameFile(held, now), nil
}

// KeepAcrossExec clears the lock's close-on-exec flag, right before an exec, so the command
// exec'd holds the lock for as long as it runs and the kernel drops it when it ends.
func (l *Lock) KeepAcrossExec() error {
	_, err := unix.FcntlInt(l.f.Fd(), unix.F_SETFD, 0)
	return err
}

// Close releases the lock.
func (l *Lock) Close() error { return l.f.Close() }

// Free removes o's claim, and its start lock's file, which held must be: o's start lock
// taken exclusive, so no start is on its uid meanwhile. A claim already gone is no error.
func (s Store) Free(o Owner, held *Lock) error {
	if held == nil || held.path != filepath.Join(s.Dir, locksDir, o.String()) {
		return fmt.Errorf("freeing %s needs its start lock held", o)
	}
	if err := os.Remove(s.path(o)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(held.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// FreeIfUnused frees o, a project's (or an instance's) own claim, when no environment claim
// of its project and instance is left, deciding under the directory's lock so no first claim
// of another environment comes between; held is o's start lock, exclusive, as Free needs.
// It says whether it freed.
func (s Store) FreeIfUnused(o Owner, held *Lock) (bool, error) {
	if o.Env != "" {
		return false, fmt.Errorf("%s is an environment's claim", o)
	}
	unlock, err := s.lockDir()
	if err != nil {
		return false, err
	}
	defer unlock()
	claims, err := s.List()
	if err != nil {
		return false, err
	}
	for _, c := range claims {
		if c.Bad == "" && c.Owner.Env != "" && c.Owner.ProjectOf() == o {
			return false, nil
		}
	}
	return true, s.Free(o, held)
}
