package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// Bug: a key and a project's heading are printed as they came, never through
// report.Copyable, which every other line goes through (Output: a name another user chose,
// a project user's environment directory, reaches an admin's terminal). Before the layout an
// environment's path was in its line, escaped; now it is the entry's key and group, in the
// text and in --json.
func TestEntryKeysAreEscaped(t *testing.T) {
	t.Parallel()
	es := []remote.StatusEntry{
		{Level: remote.StatusInfo, Section: "projects", Group: "sh\x1b[2Jop", Key: "pr\x1b[2Jod", Lines: []string{"released"}},
		{Level: remote.StatusInfo, Section: "projects", Group: "ap\x1b[2Jp", Key: "ap\x1b[2Jp", Lines: []string{"project uid 1"}},
	}
	var b bytes.Buffer
	newEntryTable(&b, style{}, es).print(es)
	if strings.Contains(b.String(), "\x1b") {
		t.Errorf("a key or a heading reaches the terminal with its escape:\n%q", b.String())
	}
	for _, e := range copyableEntries(es) {
		if strings.Contains(e.Key+e.Group, "\x1b") {
			t.Errorf("--json: key %q, group %q", e.Key, e.Group)
		}
	}
	// A flat report's key too, as before the layout (the whole row went through Copyable).
	flat := []remote.StatusEntry{{Level: remote.StatusInfo, Key: "k\x1b[2Jk", Lines: []string{"v"}}}
	b.Reset()
	newEntryTable(&b, style{}, flat).print(flat)
	if strings.Contains(b.String(), "\x1b") {
		t.Errorf("a flat report's key: %q", b.String())
	}
}

// Bug: the formatter takes an entry whose key is its group's name for the project's heading,
// so an environment named as its project (shop/shop) is laid out as the heading: with a
// project uid claim its row reads as a second line of the heading, with no key, and without
// one it is the heading itself, at the project's indent. A gone compose name whose rest is the
// project's name (shop-shop under shop) goes the same way.
func TestServerInfoEnvironmentNamedAsItsProject(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	info := remote.ServerInfo{Environments: []remote.Environment{{Path: "shop/prod"}, {Path: "shop/shop"}}}
	for name, claims := range map[string][]remote.UIDClaim{
		"no project claim": nil,
		"a project claim":  {{Owner: "project:shop", UID: 1000001}},
	} {
		info.UIDs = claims
		out, _ := serverInfoSays(info, "vm1", "vm1:", "0.1.0", now)
		_, projects, _ := strings.Cut(out, "\nprojects\n")
		rows := 0
		for _, l := range strings.Split(projects, "\n") {
			if strings.HasPrefix(l, "    shop ") {
				rows++
			}
		}
		if rows != 1 {
			t.Errorf("%s: shop/shop has no row of its own under shop:\n%s", name, projects)
		}
	}
}

// Bug: a uid claim whose environment is gone reads "gone, only its uid claim is left" even
// when containers of that compose name are left: a claimed name is a live one (liveNames), so
// what is left of it is never looked for, and "only" says what was not checked.
func TestServerInfoStaleClaimSaysOnlyWhatItKnows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	info := remote.ServerInfo{
		Environments: []remote.Environment{{Path: "shop/prod"}},
		UIDs:         []remote.UIDClaim{{Owner: "env:shop:pr-1", UID: 1000002, Name: "1000002"}},
		Containers:   []remote.HostContainer{{Project: "shop-pr-1", Service: "web", State: "exited", Released: true}},
	}
	out, _ := serverInfoSays(info, "vm1", "vm1:", "0.1.0", now)
	if strings.Contains(out, "only its uid claim") && !strings.Contains(out, "web (exited)") {
		t.Errorf("shop-pr-1's container is left, and the report says only its claim is:\n%s", out)
	}
}

// Bug: rows of cells under one key stop aligning once a warning sits between them, as a full
// disk's does between two disks' rows: each row is then a run of its own (cellWidths).
func TestEntryCellsAlignAcrossAWarning(t *testing.T) {
	t.Parallel()
	row := func(cells ...string) remote.StatusEntry {
		return remote.StatusEntry{Level: remote.StatusInfo, Section: "server", Key: "disks", Lines: []string{remote.CellsJoined(cells...)}, Cells: cells}
	}
	es := []remote.StatusEntry{
		row("/", "ext4", "releases"),
		{Level: remote.StatusWarning, Section: "server", Key: "disks", Lines: []string{"/ is 94% full"}},
		row("/mnt/backup", "ext4", "snapshots"),
	}
	var b bytes.Buffer
	newEntryTable(&b, style{}, es).print(es)
	want := `
server
  disks  /            ext4  releases
!        / is 94% full
         /mnt/backup  ext4  snapshots
`
	if b.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

// Bug: a remedy's line that starts indented (a numbered fix's second line, under its number)
// loses its indent on its first part when it wraps, and keeps it on the next: wrapAt drops the
// leading spaces it splits on.
func TestEntryWrappedRemedyKeepsItsIndent(t *testing.T) {
	t.Parallel()
	long := strings.TrimSpace(strings.Repeat("words ", 20))
	es := []remote.StatusEntry{{Level: remote.StatusWarning, Section: "server", Key: "disks", Lines: []string{"full"},
		Remedies: []report.Remedy{report.Fix("first\n" + long), report.Fix("second")}}}
	var b bytes.Buffer
	newEntryTable(&b, style{}, es).print(es)
	lines := strings.Split(b.String(), "\n")
	at := func(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }
	for i, l := range lines {
		if strings.Contains(l, "1. first") {
			if want := at(l) + 3; at(lines[i+1]) != want || at(lines[i+2]) != want {
				t.Errorf("the fix's second line and its wrapped rest start at %d and %d, want %d:\n%s", at(lines[i+1]), at(lines[i+2]), want, b.String())
			}
			return
		}
	}
	t.Fatalf("no numbered fix:\n%s", b.String())
}
