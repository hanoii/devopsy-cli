package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// leftSays is each entry of what may be dead or gone as one text: its key, its lines, then
// each remedy's words and commands.
func leftSays(info remote.ServerInfo, host, ref string) []string {
	var out []string
	for _, e := range leftEntries(info, host, ref) {
		s := e.Key + ": " + strings.Join(e.Lines, "\n")
		for _, r := range e.Remedies {
			s += "\n| " + r.Text
			for _, c := range r.Cmds {
				s += "\n$ " + c
			}
		}
		out = append(out, s)
	}
	return out
}

// What --server-info shows of what may be dead or gone, each with its hint: what --gc would
// remove, tagged images no release holds and no container uses (never a hold, a build, the
// util image or one a container uses), what is left of a compose name with no environment
// directory and no uid claim (a release's tags, anchored, so shop-stage's never take
// shop-stage-2's nor the other way, a name over 94 characters by its cut prefix; a project
// user's builds; a release's containers; volumes of such a name only, never of the admin's own
// compose projects; snapshots under the snapshot root), dangling anonymous volumes, a cut
// --destroy's directory, and a project user with no key that holds environments. Locally the
// hints say in words what needs an address.
func TestServerInfoLeft(t *testing.T) {
	t.Parallel()
	long := "shop-" + strings.Repeat("e", 95)
	sum := sha256.Sum256([]byte(long))
	longTag := "app:devopsy-" + long[:85] + "-" + hex.EncodeToString(sum[:4]) + "-20261101000000"
	info := remote.ServerInfo{
		Root: "/home/devopsy", SnapshotRoot: "/backups", UIDsDir: "/home/devopsy/.devopsy-uids",
		Environments: []remote.Environment{
			{Path: "shop/prod"}, {Path: "shop/stage"}, {Path: "wiki/stage-2"}, {Path: "shop/" + strings.Repeat("e", 95)},
			// A project user's tree is never a live name: its claims are.
			{Path: "blog/dev", User: "devopsy-blog"},
		},
		UIDs: []remote.UIDClaim{{Owner: "env:shop:prod", UID: 1}, {Owner: "env:news:prod", UID: 2}, {Owner: "project:news", UID: 3}},
		Containers: []remote.HostContainer{
			{Project: "shop-prod", Service: "app", State: "running", Released: true},
			{Project: "gone-prod", Service: "web", State: "exited", Released: true},
			{Project: "gone-prod", Service: "migrate", State: "exited", Released: true, OneOff: true},
			{Project: "tools", Service: "x", State: "running"},
		},
		GC: &remote.GCFacts{OneOffs: 2, Builders: 1, Images: 3, Size: 2e9},
		Images: []remote.HostImage{
			{ID: "i1", Tags: []string{"app:devopsy-shop-prod-20261101000000", "app:devopsy-shop-stage-20261101000000"}, Containers: 1},
			{ID: "i2", Tags: []string{"app:devopsy-shop-stage-2-20261101000000", "app:devopsy-shop-stage-2-s20261102000000-a1"}},
			{ID: "i3", Tags: []string{"app:devopsy-wiki-stage-20261101000000", longTag, "devopsy.internal/news-prod/app:latest"}},
			{ID: "i4", Tags: []string{"devopsy.internal/gone-prod/web:latest", "app:devopsy-blog-claim.abababababab"}},
			{ID: "i5", Tags: []string{"busybox:1.37"}, Size: 4e6},
			{ID: "i6", Tags: []string{"nginx:1.27"}, Containers: 1},
			{ID: "i7", Tags: []string{remote.UtilImage}},
		},
		Volumes: []remote.HostVolume{
			{Name: "shop-prod_db", Project: "shop-prod", Links: 1},
			{Name: "gone-prod_db", Project: "gone-prod", Size: 1.2e9},
			{Name: "tools_data", Project: "tools"},
			{Name: "a1", Anonymous: true, Size: 5e6}, {Name: "a2", Anonymous: true, Size: 5e6}, {Name: "a3", Anonymous: true, Links: 1},
		},
		SnapshotDirs: []string{"shop/prod", "old/prod"},
		Removing:     []remote.RemovingDir{{Path: "shop/eu/old.rm.123", Size: 3e6}},
		Door:         &remote.DoorFacts{Space: "/space", Users: 1, Keyless: []remote.KeylessUser{{User: "devopsy-blog", Project: "blog", Names: []string{"blog-dev"}}}},
	}
	want := []string{
		"gc: --gc would remove up to 2.0 GB: 2 one-off containers, 1 builder's build cache, 3 images\n| list it, then remove it:\n$ devopsy @vm1: --gc",
		"images: busybox:1.37, 4.0 MB: no release holds it, nor any container uses it\n| remove it once nothing needs it, on the server:\n$ devopsy @vm1: --shell-host\n$ docker rmi busybox:1.37",
		"gone: gone-prod: no environment and no uid claim on the server, and left:\ncontainers web (exited)\nvolumes gone-prod_db 1.2 GB\n1 image name of its releases\n" +
			"| an environment removed by hand: remove what is left once none of it is needed, on the server:\n$ devopsy @vm1: --shell-host\n$ docker compose -p gone-prod down\n$ docker volume rm gone-prod_db\n$ docker rmi devopsy.internal/gone-prod/web:latest",
		"gone: old-prod: no environment and no uid claim on the server, and left:\nsnapshots in /backups/old/prod\n" +
			"| an environment removed by hand: remove what is left once none of it is needed, on the server:\n$ devopsy @vm1: --shell-host\n$ rm -rf /backups/old/prod",
		"gone: shop-stage-2: no environment and no uid claim on the server, and left:\n2 image names of its releases\n" +
			"| an environment removed by hand: remove what is left once none of it is needed, on the server:\n$ devopsy @vm1: --shell-host\n$ docker rmi app:devopsy-shop-stage-2-20261101000000 app:devopsy-shop-stage-2-s20261102000000-a1",
		"gone: wiki-stage: no environment and no uid claim on the server, and left:\n1 image name of its releases\n" +
			"| an environment removed by hand: remove what is left once none of it is needed, on the server:\n$ devopsy @vm1: --shell-host\n$ docker rmi app:devopsy-wiki-stage-20261101000000",
		"volumes: 2 anonymous volumes no container uses, 10.0 MB (an image's VOLUME after a down without -v: a database's data, maybe)\n| once checked that none holds what is needed, remove them, on the server:\n$ devopsy @vm1: --shell-host\n$ docker volume prune",
		"environments: shop/eu/old.rm.123: left by a --destroy cut off while it removed shop/eu/old, 3.0 MB\n| run it again, which resumes it, in shop's checkout:\n$ devopsy @devopsy@vm1:eu/old --destroy",
		"door: devopsy-blog has no key and still holds blog-dev\n| remove it with what it holds:\n$ devopsy @vm1: --project-users remove blog --destroy\n| or a key added for it (--ssh-keys) takes it back",
	}
	if got := leftSays(info, "devopsy@vm1", "vm1:"); !slices.Equal(got, want) {
		t.Errorf("left:\n%s\n\nwant:\n%s", strings.Join(got, "\n\n"), strings.Join(want, "\n\n"))
	}

	// Locally, what needs an address in words, the commands run here.
	local := strings.Join(leftSays(info, "", ""), "\n\n")
	for _, w := range []string{
		"| --gc, run on this server from a checkout or an alias, lists and removes it",
		"| remove it once nothing needs it:\n$ docker rmi busybox:1.37",
		"| a --destroy of shop/eu/old run again from shop's checkout resumes it",
		"| removing blog's project user with --project-users remove and --destroy removes them with it; a key added for it (--ssh-keys) takes it back",
	} {
		if !strings.Contains(local, w) {
			t.Errorf("local: no %q in:\n%s", w, local)
		}
	}
	if strings.Contains(local, "--shell-host") {
		t.Errorf("local: through --shell-host:\n%s", local)
	}

	// What goes by an absence is made only when the release root lists environments, all read,
	// and the claims read: with the root's disk not mounted, every environment reads as gone.
	gone := func(i remote.ServerInfo) bool {
		return slices.ContainsFunc(leftSays(i, "devopsy@vm1", "vm1:"), func(s string) bool { return strings.HasPrefix(s, "gone: ") })
	}
	for name, change := range map[string]func(*remote.ServerInfo){
		"no root environment": func(i *remote.ServerInfo) { i.Environments = i.Environments[4:] },
		"environments unread": func(i *remote.ServerInfo) { i.EnvironmentsUnread = "permission denied" },
		"claims unread":       func(i *remote.ServerInfo) { i.UIDs = []remote.UIDClaim{{Name: i.UIDsDir, Bad: "permission denied"}} },
		"docker down":         func(i *remote.ServerInfo) { i.Docker.Error = "docker info: Cannot connect" },
	} {
		i := info
		change(&i)
		if gone(i) {
			t.Errorf("%s: a name reported gone", name)
		}
	}
	if !gone(info) {
		t.Errorf("nothing gone")
	}
	// A read that failed is unknown, said, never none.
	i := info
	i.ImagesUnread, i.VolumesUnread = "docker system df: EOF", "docker system df: EOF"
	says := strings.Join(leftSays(i, "devopsy@vm1", "vm1:"), "\n")
	if !strings.Contains(says, "images: the images could not be listed: docker system df: EOF") || !strings.Contains(says, "volumes: the volumes could not be listed") ||
		strings.Contains(says, "busybox") || strings.Contains(says, "anonymous") || strings.Contains(says, "image name") {
		t.Errorf("unread:\n%s", says)
	}
}

