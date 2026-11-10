//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// BUG: a project's builder, its cache mounts and its layer cache, outlives the removal of a
// key: a leaked key's build fills the cache that the project's own Dockerfile mounts, and
// the next build after the removal, the owner's release again as docs/project-users.md
// tells after a leak, runs on what the leaked key left there. The end of the user's
// processes, and the doc's steps after a leak, leave the builder as it was.
func TestReviewOpusBuildCacheOutlivesAKeysRemoval(t *testing.T) {
	user := doorUser(t, "opuscache")
	keys := func(args string) {
		t.Helper()
		if out, code := op(t, "/", "devopsy @devopsy@doorsrv: --ssh-keys --for opuscache "+args+" 2>&1"); code != 0 {
			t.Fatalf("--ssh-keys --for opuscache %s (%d): %s", args, code, out)
		}
	}
	if out, code := op(t, "/", "rm -f /tmp/opuscache /tmp/opuscache.pub && ssh-keygen -q -t ed25519 -N '' -C opuscache-leaked -f /tmp/opuscache"); code != 0 {
		t.Fatalf("a second key (%d): %s", code, out)
	}
	t.Cleanup(func() {
		doorRoot(t, "docker image rm -f devopsy.internal/opuscache-prod/app >/dev/null 2>&1; "+
			"su -s /bin/sh devopsy -c 'BUILDX_CONFIG="+doorSpace+"/buildx docker buildx rm devopsy-opuscache' >/dev/null 2>&1; "+
			"docker network rm devopsy-opuscache-build >/dev/null 2>&1; true")
	})
	keys("add /tmp/opuscache.pub")
	build := func(dockerfile string) {
		t.Helper()
		script := `set -e
mkdir -p ~/opuscache/prod/b && cd ~/opuscache/prod/b
cat >Dockerfile <<'D'
` + dockerfile + `D
printf 'services:\n  app:\n    build: .\n    user: "10001:10001"\n    security_opt: [no-new-privileges]\n' >compose.yaml
COMPOSE_PROJECT_NAME=opuscache-prod docker compose build -q`
		if out, code := doorAs(t, user, script); code != 0 {
			t.Fatalf("a build (%d):\n%s", code, out)
		}
	}
	// The leaked key's build: what the project's Dockerfile mounts as its cache (a package
	// manager's, say) now holds the attacker's file.
	build("FROM busybox:1.37\nRUN --mount=type=cache,target=/cache/tool sh -c 'echo poisoned >/cache/tool/bin'\n")
	keys("remove opuscache-leaked")
	// The owner's build after the removal, the project's own Dockerfile.
	build("FROM busybox:1.37\nRUN --mount=type=cache,target=/cache/tool sh -c 'cat /cache/tool/bin 2>/dev/null || echo clean' >/seen\n")
	out, code := doorRoot(t, "docker run --rm devopsy.internal/opuscache-prod/app cat /seen")
	if code != 0 {
		t.Fatalf("the owner's image (%d): %s", code, out)
	}
	if got := strings.TrimSpace(out); got == "poisoned" {
		t.Errorf("BUG: the owner's build after a key's removal ran on what the leaked key put in the project's cache: %q", got)
	}
}
