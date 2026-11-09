//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// BUG: a registry's tag shaped as a hold of the claimant's own
// (<registry>/evil/x:devopsy-<its compose name>-1) is a name the door takes as "only this
// project writes" (door.Policy.OwnRef), yet a pull writes it. The registry serves only a
// manifest that names another's private image (its digests, from a CI log), Docker's pull
// fetches no blob it holds, and the name lands on that image:
//
//   - through the door's own pull, whose claim's proof fails and refuses, after its docker
//     pull wrote the name;
//   - through compose's pull verb, which claims and proves nothing.
//
// The name being the project's own, an image inspect then answers with no claim: the other's
// configuration (Env) is read, and a start would run it. The same registry and manifest
// under a plain tag are TestDoorClaimProvesTheContent, refused.
func TestDoorPulledHoldProvesNothing(t *testing.T) {
	user := doorUser(t, "doorholdp")
	reg := doorRegistry(t, "doorholdp-reg", "5013", false)
	hold := reg + "/evil/x:devopsy-doorholdp-prod-1"
	t.Cleanup(func() {
		doorRoot(t, "docker image rm -f doorholdpv-prod-web:1 "+hold+" >/dev/null 2>&1; true")
	})
	// Another's private image: the deploy user's build here, as in TestDoorClaimProvesTheContent.
	if out, code := doorRoot(t, `set -e
d=$(mktemp -d)
printf 'FROM busybox:1.37\nENV SECRET=s3cret-image\n' >"$d/Dockerfile"
docker build -q -t doorholdpv-prod-web:1 "$d" >/dev/null
docker tag doorholdpv-prod-web:1 `+hold+`
docker push -q `+hold+` >/dev/null
docker image rm `+hold+` >/dev/null
docker exec doorholdp-reg sh -c 'for f in $(find /var/lib/registry -path "*/blobs/*" -name data); do grep -q schemaVersion "$f" || { rm "$f"; echo gone; }; done'
`); code != 0 || strings.Count(out, "gone") < 2 {
		// The control: the registry keeps manifests alone, its configuration and layers gone.
		t.Fatalf("the registry (%d): %s", code, out)
	}
	inspect := "docker image inspect --format '{{json .Config.Env}}' " + hold + " 2>&1"
	// The control: the name is not here until a pull writes it.
	if out, code := doorAs(t, user, inspect); code == 0 {
		t.Fatalf("the control: the hold is here before any pull: %s", out)
	}
	// The door's own pull: its claim proves, and refuses, but the name stays.
	pulled, _ := doorAs(t, user, "docker pull -q "+hold+" 2>&1")
	if out, code := doorAs(t, user, inspect); code == 0 || strings.Contains(out, "s3cret-image") {
		t.Errorf("after the door's pull of a hold-shaped name (%s), another's image is this project's (%d):\n%s", strings.TrimSpace(pulled), code, out)
	}
	doorRoot(t, "docker image rm "+hold+" >/dev/null 2>&1; true")
	// Compose's pull verb: no claim, no proof.
	compose := `cat >/tmp/doorholdp.json <<'J'
{"name":"doorholdp-prod","services":{"web":{"image":"HOLD","user":"10001:10001","security_opt":["no-new-privileges"]}}}
J
mkdir -p ~/doorholdp/prod
sudo -n -u devopsy /usr/local/bin/devopsy --door compose ~/.docker/config.json /tmp/doorholdp.json '' ~/doorholdp/prod pull 2>&1`
	pulled, _ = doorAs(t, user, strings.ReplaceAll(compose, "HOLD", hold))
	if out, code := doorAs(t, user, inspect); code == 0 || strings.Contains(out, "s3cret-image") {
		t.Errorf("after compose's pull of a hold-shaped name (%s), another's image is this project's (%d):\n%s", strings.TrimSpace(pulled), code, out)
	}
}
