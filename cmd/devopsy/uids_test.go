package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/uids"
)

// What an environment's containers may run as behind the door is the claimed uid of the
// environment both the request's directory and the file's name are of, with its project's:
// <tree>/<environment>/... or <tree>/<instance>/<environment>/..., read from the path, whose
// compose name the file has. A name the directory is not (another name of the project's, in
// prod's directory) allows none: a --destroy downs by name, and a container under another
// would outlive the free of its uid.
func TestDirOwners(t *testing.T) {
	t.Parallel()
	tree := "/home/devopsy-shop/shop"
	claims := []uids.Claim{
		{Owner: uids.Owner{Project: "shop", Instance: "admin", Env: "prod"}, UID: 2000000001},
		{Owner: uids.Owner{Project: "shop", Env: "admin"}, UID: 2000000002},
		{Owner: uids.Owner{Project: "shop", Env: "prod"}, UID: 2000000003},
		{Owner: uids.Owner{Project: "shop", Instance: "admin"}, UID: 1950000001},
		{Owner: uids.Owner{Project: "shop"}, UID: 1900000001},
	}
	for _, c := range []struct {
		real, name string
		want       []uint32
	}{
		{tree + "/admin/prod/current/.devopsy", "shop-admin-prod", []uint32{2000000001, 1950000001}},
		{tree + "/admin/prod/current/.devopsy", "shop-admin", []uint32{2000000002, 1900000001}},
		{tree + "/prod/releases/20260101000000/code", "shop-prod", []uint32{2000000003, 1900000001}},
		{tree + "/prod", "shop-prod", []uint32{2000000003, 1900000001}},
		{tree + "/prod", "shop-other", nil},
		{tree + "/pr-1/current", "shop-prod", nil},
		{tree, "shop-prod", nil},
		{"/home/devopsy-blog/blog/prod", "shop-prod", nil},
	} {
		if got := uidsOfOwners(claims, dirOwners("shop", tree, c.real, c.name, claims)); !slices.Equal(got, c.want) {
			t.Errorf("%s named %s: %v, want %v", c.real, c.name, got, c.want)
		}
	}
}

