package uids

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Bug (review, fable, round 1): Lookup keeps off the uid of an entry that is no claim by
// parsing the entry's name as a uid (parseUID(c.Name)), never its target, so "any entry
// holds its uid" (internals/uids.md) is not kept: an entry whose name is no owner's, or
// whose owner is of another domain than its target, pointing at a uid, loses it to the next
// owner hashed or probed there, and two sets of containers share it.
func TestBadEntryHoldsTheUIDItPointsAt(t *testing.T) {
	t.Parallel()
	s := store(t)
	o := env(t, "shop", "", "prod")
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	plain := strconv.FormatUint(uint64(o.Plain()), 10)
	// A name no owner has (an uppercase environment, from a hand's rename), on o's slot.
	if err := os.Symlink(plain, filepath.Join(s.Dir, "env:shop:Prod")); err != nil {
		t.Fatal(err)
	}
	uid, err := s.Lookup(o)
	if err != nil {
		t.Fatal(err)
	}
	if uid == o.Plain() {
		t.Errorf("%s took %d, which the entry env:shop:Prod points at", o, uid)
	}
}