// A project user with no key holds its environment claims' names and its containers' under its
// names (never another project's that extends them), each once; with the server's projects not
// read, its claims alone.
func TestProjectHolds(t *testing.T) {
	t.Parallel()
	p := &door.Policy{Project: "blog", User: "devopsy-blog", Others: []string{"blog", "blog-x", "shop"}}
	owners := []string{"env:blog:prod", "env:blog:eu:prod", "project:blog", "env:shop:prod", "env:blog:prod"}
	projects := []string{"blog-dev", "blog-x-prod", "shop-prod", "", "blog-prod"}
	if got := projectHolds("blog", p, owners, projects); !slices.Equal(got, []string{"blog-dev", "blog-eu-prod", "blog-prod"}) {
		t.Errorf("holds %v", got)
	}
	if got := projectHolds("blog", nil, owners, projects); !slices.Equal(got, []string{"blog-eu-prod", "blog-prod"}) {
		t.Errorf("without the projects: %v", got)
	}
}

// What a cut --destroy left is a <base>.rm.<pid> beside an environment (an instance's too)
// whose devopsy is gone; one whose devopsy runs is a removal under way; a project user's tree
// and what is no such directory are none.
func TestCutDestroys(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, d := range []string{
		"shop/prod/releases", "shop/old.rm.999999999/releases", "shop/eu/gone.rm.999999998/data",
		"shop/now.rm." + strconv.Itoa(os.Getpid()) + "/releases", "shop/old.rm.x/releases", "blog/dev.rm.999999997",
	} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(root, "shop", "old.rm.999999999", "releases", "f"), "12345", 0o644)
	got := cutDestroys(root, map[string]string{filepath.Join(root, "blog"): "devopsy-blog"})
	want := []remote.RemovingDir{{Path: "shop/eu/gone.rm.999999998"}, {Path: "shop/old.rm.999999999", Size: 5}}
	if !slices.Equal(got, want) {
		t.Errorf("cut destroys %+v, want %+v", got, want)
	}
}

// The snapshot root's environments are its directories that hold a snapshot or the mirror,
// an instance's too.
func TestSnapshotEnvDirs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, d := range []string{"shop/prod/20261101000000", "shop/eu/prod/" + remote.SnapshotMirror, "shop/empty", "blog/dev/notes"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if got := snapshotEnvDirs(root); !slices.Equal(got, []string{"shop/eu/prod", "shop/prod"}) {
		t.Errorf("snapshot dirs %v", got)
	}
}
