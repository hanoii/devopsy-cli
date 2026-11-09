//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// BUG: a claim takes another's image by its digests alone. Docker's pull fetches nothing it
// has already: a manifest naming a config (and layers) already on disk lands on that image
// with no blob served. So a registry the claimant runs, serving only a manifest that names
// another project's image, "pulls" it, and the door records that image as the claimant's
// (doorSide.claim): its configuration (Env, build arguments) then shows to it, and its
// starts may run it. The pull proves only that a registry answered with a manifest, never
// that the claimant may read the content. Here the manifest is a real push whose blobs are
// then deleted from the registry, which is what such a registry serves knowing only the
// digests; the control is that the registry serves none of the image's content.
func TestDoorClaimNeedsTheContent(t *testing.T) {
	user := doorUser(t, "doorclaim")
	t.Cleanup(func() {
		doorRoot(t, "docker rm -f doorclaim-reg >/dev/null 2>&1; docker image rm -f doorclaimv-prod-web:1 127.0.0.1:5002/evil/x:1 >/dev/null 2>&1; true")
	})
	// Another's private image: the deploy user's build here, which the door holds apart from
	// a project user as it does another project's.
	if out, code := doorRoot(t, `set -e
d=$(mktemp -d)
printf 'FROM busybox:1.37\nENV SECRET=s3cret-image\n' >"$d/Dockerfile"
docker build -q -t doorclaimv-prod-web:1 "$d" >/dev/null
docker run -d --name doorclaim-reg --network host -e REGISTRY_HTTP_ADDR=127.0.0.1:5002 -e REGISTRY_STORAGE_DELETE_ENABLED=true registry:3 >/dev/null
until curl -s -o /dev/null http://127.0.0.1:5002/v2/; do sleep 0.2; done
docker tag doorclaimv-prod-web:1 127.0.0.1:5002/evil/x:1
docker push -q 127.0.0.1:5002/evil/x:1 >/dev/null
docker image rm 127.0.0.1:5002/evil/x:1 >/dev/null
docker exec doorclaim-reg sh -c 'for f in $(find /var/lib/registry -path "*/blobs/*" -name data); do grep -q schemaVersion "$f" || { rm "$f"; echo gone; }; done'
`); code != 0 || strings.Count(out, "gone") < 2 {
		// The control: the registry keeps manifests alone, its configuration and layers gone.
		t.Fatalf("the registry (%d): %s", code, out)
	}
	id, _ := doorRoot(t, "docker image inspect -f '{{.Id}}' doorclaimv-prod-web:1")
	id = strings.TrimSpace(id)
	out, code := doorAs(t, user, "docker image inspect --format '{{json .Config.Env}}' 127.0.0.1:5002/evil/x:1 2>&1")
	if code == 0 || strings.Contains(out, "s3cret-image") {
		t.Errorf("a registry serving only a manifest made another's image this project's (%d):\n%s", code, out)
	}
	if out, _ := doorRoot(t, "cat "+doorSpace+"/images/"+user+" 2>/dev/null"); strings.Contains(out, id) {
		t.Errorf("the door recorded another's image %s as %s's", id, user)
	}
}