// The door's answer to uids is two numbers and nothing else.
func TestParseUIDPair(t *testing.T) {
	t.Parallel()
	if e, p, err := parseUIDPair("2000000001 1900000001\n"); err != nil || e != 2000000001 || p != 1900000001 {
		t.Errorf("%d %d %v", e, p, err)
	}
	for _, bad := range []string{"", "1", "1 2 3", "a 1", "1 -2", "99999999999 1"} {
		if _, _, err := parseUIDPair(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// busyStore is a store whose waits give up at once, to see whether a lock is held.
func busyStore(dir string) uids.Store {
	return uids.Store{Dir: dir, Wait: func(_ string, try func() (bool, error)) error {
		if ok, err := try(); ok || err != nil {
			return err
		}
		return errors.New("held")
	}}
}

// A --destroy's last step takes the environment down once more under its start lock,
// exclusive, then frees its claim; the project's goes only with its last environment.
func TestFreeUIDs(t *testing.T) {
	t.Parallel()
	s := uids.Store{Dir: filepath.Join(t.TempDir(), ".devopsy-uids")}
	prod := uids.Owner{Project: "shop", Env: "prod"}
	staging := uids.Owner{Project: "shop", Env: "staging"}
	for _, o := range []uids.Owner{prod, staging} {
		if _, _, err := s.LookupPair(o); err != nil {
			t.Fatal(err)
		}
	}
	var downs []string
	down := func(name string) error {
		downs = append(downs, name)
		if l, err := busyStore(s.Dir).LockStart(prod, syscall.LOCK_SH); err == nil {
			l.Close()
			t.Error("a start took the lock while the free took the environment down")
		}
		return nil
	}
	if err := freeUIDs(s, prod, noneThere, down); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(downs, []string{"shop-prod"}) {
		t.Errorf("downs %v", downs)
	}
	if _, ok, _ := s.Read(prod); ok {
		t.Error("prod's claim left")
	}
	if _, ok, _ := s.Read(prod.ProjectOf()); !ok {
		t.Error("the project's claim went with staging still there")
	}
	if err := freeUIDs(s, staging, noneThere, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Read(prod.ProjectOf()); ok {
		t.Error("the project's claim left with its last environment gone")
	}
	// An environment there again once the lock is held (released while the free waited):
	// nothing taken down, its claim kept.
	if _, _, err := s.LookupPair(prod); err != nil {
		t.Fatal(err)
	}
	downs = nil
	if err := freeUIDs(s, prod, func() error { return errors.New("there again") }, down); err == nil || len(downs) != 0 {
		t.Errorf("a free of an environment there again: %v, downs %v", err, downs)
	}
	if _, ok, _ := s.Read(prod); !ok {
		t.Error("the claim of an environment there again went")
	}
	// A failed down frees nothing.
	if _, _, err := s.LookupPair(prod); err != nil {
		t.Fatal(err)
	}
	if err := freeUIDs(s, prod, noneThere, func(string) error { return errors.New("no daemon") }); err == nil {
		t.Error("freed past a failed down")
	}
	if _, ok, _ := s.Read(prod); !ok {
		t.Error("the claim went with its down failed")
	}
}

// A start in a release holds both start locks, shared, with the claims read again under them:
// a free waits for it, and a claim that changed since the load fails the start.
func TestHoldStartLocks(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := uids.Store{Dir: filepath.Join(root, cli.UIDsDir)}
	owner := uids.Owner{Project: "shop", Env: "prod"}
	e, p, err := s.LookupPair(owner)
	if err != nil {
		t.Fatal(err)
	}
	plan := &cli.Plan{StartsUIDs: true, UIDRoot: root, UIDOwner: owner, Env: []string{
		cli.UIDEnv + "=" + strconv.FormatUint(uint64(e), 10), cli.ProjectUIDEnv + "=" + strconv.FormatUint(uint64(p), 10),
	}}
	locks, err := holdStartLocks(plan)
	if err != nil || len(locks) != 2 {
		t.Fatalf("%d locks, %v", len(locks), err)
	}
	for _, o := range []uids.Owner{owner, owner.ProjectOf()} {
		if l, err := busyStore(s.Dir).LockStart(o, syscall.LOCK_EX); err == nil {
			l.Close()
			t.Errorf("%s's free took its lock beside a start", o)
		}
	}
	closeLocks(locks)
	plan.Env[0] = cli.UIDEnv + "=2000000000"
	if _, err := holdStartLocks(plan); err == nil || !strings.Contains(err.Error(), "changed as it started") {
		t.Errorf("a claim changed since the load: %v", err)
	}
}

// The door frees only an environment of its own project's, once that environment's data is
// gone: what a project user's --destroy asks last.
func TestDoorFreeRefuses(t *testing.T) {
	t.Parallel()
	space := t.TempDir()
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: space}}
	for _, owner := range []string{"env:other:prod", "project:shop", "env:shop", "nonsense"} {
		if code := side.free(owner); code != 2 {
			t.Errorf("%s: freed (%d)", owner, code)
		}
	}
	if err := os.MkdirAll(filepath.Join(side.policy.Mnt("shop-prod"), "storage"), 0o700); err != nil {
		t.Fatal(err)
	}
	if code := side.free("env:shop:prod"); code != 2 {
		t.Errorf("freed with its data there (%d)", code)
	}
}

// --server-info lists every uid claim with its owner, and warns of one whose environment is
// gone (removed without --destroy) or whose project has no environment claim left, and of an
// entry that is no claim, each with its removal by hand.
func TestServerInfoListsUIDClaims(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := claimsStore(root)
	for _, o := range []uids.Owner{{Project: "shop", Env: "prod"}, {Project: "blog", Env: "prod"}} {
		if _, _, err := s.LookupPair(o); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("x", filepath.Join(s.Dir, "stray")); err != nil {
		t.Fatal(err)
	}
	// A door project's tree that answered nothing: its claims taken for live.
	if _, _, err := s.LookupPair(uids.Owner{Project: "door", Env: "prod"}); err != nil {
		t.Fatal(err)
	}
	info := remote.ServerInfo{Version: "0.53.0", Root: root, Environments: []remote.Environment{{Path: "shop/prod", Status: "none\t\n"}}, DoorProjects: []string{"door"}}
	info.UIDsDir, info.UIDs = uidFacts(root)
	out, warnings := serverInfoSays(info, "", "", "0.53.0", time.Now())
	for _, want := range []string{"env:shop:prod", "project:shop", "env:blog:prod", "project:blog", "stray  no claim"} {
		if !strings.Contains(out, want) {
			t.Errorf("no uids row for %s:\n%s", want, out)
		}
	}
	stale := map[string]bool{}
	for _, w := range warnings {
		for _, owner := range []string{"env:shop:prod", "env:blog:prod", "project:blog", "project:shop", "stray", "env:door:prod"} {
			if strings.HasPrefix(w.msg, owner+"'s uid claim is left") || strings.HasPrefix(w.msg, owner+" in ") {
				stale[owner] = true
				if len(w.fixes) != 1 || len(w.fixes[0].Cmds) != 1 || !strings.HasPrefix(w.fixes[0].Cmds[0], "rm ") {
					t.Errorf("%s: no removal offered: %+v", owner, w.fixes)
				}
			}
		}
	}
	for owner, want := range map[string]bool{"env:blog:prod": true, "stray": true, "env:shop:prod": false, "project:shop": false, "project:blog": false, "env:door:prod": false} {
		if stale[owner] != want {
			t.Errorf("%s warned %v, want %v", owner, stale[owner], want)
		}
	}
	// With the environments not all read, an environment's claim is taken for live: blog's
	// directory may be the one that failed.
	info.EnvironmentsUnread = "open " + filepath.Join(root, "blog") + ": permission denied"
	_, warnings = serverInfoSays(info, "", "", "0.53.0", time.Now())
	for _, w := range warnings {
		if strings.HasPrefix(w.msg, "env:blog:prod's uid claim is left") {
			t.Errorf("with environments unread: %s", w.msg)
		}
	}
}

// A --destroy empties the door's mnt directory and leaves it: an empty one is no data left.
func TestDataLeft(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "shop-prod")
	if left, err := dataLeft(dir); err != nil || left {
		t.Errorf("none there: %v %v", left, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if left, err := dataLeft(dir); err != nil || left {
		t.Errorf("an emptied directory: %v %v", left, err)
	}
	if err := os.Mkdir(filepath.Join(dir, "storage"), 0o700); err != nil {
		t.Fatal(err)
	}
	if left, err := dataLeft(dir); err != nil || !left {
		t.Errorf("an entry left: %v %v", left, err)
	}
}

// lockedDockerHelper is a docker for TestComposeRunHoldsStartLocks, spec "<lock>|<out>": a
// compose config answers an empty project, and an up writes to out whether lock, a start
// lock's file, is held (its exclusive lock refused) while compose runs.
func lockedDockerHelper(spec string) {
	lock, out, _ := strings.Cut(spec, "|")
	args := strings.Join(os.Args[1:], " ")
	switch {
	case strings.Contains(args, " config"):
		os.Stdout.WriteString(`{"name":"shop-prod","services":{}}`)
	case strings.Contains(args, " up"):
		state := "free"
		if l, err := busyStore(filepath.Dir(filepath.Dir(lock))).LockStart(uids.Owner{Project: "shop", Env: "prod"}, syscall.LOCK_EX); err != nil {
			state = "held"
		} else {
			l.Close()
		}
		_ = os.WriteFile(out, []byte(state), 0o644)
	}
	os.Exit(0)
}

// A start devopsy runs as a child in a release (composeRun: a release's step, ensureVolumes)
// holds the environment's start lock while compose runs, as an exec'd one does: a --destroy
// frees a claim after its release lock went with the environment's directory, so the release
// lock keeps no free out.
//
// Not parallel: it sets PATH, DOCKER_HOST and DEVOPSY_TEST_LOCKED_DOCKER.
func TestComposeRunHoldsStartLocks(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(root, "shop", "prod", "releases", "1")
	write(t, filepath.Join(rel, remote.CodeDir, ".devopsy", "compose.yaml"), "services:\n  web:\n    image: x\n", 0o644)
	write(t, filepath.Join(rel, remote.TargetEnvFile), "COMPOSE_PROJECT_NAME='shop-prod'\nDEVOPSY_PROJECT='shop'\nDEVOPSY_ENVIRONMENT='prod'\n", 0o644)
	bin := t.TempDir()
	if err := os.Symlink(os.Args[0], filepath.Join(bin, "docker")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "state")
	lock := filepath.Join(root, cli.UIDsDir, "locks", "env:shop:prod")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DEVOPSY_TEST_LOCKED_DOCKER", lock+"|"+out)
	// The deploy user's devopsy, which reaches Docker itself: its docker is no door's.
	t.Setenv("DOCKER_HOST", "unix:///nonexistent")
	if err := composeRun(context.Background(), filepath.Join(rel, remote.CodeDir), io.Discard, false, "up", "-d"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(out); string(got) != "held" {
		t.Errorf("compose ran with the start lock %q", got)
	}
}

// The door's free takes an environment directory of the caller's tree and reads the owner
// from it, never the caller's words: a name that is another project's (an instance admin's
// prod is shop-admin-prod, project shop-admin's) is refused before anything is taken down,
// so is an environment still there, and a gone one of its own with no claim takes nothing
// down either.
//
// Not parallel: it sets PATH (doorFakes) and doorLog.
func TestDoorFreeTakesWhatItsPathSays(t *testing.T) {
	_, docker := doorFakes(t)
	dir := t.TempDir()
	saved := doorLog
	t.Cleanup(func() { doorLog = saved })
	doorLog = filepath.Join(dir, "refused.log")
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "prod"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "root")
	side := &doorSide{
		policy:  door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space"), Tree: tree, Others: []string{"shop-admin"}},
		uidRoot: root, real: docker, env: os.Environ(),
	}
	for _, base := range []string{filepath.Join(tree, "admin", "prod"), filepath.Join(tree, "prod"), "env:shop:admin-prod", filepath.Join(dir, "elsewhere", "prod")} {
		if code := side.free(base); code != 2 {
			t.Errorf("%s: freed (%d)", base, code)
		}
	}
	if _, err := os.Stat(docker); err == nil {
		t.Errorf("docker ran for a refused free")
	}
	// Its mnt, which its --destroy emptied and left, goes with the free.
	mnt := side.policy.Mnt("shop-gone")
	if err := os.MkdirAll(mnt, 0o700); err != nil {
		t.Fatal(err)
	}
	if code := side.free(filepath.Join(tree, "gone")); code != 0 {
		t.Errorf("a gone environment of its own, never claimed: %d", code)
	}
	if _, err := os.Stat(docker); err == nil {
		t.Errorf("a free of what held no claim took a name down")
	}
	if _, err := os.Lstat(mnt); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the emptied mnt %s stays after the free: %v", mnt, err)
	}
}

// noneThere is a free's gone check for an environment that is gone.
func noneThere() error { return nil }

// Behind the door a start runs as an environment's uid only under that environment's own name:
// a file in prod's directory named as another of the project's environments gets no uid, since
// prod's --destroy downs by name and a container under another would outlive the free.
//
// Not parallel: it sets doorLog and doorWho, and PATH (doorFakes).
func TestDoorHoldsAUIDsContainersToItsName(t *testing.T) {
	doorFakes(t)
	dir := t.TempDir()
	savedLog, savedWho := doorLog, doorWho
	t.Cleanup(func() { doorLog, doorWho = savedLog, savedWho })
	doorLog, doorWho = filepath.Join(dir, "refused.log"), "devopsy-shop"
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "prod"), 0o755); err != nil {
		t.Fatal(err)
	}
	docker := doorImageFake(t, dir)
	fakeManifest(t, dir, plainManifest("busybox"), "busybox:1.37")
	write(t, filepath.Join(dir, "store"), "", 0o644)
	seen := filepath.Join(dir, "seen")
	vet := filepath.Join(dir, "vet")
	write(t, vet, "#!/bin/sh\ncat >"+seen+"\nexit 2\n", 0o755)
	claims := []uids.Claim{
		{Owner: uids.Owner{Project: "shop", Env: "prod"}, UID: 2000000003},
		{Owner: uids.Owner{Project: "shop"}, UID: 1900000001},
	}
	file := func(name string) map[string]any {
		return map[string]any{"name": name, "services": map[string]any{
			"web": map[string]any{
				"image": "busybox:1.37", "user": "2000000003:2000000003", "security_opt": []any{"no-new-privileges"},
				"labels": map[string]any{"vetted.x": "1"},
			},
		}}
	}
	side := func() *doorSide {
		return &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space"), Tree: tree},
			claims: claims, vets: []remote.DoorVet{{Prefix: "vetted.", Command: vet}}, real: docker, imagesRoot: dir}
	}
	// The control: prod's own name gets past the checks to the vet.
	side().compose("", file("shop-prod"), "", filepath.Join(tree, "prod"), []string{"up", "-d"})
	if _, err := os.Stat(seen); err != nil {
		t.Fatalf("the control: prod's own start did not reach the vet: %v", err)
	}
	os.Remove(seen)
	side().compose("", file("shop-other"), "", filepath.Join(tree, "prod"), []string{"up", "-d"})
	if _, err := os.Stat(seen); err == nil {
		t.Error("a file named shop-other in prod's directory passed the door's checks running as prod's uid: the free of prod's claim takes only shop-prod down")
	}
}

