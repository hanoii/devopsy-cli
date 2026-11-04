package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// What a restore's failure after its mark promises holds at the steps that
// only just said it (moving --with-env's files, reading the landing's
// settings): its way out names the environment's real address, never the
// <target> targetAddress falls back on, and nothing starts the services it
// says stay down.
func TestRestoreFailureKeepsItsPromises(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run(nil, "@prod", "--snapshot")
	m := snapshotID.FindStringSubmatch(out)
	if code != 0 || m == nil {
		t.Fatalf("take (%d):\n%s", code, out)
	}
	restoring := filepath.Join(s.root, "shop", "prod", "shared", ".env.restoring")
	if err := os.MkdirAll(restoring, 0o755); err != nil {
		t.Fatal(err)
	}
	wayOut := regexp.MustCompile(`\$ devopsy @devopsy@one:prod --snapshots restore ` + m[1] + `\n`)
	started := regexp.MustCompile(`(?m) (start|up)( |$)`)
	said := func(what string, env []string, args ...string) {
		t.Helper()
		s.logs()
		out, code := s.run(env, append([]string{"@prod", "--snapshots", "restore", m[1], "--yes"}, args...)...)
		docker, _ := s.logs()
		if code == 0 || !wayOut.MatchString(out) {
			t.Errorf("%s (%d): no way out with the real address:\n%s", what, code, out)
		}
		if started.MatchString(docker) {
			t.Errorf("%s: the services said to stay down were started:\n%s", what, docker)
		}
	}
	said("the --with-env copy", nil, "--with-env")
	if err := os.Remove(restoring); err != nil {
		t.Fatal(err)
	}
	said("the landing's settings", []string{"FAKE_EXTRACT_BREAK=" + filepath.Join(filepath.Dir(s.root), "home", "config.yaml")})
}
