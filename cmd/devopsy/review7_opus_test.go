package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// A project user under door: users: in config.yaml, its removal asked, is told to be removed
// there ("remove it there"), and once the admin does, --project-users remove says there is no
// such project user: no step of devopsy's empties its keys, which sshd takes from the keys
// file alone (--authorized-keys reads no entry), nor ends what it runs, nor drops its sync
// keys or what the door kept for it. So the remedy devopsy offers for removing such a user
// leaves it logging in, a shell behind the firewall, its leaked key's processes running.
//
// Review 7 (Opus): the cause is that a config user's removal does nothing of a removal, only
// refuses; the end and the keys are the door's, wherever its entry is written.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the user's keys are.
func TestRemovalOfAConfigUserLeavesNoKeys(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	who := "devopsy-shop"
	entry := remote.DoorDir() + "/users/" + who + ".yaml"
	keys, syncKeys := remote.DoorKeysFile(who), remote.DoorSyncKeysFile(who)
	write(t, keys, removedUserKey, 0o600)
	write(t, syncKeys, "a sync's key\n", 0o600)
	var ended []string
	end := func(w string) error { ended = append(ended, w); return nil }
	inConfig := &remote.Door{Space: t.TempDir(), Users: map[string]remote.DoorUser{who: {Project: "shop"}}}
	if code := removeProjectUser(inConfig, "shop", who, entry, end); code == 0 {
		t.Fatal("a config user's removal passed")
	}
	// The admin follows the refusal: the user goes from config.yaml, and the removal is asked again.
	gone := &remote.Door{Space: inConfig.Space}
	code := removeProjectUser(gone, "shop", who, entry, end)
	var printed bytes.Buffer
	_ = printAuthorizedKeys(&printed, keys)
	if remote.KeysLeft(keys) || strings.Contains(printed.String(), "AAAA") || fileExists(syncKeys) || len(ended) == 0 {
		t.Errorf("a config user removed as devopsy says (exit %d): sshd still takes its key %v, its sync keys there %v, its processes ended %q",
			code, strings.Contains(printed.String(), "AAAA"), fileExists(syncKeys), ended)
	}
}
