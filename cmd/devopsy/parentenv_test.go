package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A child on another server gets its parent's server's instance and
// project levels as its shared/parent.env, which names where from:
// written by a sync from its source, a secret new in it named; refreshed
// by a release that does not sync and a reload, from the address it
// names (a sync --from included), never from what parent: resolves to.
func TestParentEnvAcrossServers(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	config := filepath.Join(s.dot, "config.yaml")
	write(t, config, "secrets: [DB_PASSWORD]\n"+readString(config), 0o644)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@solo", "--release"); code != 0 {
		t.Fatalf("solo's release (%d):\n%s", code, out)
	}
	// Both fake servers share one release root: the project's .env is
	// the parent's server's here.
	write(t, filepath.Join(s.root, "shop", ".env"), "PROJ=one\nDB_PASSWORD=pw\n", 0o600)
	agent := []string{"DEVOPSY_SERVER_PROD=devopsy@one", "SSH_AUTH_SOCK=/tmp/agent.sock"}
	copied := filepath.Join(s.root, "shop", "dev", "shared", "parent.env")

	out, code := s.run(agent, "@devopsy@two:dev", "--release")
	if code != 0 || !strings.Contains(out, "copied 2 variables of @devopsy@one:prod's instance and project .env into @devopsy@two:dev's shared/parent.env") ||
		!strings.Contains(out, "warning: @devopsy@two:dev's shared/parent.env now holds DB_PASSWORD (in secrets:), copied from @devopsy@one:prod's server") {
		t.Fatalf("first release (%d):\n%s", code, out)
	}
	if got := readString(copied); got != "# devopsy: copied from devopsy@one:prod's instance and project .env\nDB_PASSWORD='pw'\nPROJ='one'\n" {
		t.Errorf("parent.env:\n%s", got)
	}
	// What the child's own server says wins over the copy: here its
	// project's .env is the same file.
	if out := getSays(t, s, "@devopsy@two:dev", "PROJ"); !strings.Contains(out, "PROJ: the value from the project's .env") {
		t.Errorf("get of a value its server sets too:\n%s", out)
	}

	// A release that does not sync copies again; the secret is no news.
	write(t, filepath.Join(s.root, "shop", ".env"), "PROJ=two\nDB_PASSWORD=pw\n", 0o600)
	out, code = s.run(agent, "@devopsy@two:dev", "--release")
	if code != 0 || !strings.Contains(out, "copied 2 variables of @devopsy@one:prod's") || strings.Contains(out, "now holds") || !strings.Contains(readString(copied), "PROJ='two'\n") {
		t.Errorf("a release that does not sync (%d):\n%s\n%s", code, out, readString(copied))
	}

	// A sync from elsewhere names its source, which a reload follows.
	out, code = s.run(agent, "@devopsy@two:dev", "--sync", "--from", "@devopsy@one:solo", "--yes")
	if code != 0 || !strings.HasPrefix(readString(copied), "# devopsy: copied from devopsy@one:solo's instance and project .env\n") {
		t.Fatalf("sync --from (%d):\n%s\n%s", code, out, readString(copied))
	}
	if out, code := s.run(agent, "@devopsy@two:dev", "--reload"); code != 0 || !strings.Contains(out, "copied 2 variables of @devopsy@one:solo's") {
		t.Errorf("reload (%d):\n%s", code, out)
	}

	// A sync from its own server: the copy goes, so no release follows
	// its reference elsewhere again.
	if out, code := s.run(agent, "@devopsy@one:dev", "--sync", "--yes"); code != 0 || strings.Contains(out, "copied") {
		t.Fatalf("a sync on one server (%d):\n%s", code, out)
	}
	if _, err := os.Stat(copied); err == nil {
		t.Errorf("parent.env stays after a sync from the same server:\n%s", readString(copied))
	}
}

// getSays is the stderr of --vars get key on addr, which says where its
// value comes from.
func getSays(t *testing.T, s *syncSetup, addr, key string) string {
	t.Helper()
	_, stderr, code := s.runIO(nil, nil, addr, "--vars", "get", key)
	if code != 0 {
		t.Fatalf("get %s (%d):\n%s", key, code, stderr)
	}
	return stderr
}
