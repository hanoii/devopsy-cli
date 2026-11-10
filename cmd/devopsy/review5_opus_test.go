package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Review 5 (Opus), a failing test proving a bug: --project-users remove whose end of the
// user's processes fails (Docker down, the util image's rebuild failing on a registry) leaves
// no way to end them again. A key's removal marks its end (remote.DoorEndingFile) and the
// next edit or listing of the user's keys settles it; remove writes no mark, and a second
// remove fails on the entry already gone without asking for the end. What the removed user
// left running (setsid, nohup) then outlives the removal for good, and once the admin adds the
// project again and gives it a key, the door answers it again (KeysLeft, no mark).
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the user's keys are.
func TestReview5RemoveProjectUserEndIsAskedAgain(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	who := "devopsy-shop"
	entry := filepath.Join(t.TempDir(), who+".yaml")
	write(t, entry, "project: shop\n", 0o600)
	write(t, remote.DoorKeysFile(who), "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g op@laptop\n", 0o600)
	if code := removeProjectUser(&remote.Door{Space: t.TempDir()}, "shop", who, entry, func(string) error { return errors.New("no docker") }); code == 0 {
		t.Fatal("a remove whose end failed passes")
	}
	marked := fileExists(remote.DoorEndingFile(who))
	asked := false
	_ = removeProjectUser(&remote.Door{Space: t.TempDir()}, "shop", who, entry, func(string) error { asked = true; return nil })
	if !marked && !asked {
		t.Error("a remove whose end failed left no mark for the keys' next edit to settle, and a second remove does not end the user either")
	}
}
