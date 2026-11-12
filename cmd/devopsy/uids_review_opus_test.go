package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/uids"
)

// Bug: the door's free takes the owner from the caller's words and runs
// `compose -p <its compose name> down --volumes --remove-orphans` with none of downProject's
// guards (OwnName, foreign, from /): project shop's user names env:shop:admin-prod, whose
// compose name, shop-admin-prod, is the deploy user's project shop-admin's prod, and the door
// takes another project's containers and volumes down.
//
// Not parallel: it sets HOME and DEVOPSY_HOME, which remote.ReleaseSettings reads.
func TestDoorFreeDownsOnlyItsOwnNamesReviewOpus(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	t.Setenv("DEVOPSY_HOME", filepath.Join(dir, "config"))
	// The deploy user's project shop-admin, released at prod, with its claim.
	if err := os.MkdirAll(filepath.Join(home, "shop-admin", "prod", "releases"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (uids.Store{Dir: filepath.Join(home, ".devopsy-uids")}).LookupPair(uids.Owner{Project: "shop-admin", Env: "prod"}); err != nil {
		t.Fatal(err)
	}
	calls := filepath.Join(dir, "calls")
	fake := filepath.Join(dir, "bin", "docker")
	write(t, fake, "#!/bin/sh\necho \"$*\" >>"+calls+"\n", 0o755)
	side := &doorSide{
		policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space"), Others: []string{"shop-admin"}},
		real:   fake, env: os.Environ(),
	}
	if side.policy.OwnName("shop-admin-prod") {
		t.Fatal("the control: shop-admin-prod is shop's name")
	}
	for _, owner := range []string{"env:shop:admin-prod", "env:shop:admin:prod"} {
		code := side.free(owner)
		got, _ := os.ReadFile(calls)
		if code == 0 || strings.Contains(string(got), "-p shop-admin-prod down") {
			t.Errorf("%s: the door freed (%d) and took another project's compose name down:\n%s", owner, code, got)
		}
		os.Remove(calls)
	}
}

// Bug: behind the door a container's allowed uids come from the rendered file's compose name
// (uidsOf(s.claims, name)), which the branch picks (a top-level name:, COMPOSE_PROJECT_NAME
// in its .env, which wins over devopsy-target.env), never from the request's directory as
// the plan says: a pr-1 checkout naming itself shop-prod runs as prod's uid, which any
// branch computes (Owner.Plain), so a pr-* one does run as prod.
//
// Not parallel: it sets doorLog and doorWho, and PATH (doorFakes).
func TestDoorUIDsAreTheDirectorysReviewOpus(t *testing.T) {
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
			claims: claims, vets: []remote.DoorVet{{Prefix: "vetted.", Command: vet}}, real: docker}
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

// Bug: freeUIDs takes the project's start lock exclusive whether or not the project's claim
// goes: with another environment of the project left (its claim keeps the project's), a start
// of that one holding the project's lock shared (a long `devopsy run`, a foreground up) makes
// this environment's --destroy fail at its last step, and every rerun with it.
func TestFreeUIDsWaitsOnlyForWhatItFreesReviewOpus(t *testing.T) {
	t.Parallel()
	s := busyStore(filepath.Join(t.TempDir(), ".devopsy-uids"))
	prod := uids.Owner{Project: "shop", Env: "prod"}
	pr := uids.Owner{Project: "shop", Env: "pr-1"}
	for _, o := range []uids.Owner{prod, pr} {
		if _, _, err := s.LookupPair(o); err != nil {
			t.Fatal(err)
		}
	}
	// prod's long run: both its start locks, shared.
	for _, o := range []uids.Owner{prod, prod.ProjectOf()} {
		l, err := s.LockStart(o, syscall.LOCK_SH)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
	}
	if err := freeUIDs(s, pr, func(string) error { return nil }); err != nil {
		t.Errorf("pr-1's free failed on prod's start, with prod's claim keeping the project's: %v", err)
	}
	if _, ok, _ := s.Read(prod.ProjectOf()); !ok {
		t.Error("the project's claim went with prod there")
	}
}
