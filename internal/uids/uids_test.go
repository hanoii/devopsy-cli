package uids

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func must(t *testing.T, o Owner, err error) Owner {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func env(t *testing.T, project, instance, name string) Owner {
	t.Helper()
	o, err := EnvOwner(project, instance, name)
	return must(t, o, err)
}

func project(t *testing.T, name, instance string) Owner {
	t.Helper()
	o, err := ProjectOwner(name, instance)
	return must(t, o, err)
}

// TestPlainIsPinned keeps the hash what it is: a uid an image baked, or a claim on a server,
// depends on it, so a change to the hash, the ranges or an owner's name moves every uid.
func TestPlainIsPinned(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		o    Owner
		name string
		uid  uint32
		d    Domain
	}{
		{Owner{Project: "shop", Env: "prod"}, "env:shop:prod", 2064483025, EnvDomain},
		{Owner{"shop", "eu", "prod"}, "env:shop:eu:prod", 2092396053, EnvDomain},
		{Owner{Project: "shop"}, "project:shop", 1911770400, ProjectDomain},
		{Owner{Project: "shop", Instance: "eu"}, "project:shop:eu", 1961611468, InstanceDomain},
	} {
		if got := c.o.String(); got != c.name {
			t.Errorf("%v: name %s, want %s", c.o, got, c.name)
		}
		if got := c.o.Plain(); got != c.uid {
			t.Errorf("%s: plain %d, want %d", c.name, got, c.uid)
		}
		if d, ok := DomainOf(c.o.Plain()); !ok || d != c.d || c.o.Domain() != c.d {
			t.Errorf("%s: domain %v %v, owner's %v, want %v", c.name, d, ok, c.o.Domain(), c.d)
		}
	}
}

// TestOwnersAreApart keeps owners one by their parts, where compose names are flat: an
// instance admin's prod and a project shop-admin's prod are both shop-admin-prod, and a
// project's own uid never shares an environment's name.
func TestOwnersAreApart(t *testing.T) {
	t.Parallel()
	owners := []Owner{
		env(t, "shop", "admin", "prod"),
		env(t, "shop-admin", "", "prod"),
		project(t, "shop-prod", ""),
		project(t, "shop", "prod"),
		env(t, "shop", "", "prod"),
	}
	seen := map[string]bool{}
	for _, o := range owners {
		if seen[o.String()] {
			t.Errorf("%s twice", o)
		}
		seen[o.String()] = true
		back, err := ParseOwner(o.String())
		if err != nil || back != o {
			t.Errorf("%s read back as %v, %v", o, back, err)
		}
	}
}

// TestOwnerNamesChecked keeps every part a name the config takes, lowercase, with no ':' and
// no empty part, and a claim's name short enough for a file.
func TestOwnerNamesChecked(t *testing.T) {
	t.Parallel()
	long := ""
	for len(long) < nameMax {
		long += "a"
	}
	for _, c := range []struct{ p, i, e string }{
		{"Shop", "", "prod"}, {"shop", "EU", "prod"}, {"shop", "", "Prod"}, {"shop", "", "pr:1"},
		{"", "", "prod"}, {"shop", "", "-prod"}, {"sh/op", "", "prod"}, {"shop", "", long},
	} {
		if _, err := EnvOwner(c.p, c.i, c.e); err == nil {
			t.Errorf("%q %q %q: no error", c.p, c.i, c.e)
		}
	}
	for _, s := range []string{"env:shop", "env::prod", "project:", "user:shop", "env:shop:eu:prod:x", "project:Shop"} {
		if _, err := ParseOwner(s); err == nil {
			t.Errorf("%q parsed", s)
		}
	}
	if _, err := EnvOwner("shop", "", "pr-12.b_x"); err != nil {
		t.Error(err)
	}
}

func store(t *testing.T) Store {
	return Store{Dir: filepath.Join(t.TempDir(), "uids")}
}

// claim plants a claim for o on uid, as another owner's made earlier.
func claim(t *testing.T, s Store, o Owner, uid uint32) {
	t.Helper()
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(strconv.FormatUint(uint64(uid), 10), filepath.Join(s.Dir, o.String())); err != nil {
		t.Fatal(err)
	}
}

