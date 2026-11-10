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
	// A project in the release root holds its names too, the deploy user's own: one whose
	// name extends a project user's is not that user's.
	if err := os.Mkdir(filepath.Join(root, "shop-eu"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"shop-eu-prod": "", "shop-eur-prod": "devopsy-shop", "shop-stage": "devopsy-shop"} {
		if got := doorNameOwner(d, root, name); got != want {
			t.Errorf("%s, beside the release root's shop-eu: %q, want %q", name, got, want)
		}
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
// logins. What the door kept for it goes too (its registry logins, its claimed images'
// record), another project's staying. A kill that fails fails the command, which says the
// rest is done.
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
	// Its keys and its sync keys go with its entry: it logs in no more, by either.
	keys, syncKeys := remote.DoorKeysFile("devopsy-shop"), remote.DoorSyncKeysFile("devopsy-shop")
	write(t, keys, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g op@laptop\n", 0o600)
	write(t, syncKeys, "a sync's key\n", 0o600)
	other := remote.DoorKeysFile("devopsy-blog")
	write(t, other, "another's keys\n", 0o600)
	space := t.TempDir()
	logins, record := filepath.Join(space, "logins", "shop", "config.json"), filepath.Join(space, "images", "devopsy-shop")
	theirs := filepath.Join(space, "logins", "blog", "config.json")
	for _, f := range []string{logins, record, theirs} {
		write(t, f, "{}\n", 0o600)
	}
	if code := removeProjectUser(&remote.Door{Space: space}, "shop", "devopsy-shop", entry, end(nil)); code != 0 || strings.Join(ended, " ") != "devopsy-shop" {
		t.Errorf("remove (exit %d) ended %q", code, ended)
	}
	if fileExists(logins) || fileExists(record) || !fileExists(theirs) {
		t.Errorf("after remove: its logins %v, its images' record %v, another's logins %v", fileExists(logins), fileExists(record), fileExists(theirs))
	}
	if fileExists(entry) || fileExists(keys) || fileExists(syncKeys) || !fileExists(other) {
		t.Errorf("after remove: its entry %v, its keys %v, its sync keys %v, another's keys %v", fileExists(entry), fileExists(keys), fileExists(syncKeys), fileExists(other))
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

// --project-users add closes the deploy user's projects in the release root to everyone else:
// a project user enters the root to reach its own tree when it is not the deploy user's home,
// and what a project made before the server had project users (with the session's umask) is
// then that user's to read. Only a project's directory (none of a home's own, .cache or
// .config, nor the door's space) that the deploy user owns and others may enter changes, and
// add says which.
func TestCloseDeployProjects(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	space := filepath.Join(root, "door")
	for name, mode := range map[string]os.FileMode{"shop": 0o775, "blog": 0o755, "closed": 0o700, ".cache": 0o755, "door": 0o755} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(root, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, "note"), "a file\n", 0o644)
	if closed, err := closeDeployProjects(root, space, os.Getuid()+1); err != nil || len(closed) != 0 {
		t.Errorf("another user's directories closed: %q (%v)", closed, err)
	}
	closed, err := closeDeployProjects(root, space, os.Getuid())
	if err != nil || strings.Join(closed, " ") != "blog shop" {
		t.Fatalf("closed %q (%v)", closed, err)
	}
	for _, name := range []string{"shop", "blog", "closed"} {
		if fi, err := os.Stat(filepath.Join(root, name)); err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v (%v)", name, fi.Mode(), err)
		}
	}
	for _, name := range []string{".cache", "door"} {
		if fi, err := os.Stat(filepath.Join(root, name)); err != nil || fi.Mode().Perm() != 0o755 {
			t.Errorf("%s, no project, closed: %v (%v)", name, fi.Mode(), err)
		}
	}
	if fi, _ := os.Stat(filepath.Join(root, "note")); fi.Mode().Perm() != 0o644 {
		t.Errorf("a file changed: %v", fi.Mode())
	}
}

// A project user whose Unix user is gone already runs nothing to end: its removal goes on to
// its entry and keys rather than failing on the lookup.
func TestEndProcessesOfAUserGone(t *testing.T) {
	t.Parallel()
	if err := endProcesses("devopsy-nosuchuser-zz9"); err != nil {
		t.Errorf("a user gone: %v", err)
	}
}
