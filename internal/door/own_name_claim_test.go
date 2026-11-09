package door

import (
	"testing"
)

// BUG: a claim writes a tag under another project's own names. An image is a project's when
// only it writes one of its names (OwnRef: a build's, under its names), and that project's
// start runs such a name as it is (startImage, by name, under its own image lock alone). A
// claim of that name by any other project (its pull, an inspect, a tag's source, a start of
// it) is `docker pull <name>` with the claimant's logins, which moves the tag in the server's
// one store to whatever the registry serves under it, and compose's pull verb does the same
// with no claim at all: Claimable knows only holds. mongo builds mongo-express:1, a Docker
// Hub name; shop names it, and mongo's next up, restart or rollback runs Docker Hub's image
// on mongo's data and secrets, its own build untagged, with nothing pulled on mongo's side.
// The other project's lock does not reach the claimant's pull. A claim must write no tag
// another project writes (pull by digest, or refuse a repository under another's names), and
// the pull verb must refuse a service image under another's names as it should a hold.
func TestClaimWritesNoNameAnotherProjectWrites(t *testing.T) {
	t.Parallel()
	p := testPolicy
	p.Others = []string{"blog"}
	for _, ref := range []string{"blog-prod-web", "blog-prod-web:latest", "blog-express:1"} {
		for _, argv := range [][]string{
			{"pull", "-q", ref},
			{"image", "inspect", ref},
			{"tag", ref, "busybox:devopsy-shop-prod-20240101000001"},
		} {
			if a, err := Decide(argv, p, testDaemon, &fakeHost{}, testKnown); err == nil {
				t.Errorf("%q: taken, with a claim that pulls over blog's name: %+v", argv, a)
			}
		}
		file := parse(t, `{"name":"shop-prod","services":{"web":`+safe(``)+`}}`)
		file["services"].(map[string]any)["web"].(map[string]any)["image"] = ref
		if errs := CheckFile(file, p, "pull", &fakePaths{}); len(errs) == 0 {
			t.Errorf("compose's pull of %s, blog's name, is taken", ref)
		}
	}
	// The controls: a name nobody on the server writes is claimed, and blog's own names
	// are blog's to pull.
	if a, err := Decide([]string{"pull", "-q", "postgres:16"}, p, testDaemon, &fakeHost{}, testKnown); err != nil || len(a.Claims) != 1 {
		t.Errorf("the control, a registry's name under nobody's: %v, %+v", err, a)
	}
	blog := Policy{Project: "blog", User: "devopsy-blog", Space: "/space", Tree: "/home/devopsy-blog/blog", Others: []string{"shop"}}
	if a, err := Decide([]string{"image", "inspect", "blog-prod-web"}, blog, testDaemon, &fakeHost{}, testKnown); err != nil || len(a.Claims) != 0 {
		t.Errorf("the control, blog's own name for blog: %v, %+v", err, a)
	}
	file := parse(t, `{"name":"blog-prod","services":{"web":`+safe(``)+`}}`)
	file["services"].(map[string]any)["web"].(map[string]any)["image"] = "blog-prod-web"
	if errs := CheckFile(file, blog, "pull", &fakePaths{}); len(errs) != 0 {
		t.Errorf("the control, compose's pull of blog's own name for blog: %v", errs)
	}
}
