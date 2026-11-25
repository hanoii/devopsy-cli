package main

import (
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/layout"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// review5Report is --server-info's report of info as it prints, uncolored, in
// boxes with borders.
func review5Report(info remote.ServerInfo, borders bool) string {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	var b strings.Builder
	tab := newEntryTable(&b, style{})
	tab.borders = borders
	tab.print(serverInfoReport(info, "vm1", "vm1:", "0.53.0", now))
	return b.String()
}

// Bug (review 5): a project's heading with facts of its own (its project uid
// claim, its project user) puts its name in the report's key column, so a long
// project name moves every section's value column and wraps their prose
// narrower, with borders as without; the same heading with no facts runs free
// (TestBoxedKeyColumnLeavesAProjectHeadingOut). Its name should set no key
// column either way.
func TestReview5HeadingWithFactsSetsNoKeyColumn(t *testing.T) {
	t.Parallel()
	// valueColumn is where the system line's value starts.
	valueColumn := func(out string) int {
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "system") {
				return layout.Width(l[:strings.Index(l, "Debian")])
			}
		}
		t.Fatalf("no system line in:\n%s", out)
		return 0
	}
	long := healthyServerInfo()
	long.Environments = append(long.Environments, remote.Environment{Path: "storefront-checkout-service/prod",
		Status: "release\t20261027115000\t{\"mode\":\"image\",\"time\":\"2026-10-27T11:50:00Z\",\"by\":\"ana\"}\nkept\t1\nsynced\t\n"})
	long.UIDs = []remote.UIDClaim{{Owner: "project:storefront-checkout-service", UID: 7}}
	for _, borders := range []bool{false, true} {
		short, wide := valueColumn(review5Report(healthyServerInfo(), borders)), valueColumn(review5Report(long, borders))
		if short != wide {
			t.Errorf("borders %v: a project heading with a uid claim moved the value column from %d to %d:\n%s", borders, short, wide, review5Report(long, borders))
		}
	}
}

// Bug (review 5): with borders an instance's heading is drawn two ways by
// whether it has facts: one with its project uid claim sits in the key column
// among its project's rows, one with none runs across the box between two
// rules, cutting its project's block in two and its heading off its own
// environments. Without borders both are one line at the instance's indent.
func TestReview5BoxedInstanceHeadingsDrawnAlike(t *testing.T) {
	t.Parallel()
	st := "release\t20261027115000\t{\"mode\":\"image\",\"time\":\"2026-10-27T11:50:00Z\",\"by\":\"ana\"}\nkept\t1\nsynced\t\n"
	info := healthyServerInfo()
	info.Environments = append(info.Environments,
		remote.Environment{Path: "bookshop/asia/prod", Status: st},
		remote.Environment{Path: "bookshop/eu/prod", Status: st})
	info.UIDs = []remote.UIDClaim{{Owner: "project:bookshop:asia", UID: 8}, {Owner: "env:bookshop:asia:prod", UID: 9}}
	out := review5Report(info, true)
	lines := strings.Split(out, "\n")
	// shape is a line's box characters by column, its words blanked.
	shape := func(l string) string {
		var b strings.Builder
		for _, r := range l {
			if strings.ContainsRune("│├┤┼┬┴─╭╮╰╯", r) {
				b.WriteRune(r)
			} else {
				b.WriteString(strings.Repeat(" ", layout.Width(string(r))))
			}
		}
		return strings.TrimRight(b.String(), " ")
	}
	at := func(word string) int {
		for i, l := range lines {
			if strings.HasPrefix(strings.TrimLeft(strings.TrimPrefix(l, "│"), " "), word) {
				return i
			}
		}
		t.Fatalf("no %q line in:\n%s", word, out)
		return 0
	}
	asia, eu := at("asia"), at("eu ")
	// Each instance's line, and the environment rows around eu (asia's prod
	// before it, its own after) as asia's own prod after it: no rule between.
	if shape(lines[asia]) != shape(lines[eu]) || shape(lines[eu-1]) != shape(lines[asia+1]) || shape(lines[eu+1]) != shape(lines[asia+1]) {
		t.Errorf("the instances asia (a uid claim) and eu (none) drawn two ways:\n%s", out)
	}
}

// Bug (review 5): --snapshots --borders with a snapshot that has a state lays
// the header's SIZE out to the box's side (its row stops short of the state
// column), so it loses its column's right alignment and its column line, where
// the list without borders, and with borders and no state, right-align it
// over the sizes.
func TestReview5BoxedSnapshotsHeaderKeepsItsColumn(t *testing.T) {
	t.Parallel()
	snaps := []snap{
		{id: "20261101000000", kind: "full", release: "20261031000000", taken: "2026-11-01 00:00 UTC", size: "1.2 GB"},
		{id: "20261102000000", kind: "data", release: "20261031000000", taken: "2026-11-02 00:00 UTC", size: "12 MB", state: "incomplete"},
	}
	// end is the column a word in out ends at, on the first line holding it.
	end := func(out, word string) int {
		for _, l := range strings.Split(out, "\n") {
			if i := strings.Index(l, word); i >= 0 {
				return layout.Width(l[:i+len(word)])
			}
		}
		t.Fatalf("no %q in:\n%s", word, out)
		return 0
	}
	for _, borders := range []bool{false, true} {
		out := snapshotsGrid(snaps, borders).String()
		if end(out, "SIZE") != end(out, "1.2 GB") {
			t.Errorf("borders %v: SIZE not right-aligned over the sizes:\n%s", borders, out)
		}
	}
}

// Bug (review 5): with borders, one command wider than the report's 100
// columns (leftovers' docker rmi of every image no release holds) widens its
// box, and with it every box sharing its columns: the server's and devopsy's
// boxes, whose lines all fit, are drawn as wide as that command, each line
// padded to it, so a terminal narrower than the command wraps every line of
// every box. Without borders only the command's own line is long.
func TestReview5BoxedLongCommandWidensOnlyItsBox(t *testing.T) {
	t.Parallel()
	info := healthyServerInfo()
	for i := range 8 {
		info.Images = append(info.Images, remote.HostImage{ID: "i" + string(rune('a'+i)), Tags: []string{"registry.example.com/team/app-" + string(rune('a'+i)) + ":2026-11-14-build"}, Size: 1e6})
	}
	out := review5Report(info, true)
	if !strings.Contains(out, "docker rmi") {
		t.Fatalf("the control: no docker rmi in:\n%s", out)
	}
	// The server's box: from its title to the next.
	server := out[strings.Index(out, "server\n"):strings.Index(out, "devopsy\n")]
	for _, l := range strings.Split(server, "\n") {
		if w := layout.Width(l); w > reportWidth {
			t.Fatalf("the server's box drawn %d wide for leftovers' command, past its %d:\n%s", w, reportWidth, l)
		}
	}
}
