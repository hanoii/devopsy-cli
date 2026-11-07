//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// BUG: the door reads an image's labels from the server's store and keeps the image in the
// file by its tag; compose, with `pull_policy: always`, pulls that tag again before it
// creates the container. So an image pushed to the registry after the door's first pull,
// carrying `devopsy.role` and a vet's prefix, runs with those labels on its container, past
// checkImageLabels and past the vet, which reads the file's labels only. The control shows
// the same labels refused when the store holds them: the check is there, and the tag is
// resolved twice.
func TestDoorImageLabelsPulledPastTheCheck(t *testing.T) {
	image := "localhost:5000/pub/app:1"
	dir := doorProject(t, "doorpull", `services:
  web:
    image: `+image+`
    pull_policy: always
    user: "10001:10001"
    security_opt: [no-new-privileges]
    command: sleep 100000
`, nil)
	if out, code := doorRoot(t, `set -e
docker run -d --name doorpull-registry --network host registry:3 >/dev/null
until curl -s -o /dev/null http://127.0.0.1:5000/v2/; do sleep 0.2; done
docker pull -q busybox:1.37 >/dev/null
docker tag busybox:1.37 `+image+`
docker push -q `+image+` >/dev/null
docker image rm `+image+` >/dev/null
`); code != 0 {
		t.Fatalf("the registry (%d): %s", code, out)
	}
	t.Cleanup(func() {
		doorRoot(t, "docker rm -f doorpull-registry doorpull-prod-web-1 >/dev/null 2>&1; docker image rm -f "+image+" doorpull-labeled >/dev/null 2>&1")
	})
	if out, code := op(t, dir, "devopsy @prod --release"); code != 0 {
		t.Fatalf("a release of the clean image (%d):\n%s", code, out)
	}
	// The registry's tag moves to an image labelling its containers; the store's stays clean.
	if out, code := doorRoot(t, `set -e
mkdir -p /tmp/doorpull && printf 'FROM busybox:1.37\nLABEL devopsy.role=hijacked\nLABEL vetted.refuse=1\n' >/tmp/doorpull/Dockerfile
docker build -q -t doorpull-labeled /tmp/doorpull >/dev/null
docker tag doorpull-labeled `+image+`
docker push -q `+image+` >/dev/null
docker tag busybox:1.37 `+image+`
`); code != 0 {
		t.Fatalf("the labelled push (%d): %s", code, out)
	}
	labels := func() string {
		out, _ := doorRoot(t, "docker inspect -f '{{index .Config.Labels \"devopsy.role\"}} {{index .Config.Labels \"vetted.refuse\"}}' doorpull-prod-web-1")
		return strings.TrimSpace(out)
	}
	// As the project user would, on its own release: the file as committed, by tag.
	up := "cd ~/doorpull/prod/current/.devopsy && COMPOSE_PROJECT_NAME=doorpull-prod docker compose up -d --force-recreate 2>&1"
	out, code := doorAs(t, "devopsy-doorpull", up)
	if code == 0 || !strings.Contains(out, "refused") {
		t.Errorf("an up that pulls the tag again was not refused (%d):\n%s", code, out)
	}
	if got := labels(); got != "" {
		t.Errorf("the container carries the labels the registry gave it: %q", got)
	}
	// The control: the same image, in the store under the tag, is refused by the check.
	if out, code := doorRoot(t, "docker tag doorpull-labeled "+image); code != 0 {
		t.Fatal(out)
	}
	if out, code := doorAs(t, "devopsy-doorpull", up); code == 0 || !strings.Contains(out, "labels its containers") {
		t.Errorf("the control: a labelled image in the store was not refused (%d):\n%s", code, out)
	}
}
