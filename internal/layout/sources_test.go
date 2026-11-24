package layout

import (
	"regexp"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/sources"
)

// Every column devopsy prints is laid out here: outside the package, no
// format pads a value to a width (%-Ns, %-*s, %*s), no code imports
// text/tabwriter, and none counts a string's runes, which measures no
// column as it shows. A column padded by hand drifts from the grid and
// misses its fixes.
func TestColumnsThroughLayout(t *testing.T) {
	t.Parallel()
	hand := regexp.MustCompile(`%-[0-9]+s|%-\*s|%\*s|"text/tabwriter"|utf8\.RuneCountInString`)
	for _, f := range sources.Go(t, false) {
		if strings.HasPrefix(f.Path, "internal/layout/") {
			continue
		}
		for i, line := range strings.Split(string(f.Data), "\n") {
			if m := hand.FindString(line); m != "" {
				t.Errorf("%s:%d: %s: a column laid out by hand, not through internal/layout", f.Path, i+1, m)
			}
		}
	}
}
