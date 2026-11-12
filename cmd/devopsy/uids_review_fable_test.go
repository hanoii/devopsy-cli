package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/testfile"
	"github.com/hanoii/devopsy-cli/internal/uids"
)

// Bug (review, fable, round 1): the door's "free" checks only the owner's project, never
// that the owner's compose name is this project's (Policy.OwnName, as downProject does).
// Compose names are flat: project shop's instance admin, environment prod, is
// shop-admin-prod, the name of project shop-admin's prod. So shop's user asks
// "free env:shop:admin:prod" and the door runs, as the deploy user,
// `docker compose -p shop-admin-prod down --volumes --remove-orphans`: another tenant's
// containers and volumes.
func TestDoorFreeDownsOnlyItsOwnName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	docker := filepath.Join(dir, "docker")
	// It fails, so nothing is freed whatever the door decides.
	if err := testfile.Write(docker, []byte("#!/bin/sh\necho \"$*\" >> "+calls+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Without the check the request reaches the server's own claims directory: leave no
	// lock file of this test's there.
	if r, err := remote.ReleaseSettings(); err == nil {
		t.Cleanup(func() {
			locks := filepath.Join(claimsStore(r.Root).Dir, "locks")
			_ = os.Remove(filepath.Join(locks, "env:shop:admin:prod"))
			_ = os.Remove(locks)
			_ = os.Remove(filepath.Dir(locks))
		})
	}
	side := &doorSide{
		policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space"), Others: []string{"shop-admin"}},
		real:   docker, env: os.Environ(),
	}
	if code := side.free("env:shop:admin:prod"); code != 2 {
		t.Errorf("freed another project's compose name (%d)", code)
	}
	if out, _ := os.ReadFile(calls); len(out) != 0 {
		t.Errorf("the door ran docker on shop-admin's name for shop's user:\n%s", out)
	}
}

// Bug (review, fable, round 1): freeUIDs takes the project's start lock exclusive whether or
// not the project's claim goes. With another environment of the project left, the claim
// stays, yet a --destroy of one environment (a pr-* one) waits for every start of its
// siblings to end (prod's long `run`, an attached `up`), and fails at its bound, after
// everything else of the environment is removed, with "the uid is not freed".
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
	if err := freeUIDs(s, pr, func(string) error { return nil }); err != nil {
		t.Errorf("pr-1's free waited for prod's start, though the project's claim stays: %v", err)
	}
	if _, ok, _ := s.Read(prod.ProjectOf()); !ok {
		t.Error("the project's claim went with prod still there")
	}
}

// Bug (review, fable, round 1): freeUIDs takes the compose name down whether or not the
// environment ever held a claim. A --destroy of an address with nothing on the server (its
// "nothing at" branch, which took no lock and asked nothing) now runs
// `docker compose -p <name> down --volumes --remove-orphans` on a flat name that may be
// another project's: `@vm1:admin/prod --destroy` in project shop, never released there,
// takes down project shop-admin's prod and its volumes.
func TestFreeUIDsDownsNothingNeverClaimed(t *testing.T) {
	t.Parallel()
	s := uids.Store{Dir: filepath.Join(t.TempDir(), ".devopsy-uids")}
	var downs []string
	err := freeUIDs(s, uids.Owner{Project: "shop", Instance: "admin", Env: "prod"}, func(name string) error {
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
