package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/uids"
)

// Bug (review, fable, round 2): behind the door a container's uid now follows the request's
// directory (dirOwners), while what frees that uid still takes containers down by the
// environment's compose name alone (freeUIDs: down(env.ComposeName())), and the door holds a
// file's name only to the project's (OwnName). So a file in prod's directory under any other
// name of the project (shop-other, or a sibling's shop-pr-1) starts containers as prod's uid
// that prod's --destroy never takes down: its down and its free's down go by shop-prod, the
// claim is freed with them still running, and the next owner whose slot it is (another
// tenant's environment, whose plain hash anyone computes and a tenant's own environment name
// can be picked to match) runs on a uid a foreign container holds. Before round 1's fix the
// uid followed the name, so the name's down took every container of the uid.
//
// Not parallel: it sets doorLog and doorWho, and PATH (doorFakes).
func TestDoorHoldsAUIDsContainersToItsNameReview2Fable(t *testing.T) {
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
			claims: claims, vets: []remote.DoorVet{{Prefix: "vetted.", Command: vet}}, real: docker}
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
