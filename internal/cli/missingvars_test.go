package cli

import (
	"errors"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// On a target, the fix names the environment's shared/.env, which
// --vars set writes, before the other two levels.
func TestMissingVarsFix(t *testing.T) {
	t.Parallel()
	root := project(t, "tunnel", map[string]string{
		"compose.yaml": "services:\n  app:\n    image: busybox\n    environment:\n      TOKEN: ${TOKEN:?the token}\n",
		"target.env":   "COMPOSE_PROJECT_NAME='tunnel-own-prod'\nDEVOPSY_TARGET='vm1:own/prod'\n",
	})
	p, err := loadProject(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	var failure *report.Failure
	if err := missingVars(p); !errors.As(err, &failure) {
		t.Fatalf("missingVars: %v, want a failure", err)
	}
	if len(failure.Details) != 1 || failure.Details[0] != "  TOKEN  the token" {
		t.Errorf("details: %q", failure.Details)
	}
	want := report.Fix("set them in the environment's shared/.env (--vars-instance or --vars-project for the instance's or the project's .env), then try again:", "devopsy @vm1:own/prod --vars set <KEY>...")
	if len(failure.Fixes) != 1 || failure.Fixes[0].Text != want.Text || len(failure.Fixes[0].Cmds) != 1 || failure.Fixes[0].Cmds[0] != want.Cmds[0] {
		t.Errorf("fixes: %+v\nwant: %+v", failure.Fixes, want)
	}
}