// TestLookupClaims keeps a lookup taking the hashed slot when free, and the same uid after.
func TestLookupClaims(t *testing.T) {
	t.Parallel()
	s := store(t)
	o := env(t, "shop", "", "prod")
	uid, err := s.Lookup(o)
	if err != nil || uid != o.Plain() {
		t.Fatalf("lookup %d, %v, want %d", uid, err, o.Plain())
	}
	if again, err := s.Lookup(o); err != nil || again != uid {
		t.Fatalf("again %d, %v", again, err)
	}
	if info, err := os.Stat(s.Dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("dir %v, %v", info, err)
	}
}

// TestLookupProbes keeps a lookup off a slot another owner holds, or the host's, taking the
// next free one, going round at the range's end.
func TestLookupProbes(t *testing.T) {
	t.Parallel()
	s := store(t)
	o := env(t, "shop", "", "prod")
	h := o.Plain()
	claim(t, s, env(t, "other", "", "x"), h)
	s.Taken = func(uid uint32) bool { return uid == h+1 }
	if uid, err := s.Lookup(o); err != nil || uid != h+2 {
		t.Fatalf("lookup %d, %v, want %d", uid, err, h+2)
	}

	// At the range's end it goes round to the start.
	s = store(t)
	start, size := Range(EnvDomain)
	last := start + size - 1
	var at Owner
	for i := 0; ; i++ {
		at = env(t, "p", "", "e"+strconv.Itoa(i))
		if at.Plain() > last-1000 {
			break
		}
	}
	for uid := at.Plain(); uid <= last; uid++ {
		claim(t, s, env(t, "q", "", "e"+strconv.FormatUint(uint64(uid), 10)), uid)
	}
	if uid, err := s.Lookup(at); err != nil || uid != start {
		t.Fatalf("wrap %d, %v, want %d", uid, err, start)
	}
}

// TestUIDNeverMoves keeps a probed owner on its uid once the slot it was pushed from is
// freed: claims are keyed by owner, so B stays at h+1 when A, at h, goes.
func TestUIDNeverMoves(t *testing.T) {
	t.Parallel()
	s := store(t)
	b := env(t, "shop", "", "prod")
	a := env(t, "other", "", "x")
	claim(t, s, a, b.Plain())
	uid, err := s.Lookup(b)
	if err != nil || uid != b.Plain()+1 {
		t.Fatalf("lookup %d, %v", uid, err)
	}
	lock, err := s.LockStart(a, syscall.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Free(a, lock); err != nil {
		t.Fatal(err)
	}
	lock.Close()
	if again, err := s.Lookup(b); err != nil || again != uid {
		t.Fatalf("after the free %d, %v, want %d", again, err, uid)
	}
}

// TestLookupsAtOnce keeps owners hashing alike, claimed at once, on distinct uids, and one
// owner looked up at once on one uid: each lookup opens its own lock, as processes do.
func TestLookupsAtOnce(t *testing.T) {
	t.Parallel()
	s := store(t)
	o := env(t, "shop", "", "prod")
	others := make([]Owner, 20)
	for i := range others {
		others[i] = env(t, "shop", "", "pr-"+strconv.Itoa(i))
	}
	// Every owner lands in a span of 32 slots, so most collide with another.
	s.span = 32
	got := make([]uint32, 2*len(others))
	var wg sync.WaitGroup
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			who := o
			if i%2 == 1 {
				who = others[i/2]
			}
			uid, err := s.Lookup(who)
			if err != nil {
				t.Error(err)
			}
			got[i] = uid
		}()
	}
	wg.Wait()
	byOwner := map[string]uint32{}
	byUID := map[uint32]string{}
	for i, uid := range got {
		who := o
		if i%2 == 1 {
			who = others[i/2]
		}
		if prev, ok := byOwner[who.String()]; ok && prev != uid {
			t.Errorf("%s got %d and %d", who, prev, uid)
		}
		byOwner[who.String()] = uid
		if prev, ok := byUID[uid]; ok && prev != who.String() {
			t.Errorf("%d given to %s and %s", uid, prev, who)
		}
		byUID[uid] = who.String()
	}
	if len(byUID) != len(others)+1 {
		t.Errorf("%d uids for %d owners", len(byUID), len(others)+1)
	}
}

