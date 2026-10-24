//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// A step that exits 255 (drush does, on a PHP error) is a failed step,
// not a lost connection. Fails on v0.42.0: ssh passes the script's
// status on, and devopsy takes any 255 for its own (sshLost:
// cmd/devopsy/remote.go:380 for activations, before and after the
// switch, and cmd/devopsy/sync.go:289 for a sync's run step), so a
// failed run step that went back to the previous release reads "the
// release goes on there".
func TestStepExit255(t *testing.T) {
	dir := project(t, "exit255", map[string]string{
		".devopsy/.env":            "DEVOPSY_SERVER=server\n",
		".devopsy/config.yaml":     "project: exit255\ndefaults:\n  mode: image\n  release: {prepare: [fail prepare], run: deploy}\n  rollback: deploy\nenvironments:\n  prod: {}\n  dev: {parent: prod}\n",
		".devopsy/compose.yaml":    "services:\n  app:\n    image: busybox:1.37\n    command: [sleep, infinity]\n",
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts, or fails with 255 while ~/fail-run exists\n[ ! -e \"$HOME/fail-run\" ] || exit 255\nexec devopsy up -d --wait\n",
		".devopsy/commands/fail":   "#!/bin/sh\n## Description: fails with 255 while ~/fail-<its argument> exists\n[ ! -e \"$HOME/fail-$1\" ] || exit 255\n",
	})
	t.Cleanup(func() { srv(t, "rm -f fail-prepare fail-run") })
	for _, env := range []string{"prod", "dev"} {
		if out, code := op(t, dir, "devopsy @"+env+" --release"); code != 0 {
			t.Fatalf("%s (%d):\n%s", env, code, out)
		}
	}
	for _, c := range []struct{ fail, command string }{
		{"prepare", "devopsy @prod --release"},
		{"run", "devopsy @prod --release"},
		{"run", "devopsy @dev --sync --yes"},
	} {
		srv(t, "rm -f fail-prepare fail-run; touch fail-"+c.fail)
		out, code := op(t, dir, c.command)
		if code == 0 {
			t.Errorf("%s, %s failing: went through:\n%s", c.command, c.fail, out)
		}
		if strings.Contains(out, "connection to @") {
			t.Errorf("%s, %s exiting 255: reported as a lost connection:\n%s", c.command, c.fail, out)
		}
	}
}
