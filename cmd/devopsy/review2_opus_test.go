package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// Bug (review round 2): commands still offered in a message's words,
// locally, beside the --shell-host refusal round 1 made a fix: a server
// named alone for a command that needs an environment (runRemote's
// "(for a server: ... devopsy @<host>:<environment> <flag>)"), and
// --release-no-sync with --from ("devopsy @... --release-sync --from
// @... syncs from it", its sibling refusal on a released child a fix).
// Each should be a fix, its command after "$ ".
func TestLocalRefusalsOfferCommandsAsFixes(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	dot := filepath.Join(project, ".devopsy")
	write(t, filepath.Join(dot, "compose.yaml"), "services: {}\n", 0o644)
	write(t, filepath.Join(dot, "config.yaml"), "project: shop\nenvironments:\n  prod:\n    release: deploy\n  dev:\n    parent: prod\n    release: deploy\n", 0o644)
	env := []string{"DEVOPSY_SSH_COMMAND=false"}

	out, code := runDevopsy(t, project, env, "@devopsy@server", "--vars-instance")
	if code == 0 || strings.Contains(out, ", devopsy @devopsy@server:<environment> --vars-instance)") || !strings.Contains(out, "devopsy: | $ devopsy @devopsy@server:<environment> --vars-instance\n") {
		t.Errorf("a server named alone, its command in the words (%d):\n%s", code, out)
	}
	out, code = runDevopsy(t, project, env, "@devopsy@server:dev", "--release-no-sync", "--from", "@devopsy@server:prod")
	if code == 0 || strings.Contains(out, "--release-sync --from @devopsy@server:prod syncs from it") || !strings.Contains(out, "devopsy: | $ devopsy @devopsy@server:dev --release-sync --from @devopsy@server:prod\n") {
		t.Errorf("--release-no-sync --from, its command in the words (%d):\n%s", code, out)
	}
}
