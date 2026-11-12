package uids

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Bug: Lookup means to keep the uid an entry that is no claim points at ("never take its
// uid"), but parses the entry's name as the uid instead of its target: a stray link, a claim
// renamed by hand (an uppercase environment's), or one whose target left its range (Read's
// error) points at a uid that Lookup then hands another owner.
func TestLookupSkipsWhatABadEntryPointsAtReviewOpus(t *testing.T) {
	t.Parallel()
	s := Store{Dir: filepath.Join(t.TempDir(), ".devopsy-uids"), span: 1}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	start, _ := Range(EnvDomain)
	// The one slot the span leaves, held by an entry no owner's.
	if err := os.Symlink(strconv.FormatUint(uint64(start), 10), filepath.Join(s.Dir, "stray")); err != nil {
		t.Fatal(err)
	}
	if uid, err := s.Lookup(Owner{Project: "shop", Env: "prod"}); err == nil {
		t.Errorf("took %d, which the entry stray points at", uid)
	}
}