// TestLookupCaps keeps a project from claiming past MaxClaims in a domain.
func TestLookupCaps(t *testing.T) {
	t.Parallel()
	s := store(t)
	start, _ := Range(EnvDomain)
	for i := 0; i < MaxClaims; i++ {
		claim(t, s, env(t, "shop", "", "e"+strconv.Itoa(i)), start+uint32(i))
	}
	if _, err := s.Lookup(env(t, "shop", "", "one-more")); err == nil {
		t.Fatal("no refusal past the cap")
	}
	if _, err := s.Lookup(env(t, "other", "", "prod")); err != nil {
		t.Fatalf("another project refused: %v", err)
	}
}

// TestBadEntriesHoldTheirUIDs keeps a lookup off a uid an entry names that is no claim, and
// lists the entry with why.
func TestBadEntriesHoldTheirUIDs(t *testing.T) {
	t.Parallel()
	s := store(t)
	o := env(t, "shop", "", "prod")
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nowhere", filepath.Join(s.Dir, "user:someone")); err != nil {
		t.Fatal(err)
	}
	claims, err := s.List()
	if err != nil || len(claims) != 1 || claims[0].Bad == "" {
		t.Fatalf("list %+v, %v", claims, err)
	}
	if _, err := s.Lookup(o); err != nil {
		t.Fatal(err)
	}
	// A claim whose target is out of its range is refused, not taken.
	other := env(t, "shop", "", "staging")
	claim(t, s, other, 5)
	if _, _, err := s.Read(other); err == nil {
		t.Fatal("a claim out of its range read")
	}
}

// TestStartLock keeps the start lock shared among starts, exclusive against a free, waited
// for through Wait, and close-on-exec until KeepAcrossExec.
func TestStartLock(t *testing.T) {
	t.Parallel()
	s := store(t)
	o := env(t, "shop", "", "prod")
	waited := ""
	s.Wait = func(what string, try func() (bool, error)) error {
		ok, err := try()
		if err != nil || ok {
			return err
		}
		waited = what
		return syscall.EWOULDBLOCK
	}
	a, err := s.LockStart(o, syscall.LOCK_SH)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.LockStart(o, syscall.LOCK_SH)
	if err != nil {
		t.Fatalf("a second start refused: %v", err)
	}
	if _, err := s.LockStart(o, syscall.LOCK_EX); err == nil || waited == "" {
		t.Fatalf("exclusive taken beside starts: %v, waited %q", err, waited)
	}
	flags, err := unix.FcntlInt(a.f.Fd(), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("not close-on-exec: %d, %v", flags, err)
	}
	if err := a.KeepAcrossExec(); err != nil {
		t.Fatal(err)
	}
	if flags, _ := unix.FcntlInt(a.f.Fd(), unix.F_GETFD, 0); flags&unix.FD_CLOEXEC != 0 {
		t.Fatal("still close-on-exec")
	}
	a.Close()
	b.Close()
	ex, err := s.LockStart(o, syscall.LOCK_EX)
	if err != nil {
		t.Fatalf("exclusive with no start: %v", err)
	}
	ex.Close()
}

// TestStartLockAfterFree keeps a start that waited on a free from locking the file the free
// removed: it opens the one there now, so the next free excludes it.
func TestStartLockAfterFree(t *testing.T) {
	t.Parallel()
	s := store(t)
	o := env(t, "shop", "", "prod")
	if _, err := s.Lookup(o); err != nil {
		t.Fatal(err)
	}
	ex, err := s.LockStart(o, syscall.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan *Lock)
	go func() {
		l, err := s.LockStart(o, syscall.LOCK_SH)
		if err != nil {
			t.Error(err)
		}
		got <- l
	}()
	// The start is waiting on the old file by the time the free removes it; the free's own
	// close then lets it lock that old file, which it must notice.
	if err := s.Free(o, ex); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Read(o); ok {
		t.Fatal("claim left")
	}
	ex.Close()
	start := <-got
	defer start.Close()
	if same, err := samePath(start.f, start.path); err != nil || !same {
		t.Fatalf("the start holds a removed file: %v", err)
	}
	s.Wait = func(string, func() (bool, error)) error { return syscall.EWOULDBLOCK }
	if _, err := s.LockStart(o, syscall.LOCK_EX); err == nil {
		t.Fatal("a second free took the lock beside the start")
	}
}

