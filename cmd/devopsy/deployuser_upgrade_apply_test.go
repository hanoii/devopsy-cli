package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// --upgrade's apply refused as another deploy user's server warns with that refusal's own ways
// out and nothing else: never --server-setup as the login, which refuses the same way, as the
// fix of what it left or as what makes the release root.
//
// BUG (review round 3): the apply under --upgrade words every refusal as any other, so after
// the deploy user's refusal its closing warning offers "run --server-setup for this server",
// and the missing release root says "--server-setup makes it the deploy user's", a setup that
// refuses this login the same way.
func TestUpgradeRefusedAsAnotherDeployUserOffersNoSetup(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	me := currentLogin().name
	write(t, filepath.Join(host, "etc", "group"), "docker:x:990:"+me+",ops\n", 0o644)
	write(t, filepath.Join(host, "etc", "passwd"), me+":x:1000:1000::/home/"+me+":/bin/sh\nops:x:1001:1001::/home/ops:/bin/sh\n", 0o644)
	write(t, filepath.Join(host, "home", "ops", ".devopsy-uids", "env:shop:prod"), "x\n", 0o644)
	write(t, filepath.Join(filepath.Dir(s.root), "home", setup.RecordFile), "{}\n", 0o600)
	out, code := s.run([]string{remote.HostEnv + "=admin@one"}, remote.ServerSetupFlag, "apply", "--upgrade")
	if code != 0 || !strings.Contains(out, "warning: deploy user: devopsy was set up as ops on this server") {
		t.Fatalf("--upgrade's apply not refused as another deploy user's, as a warning (%d):\n%s", code, out)
	}
	for _, said := range []string{"run --server-setup for this server", "--server-setup makes it the deploy user's"} {
		if strings.Contains(out, said) {
			t.Errorf("a refused --upgrade offers a setup refused the same way, %q:\n%s", said, out)
		}
	}
}
