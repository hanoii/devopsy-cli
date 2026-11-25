package remote

import (
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/images"
)

// Bug (review 4): FormatReleases writes the names a project's removals
// left or kept as "  <ref>  <name>, release ..." by hand, two spaces
// between, so consecutive rows do not line up: the same ragged columns
// --ssh-keys had before it took the grid (f8dcb38), against Output's
// "every column devopsy prints is laid out by internal/layout", and no
// report.Copyable on a name read from the server's file. The rows of one
// list should put their environment in one column.
func TestReview4ReleasesKeptNamesInColumns(t *testing.T) {
	t.Parallel()
	out := "left\t1760000000\tapp-prod\t20261001000000\tjq:1.8\tsha256:j\t" + images.ToolKeeps + "\n" +
		"left\t1760000000\tapp-prod\t20261001000000\tbusybox:1.37\tsha256:b\t" + images.ToolKeeps + "\n"
	got := FormatReleases(out, false)
	var at []int
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "  ") && strings.Contains(l, "app-prod, release") {
			at = append(at, strings.Index(l, "app-prod"))
		}
	}
	if len(at) != 2 {
		t.Fatalf("the control: two kept names in:\n%s", got)
	}
	if at[0] != at[1] {
		t.Errorf("the kept names' environments at columns %v, not one:\n%s", at, got)
	}
}
