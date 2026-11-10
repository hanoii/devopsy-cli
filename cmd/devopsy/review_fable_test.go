package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// reviewSyncKey writes a sync's key as the door writes it for who (remote.SyncKeyLine) into
// its sync keys file.
func reviewSyncKey(t *testing.T, who string) {
	t.Helper()
	_, pub, err := remote.NewSyncKey()
	if err != nil {
		t.Fatal(err)
	}
	line, err := remote.SyncKeyLine("/usr/local/bin/devopsy", "1762600000.00000a", "/srv/shop/dev", "vm2:dev", false, strings.Fields(pub)[1], time.Now().Add(remote.SyncKeyLife))
	if err != nil {
		t.Fatal(err)
	}
	write(t, remote.DoorSyncKeysFile(who), line+"\n", 0o600)
}

// BUG: a project user's sync keys still open a login while its processes are ended, and until
// the drop that follows the end. endProjectUser ends first and drops after (the fourth
// review's fix, for a key added during the end), so through the whole end, and the time its
// container takes to go, sshd is still told the sync keys (--authorized-keys prints them): a
// login by one a leaked key added before, repeated from outside, lands after the last scan
// found no process and before the drop, and its session, the user's (--sync-child runs the
// environment's own commands, in a tree the leaked key wrote), outlives the key's removal and
// keeps the door where a key is left. The sync keys are to go before the end, and stay gone
// through it: the end under their lock, with the door refusing every add begun before it was
// over (TestReviewOpusSyncKeyAddedPastTheEnd is that other half).
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the user's sync keys are.
func TestSyncKeysOpenNoLoginThroughTheEnd(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	who := "devopsy-shop"
	reviewSyncKey(t, who)
	var told bytes.Buffer
	err := endProjectUser(who, func(string) error {
		// What sshd is told of the user's sync keys while its processes are ended.
		if code := printAuthorizedKeys(&told, remote.DoorSyncKeysFile(who)); code != 0 {
			t.Errorf("--authorized-keys' read of the sync keys: exit %d", code)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if told.Len() != 0 {
		t.Errorf("while %s's processes are ended, sshd still takes its sync key for a login:\n%s", who, told.String())
	}
	if fileExists(remote.DoorSyncKeysFile(who)) {
		t.Error("the sync keys stay after the end")
	}
}
