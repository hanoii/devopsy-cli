package remote

import (
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/layout"
)

// Bug (review 6): --releases --borders puts a release's images under its
// source, in its cell, so one release's long images line widens the source
// column of every release and pushes each one's state past it: the row of
// a FAILED release with one short image is drawn 187 columns wide, past any
// terminal, every line of the box wrapping there, where without borders
// that row is 44 wide and the images run free under their own release. The
// reports' boxes stop at the terminal (100 in a pipe) and run a longer line
// past their side alone; the lists take no MaxWidth, and the images are in
// no last column it could cap.
func TestOpus6ReleasesBoxKeepsStatesNearTheirRows(t *testing.T) {
	t.Parallel()
	out := "20261005120000\tcurrent\tok\tapp=registry.example.com/team/app:2026-11-14-build-with-a-long-tag db=postgres:17.2-bookworm worker=registry.example.com/team/worker:2026-11-14\t{\"by\":\"ci\",\"mode\":\"build\",\"branch\":\"main\",\"commit\":\"abc1234567890\"}\n" +
		"20261004120000\t\tfailed\tapp=sha256:abcdef\t{\"by\":\"ana\",\"mode\":\"image\"}\n"
	boxed := FormatReleases(out, true)
	for _, l := range strings.Split(boxed, "\n") {
		if strings.Contains(l, "FAILED") && layout.Width(l) > 100 {
			t.Errorf("a release's state at column %d, pushed by another release's images:\n%s", layout.Width(l), boxed)
		}
	}
}
