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

// BUG: PermitUserRC no closes one of the home's login hooks, ~/.ssh/rc, while the login shell's
// ~/.profile, as much the project user's to write, runs at the next login of a key kept: a
// leaked key writes it, its removal leaves it, and --shell-host, which execs a login shell
// (remote.ShellScript's `sh -l`), runs it for the owner, as would a plain `ssh` with no
// command. The doc's steps after a leak reset the home last, after a release logs in again.
func TestReviewOpusProfileRunsAtAKeptKeysLogin(t *testing.T) {
	user := doorUser(t, "opusprofile")
	keys := func(args string) {
		t.Helper()
		if out, code := op(t, "/", "devopsy @devopsy@doorsrv: --ssh-keys --for opusprofile "+args+" 2>&1"); code != 0 {
			t.Fatalf("--ssh-keys --for opusprofile %s (%d): %s", args, code, out)
		}
	}
	if out, code := op(t, "/", "rm -f /tmp/opusprofile /tmp/opusprofile.pub && ssh-keygen -q -t ed25519 -N '' -C opusprofile-leaked -f /tmp/opusprofile"); code != 0 {
		t.Fatalf("a second key (%d): %s", code, out)
	}
	mark := "/tmp/opusprofile-ran"
	t.Cleanup(func() { doorRoot(t, "rm -f "+mark+"; sed -i '/opusprofile-ran/d' ~"+user+"/.profile") })
	keys("add /tmp/opusprofile.pub")
	// The leaked key's shell.
	if out, code := doorAs(t, user, "echo 'touch "+mark+"' >>~/.profile"); code != 0 {
		t.Fatalf("its ~/.profile (%d): %s", code, out)
	}
	keys("remove opusprofile-leaked")
	// The owner's way in with a key kept: --shell-host's login shell.
	if out, code := op(t, "/", `ssh -o BatchMode=yes -T `+user+`@doorsrv 'exec "${SHELL:-/bin/sh}" -l' </dev/null`); code != 0 {
		t.Fatalf("the owner's login (%d): %s", code, out)
	}
	if _, code := doorRoot(t, "test -e "+mark); code == 0 {
		t.Error("BUG: a removed key's ~/.profile ran at the login of a key kept")
	}
}
