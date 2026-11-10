//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// BUG: a container behind the door runs under the deploy user's own group. The ids no
// container may take (hostIDs) hold the deploy user's by os.Getuid and os.Getgid, and since
// the door runs under its own group through sudo's -g (devopsy-door), os.Getgid is that
// group's: the deploy user's primary group is no longer among them. Where its gid is also
// its uid's number, as useradd gives them by default and as this server's deploy user has
// them, the uid still refuses it; where they differ (a deploy user made in a group of
// another id), `user: "10001:<its gid>"` passes the rules, against internals/door.md ("never
// 0, the deploy user's ids") and docs/project-users.md ("an id of the host's own users and
// groups (the deploy user, ...)").
//
// A service's user holds neither of the deploy user's ids, its uid or its primary group's,
// whatever their numbers: each is refused before anything starts, as a host id. The deploy
// user has a primary group of another id for the test, put back after it (a leftover the
// lane's reset does not know: one for resetLane once this test moves beside the door's).
func TestDoorContainerTakesNeitherOfTheDeployUsersIDs(t *testing.T) {
	compose := func(user string) string {
		return `services:
  web:
    image: busybox:1.37
    user: "` + user + `"
    security_opt: [no-new-privileges]
    command: [sleep, infinity]
`
	}
	dir := doorProject(t, "doorgid", compose("10001:10001"), nil)
	const group, gid = "doorgid-deploy", "4321"
	t.Cleanup(func() {
		doorRoot(t, "docker rm -f doorgid-prod-web-1 >/dev/null 2>&1; usermod -g devopsy devopsy; groupdel "+group)
	})
	if out, code := doorRoot(t, "groupadd -g "+gid+" "+group+" && usermod -g "+group+" devopsy"); code != 0 {
		t.Fatalf("the deploy user's group of another id (%d): %s", code, out)
	}
	out, code := doorRoot(t, "id -u devopsy; id -g devopsy")
	ids := strings.Fields(out)
	if code != 0 || len(ids) != 2 || ids[1] != gid || ids[0] == gid {
		t.Fatalf("the deploy user's ids (%d): %s", code, out)
	}
	// Its uid first: the control, which shows where the rule answers and what it says.
	for _, c := range []struct{ what, user string }{
		{"the deploy user's uid", ids[0] + ":10001"},
		{"the deploy user's primary group", "10001:" + ids[1]},
	} {
		op(t, dir, `printf '%s' `+quote(compose(c.user))+` >.devopsy/compose.yaml`)
		out, code := op(t, dir, "devopsy @prod --release")
		if code == 0 || !strings.Contains(out, "a host id") {
			t.Errorf("a service as %s, %s, was not refused as a host id (%d):\n%s", c.what, c.user, code, out)
		}
	}
	if out, _ := doorRoot(t, "docker ps -q --filter name=doorgid-prod-web-1"); strings.TrimSpace(out) != "" {
		got, _ := doorRoot(t, "docker inspect -f '{{.Config.User}}' doorgid-prod-web-1")
		t.Errorf("a container of the project runs as %s", strings.TrimSpace(got))
	}
}
