package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// reviewInfo is a server with environments at paths, each released, and
// claims for owners, uid 100 on.
func reviewInfo(paths []string, owners ...string) remote.ServerInfo {
	info := remote.ServerInfo{Version: "0.53.0", Root: "/home/devopsy", Keep: 5, MaxKeep: 5, SnapshotMaxKeep: 5, UIDsDir: "/home/devopsy/.config/devopsy/uids"}
	for _, p := range paths {
		info.Environments = append(info.Environments, remote.Environment{Path: p,
			Status: "release\t20261027115000\t{\"mode\":\"build\",\"time\":\"2026-10-27T11:50:00Z\",\"by\":\"ana\"}\nkept\t1\nsynced\t\n"})
	}
	for i, o := range owners {
		info.UIDs = append(info.UIDs, remote.UIDClaim{Owner: o, UID: uint32(100 + i), Name: strings.ReplaceAll(o, ":", "_")})
	}
	return info
}

// reviewPrint is info's report as print lays it out, at its real width.
func reviewPrint(info remote.ServerInfo) string {
	es := serverInfoReport(info, "vm1", "vm1:", "0.53.0", time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC))
	var b bytes.Buffer
	newEntryTable(&b, style{}, es).print(es)
	return b.String()
}

// Review round 1 (bug): a disk that did not read has no mount (diskFacts sets none when
// its stat fails), and judgeDisk then takes its first path for the mount and says it twice:
// "releases /srv, /srv: ...".
func TestReviewDiskUnreadSaysItsPathOnce(t *testing.T) {
	t.Parallel()
	u := judgeDisk(remote.Disk{Paths: []string{"releases=/srv"}, Error: "stat /srv: permission denied"}, "", "vm1:")
	if u.line != "releases /srv: stat /srv: permission denied" {
		t.Errorf("a disk unread: %q", u.line)
	}
}

// Review round 1 (bug): environments are sorted by name, but the rows of those only a
// claim is left of are appended after every live one (projectEntries), out of order.
func TestReviewGoneEnvironmentsSortWithTheOthers(t *testing.T) {
	t.Parallel()
	out := reviewPrint(reviewInfo([]string{"shop/a", "shop/z"}, "project:shop", "env:shop:a", "env:shop:m", "env:shop:z"))
	a, m, z := strings.Index(out, "\n    a "), strings.Index(out, "\n    m "), strings.Index(out, "\n    z ")
	if a < 0 || m < 0 || z < 0 || !(a < m && m < z) {
		t.Errorf("rows not in order a, m, z:\n%s", out)
	}
}

// Review round 1 (bug): a row whose cells run past reportWidth is wrapped as prose, at any
// word, so a cell is cut ("uid" on one line, its number on the next), where the layout agreed
// that the last cell moves whole to its own line.
func TestReviewLastCellMovesWhole(t *testing.T) {
	t.Parallel()
	cells := []string{"released 10 min ago by somebody@a-long-runner-name.example.com", "3 of 3 running", "uid 2091094815", "a sync or an export from it"}
	es := []remote.StatusEntry{
		{Level: remote.StatusInfo, Section: "projects", Group: "shop", Key: "shop", Lines: []string{"project uid 1"}},
		{Level: remote.StatusInfo, Section: "projects", Group: "shop", Key: "prod", Lines: []string{remote.CellsJoined(cells...)}, Cells: cells},
	}
	var b bytes.Buffer
	newEntryTable(&b, style{}, es).print(es)
	// Each cell whole on a line, the last on its own: never "uid" on one line and its number on the next.
	lines := strings.Split(b.String(), "\n")
	whole := func(cell string) bool {
		for _, l := range lines {
			if strings.Contains(l, cell) {
				return true
			}
		}
		return false
	}
	last := false
	for _, l := range lines {
		last = last || strings.TrimSpace(l) == cells[3]
	}
	if !whole(cells[0]) || !whole(cells[1]) || !whole(cells[2]) || !last {
		t.Errorf("a cell cut, or the last cell not moved whole to its own line:\n%s", b.String())
	}
}
