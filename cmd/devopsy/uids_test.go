package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/uids"
)

// What an environment's containers may run as behind the door: the uid of each of the
// project's owners whose compose name the file has (an instance admin's prod and an
// environment admin-prod are both shop-admin-prod, one tenant's) and each one's project uid,
// never another environment's, and nothing when no claim holds the name.
func TestUIDsOfAComposeName(t *testing.T) {
	t.Parallel()
	claims := []uids.Claim{
		{Owner: uids.Owner{Project: "shop", Instance: "admin", Env: "prod"}, UID: 2000000001},
		{Owner: uids.Owner{Project: "shop", Env: "admin-prod"}, UID: 2000000002},
		{Owner: uids.Owner{Project: "shop", Env: "prod"}, UID: 2000000003},
		{Owner: uids.Owner{Project: "shop", Instance: "admin"}, UID: 1950000001},
		{Owner: uids.Owner{Project: "shop"}, UID: 1900000001},
	}
	got := uidsOf(claims, "shop-admin-prod")
	slices.Sort(got)
	if want := []uint32{1900000001, 1950000001, 2000000001, 2000000002}; !slices.Equal(got, want) {
		t.Errorf("shop-admin-prod: %v, want %v", got, want)
	}
	if got := uidsOf(claims, "shop-prod"); !slices.Equal(got, []uint32{2000000003, 1900000001}) {
		t.Errorf("shop-prod: %v", got)
	}
	if got := uidsOf(claims, "shop-staging"); len(got) != 0 {
		t.Errorf("an environment with no claim: %v", got)
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
	if err := freeUIDs(s, prod, down); err != nil {
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
	if err := freeUIDs(s, staging, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Read(prod.ProjectOf()); ok {
		t.Error("the project's claim left with its last environment gone")
	}
	// A failed down frees nothing.
	if _, _, err := s.LookupPair(prod); err != nil {
		t.Fatal(err)
	}
	if err := freeUIDs(s, prod, func(string) error { return errors.New("no daemon") }); err == nil {
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
	if err := os.MkdirAll(side.policy.Mnt("shop-prod"), 0o700); err != nil {
		t.Fatal(err)
	}
	if code := side.free("env:shop:prod"); code != 2 {
		t.Errorf("freed with its data there (%d)", code)
	}
}