// Behind the door a start's uids follow the request's directory, never a name the file picks:
// a pr-1 checkout naming itself shop-prod gets no prod uid.
//
// Not parallel: it sets doorLog and doorWho, and PATH (doorFakes).
func TestDoorUIDsAreTheDirectorys(t *testing.T) {
	doorFakes(t)
	dir := t.TempDir()
	savedLog, savedWho := doorLog, doorWho
	t.Cleanup(func() { doorLog, doorWho = savedLog, savedWho })
	doorLog, doorWho = filepath.Join(dir, "refused.log"), "devopsy-shop"
	tree := filepath.Join(dir, "tree")
	for _, env := range []string{"prod", "pr-1"} {
		if err := os.MkdirAll(filepath.Join(tree, env), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	docker := doorImageFake(t, dir)
	fakeManifest(t, dir, plainManifest("busybox"), "busybox:1.37")
	write(t, filepath.Join(dir, "store"), "", 0o644)
	seen := filepath.Join(dir, "seen")
	vet := filepath.Join(dir, "vet")
	write(t, vet, "#!/bin/sh\ncat >"+seen+"\nexit 2\n", 0o755)
	claims := []uids.Claim{
		{Owner: uids.Owner{Project: "shop", Env: "prod"}, UID: 2000000003},
		{Owner: uids.Owner{Project: "shop", Env: "pr-1"}, UID: 2000000004},
		{Owner: uids.Owner{Project: "shop"}, UID: 1900000001},
	}
	// compose changes the file it checks (its labels, its images pinned): one each time.
	file := func() map[string]any {
		return map[string]any{"name": "shop-prod", "services": map[string]any{
			"web": map[string]any{
				"image": "busybox:1.37", "user": "2000000003:2000000003", "security_opt": []any{"no-new-privileges"},
				"labels": map[string]any{"vetted.x": "1"},
			},
		}}
	}
	side := func() *doorSide {
		return &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space"), Tree: tree},
			claims: claims, vets: []remote.DoorVet{{Prefix: "vetted.", Command: vet}}, real: docker, imagesRoot: dir}
	}
	// The control: prod's own directory gets past the checks to the vet.
	side().compose("", file(), "", filepath.Join(tree, "prod"), []string{"up", "-d"})
	if _, err := os.Stat(seen); err != nil {
		t.Fatalf("the control: prod's own start did not reach the vet: %v", err)
	}
	os.Remove(seen)
	side().compose("", file(), "", filepath.Join(tree, "pr-1"), []string{"up", "-d"})
	if _, err := os.Stat(seen); err == nil {
		t.Error("pr-1's directory passed the door's checks running as prod's uid")
	}
}

// A free takes the project's start lock only when the project's claim goes: a sibling's start
// (prod's long run) never holds up pr-1's --destroy.
func TestFreeUIDsWaitsForNoSiblingsStart(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), ".devopsy-uids")
	s := busyStore(dir)
	prod := uids.Owner{Project: "shop", Env: "prod"}
	pr := uids.Owner{Project: "shop", Env: "pr-1"}
	for _, o := range []uids.Owner{prod, pr} {
		if _, _, err := s.LookupPair(o); err != nil {
			t.Fatal(err)
		}
	}
	// prod runs a container: its start holds both locks, shared, for as long as compose runs.
	for _, o := range []uids.Owner{prod, prod.ProjectOf()} {
		l, err := s.LockStart(o, syscall.LOCK_SH)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
	}
	if err := freeUIDs(s, pr, noneThere, func(string) error { return nil }); err != nil {
		t.Errorf("pr-1's free waited for prod's start, though the project's claim stays: %v", err)
	}
	if _, ok, _ := s.Read(prod.ProjectOf()); !ok {
		t.Error("the project's claim went with prod still there")
	}
}

