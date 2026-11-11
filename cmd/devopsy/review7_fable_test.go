package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// A project user added again while its removal's marks are there (a removal whose end failed,
// then `--project-users add` in place of the removal asked again) stays a project user once
// its keys are next edited: addProjectUser reads no mark, so it writes the entry and says the
// user is there, and the first `--ssh-keys --for <project> add`, which add's own words ask
// for, settles the old removal (keysSettle, finishPendingRemoval), which takes the new entry
// with the marks, then adds the key. Both commands said they worked, and what is left is a
// Unix user with a key sshd takes and no entry: it logs in, and the door says it is no
// project user here.
//
// Review 7 (Fable): the cause is add taking no account of a removal pending (it neither
// refuses nor finishes it first); a settle that spared an entry newer than its mark would
// treat the symptom. The entry is written here as add writes it, add itself needing a host.
//
// Not parallel: it sets DEVOPSY_HOME (newRemovalCut).
func TestProjectUserAddedAgainOverAPendingRemovalStays(t *testing.T) {
	who := door.UserOf("shop")
	c := newRemovalCut(t)
	d, err := remote.ServerDoor()
	if err != nil {
		t.Fatal(err)
	}
	if code := removeProjectUser(d, "shop", who, c.entry, func(string) error { return errors.New("no docker") }); code == 0 {
		t.Fatal("a removal whose end failed passed")
	}
	if !fileExists(c.removing) || fileExists(c.entry) {
		t.Fatalf("a removal whose end failed: its mark %v, its entry %v", fileExists(c.removing), fileExists(c.entry))
	}
	// --project-users add shop, as addProjectUser writes its entry for a user not listed.
	write(t, c.entry, "project: shop\n", 0o600)
	// --ssh-keys --for shop add, the next thing add asks for.
	out := c.script(remote.KeysAddScript(who), strings.TrimSpace(cutKept)+"\n")
	if remote.KeysLeft(c.keys) && !fileExists(c.entry) {
		t.Errorf("added again and given a key, %s has a key sshd takes and no entry: the door answers it is no project user here\n%s", who, out)
	}
	if fileExists(c.removing) || fileExists(c.ending) {
		t.Errorf("its removal's marks stay: removing %v, ending %v\n%s", fileExists(c.removing), fileExists(c.ending), out)
	}
}
