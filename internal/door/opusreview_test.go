package door

import (
	"slices"
	"testing"
)

// An attack round on what crosses projects, by one reviewer.

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
