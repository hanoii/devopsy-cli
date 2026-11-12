package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/door"
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