// TestFreeNeedsTheLock keeps a free from removing a claim without its start lock held.
func TestFreeNeedsTheLock(t *testing.T) {
	t.Parallel()
	s := store(t)
	o := env(t, "shop", "", "prod")
	if _, err := s.Lookup(o); err != nil {
		t.Fatal(err)
	}
	other, err := s.LockStart(env(t, "shop", "", "staging"), syscall.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := s.Free(o, nil); err == nil {
		t.Fatal("freed with no lock")
	}
	if err := s.Free(o, other); err == nil {
		t.Fatal("freed with another owner's lock")
	}
	if _, ok, _ := s.Read(o); !ok {
		t.Fatal("claim gone")
	}
}

// TestHostTaken keeps a claim off the host's own users, groups and subordinate ranges.
func TestHostTaken(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	for f, data := range map[string]string{
		"passwd": "root:x:0:0::/root:/bin/sh\nbig:x:2000000001:5::/:/bin/sh\n",
		"group":  "wheel:x:2000000002:\n",
		"subuid": "someone:2000000100:10\n",
		"subgid": "someone:2000000200:10\n",
	} {
		if err := os.WriteFile(filepath.Join(root, "etc", f), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	taken := HostTaken(root)
	for uid, want := range map[uint32]bool{
		0: true, 2000000001: true, 2000000002: true, 2000000100: true, 2000000109: true,
		2000000110: false, 2000000205: true, 2000000003: false,
	} {
		if taken(uid) != want {
			t.Errorf("%d: %v, want %v", uid, !want, want)
		}
	}
	if HostTaken(filepath.Join(root, "none"))(0) {
		t.Error("a host with no files takes 0")
	}
}

// A project's claim goes only with the last environment claim of its project and instance.
func TestFreeIfUnused(t *testing.T) {
	t.Parallel()
	s := store(t)
	a, b := env(t, "shop", "", "prod"), env(t, "shop", "", "staging")
	eu := env(t, "shop", "eu", "prod")
	for _, o := range []Owner{a, b, eu} {
		if _, _, err := s.LookupPair(o); err != nil {
			t.Fatal(err)
		}
	}
	free := func(o Owner) {
		t.Helper()
		l, err := s.LockStart(o, syscall.LOCK_EX)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		if err := s.Free(o, l); err != nil {
			t.Fatal(err)
		}
	}
	project := func() bool {
		t.Helper()
		l, err := s.LockStart(a.ProjectOf(), syscall.LOCK_EX)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		freed, err := s.FreeIfUnused(a.ProjectOf(), l)
		if err != nil {
			t.Fatal(err)
		}
		return freed
	}
	free(a)
	if project() {
		t.Fatal("freed with staging left")
	}
	free(b)
	if !project() {
		t.Fatal("not freed with no environment left")
	}
	if _, ok, _ := s.Read(eu.ProjectOf()); !ok {
		t.Fatal("the instance's project claim went with the project's")
	}
}

// An entry that is no claim (a name no owner has) still holds the uid it points at: a lookup
// never takes it.
func TestBadEntryHoldsTheUIDItPointsAt(t *testing.T) {
	t.Parallel()
	s := store(t)
	o := env(t, "shop", "", "prod")
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	plain := strconv.FormatUint(uint64(o.Plain()), 10)
	// A name no owner has (a hand's rename), on o's slot; never one differing in case alone,
	// which macOS's filesystem takes for o's own.
	if err := os.Symlink(plain, filepath.Join(s.Dir, "env:shop:prod:old")); err != nil {
		t.Fatal(err)
	}
	uid, err := s.Lookup(o)
	if err != nil {
		t.Fatal(err)
	}
	if uid == o.Plain() {
		t.Errorf("%s took %d, which the entry env:shop:prod:old points at", o, uid)
	}
}
