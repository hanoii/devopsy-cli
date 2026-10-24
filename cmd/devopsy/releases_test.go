package main

import (
	"slices"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// failed takes failed, rolled-back and incomplete releases, but an
// incomplete one newer than current only by its id: its activation may
// not have started yet.
func TestChooseFailed(t *testing.T) {
	t.Parallel()
	rs := []remote.Release{
		{ID: "6", Incomplete: true},
		{ID: "5", Current: true},
		{ID: "4", Incomplete: true},
		{ID: "3", Failed: true},
		{ID: "2", RolledBack: true},
		{ID: "1"},
	}
	got, err := chooseReleases(&remote.Target{Name: "prod"}, rs, []string{"failed"})
	var ids []string
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	slices.Sort(ids)
	if err != nil || !slices.Equal(ids, []string{"2", "3", "4"}) {
		t.Errorf("failed: %v (%v)", ids, err)
	}
	if got, err := chooseReleases(&remote.Target{Name: "prod"}, rs, []string{"6"}); err != nil || len(got) != 1 || got[0].ID != "6" {
		t.Errorf("by id: %v (%v)", got, err)
	}
}
