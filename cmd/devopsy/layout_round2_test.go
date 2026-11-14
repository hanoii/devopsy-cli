package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Review round 2, bug: one wide cell in a run pads every row of the run past reportWidth, so
// a row whose own cells fit (a:1 and its size) still has its last cell moved to a line of its
// own, its size printed alone under it; the move goes by the padded row, never by what the
// row holds.
func TestReviewShortRowKeepsItsLastCellBesideAWideOne(t *testing.T) {
	t.Parallel()
	row := func(cells ...string) remote.StatusEntry {
		return remote.StatusEntry{Level: remote.StatusInfo, Section: "leftovers", Key: "images", Lines: []string{remote.CellsJoined(cells...)}, Cells: cells}
	}
	wide := "registry.example.com/team/app:2026-11-14-build-1234, registry.example.com/team/app:latest"
	es := []remote.StatusEntry{row("a:1", "4.3 MB"), row(wide, "8.0 MB")}
	var b bytes.Buffer
	newEntryTable(&b, style{}, es).print(es)
	for _, l := range strings.Split(b.String(), "\n") {
		if strings.Contains(l, "a:1") && !strings.Contains(l, "4.3 MB") {
			t.Errorf("a:1's row fits the width, and its size moved to a line of its own:\n%s", b.String())
		}
	}
}

// Review round 2, bug: a stale project uid claim's hint is the same removal by hand as a gone
// environment's claim, written apart (projectEntries' rm, leftEntries' onServer): the
// environment's runs through --shell-host on the server, the project's offers its rm on the
// operator's machine with no way onto the server.
func TestReviewStaleProjectClaimHintGoesOnTheServer(t *testing.T) {
	t.Parallel()
	// blog's project claim is left: no environment of it holds a claim.
	info := releasedAt([]string{"shop/prod"}, "project:shop", "env:shop:prod", "project:blog")
	es := serverInfoReport(info, "vm1", "vm1:", "0.53.0", time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC))
	found := false
	for _, e := range es {
		if e.Group != "blog" || !strings.Contains(strings.Join(e.Lines, " "), "uid claim") {
			continue
		}
		found = true
		cmds := fixText(e.Remedies)
		if !strings.Contains(cmds, "$ devopsy @vm1: --shell-host\n$ rm ") {
			t.Errorf("project:blog's claim left offers its rm without --shell-host, as an environment's claim does not:\n%s", cmds)
		}
	}
	if !found {
		t.Fatalf("no line of project:blog's claim left in:\n%+v", es)
	}
}

// Review round 2, bug: a gone environment whose name is past goneKeyMax is keyed "gone", and
// projectRows joins rows by key, so a live environment named gone takes it as one of its own
// lines: the long name's leftovers read as what the live gone environment holds.
func TestReviewLongGoneNameIsNoLineOfALiveEnvironmentNamedGone(t *testing.T) {
	t.Parallel()
	info := releasedAt([]string{"shop/gone", "shop/prod"})
	info.Containers = []remote.HostContainer{{Project: "shop-a-feature-branch-name", Service: "web", State: "exited", Released: true}}
	out := reportAt(info)
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.Contains(l, "a-feature-branch-name: ") && !strings.HasPrefix(l, "    gone ") {
			t.Errorf("the gone a-feature-branch-name is a line of the live shop/gone's row (after %q):\n%s", lines[i-1], out)
		}
	}
}
