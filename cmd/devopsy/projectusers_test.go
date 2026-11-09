package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// The deploy user's release of a compose project under a project user's names is refused
// before its switch (--check-names): the door acts only on what it made, and would refuse
// that user's every compose command while it runs. Names are the door's (door.Policy's
// OwnName): a longer project's names are that project's.
func TestDoorNameOwner(t *testing.T) {
	t.Parallel()
	d := &remote.Door{Users: map[string]remote.DoorUser{"devopsy-shop": {Project: "shop"}, "devopsy-shop-admin": {Project: "shop-admin"}}}
	root := t.TempDir()
	for name, want := range map[string]string{
		"shop-stage":      "devopsy-shop",
		"shop-eu-prod":    "devopsy-shop",
		"shop-admin-prod": "devopsy-shop-admin",
		"blog-prod":       "",
		"shopping-prod":   "",
	} {
		if got := doorNameOwner(d, root, name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	if got := doorNameOwner(nil, root, "shop-stage"); got != "" {
		t.Errorf("no door: %q", got)
	}
}

// --check-names, the activation's check before its switch, refuses it as the deploy user.
func TestCheckNamesRefusesAProjectUsersNames(t *testing.T) {
	t.Parallel()
	home, rel := t.TempDir(), t.TempDir()
	write(t, filepath.Join(home, "config.yaml"), "releases:\n  root: "+t.TempDir()+"\ndoor:\n  space: /var/lib/devopsy-door\n", 0o644)
	write(t, filepath.Join(home, "door", "users", "devopsy-shop.yaml"), "project: shop\n", 0o644)
	write(t, filepath.Join(rel, ".devopsy", "compose.yaml"), "services: {}\n", 0o644)
	for name, refused := range map[string]bool{"shop-stage": true, "blog-prod": false} {
		// As the activation calls it: the release's compose project in its environment.
		out, code := runDevopsy(t, rel, []string{"DEVOPSY_HOME=" + home, "COMPOSE_PROJECT_NAME=" + name}, "--check-names", filepath.Dir(rel))
		if (code != 0) != refused || refused != strings.Contains(out, "is under the names of project user devopsy-shop here") {
			t.Errorf("%s (%d):\n%s", name, code, out)
		}
	}
}

// --project-users remove ends the project user's processes once its entry and keys are
// gone, so the door answers no new call meanwhile: one it left running would outlive its
// logins. A kill that fails fails the command, which says the rest is done.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the user's keys are.
func TestRemoveProjectUserEndsItsProcesses(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	entry := filepath.Join(t.TempDir(), "devopsy-shop.yaml")
	var ended []string
	end := func(fails error) func(string) error {
		return func(who string) error {
			if _, err := os.Stat(entry); err == nil {
				t.Errorf("%s's processes ended while its entry stays", who)
			}
			ended = append(ended, who)
			return fails
		}
	}
	write(t, entry, "project: shop\n", 0o600)
	if code := removeProjectUser(nil, "shop", "devopsy-shop", entry, end(nil)); code != 0 || strings.Join(ended, " ") != "devopsy-shop" {
		t.Errorf("remove (exit %d) ended %q", code, ended)
	}
	ended = nil
	write(t, entry, "project: shop\n", 0o600)
	if code := removeProjectUser(nil, "shop", "devopsy-shop", entry, end(errors.New("no docker"))); code == 0 {
		t.Error("a kill that failed passes")
	}
	ended = nil
	if code := removeProjectUser(nil, "shop", "devopsy-shop", entry, end(nil)); code == 0 || len(ended) != 0 {
		t.Errorf("no such user (exit %d) ended %q", code, ended)
	}
}
