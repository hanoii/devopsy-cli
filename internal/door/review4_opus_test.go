package door

import (
	"testing"
)

// Review 4 (Opus), a failing test proving a bug: a name a project user gives the door's claim
// goes to buildx's imagetools, whose references are wider than Docker's: `oci-layout://<path>:
// <tag>` reads a local OCI layout (checked with buildx 0.37.2: `imagetools inspect --raw
// oci-layout:///tmp/lay:t` printed the layout's manifest). Claimable takes it (its "host"
// `oci-layout:` is no host of the server's, and it is no hold or build's name), so a pull, an
// inspect, a tag's source or a start's image has the deploy user's buildx read, then copy, any
// layout the deploy user can read: another project's claim in <space>/work while it loads
// (<space>/work is 0755, so its directory's name is listed; the copy's tag is always
// `claim`), or the deploy user's own files, the "registry"'s errors coming back on stderr.
// The deploy user never reads a path a project user names (internals/door.md).
func TestReview4ClaimTakesOnlyADockerReference(t *testing.T) {
	t.Parallel()
	layout := "oci-layout:///srv/space/work/blog-123/layout:claim"
	if err := testPolicy.Claimable(layout); err == nil {
		t.Errorf("Claimable takes %s", layout)
	}
	for _, argv := range [][]string{
		{"pull", "-q", layout},
		{"image", "inspect", layout},
		{"tag", layout, "busybox:devopsy-shop-prod-20240101000000"},
	} {
		if a, err := decide(t, argv...); err == nil {
			t.Errorf("%q: not refused, claims %q", argv, a.Claims)
		}
	}
}
