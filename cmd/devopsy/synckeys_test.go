package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// A sync's key goes in beside the file's other lines, one per sync: a second for one sync
// is refused, the keys whose expiry passed go with the next add, and remove drops only that
// sync's, a removal of none being no error (what settles a cut sync removes whatever is
// left). Every other line stays as it was.
func TestEditSyncKeys(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), ".ssh", "authorized_keys")
	_, pub, err := remote.NewSyncKey()
	if err != nil {
		t.Fatal(err)
	}
	blob := strings.Fields(pub)[1]
	admin := "ssh-ed25519 " + blob + " admin@laptop"
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("# a comment\n"+admin+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	add := func(id string, at time.Time) error {
		return editSyncKeys(file, "/usr/local/bin/devopsy", []string{"add", id, "/srv/shop/stage", "vm2:stage", "-", blob}, at, nil)
	}
	if err := add("1762600000.000001", now.Add(-remote.SyncKeyLife-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Another environment's sync key stays; an earlier sync's into this
	// one goes with the next add, its holder gone.
	if err := editSyncKeys(file, "/usr/local/bin/devopsy", []string{"add", "1762600000.000009", "/srv/shop/prod", "vm2:prod", "-", blob}, now, nil); err != nil {
		t.Fatal(err)
	}
	if err := add("1762600000.000003", now); err != nil {
		t.Fatal(err)
	}
	if err := add("1762600000.000002", now); err != nil {
		t.Fatal(err)
	}
	if err := editSyncKeys(file, "", []string{"remove", "1762600000.000009"}, now, nil); err != nil {
		t.Fatal(err)
	}
	if err := add("1762600000.000002", now); err == nil || !strings.Contains(err.Error(), "has a key already") {
		t.Fatalf("a second key for one sync: %v", err)
	}
	got, _ := os.ReadFile(file)
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	if len(lines) != 3 || lines[0] != "# a comment" || lines[1] != admin || !strings.HasSuffix(lines[2], " devopsy-sync-1762600000.000002") {
		t.Fatalf("after the adds:\n%s", got)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	for _, id := range []string{"1762600000.000002", "1762600000.000002"} {
		if err := editSyncKeys(file, "", []string{"remove", id}, now, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := os.ReadFile(file); string(got) != "# a comment\n"+admin+"\n" {
		t.Fatalf("after remove:\n%s", got)
	}
	for _, bad := range [][]string{{"add", "x", "/srv/shop/stage", "vm2:stage", "-", blob}, {"add", "1762600000.000003", "/srv/shop/stage", "vm2:stage", "maybe", blob}, {"remove", `1" x`}, {"list"}} {
		if err := editSyncKeys(file, "/usr/local/bin/devopsy", bad, now, nil); err == nil {
			t.Errorf("%q: taken", bad)
		}
	}
}

// The door writes a project user's sync key into its sync keys, apart from its keys, only
// for an environment in that user's tree; --authorized-keys serves both, the sync key's
// quoted options whole. A request its user's keys changed under adds none: a key removal
// (--ssh-keys --for remove) ends what the user left running, then drops its sync keys, which
// a leaked key could have added, and a door already past its sudo would add one after.
//
// Not parallel: it sets DEVOPSY_HOME (the door's directory) and PATH (doorFakes).
func TestDoorSyncKeys(t *testing.T) {
	doorFakes(t)
	home := t.TempDir()
	t.Setenv("DEVOPSY_HOME", home)
	tree := filepath.Join(t.TempDir(), "shop")
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Tree: tree, Space: t.TempDir()}}
	_, pub, err := remote.NewSyncKey()
	if err != nil {
		t.Fatal(err)
	}
	blob := strings.Fields(pub)[1]
	keys := remote.DoorKeysFile("devopsy-shop")
	if err := os.MkdirAll(filepath.Dir(keys), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keys, []byte("ssh-ed25519 "+blob+" admin@laptop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if side.keysAt, err = os.Stat(keys); err != nil {
		t.Fatal(err)
	}
	if code := side.syncKey([]string{"add", "1762600000.000001", "/srv/other/prod", "vm2:prod", "-", blob}); code == 0 {
		t.Fatal("a sync key outside the caller's tree")
	}
	if code := side.syncKey([]string{"add", "1762600000.000001", tree + "/stage", "vm2:stage", "-", blob}); code != 0 {
		t.Fatalf("add: %d", code)
	}
	// The keys replaced, as a removal replaces them: a request from before adds nothing.
	if err := os.WriteFile(keys+".tmp", []byte("ssh-ed25519 "+blob+" admin@laptop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(keys+".tmp", keys); err != nil {
		t.Fatal(err)
	}
	if code := side.syncKey([]string{"add", "1762600000.000002", tree + "/prod", "vm2:prod", "-", blob}); code == 0 || strings.Contains(readString(remote.DoorSyncKeysFile("devopsy-shop")), "1762600000.000002") {
		t.Errorf("a sync key added after the user's keys changed (%d)", code)
	}
	var out bytes.Buffer
	if code := printAuthorizedKeys(&out, keys); code != 0 {
		t.Fatal(code)
	}
	if code := printAuthorizedKeys(&out, remote.DoorSyncKeysFile("devopsy-shop")); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(out.String(), "admin@laptop\n") || !strings.Contains(out.String(), `,command="`+selfPath()+` --sync-child 1762600000.000001 `+tree+`/stage vm2:stage" ssh-ed25519 `+blob+" devopsy-sync-1762600000.000001\n") {
		t.Fatalf("served:\n%s", out.String())
	}
}
