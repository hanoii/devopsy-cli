package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// leftSays is each entry of what may be dead or gone as one text, a project's first by
// project: its project and its key, its lines, then each remedy's words and commands.
func leftSays(info remote.ServerInfo, host, ref string) []string {
	var out []string
	groups, leftovers := leftEntries(info, host, ref)
	var es []remote.StatusEntry
	for _, p := range slices.Sorted(maps.Keys(groups)) {
		es = append(es, groups[p]...)
	}
	for _, e := range append(es, leftovers...) {
		s := e.Key + ": " + strings.Join(e.Lines, "\n")
		if e.Group != "" {
			s = e.Group + " " + s
		}
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
// --destroy's directory, and a project user with no key that holds environments. Without a
// target the hints say in words what needs an address. A read that failed (the images, the
// volumes, what --gc's count could not read, the containers an image's use is counted from) is
// said or leaves its lines out, never taken for none.
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
		GC: &remote.GCFacts{OneOffs: 2, Left: 1, Dangling: 3},
		Images: []remote.HostImage{
			{ID: "i1", Tags: []string{"app:devopsy-shop-prod-20261101000000", "app:devopsy-shop-stage-20261101000000"}, Containers: 1},
			{ID: "i2", Tags: []string{"app:devopsy-shop-stage-2-20261101000000", "app:devopsy-shop-stage-2-s20261102000000-a1"}},
			{ID: "i3", Tags: []string{"app:devopsy-wiki-stage-20261101000000", longTag, "devopsy.internal/news-prod/app:latest"}},
			{ID: "i4", Tags: []string{"devopsy.internal/gone-prod/web:latest", "app:devopsy-blog-claim.abababababab"}},
			{ID: "i5", Tags: []string{"busybox:1.37"}, Size: 4e6},
			{ID: "i6", Tags: []string{"nginx:1.27"}, Containers: 1},
			{ID: "i7", Tags: []string{remote.UtilImage}},
			{ID: "i8", Tags: []string{"alpine:3", "alpine:3.22"}, Size: 8e6},
		},
		Volumes: []remote.HostVolume{
			{Name: "shop-prod_db", Project: "shop-prod"},
			{Name: "gone-prod_db", Project: "gone-prod", Unused: true},
			{Name: "tools_data", Project: "tools"},
			{Name: "a1", Anonymous: true, Unused: true}, {Name: "a2", Anonymous: true, Unused: true}, {Name: "a3", Anonymous: true},
		},
		SnapshotDirs: []string{"shop/prod", "old/prod"},
		Removing:     []remote.RemovingDir{{Path: "shop/eu/old.rm.123", Size: 3e6}},
		Door:         &remote.DoorFacts{Space: "/space", Users: 1, Keyless: []remote.KeylessUser{{User: "devopsy-blog", Project: "blog", Held: []string{"the uid claims env:blog:dev", "the volumes blog-dev_db"}}}},
	}
	gone := "| an environment removed by hand: remove what is left once none of it is needed, on the server:\n$ devopsy @vm1: --shell-host\n"
	want := []string{
		// Under its project's heading, never a key that widens every other.
		"blog blog: devopsy-blog has no key and still holds:\nthe uid claims env:blog:dev\nthe volumes blog-dev_db\n| remove it with what it holds:\n$ devopsy @vm1: --project-users remove blog --destroy\n| or a key added for it (--ssh-keys with --for) takes it back",
		// A name of no project on the server is a project of its own, its name the line above.
		"gone-prod : gone, no environment and no uid claim on the server, and left: containers web (exited), volumes gone-prod_db, 1 image name of its releases\n" +
			gone + "$ docker compose -p gone-prod down\n$ docker volume rm gone-prod_db\n$ docker rmi devopsy.internal/gone-prod/web:latest",
		// A claim whose environment is gone is no live name: what is left of it is looked for,
		// its claim said with it and removed last.
		"news prod: gone, no environment on the server, and left: its uid claim 2, 1 image name of its releases\n" +
			gone + "$ docker rmi devopsy.internal/news-prod/app:latest\n$ rm /home/devopsy/.devopsy-uids/env:news:prod",
		"old-prod : gone, no environment and no uid claim on the server, and left: snapshots in /backups/old/prod\n" + gone + "$ rm -rf /backups/old/prod",
		// Under the project its name starts with, the longest: shop's, though wiki has a stage-2.
		"shop stage-2: gone, no environment and no uid claim on the server, and left: 2 image names of its releases\n" +
			gone + "$ docker rmi app:devopsy-shop-stage-2-20261101000000 app:devopsy-shop-stage-2-s20261102000000-a1",
		"shop eu/old: a --destroy cut off while it removed it left old.rm.123, 3.0 MB\n| run it again, which resumes it, in shop's checkout:\n$ devopsy @devopsy@vm1:eu/old --destroy",
		"wiki stage: gone, no environment and no uid claim on the server, and left: 1 image name of its releases\n" + gone + "$ docker rmi app:devopsy-wiki-stage-20261101000000",
		"gc: --gc would remove 2 one-off containers, 1 image name a project's record left, 3 dangling images\n| its sizes, and the build cache it removes too:\n$ devopsy @vm1: --gc --dry-run",
		// One row each, then one line for all and one rmi of every tag, to copy at once.
		"images: busybox:1.37  4.0 MB",
		"images: alpine:3, alpine:3.22  8.0 MB",
		"images: no release holds them, nor any container uses them\n| remove them once nothing needs them, on the server:\n$ devopsy @vm1: --shell-host\n$ docker rmi busybox:1.37 alpine:3 alpine:3.22",
		"volumes: 2 anonymous volumes no container uses (an image's VOLUME after a down without -v: a database's data, maybe)\n| once checked that none holds what is needed, remove them, on the server:\n$ devopsy @vm1: --shell-host\n$ docker volume prune",
	}
	if got := leftSays(info, "devopsy@vm1", "vm1:"); !slices.Equal(got, want) {
		t.Errorf("left:\n%s\n\nwant:\n%s", strings.Join(got, "\n\n"), strings.Join(want, "\n\n"))
	}

	// A gone name too long for a key is said in its line, under gone: a key widens every other.
	long2 := info
	long2.Containers = append(slices.Clone(info.Containers), remote.HostContainer{Project: "shop-a-very-long-preview", Service: "web", State: "exited", Released: true})
	if got := strings.Join(leftSays(long2, "devopsy@vm1", "vm1:"), "\n\n"); !strings.Contains(got, "\n\nshop gone: a-very-long-preview: no environment and no uid claim on the server, and left: containers web (exited)\n") {
		t.Errorf("a long gone name:\n%s", got)
	}

	// Without a target, what needs an address in words, the commands run here.
	local := strings.Join(leftSays(info, "", ""), "\n\n")
	for _, w := range []string{
		"| --gc with --dry-run, run on this server from a checkout or an alias, lists it with its sizes, and the build cache it removes too",
		"| remove them once nothing needs them:\n$ docker rmi busybox:1.37 alpine:3 alpine:3.22",
		"| a --destroy of shop/eu/old run again from shop's checkout resumes it",
		"| a --project-users remove of blog with --destroy removes it with what it holds\n| or a key added for it (--ssh-keys with --for) takes it back",
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
	// Found by what is left of it; a claim left is said whatever reads (TestServerInfoGoneClaimUnlookedStaysSaid).
	isGone := func(i remote.ServerInfo) bool {
		return slices.ContainsFunc(leftSays(i, "devopsy@vm1", "vm1:"), func(s string) bool {
			return strings.Contains(s, ", and left: ") || strings.Contains(s, "only its uid claim")
		})
	}
	for name, change := range map[string]func(*remote.ServerInfo){
		"no root environment": func(i *remote.ServerInfo) { i.Environments = i.Environments[4:] },
		"environments unread": func(i *remote.ServerInfo) { i.EnvironmentsUnread = "permission denied" },
		"claims unread":       func(i *remote.ServerInfo) { i.UIDs = []remote.UIDClaim{{Name: i.UIDsDir, Bad: "permission denied"}} },
		"docker down":         func(i *remote.ServerInfo) { i.Docker.Error = "docker info: Cannot connect" },
	} {
		i := info
		change(&i)
		if isGone(i) {
			t.Errorf("%s: a name reported gone", name)
		}
	}
	if !isGone(info) {
		t.Errorf("nothing gone")
	}
	// A read that failed is unknown, said, never none.
	i := info
	i.ImagesUnread, i.VolumesUnread = "docker image ls: EOF", "docker volume ls: EOF"
	says := strings.Join(leftSays(i, "devopsy@vm1", "vm1:"), "\n")
	if !strings.Contains(says, "images: the images could not be listed: docker image ls: EOF") || !strings.Contains(says, "volumes: the volumes could not be listed") ||
		strings.Contains(says, "busybox") || strings.Contains(says, "anonymous") || strings.Contains(says, "of its releases") {
		t.Errorf("unread:\n%s", says)
	}
	// What --gc's count could not read is a warning under it, with something counted or not:
	// the count is not all there is.
	for name, gc := range map[string]*remote.GCFacts{
		"with something counted": {OneOffs: 1, Unread: []string{"the dangling images: docker image ls: EOF"}},
		"with nothing counted":   {Unread: []string{"the dangling images: docker image ls: EOF"}},
	} {
		i := info
		i.GC = gc
		if says := strings.Join(leftSays(i, "devopsy@vm1", "vm1:"), "\n"); !strings.Contains(says, "gc: --gc's count could not read the dangling images: docker image ls: EOF") {
			t.Errorf("%s: what --gc's count could not read is said nowhere:\n%s", name, says)
		}
	}
	// With the containers unread, no image is said to be used by none: each image's count of
	// containers comes from them.
	i = info
	i.ContainersUnread = "docker inspect: EOF"
	if says := strings.Join(leftSays(i, "devopsy@vm1", "vm1:"), "\n"); strings.Contains(says, "nor any container uses") {
		t.Errorf("with the containers unread, an image is used by none:\n%s", says)
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

// --server-info counts what --gc would remove and sizes nothing of Docker's but its images, as
// monitors run it on a timer: no docker system df -v (it sizes every volume), no inspect --size,
// no builder's cache (buildx, buildctl), no image inspect for a size, no second ps or inspect of
// the containers (the one-offs and the images in use are hostContainers' read), and no walk of a
// release snapshot. An exited one-off of a listed environment past the age, a release snapshot
// whose release is gone and a dangling image no container uses are counted; an image's size
// comes from docker image ls, a volume no container uses from docker volume ls.
func TestServerFactsCountGC(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	base := filepath.Join(root, "shop", "prod")
	write(t, filepath.Join(base, "releases", "20261101000000", "code", "x"), "", 0o644)
	write(t, filepath.Join(base, "snapshots", "20261020000001", remote.SnapshotManifest), "format\t1\nkind\trelease\nrelease\t20261001000000\n", 0o600)
	bin, log := t.TempDir(), filepath.Join(t.TempDir(), "docker.log")
	write(t, filepath.Join(bin, "docker"), `#!/bin/sh
echo "$*" >>`+log+`
case "$*" in
  'info --format '*) echo '{"ServerVersion":"29.9.1","Driver":"overlayfs","DockerRootDir":"/var/lib/docker"}' ;;
  'ps -aq --no-trunc') echo c1; echo c2 ;;
  'inspect --format '*) echo 'exited 0 - sha256:old "2026-01-01T00:00:00Z" {"com.docker.compose.project":"shop-prod","com.docker.compose.oneoff":"True"}'
    echo 'running 0 - sha256:d2 "0001-01-01T00:00:00Z" {"com.docker.compose.project":"shop-prod"}' ;;
  'image ls -q --no-trunc --filter dangling=true') echo sha256:d1; echo sha256:d2 ;;
  'image ls --no-trunc --format {{.ID}} {{.Repository}}:{{.Tag}} {{.Size}}') echo 'sha256:t1 busybox:1.37 4.26MB'; echo 'sha256:d2 <none>:<none> 1MB' ;;
  'volume ls --format '*) echo '["a1","com.docker.volume.anonymous="]'; echo '["shop-prod_db","com.docker.compose.project=shop-prod"]' ;;
  'volume ls -q --filter dangling=true') echo a1 ;;
esac
exit 0
`, 0o755)
	cmd := exec.Command(binary, remote.ServerFactsFlag)
	cmd.Dir = root
	cmd.Env = []string{"HOME=" + root, "DEVOPSY_HOME=" + t.TempDir(), remote.HostRootEnv + "=" + t.TempDir(), "PATH=" + bin + ":" + fakeBin(t) + ":/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("--server-facts: %v", err)
	}
	var info remote.ServerInfo
	if err := json.Unmarshal(out, &info); err != nil || info.GC == nil {
		t.Fatalf("--server-facts: %v:\n%s", err, out)
	}
	if g := *info.GC; g.OneOffs != 1 || g.Snapshots != 1 || g.Dangling != 1 || g.Left != 0 || len(g.Unread) > 0 {
		t.Errorf("gc facts %+v", g)
	}
	if len(info.Images) != 1 || info.Images[0].Size != 4260000 || info.Images[0].Containers != 0 {
		t.Errorf("images %+v", info.Images)
	}
	if !slices.Equal(info.Volumes, []remote.HostVolume{{Name: "a1", Anonymous: true, Unused: true}, {Name: "shop-prod_db", Project: "shop-prod"}}) {
		t.Errorf("volumes %+v", info.Volumes)
	}
	calls := readString(log)
	for _, costly := range []string{"system df", "--size", "buildx ls", "buildx du", "buildctl", "image inspect", "label=com.docker.compose.oneoff", "container inspect"} {
		if strings.Contains(calls, costly) {
			t.Errorf("--server-facts ran %q:\n%s", costly, calls)
		}
	}
	if n := strings.Count(calls, "ps -aq --no-trunc\n"); n != 1 {
		t.Errorf("%d listings of the containers:\n%s", n, calls)
	}
}

// A claim whose environment is gone is no live name: what else of it is left (its containers)
// is said with it, never "only its uid claim".
func TestServerInfoGoneClaimSaysWhatIsLeft(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	info := remote.ServerInfo{
		Environments: []remote.Environment{{Path: "shop/prod"}},
		UIDs:         []remote.UIDClaim{{Owner: "env:shop:pr-1", UID: 1000002, Name: "1000002"}},
		Containers:   []remote.HostContainer{{Project: "shop-pr-1", Service: "web", State: "exited", Released: true}},
	}
	out, _ := serverInfoSays(info, "vm1", "vm1:", "0.1.0", now)
	if !strings.Contains(out, "\n    pr-1     gone, no environment on the server, and left: its uid claim 1000002, containers web (exited)\n") || strings.Contains(out, "only its uid claim") {
		t.Errorf("shop-pr-1's row, its claim and its container:\n%s", out)
	}
}

// A claim whose environment is gone never goes unsaid: when what else is left cannot be looked
// for (no environment listed, Docker down), it is a gone row of its own with its removal,
// never "only its uid claim", since nothing else was looked for.
func TestServerInfoGoneClaimUnlookedStaysSaid(t *testing.T) {
	t.Parallel()
	base := remote.ServerInfo{
		UIDsDir: "/home/devopsy/.devopsy-uids",
		UIDs:    []remote.UIDClaim{{Owner: "env:shop:pr-1", UID: 1000002, Name: "env:shop:pr-1"}},
	}
	for name, c := range map[string]struct {
		change func(*remote.ServerInfo)
		why    string
	}{
		"no environment listed": {func(*remote.ServerInfo) {}, "the release root lists no environment"},
		"docker down": {func(i *remote.ServerInfo) {
			i.Environments, i.Docker.Error = []remote.Environment{{Path: "shop/prod"}}, "docker info: Cannot connect"
		}, "Docker did not answer"},
	} {
		i := base
		c.change(&i)
		want := "shop pr-1: gone, its uid claim 1000002 is left; what else is left was not looked for: " + c.why +
			"\n| remove it by hand once nothing of it is left, on the server:\n$ devopsy @vm1: --shell-host\n$ rm /home/devopsy/.devopsy-uids/env:shop:pr-1"
		if got := leftSays(i, "devopsy@vm1", "vm1:"); !slices.Contains(got, want) {
			t.Errorf("%s: no row of the claim:\n%s", name, strings.Join(got, "\n\n"))
		}
	}
}
