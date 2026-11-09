package door

import "testing"

// BUG: an image inspect of an image this project owns by its id (claimed, or named by a hold
// of its own) goes to Docker by the name asked, which the door read once to find that id and
// Docker then reads again: the name is a registry's, anyone's on the server to move in
// between (the deploy user's build of it, another project's pull of a tag its registry
// moved), and the answer is then the configuration (Env, build arguments) of an image this
// project never claimed. A tag's source takes the id the door read for exactly this
// (ownedImage); the inspect, with the same check, does not.
func TestInspectOfAClaimedImageGoesByTheIdRead(t *testing.T) {
	t.Parallel()
	p := testPolicy
	p.Images = []string{"sha256:eee"}
	// The control: a tag's source, owned the same way, goes by the id.
	a, err := Decide([]string{"tag", "postgres:16", "busybox:devopsy-shop-prod-20240101000001"}, p, testDaemon, &fakeHost{}, testKnown)
	if err != nil || len(a.Docker) != 3 || a.Docker[1] != "sha256:eee" {
		t.Fatalf("the control: a tag of a claimed image tags %+v (%v)", a, err)
	}
	a, err = Decide([]string{"image", "inspect", "--format", "{{json .}}", "postgres:16"}, p, testDaemon, &fakeHost{}, testKnown)
	if err != nil || len(a.Claims) != 0 {
		t.Fatalf("an inspect of a claimed image: %+v (%v)", a, err)
	}
	if got := a.Docker[len(a.Docker)-1]; got != "sha256:eee" {
		t.Errorf("an inspect of a claimed image reads %q again, not the id the door read, sha256:eee: %v", got, a.Docker)
	}
}
