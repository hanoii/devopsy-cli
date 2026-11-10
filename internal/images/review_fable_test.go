package images

import "testing"

// BUG (review, round 1): registry-1.docker.io is a registry host like any other to Docker's
// naming (distribution/reference folds only index.docker.io into docker.io, and adds library/
// only under docker.io), and Canonical and Familiar (repoKey) fold it into Docker Hub's short
// names. A claim of registry-1.docker.io/library/busybox then loads its copy under
// docker.io/library/busybox@<digest>, hears `Loaded image: busybox@<digest>` as it expects,
// and fails at its inspect of registry-1.docker.io/library/busybox@<digest>, a name nothing
// made; Familiar no longer compares equal to what Docker shows for such a name, which it did
// before repoKey took HubDomain.
func TestReviewFableRegistry1IsNoShortName(t *testing.T) {
	t.Parallel()
	const d = "@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	for ref, want := range map[string]string{
		"registry-1.docker.io/library/busybox" + d: "registry-1.docker.io/library/busybox" + d,
		"registry-1.docker.io/busybox:1.37":        "registry-1.docker.io/busybox:1.37",
	} {
		if got := Canonical(ref); got != want {
			t.Errorf("Canonical(%s): %s, want %s", ref, got, want)
		}
		if got := Familiar(ref); got != want {
			t.Errorf("Familiar(%s): %s, want %s", ref, got, want)
		}
	}
}
