package main

import (
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Bug (review, opus): whether a warning is bound to a row is decided
// after orderWarnings moved it (entryTable.rows runs blocks on the
// ordered entries), and moving changes the answer, against blocks' own
// comment ("the same before and after the warnings move"): a warning bound
// to nothing ahead of a row of cells in its run moves after that row and
// then reads as the row's, under it with no blank line. Here the release
// root's disk did not read (a line, no cells) and its filesystem makes no
// reflinks: the warning about the release root lands glued under
// /mnt/backup's row, as if /mnt/backup made no reflinks. A warning bound
// to nothing gets a blank line before it (TestReportBlankLines).
func TestReviewOpusMovedWarningStaysUnbound(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	i := healthyServerInfo()
	i.Disks[0].Error, i.Disks[0].Reflinks = "df: timed out", "no"
	i.Disks = append(i.Disks, remote.Disk{Paths: []string{"snapshots=/mnt/backup"}, Mount: "/mnt/backup", Type: "ext4", Size: 100e9, Free: 50e9, Used: 50e9})
	out, _ := serverInfoSays(i, "vm1", "vm1:", "0.53.0", now)
	row := "/mnt/backup  ext4  50.0 GB free of 100.0 GB  50% used  snapshots\n"
	warn := "             warning: the release root's filesystem (xfs) makes no reflinks"
	if !strings.Contains(out, row) || !strings.Contains(out, warn) {
		t.Fatalf("the control: no disk row or no reflinks warning in:\n%s", out)
	}
	if !strings.Contains(out, row+"\n"+warn) {
		t.Errorf("a warning bound to no row reads as /mnt/backup's, under its row with no blank line:\n%s", out)
	}
	// The cause: blocks says bound once the warning moved, not before.
	es := serverInfoReport(i, "vm1", "vm1:", "0.53.0", now)
	boundOf := func(es []remote.StatusEntry) bool {
		_, bound := blocks(es)
		for k, e := range es {
			if e.Level == remote.StatusWarning && strings.Contains(e.Lines[0], "makes no reflinks") {
				return bound[k]
			}
		}
		t.Fatal("no reflinks warning")
		return false
	}
	if before, after := boundOf(es), boundOf(orderWarnings(es)); before != after {
		t.Errorf("bound before the move %v, after it %v", before, after)
	}
}
