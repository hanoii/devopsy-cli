package door

import (
	"strings"
	"testing"
)

// BUG: the end of a project user's processes (cmd/devopsy runEndUID) refuses to kill anything
// while any process on the host runs as uid 2147483646 (noProcessUID, the real and saved uid of
// its kill(-1) child), and a project's container may run as that uid: userRefused takes any
// number up to 2147483647 outside the host's ids and the project users' range. An attacker
// holding a project user's key starts a service as "2147483646:2147483646", leaves a setsid
// process of the user's on the host, and every later end (a key's removal, --project-users
// remove), its own and every other project user's, fails before its first kill, so what the
// leaked key left keeps running with the user's group, and the door with it while a key is
// left. Refusing the uid here covers a service and exec -u only: a build's RUN step takes
// any USER, so the end's own refusal is the cause (e2e: TestReviewOpusEndFailsBesideItsOwnUID).
func TestReviewOpusNoContainerTakesTheEndsUID(t *testing.T) {
	t.Parallel()
	for _, user := range []string{"2147483646:2147483646", "2147483646:10001"} {
		if why := testPolicy.userRefused(user); why == "" {
			t.Errorf("BUG: a container may run as %s, the end's own uid", user)
		}
	}
	// The control: an ordinary id is taken.
	if why := testPolicy.userRefused("10001:10001"); why != "" {
		t.Errorf("the control, 10001:10001: %s", why)
	}
	// And through a whole file, as a release's up checks it.
	file := parse(t, `{"name":"shop-prod","services":{"web":`+strings.Replace(safe(``), `"10001:10001"`, `"2147483646:2147483646"`, 1)+`}}`)
	if errs := CheckFile(file, testPolicy, "up", &fakePaths{}); len(errs) == 0 {
		t.Errorf("BUG: an up of a service as 2147483646:2147483646 is taken")
	}
}