// A free of an environment that never held a claim takes nothing down: its flat compose name
// may be another project's (an instance admin's prod, project shop-admin's prod).
func TestFreeUIDsDownsNothingNeverClaimed(t *testing.T) {
	t.Parallel()
	s := uids.Store{Dir: filepath.Join(t.TempDir(), ".devopsy-uids")}
	var downs []string
	err := freeUIDs(s, uids.Owner{Project: "shop", Instance: "admin", Env: "prod"}, noneThere, func(name string) error {
		downs = append(downs, name)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(downs) != 0 {
		t.Errorf("an environment that never claimed a uid took down %s", strings.Join(downs, ", "))
	}
}

// A free's down runs only while something still carries the compose name: with nothing left,
// compose's down by name says "No resource found to remove" in its own words. A listing that
// fails reads as something left.
func TestFreeDownOnlyWhatIsLeft(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		left map[string]string
		fail bool
		down bool
	}{
		{name: "nothing left"},
		{name: "a container", left: map[string]string{"ps": "abc\n"}, down: true},
		{name: "a volume", left: map[string]string{"volume": "shop-prod_data\n"}, down: true},
		{name: "a network", left: map[string]string{"network": "def\n"}, down: true},
		{name: "a listing that fails", fail: true, down: true},
	} {
		var asked []string
		docker := func(args ...string) ([]byte, error) {
			asked = append(asked, strings.Join(args, " "))
			if c.fail {
				return nil, errors.New("no daemon")
			}
			return []byte(c.left[args[0]]), nil
		}
		downs := 0
		err := downIfLeft(docker, func(name string) error {
			if name != "shop-prod" {
				t.Errorf("%s: down of %s", c.name, name)
			}
			downs++
			return nil
		})("shop-prod")
		if err != nil || (downs == 1) != c.down {
			t.Errorf("%s: %d downs, %v", c.name, downs, err)
		}
		for _, a := range asked {
			if !strings.Contains(a, "--filter label=com.docker.compose.project=shop-prod") {
				t.Errorf("%s: listed %q", c.name, a)
			}
		}
	}
}

// Bug: docs/config.md moves a release root by its projects' directories alone ("mv the
// projects' directories under the new root"). The claims directory (uids.DirName, under the
// root, dot-named, so a glob of the projects misses it) stays in the old root, and the new
// root's claims are made again in the order the environments start: two environments whose
// hashed slots collide swap uids, each then running as the uid that owns the other's data.
// A move done as the doc says must keep every environment's uid.
func TestReleaseRootMoveKeepsUIDs(t *testing.T) {
	t.Parallel()
	// Two environments of one project whose hashed slots collide.
	var first, second uids.Owner
	seen := map[uint32]uids.Owner{}
	for i := 0; i < 1_000_000 && first.Env == ""; i++ {
		o, err := uids.EnvOwner("shop", "", "e"+strconv.Itoa(i))
		if err != nil {
			t.Fatal(err)
		}
		if other, ok := seen[o.Plain()]; ok {
			first, second = other, o
		}
		seen[o.Plain()] = o
	}
	if first.Env == "" {
		t.Fatal("no two environments' slots collide")
	}
	tmp := t.TempDir()
	old, moved := filepath.Join(tmp, "old"), filepath.Join(tmp, "new")
	was := map[uids.Owner]uint32{}
	for _, o := range []uids.Owner{first, second} {
		write(t, filepath.Join(old, o.Project, o.Env, "releases", ".keep"), "", 0o644)
		uid, _, err := claimsStore(old).LookupPair(o)
		if err != nil {
			t.Fatal(err)
		}
		was[o] = uid
	}
	// The doc's steps: the environments stopped, the projects' directories moved under the new
	// root, root: changed, then each started again, here the second first.
	if err := os.MkdirAll(moved, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(old, "shop"), filepath.Join(moved, "shop")); err != nil {
		t.Fatal(err)
	}
	for _, o := range []uids.Owner{second, first} {
		uid, _, err := claimsStore(moved).LookupPair(o)
		if err != nil {
			t.Fatal(err)
		}
		if uid != was[o] {
			t.Errorf("%s runs as %d after the move, %d before, its data's owner", o, uid, was[o])
		}
	}
	if left, err := claimsStore(old).List(); err != nil || len(left) == 0 {
		t.Errorf("the old root's claims (%v): %d", err, len(left))
	} else {
		t.Logf("claims left in the old root: %d", len(left))
	}
}
