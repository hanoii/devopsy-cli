package door

import (
	"slices"
	"testing"
)

// An attack round on what crosses projects, by one reviewer.

// BUG: an image by name is shared, so a project's file runs another project's code: what
// another builds (compose's <compose name>-<service>, its tag that project's alone), a hold
// of another's (<repo>:devopsy-<compose name>-<id>), or any image by its id. Each is
// another project's, and a start should refuse it as a build refuses writing it.
func TestOpusReviewCheckFileRunsAnothersImage(t *testing.T) {
	t.Parallel()
	p := testPolicy
	p.Others = []string{"blog"}
	for _, image := range []string{
		"blog-prod-web",
		"blog-prod-web:latest",
		"registry.example.com/app:devopsy-blog-prod-20260101000000",
		"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	} {
		file := parse(t, `{"name":"shop-prod","services":{"web":{"image":"`+image+`","user":"10001:10001","security_opt":["no-new-privileges"]}}}`)
		if errs := CheckFile(file, p, "up", &fakePaths{}); len(errs) == 0 {
			t.Errorf("BUG: a start runs another project's image %s", image)
		}
	}
}

// BUG: read_all hands a project user `docker inspect` of any container as it is, whose
// Config.Env holds another project's secrets (a database password, a token). No template
// uses it; what it is for (a proxy's listing) needs labels and addresses, never an
// environment. An inspect of another's container should be cut as a listing is.
func TestOpusReviewReadAllInspectsAnothersEnvironment(t *testing.T) {
	t.Parallel()
	p := testPolicy
	p.ReadAll = true
	a, err := Decide([]string{"inspect", "victim"}, p, testDaemon, &fakeHost{}, testKnown)
	if err == nil && slices.Equal(a.Docker, []string{"inspect", "victim"}) {
		t.Error("BUG: read_all inspects another project's container whole, its environment and secrets included")
	}
}

// BUG: network_mode: bridge puts a service on docker0, which every door project with it
// shares (Docker's default bridge lets its containers talk), with no name of the project's:
// one project's container reaches another's on any port there. The rules should take none
// but the project's own networks (or none).
func TestOpusReviewCheckFileTakesTheDefaultBridge(t *testing.T) {
	t.Parallel()
	file := parse(t, `{"name":"shop-prod","services":{"web":`+safe(`,"network_mode":"bridge"`)+`}}`)
	if errs := CheckFile(file, testPolicy, "up", &fakePaths{}); len(errs) == 0 {
		t.Error("BUG: a service joins docker0, which other projects' services share")
	}
}
