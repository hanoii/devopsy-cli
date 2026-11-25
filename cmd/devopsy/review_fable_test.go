package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Bug: blocks says a warning is bound by the nearest entry before it
// that is no block, so its answer changes once orderWarnings moves the
// warning: one about a line with no cells (a disk that did not read),
// with a row of cells after it in its key, moves after that row, is then
// taken for the row's own, and prints right under it with no blank line,
// as a warning about that row does. blocks' comment says the answer is
// the same before and after the move.
func TestFableWarningBoundTheSameOnceMoved(t *testing.T) {
	t.Parallel()
	row := remote.StatusEntry{Level: remote.StatusInfo, Section: "server", Key: "disks", Cells: []string{"/mnt", "ext4"}, Lines: []string{remote.CellsJoined("/mnt", "ext4")}}
	es := []remote.StatusEntry{
		{Level: remote.StatusInfo, Section: "server", Key: "disks", Lines: []string{"releases /home/devopsy: it did not answer"}},
		{Level: remote.StatusWarning, Section: "server", Key: "disks", Lines: []string{"the release root's filesystem makes no reflinks"}},
		row,
	}
	_, before := blocks(es)
	moved := orderWarnings(es)
	if moved[2].Level != remote.StatusWarning {
		t.Fatalf("the control: the warning moves after its key's facts: %v", moved)
	}
	_, after := blocks(moved)
	if before[1] || after[2] != before[1] {
		t.Errorf("the warning bound to a row: %v before the move, %v after", before[1], after[2])
	}
	var b bytes.Buffer
	newEntryTable(&b, style{}, es).print(es)
	if !strings.Contains(b.String(), "/mnt  ext4\n\n") {
		t.Errorf("no blank line between a row and a warning that is not about it:\n%s", b.String())
	}
}

// Bug: --status takes the Break off the first row after its heading, and
// with no heading (in a project, or in a release on a server, where the
// disks come before the containers) that heading is the project's row, a
// key as any other: a key of several lines right after it gets no blank
// line before it, and in a box no rule, the one key of the box with none.
func TestFableStatusBreaksAfterTheProjectRow(t *testing.T) {
	t.Parallel()
	r := statusReport{}
	r.addFacts(remote.StatusFacts{
		Project:    "shop-prod",
		Disks:      []remote.Disk{{Paths: []string{"releases=/home/devopsy", "/=/"}, Type: "xfs", Size: 80e9, Free: 1e9, Used: 79e9}},
		Containers: []remote.StatusContainer{{Service: "app", State: "running"}},
	}, "")
	var plain, boxed bytes.Buffer
	r.print(&plain, style{}, "", textReport)
	if !strings.Contains(plain.String(), "warning: / is 99% full") {
		t.Fatalf("the control: the disk's key has several lines:\n%s", plain.String())
	}
	if !strings.Contains(plain.String(), "shop-prod\n\ndisks") {
		t.Errorf("no blank line before a key of several lines, after the project's row:\n%s", plain.String())
	}
	r.print(&boxed, style{}, "", boxedReport)
	lines := strings.Split(boxed.String(), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "│ disks") && !strings.HasPrefix(lines[i-1], "├") {
			t.Errorf("no rule between the project's row and the next key:\n%s", boxed.String())
		}
	}
}
