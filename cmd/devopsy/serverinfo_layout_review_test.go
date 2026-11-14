package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Bug: a row of cells whose last cell is empty (an environment's with nothing running on it,
// which is most of them) is taken for prose: its padded cells are wrapped at a word, "uid" on
// one line and its number on the next, and the padding after its last shown cell counts
// toward the width, so a row that fits is cut too. A row of cells is never cut at a word,
// whatever its last cell holds.
func TestEntryRowWithAnEmptyLastCellIsNeverCut(t *testing.T) {
	t.Parallel()
	row := func(key string, cells ...string) remote.StatusEntry {
		return remote.StatusEntry{Level: remote.StatusInfo, Section: "projects", Group: "shop", Key: key, Lines: []string{remote.CellsJoined(cells...)}, Cells: cells}
	}
	whole := func(out, cell string) bool {
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, cell) {
				return true
			}
		}
		return false
	}
	// Past the width: no cell is cut in two.
	cells := []string{"released 10 min ago by somebody@a-long-runner-name.example.com", "3 of 3 running", "uid 2091094815", ""}
	es := []remote.StatusEntry{row("prod", cells...)}
	var b bytes.Buffer
	newEntryTable(&b, style{}, es).print(es)
	for _, c := range cells[:3] {
		if !whole(b.String(), c) {
			t.Errorf("the cell %q is cut at a word:\n%s", c, b.String())
		}
	}
	// Within the width as it shows (10 of key column, 86 of cells), past it only by the
	// padding under another row's wider uid cell: one line, as it fits, the next row after
	// it with no empty line between.
	short := []string{strings.Repeat("r", 60), "3 of 3 running", "uid 7", ""}
	es = []remote.StatusEntry{row("prod", short...), row("dev", "released", "1 of 1 running", "uid 2091094815 or so", "syncing")}
	b.Reset()
	newEntryTable(&b, style{}, es).print(es)
	if !strings.Contains(b.String(), short[0]+"  "+short[1]+"  "+short[2]+"\n    dev ") {
		t.Errorf("a row that fits the width is cut for the padding after its last shown cell:\n%s", b.String())
	}
}

// Bug: a gone environment named as its project (shop/shop, its compose name shop-shop) is
// taken for a name of no project, by its key being the project's name: what is left of it is
// said on the project's heading, as if of the whole project, with no row of its own, with a
// claim left (claimKey) and without (projectOfName). The heading is marked only for a name
// that is no project's on the server.
func TestServerInfoGoneEnvironmentNamedAsItsProject(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	for name, info := range map[string]remote.ServerInfo{
		"its claim left": {
			Environments: []remote.Environment{{Path: "shop/prod"}},
			UIDs:         []remote.UIDClaim{{Owner: "env:shop:shop", UID: 1000002, Name: "1000002"}},
		},
		"no claim, a container left": {
			Environments: []remote.Environment{{Path: "shop/prod"}},
			Containers:   []remote.HostContainer{{Project: "shop-shop", Service: "web", State: "exited", Released: true}},
		},
	} {
		out, _ := serverInfoSays(info, "vm1", "vm1:", "0.1.0", now)
		_, projects, _ := strings.Cut(out, "\nprojects\n")
		row := false
		for _, l := range strings.Split(projects, "\n") {
			row = row || strings.HasPrefix(l, "    shop ") && strings.Contains(l, "gone")
		}
		if !row {
			t.Errorf("%s: the gone shop/shop has no row of its own under shop:\n%s", name, projects)
		}
	}
}

// Bug: with Docker answering and the containers' list not read (or the images', or the
// volumes'), a claim whose environment is gone is still said as "only its uid claim": its
// containers were never looked for, as when Docker is down, where it is said alone.
func TestServerInfoGoneClaimPartlyUnlookedIsNotOnly(t *testing.T) {
	t.Parallel()
	base := remote.ServerInfo{
		Environments: []remote.Environment{{Path: "shop/prod"}},
		UIDsDir:      "/home/devopsy/.devopsy-uids",
		UIDs:         []remote.UIDClaim{{Owner: "env:shop:pr-1", UID: 1000002, Name: "env:shop:pr-1"}},
	}
	for name, change := range map[string]func(*remote.ServerInfo){
		"containers unread": func(i *remote.ServerInfo) { i.ContainersUnread = "docker inspect: exit status 1" },
		"images unread":     func(i *remote.ServerInfo) { i.ImagesUnread = "docker image ls: exit status 1" },
		"volumes unread":    func(i *remote.ServerInfo) { i.VolumesUnread = "docker volume ls: exit status 1" },
	} {
		i := base
		change(&i)
		got := strings.Join(leftSays(i, "devopsy@vm1", "vm1:"), "\n\n")
		if strings.Contains(got, "only its uid claim") || !strings.Contains(got, "uid claim 1000002") {
			t.Errorf("%s: the claim said as all that is left, or unsaid:\n%s", name, got)
		}
	}
}
