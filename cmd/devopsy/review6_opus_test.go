package main

import (
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/layout"
)

// Bug (review 6): a box widens for a line it cannot wrap up to
// max(reportWidth, wide), so on a terminal narrower than 100 a flat
// --status, whose lines never wrap and whose box is narrow, widens to 100
// for one long line, every rule and side of it then wrapping at the
// terminal: the field's comment (wide, "the most a box widens to"),
// internals/other.md and docs/servers.md ("up to the terminal's width")
// say the terminal's width is the most, the long line running past alone.
func TestOpus6FlatBoxWidensNoFurtherThanANarrowTerminal(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	tab := newEntryTable(&b, style{})
	tab.borders, tab.wide = true, 80
	url := "https://shop.example.com/" + strings.Repeat("a-long-path/", 6)
	tab.write([]layout.Row{tab.row("project", "shop-prod"), tab.row("url", url)})
	lines := strings.Split(b.String(), "\n")
	if !strings.Contains(b.String(), url) {
		t.Fatalf("the control: no url in:\n%s", b.String())
	}
	if w := layout.Width(lines[0]); w > 80 {
		t.Errorf("on an 80-column terminal the box is %d wide for one line it cannot wrap:\n%s", w, b.String())
	}
}
