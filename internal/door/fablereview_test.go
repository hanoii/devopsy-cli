package door

import (
	"testing"
)

// An attack round on the door's requests, by one reviewer: what a project user learns of
// another project through the image store, which the door takes for nobody's alone.

// BUG: `docker image inspect` is answered for any image ("the image store is one, so this
// tells nothing that is anyone's alone", decideImage), but an image another project built or
// pulled holds that project's configuration: its Env (a build arg lands there as a plain
// value), Cmd, Entrypoint, WorkingDir, labels and layer digests. A project user reads them
// with `docker image inspect blog-prod-web`, or of a hold of blog's. Only an image of this
// project's names (OwnName, ownHold), or one its own file names, should be answered.
func TestFableReviewImageInspectReadsAnothersImage(t *testing.T) {
	t.Parallel()
	p := testPolicy
	p.Others = []string{"blog"}
	for _, image := range []string{"blog-prod-web", "busybox:devopsy-blog-prod-20240101000000"} {
		if a, err := Decide([]string{"image", "inspect", image}, p, testDaemon, &fakeHost{}, testKnown); err == nil {
			t.Errorf("BUG: image inspect %s, another project's image, is answered: %+v", image, a)
		}
	}
	// The control: its own built image, and its own hold.
	for _, image := range []string{"shop-prod-web", "busybox:devopsy-shop-prod-20240101000000"} {
		if _, err := Decide([]string{"image", "inspect", image}, p, testDaemon, &fakeHost{}, testKnown); err != nil {
			t.Errorf("the control: its own image %s refused: %v", image, err)
		}
	}
}

// BUG: `docker tag` checks only its destination, a hold of this project's, never its source
// (Decide's "tag"): another project's built image (blog-prod-web) or hold becomes a hold of
// this project's, which its own file then names and runs, the other project's code and
// configuration with it. A fix that checks a service's `image:` alone misses this way in:
// the source of a tag is an image this project names too, and must pass the same rule.
func TestFableReviewTagTakesAnothersImageAsItsHold(t *testing.T) {
	t.Parallel()
	p := testPolicy
	p.Others = []string{"blog"}
	for _, source := range []string{"blog-prod-web", "busybox:devopsy-blog-prod-20240101000000"} {
		argv := []string{"tag", source, "busybox:devopsy-shop-prod-20240101000000"}
		if a, err := Decide(argv, p, testDaemon, &fakeHost{}, testKnown); err == nil {
			t.Errorf("BUG: %q takes another project's image as a hold of this project's: %+v", argv, a)
		}
	}
	// The control: a tag of an image by a plain name, as a release's bookmark is made.
	argv := []string{"tag", "busybox:1.37", "busybox:devopsy-shop-prod-20240101000000"}
	if _, err := Decide(argv, p, testDaemon, &fakeHost{}, testKnown); err != nil {
		t.Errorf("the control: %q refused: %v", argv, err)
	}
}
