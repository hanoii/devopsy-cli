package images

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// fakeDocker is Docker's image store as the rules see it: names (tags
// and digests) pointing to images, containers pinning them. Removing a
// name untags it while the image has another; the last one deletes the
// image, refused while a container, even a stopped one, uses it. Names
// are kept and shown as Docker shows them (docker.io/library/nginx is
// nginx). fail, when set, makes the next call of that method fail.
type fakeDocker struct {
	images     map[string]bool
	names      map[string]string
	containers []container
	fail       map[string]bool
}

func (d *fakeDocker) failing(method string) error {
	if d.fail[method] {
		delete(d.fail, method)
		return fmt.Errorf("Cannot connect to the Docker daemon")
	}
	return nil
}

type container struct{ image, project string }

func newFake() *fakeDocker {
	return &fakeDocker{images: map[string]bool{}, names: map[string]string{}}
}

func (d *fakeDocker) ID(ref string) (string, error) {
	if err := d.failing("ID"); err != nil {
		return "", err
	}
	if id, ok := d.names[familiar(ref)]; ok {
		return id, nil
	}
	if d.images[ref] {
		return ref, nil
	}
	return "", ErrNoImage
}

func (d *fakeDocker) Tags(id string) ([]string, error) {
	if err := d.failing("Tags"); err != nil {
		return nil, err
	}
	if !d.images[id] {
		return nil, ErrNoImage
	}
	var ts []string
	for n, i := range d.names {
		if i == id && !strings.Contains(n, "@") {
			ts = append(ts, n)
		}
	}
	sort.Strings(ts)
	return ts, nil
}

func (d *fakeDocker) Tag(src, dst string) error {
	id, err := d.ID(src)
	if err != nil {
		return err
	}
	d.names[familiar(dst)] = id
	return nil
}

func (d *fakeDocker) Remove(ref string) error {
	ref = familiar(ref)
	id, ok := d.names[ref]
	if !ok {
		return ErrNoImage
	}
	n := 0
	for _, i := range d.names {
		if i == id {
			n++
		}
	}
	if n == 1 {
		for _, c := range d.containers {
			if c.image == id {
				return fmt.Errorf("conflict: unable to delete %s (must be forced) - container is using its referenced image %s", ref, id)
			}
		}
		delete(d.images, id)
	}
	delete(d.names, ref)
	return nil
}

func (d *fakeDocker) Users(id string) ([]string, error) {
	if err := d.failing("Users"); err != nil {
		return nil, err
	}
	var us []string
	for _, c := range d.containers {
		if c.image == id {
			us = append(us, c.project)
		}
	}
	return us, nil
}

func (d *fakeDocker) Prune() (string, error) {
	n := 0
outer:
	for id := range d.images {
		for _, i := range d.names {
			if i == id {
				continue outer
			}
		}
		for _, c := range d.containers {
			if c.image == id {
				continue outer
			}
		}
		delete(d.images, id)
		n++
	}
	if n == 0 {
		return "0B", nil
	}
	return fmt.Sprintf("%dMB", n), nil
}

// pull points ref at a new image, or at an existing one.
func (d *fakeDocker) pull(ref, id string) {
	d.images[id] = true
	d.names[familiar(ref)] = id
}

func TestCksum(t *testing.T) {
	if _, err := exec.LookPath("cksum"); err != nil {
		t.Skip("no cksum")
	}
	for _, s := range []string{"postgres:15", "x@sha256:abc", "registry:5000/app:1", ""} {
		cmd := exec.Command("cksum")
		cmd.Stdin = strings.NewReader(s)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		if want, got := strings.Fields(string(out))[0], fmt.Sprint(cksum([]byte(s))); got != want {
			t.Errorf("cksum(%q) = %s, want %s", s, got, want)
		}
	}
}

func TestBookmarkPrefix(t *testing.T) {
	if got := bookmarkPrefix("app-prod"); got != "devopsy-app-prod-" {
		t.Errorf("short: %q", got)
	}
	if got := bookmarkPrefix(""); got != "devopsy-" {
		t.Errorf("empty: %q", got)
	}
	long1 := strings.Repeat("a", 94)
	if bookmarkPrefix(long1) != "devopsy-"+long1+"-" {
		t.Errorf("94 characters are kept")
	}
	long2, long3 := strings.Repeat("b", 120)+"x", strings.Repeat("b", 120)+"y"
	p2, p3 := bookmarkPrefix(long2), bookmarkPrefix(long3)
	if p2 == p3 || len(p2) != len("devopsy-")+85+1+8+1 {
		t.Errorf("long names: %q %q", p2, p3)
	}
	// The longest tag: prefix, a 14-digit id, - and a 10-digit cksum.
	if n := len(p2) + 14 + 1 + 10; n > 128 {
		t.Errorf("a tag can reach %d characters", n)
	}
	if bookmarkPrefix(long2) != p2 {
		t.Errorf("not stable")
	}
}

func TestRepo(t *testing.T) {
	for ref, want := range map[string]string{
		"nginx":                     "nginx",
		"nginx:1.27":                "nginx",
		"registry:5000/app":         "registry:5000/app",
		"registry:5000/app:abc":     "registry:5000/app",
		"x@sha256:abc":              "x",
		"x:1@sha256:abc":            "x",
		"docker.io/library/nginx:1": "docker.io/library/nginx",
	} {
		if got := repoOf(ref); got != want {
			t.Errorf("repoOf(%q) = %q, want %q", ref, got, want)
		}
	}
	if repoKey("docker.io/library/nginx:1") != "nginx" || repoKey("library/nginx") != "nginx" || repoKey("index.docker.io/x/y") != "x/y" {
		t.Errorf("repoKey does not resolve Docker Hub's names")
	}
}

// env is one environment of the model: its releases directory and the
// listing each release's fetch gives.
type env struct {
	set     *Set
	dir     string
	lists   map[string][]string
	current string
	n       int
}

func newEnv(t *testing.T, d *fakeDocker, name, left string, out *bytes.Buffer) *env {
	e := &env{dir: filepath.Join(t.TempDir(), name, "releases"), lists: map[string][]string{}}
	e.set = &Set{Docker: d, Name: name, Left: left, Out: out, List: func(rel string) ([]string, error) {
		return e.lists[filepath.Base(rel)], nil
	}}
	return e
}

func (e *env) rel(id string) string { return filepath.Join(e.dir, id) }

func (e *env) newRelease(refs ...string) string {
	e.n++
	id := fmt.Sprintf("2026101600%04d", e.n)
	if err := os.MkdirAll(e.rel(id), 0o755); err != nil {
		panic(err)
	}
	e.lists[id] = refs
	return id
}

// run gives the environment's containers the images its names point to.
func (e *env) run(d *fakeDocker) {
	var cs []container
	for _, c := range d.containers {
		if c.project != e.set.Name {
			cs = append(cs, c)
		}
	}
	for _, r := range e.lists[e.current] {
		if id, err := d.ID(r); err == nil {
			cs = append(cs, container{id, e.set.Name})
		}
	}
	d.containers = cs
}

func (e *env) releases() []string {
	dirs, _ := os.ReadDir(e.dir)
	var ids []string
	for _, d := range dirs {
		ids = append(ids, d.Name())
	}
	return ids
}

// Two environments sharing mariadb:11 (one sometimes as its alias
// mariadb:11.4) release, fail, roll back and remove releases at random.
// At every step: every kept release's bookmarks point to the images it
// recorded, a restore never moves a name another environment's container
// uses, and once every release and container is gone, no image is left.
func TestRandomHistory(t *testing.T) {
	for seed := int64(1); seed <= 200; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) { randomHistory(t, seed) })
	}
}

func randomHistory(t *testing.T, seed int64) {
	rng := rand.New(rand.NewSource(seed))
	d := newFake()
	var out bytes.Buffer
	left := filepath.Join(t.TempDir(), LeftFile)
	envs := []*env{newEnv(t, d, "app-a", left, &out), newEnv(t, d, "app-b", left, &out)}
	img := 0
	newImage := func() string { img++; return fmt.Sprintf("sha256:%04d", img) }
	var log []string
	gone := map[string]bool{} // bookmarks removed by hand
	check := func(step string) {
		t.Helper()
		log = append(log, step)
		for _, e := range envs {
			for _, id := range e.releases() {
				es := readList(e.rel(id))
				for i, b := range bookmarks(es, e.set.Name, id) {
					if es[i].ID == "" || gone[b] {
						continue
					}
					if got, _ := d.ID(b); got != es[i].ID {
						t.Fatalf("after %v: %s's bookmark %s is %q, want %q\n%s", log, id, b, got, es[i].ID, out.String())
					}
				}
			}
		}
		for _, c := range d.containers {
			if !d.images[c.image] {
				t.Fatalf("after %v: a container's image %s is gone", log, c.image)
			}
		}
	}
	for step := 0; step < 40; step++ {
		e := envs[rng.Intn(2)]
		other := envs[0]
		if e == other {
			other = envs[1]
		}
		// app-a writes the shared name as Docker Hub's full one: the same
		// name to Docker.
		shared := "docker.io/library/mariadb:11"
		if e.set.Name == "app-b" {
			shared = "mariadb:11"
			switch rng.Intn(4) {
			case 0:
				shared = "mariadb:11.4"
			case 1: // the same image from a mirror: another repository
				shared = "mirror.example/mariadb:11"
			}
		}
		own := e.set.Name + "-app"
		switch op := rng.Intn(9); op {
		case 8: // a container outside Compose on the shared name's image
			if i, err := d.ID("mariadb:11"); err == nil {
				d.containers = append(d.containers, container{i, ""})
				check("outside container on " + i)
			}
		case 7: // taken down (devopsy down): no containers, current stays
			var cs []container
			for _, c := range d.containers {
				if c.project != e.set.Name {
					cs = append(cs, c)
				}
			}
			d.containers = cs
			check(e.set.Name + " down")
		case 0, 1: // a release: builds its app, pulls the shared name
			refs := []string{own, shared}
			if rng.Intn(4) == 0 { // a repository named twice: suffixed bookmarks
				refs = append(refs, "postgres:15", "postgres:16")
				d.pull("postgres:15", newImage())
				d.pull("postgres:16", newImage())
			}
			id := e.newRelease(refs...)
			d.pull(own, newImage())
			switch {
			case shared == "mariadb:11.4" || shared == "mirror.example/mariadb:11":
				if cur, err := d.ID("mariadb:11"); err == nil && rng.Intn(2) == 0 {
					d.pull(shared, cur) // the same image, another name
				} else {
					d.pull(shared, newImage())
				}
			case rng.Intn(2) == 0:
				d.pull(shared, newImage())
			default:
				if _, err := d.ID(shared); err != nil {
					d.pull(shared, newImage())
				}
			}
			e.set.Bookmark(e.rel(id))
			e.current = id
			e.run(d)
			if rng.Intn(4) == 0 { // a one-off left behind
				if i, err := d.ID(own); err == nil {
					d.containers = append(d.containers, container{i, e.set.Name})
				}
			}
			check(e.set.Name + " release " + id)
		case 2: // a release whose fetch fails halfway: undone
			id := e.newRelease(own, shared)
			d.pull(shared, newImage())
			prev := ""
			if e.current != "" {
				prev = e.rel(e.current)
			}
			before := usedNames(d, other)
			e.set.Undo(e.rel(id), prev, false)
			if err := os.WriteFile(filepath.Join(e.rel(id), failedFile), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			assertKept(t, d, before, log, &out)
			check(e.set.Name + " failed fetch " + id)
		case 3: // a rollback to the release before current
			ids := e.releases()
			var target string
			for _, id := range ids {
				if id < e.current && !exists(filepath.Join(e.rel(id), failedFile)) {
					target = id
				}
			}
			if target == "" {
				continue
			}
			before := usedNames(d, other)
			shared := sharedNames(d, other)
			if e.set.Restore(e.rel(target)) {
				assertKept(t, d, before, log, &out)
				// Exact, but for the names another environment uses.
				es := readList(e.rel(target))
				for i, b := range bookmarks(es, e.set.Name, target) {
					if strings.Contains(es[i].Ref, "@") {
						continue
					}
					want, _ := d.ID(b)
					if got := d.names[familiar(es[i].Ref)]; got != want && shared[familiar(es[i].Ref)] == "" {
						t.Fatalf("after %v: rolling back to %s left %s on %s, want %s\n%s", log, target, es[i].Ref, got, want, out.String())
					}
				}
			} else {
				// Fetched again, sometimes listing another repository (a variable
				// changed since): its old bookmarks must not linger.
				if rng.Intn(2) == 0 {
					e.lists[target] = []string{own, "postgres:16"}
				}
				for _, r := range e.lists[target] {
					d.pull(r, newImage())
				}
				e.set.Bookmark(e.rel(target))
				for _, b := range bookmarks(readList(e.rel(target)), e.set.Name, target) {
					delete(gone, b)
				}
			}
			e.current = target
			e.run(d)
			check(e.set.Name + " rollback " + target)
		case 4: // a release removed (pruned or by hand), never the current one
			for _, id := range e.releases() {
				if id != e.current {
					// Names whose image another release's bookmark holds, or a
					// container uses, stay.
					held := map[string]string{}
					for n, i := range d.names {
						for _, c := range d.containers {
							if c.image == i && !isBookmark(n) {
								held[n] = i
							}
						}
					}
					for n, i := range d.names {
						if isBookmark(n) {
							continue
						}
						tags, _ := d.Tags(i)
						for _, tg := range tags {
							if isBookmark(tg) && !strings.HasSuffix(tg, "-"+id) && !strings.Contains(tg, "-"+id+"-") {
								held[n] = i
							}
						}
					}
					e.set.Remove(e.rel(id))
					for n, i := range held {
						if d.names[n] != i {
							t.Fatalf("after %v: removing %s took %s, which another release or a container holds\n%s", log, id, n, out.String())
						}
					}
					os.RemoveAll(e.rel(id))
					e.set.Prune()
					check(e.set.Name + " remove " + id)
					break
				}
			}
		case 6: // a kept release's bookmark removed by hand: its rollback fetches again
			for _, id := range e.releases() {
				if id == e.current {
					continue
				}
				bs := bookmarks(readList(e.rel(id)), e.set.Name, id)
				if len(bs) > 0 && e.set.Docker.Remove(bs[0]) == nil {
					gone[bs[0]] = true
					check(e.set.Name + " bookmark removed " + bs[0])
				}
				break
			}
		case 5: // one-offs cleaned up by hand
			var cs []container
			for _, c := range d.containers {
				if c.project != e.set.Name {
					cs = append(cs, c)
				}
			}
			d.containers = cs
			e.run(d)
			check(e.set.Name + " one-offs gone")
		}
	}
	// Everything goes: no image is left behind.
	d.containers = nil
	for _, e := range envs {
		for _, id := range e.releases() {
			e.set.Remove(e.rel(id))
			os.RemoveAll(e.rel(id))
		}
		e.set.Prune()
	}
	if len(d.images) > 0 {
		var left []string
		for id := range d.images {
			var ns []string
			for n, i := range d.names {
				if i == id {
					ns = append(ns, n)
				}
			}
			left = append(left, id+" "+strings.Join(ns, ","))
		}
		sort.Strings(left)
		t.Fatalf("after %v and removing everything, images are left: %v\n%s", log, left, out.String())
	}
}

// usedNames are the names whose image the other environment uses, with
// that image: its containers', or its current release's, even down.
func usedNames(d *fakeDocker, other *env) map[string]string {
	used := map[string]string{}
	if other.current != "" {
		for _, e := range readList(other.rel(other.current)) {
			if e.ID != "" && d.names[e.Ref] == e.ID {
				used[e.Ref] = e.ID
			}
		}
	}
	for n, id := range d.names {
		for _, c := range d.containers {
			if c.project == other.set.Name && c.image == id && !isBookmark(n) {
				used[n] = id
			}
		}
	}
	return used
}

// sharedNames are the names a restore may leave alone: whose image the
// other environment's containers or one outside Compose use, or one of
// its bookmarks, any release's, holds.
func sharedNames(d *fakeDocker, other *env) map[string]string {
	shared := map[string]string{}
	for n, id := range d.names {
		if isBookmark(n) {
			continue
		}
		for _, c := range d.containers {
			if (c.project == other.set.Name || c.project == "") && c.image == id {
				shared[n] = id
			}
		}
		tags, _ := d.Tags(id)
		for _, tg := range tags {
			if strings.Contains(tg, ":"+bookmarkPrefix(other.set.Name)) {
				shared[n] = id
			}
		}
	}
	return shared
}

func assertKept(t *testing.T, d *fakeDocker, before map[string]string, log []string, out *bytes.Buffer) {
	t.Helper()
	for n, id := range before {
		if got := d.names[n]; got != id {
			t.Fatalf("after %v: %s moved from %s to %s under another environment's container\n%s", log, n, id, got, out.String())
		}
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// A release going leaves every name of an image a container uses, so a
// project outside devopsy running mariadb:11.4 keeps it although the
// release ran the same image as mariadb:11: recorded, retried once the
// container is gone.
func TestRemoveLeavesUsedImage(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	e := newEnv(t, d, "app-prod", filepath.Join(t.TempDir(), LeftFile), &out)
	d.pull("mariadb:11", "sha256:1")
	d.pull("mariadb:11.4", "sha256:1")
	id := e.newRelease("mariadb:11")
	e.set.Bookmark(e.rel(id))
	d.containers = []container{{"sha256:1", ""}}
	if err := e.set.Remove(e.rel(id)); err != nil {
		t.Fatal(err)
	}
	if d.names["mariadb:11"] != "sha256:1" || d.names["mariadb:11.4"] != "sha256:1" {
		t.Errorf("names of a used image went: %v\n%s", d.names, out.String())
	}
	d.containers = nil
	e.set.Prune()
	if len(d.images) != 0 {
		t.Errorf("not retried once the container went: %v\n%s", d.names, out.String())
	}
}

// A bookmark Docker shows short (nginx for docker.io/library/nginx) is
// still the environment's own: its rollback restores exactly.
func TestRestoreFamiliarNames(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	e := newEnv(t, d, "app-prod", filepath.Join(t.TempDir(), LeftFile), &out)
	ref := "docker.io/library/nginx:1.27"
	d.pull(ref, "sha256:1")
	first := e.newRelease(ref)
	e.set.Bookmark(e.rel(first))
	d.pull(ref, "sha256:2")
	second := e.newRelease(ref)
	e.set.Bookmark(e.rel(second))
	if !e.set.Restore(e.rel(first)) || d.names["nginx:1.27"] != "sha256:1" {
		t.Errorf("nginx:1.27 is %s after the rollback, want sha256:1\n%s", d.names["nginx:1.27"], out.String())
	}
}

// A release fetched again whose repository count changed (a second
// postgres added) renames its bookmarks: the old one goes, and the name
// it still lists is not recorded as dropped.
func TestBookmarkRenamed(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	left := filepath.Join(t.TempDir(), LeftFile)
	e := newEnv(t, d, "app-prod", left, &out)
	d.pull("postgres:15", "sha256:1")
	d.pull("postgres:16", "sha256:2")
	id := e.newRelease("postgres:15")
	e.set.Bookmark(e.rel(id))
	e.lists[id] = []string{"postgres:15", "postgres:16"}
	e.set.Bookmark(e.rel(id))
	if _, err := d.ID("postgres:" + bookmarkPrefix("app-prod") + id); err == nil {
		t.Errorf("the old bookmark stays")
	}
	if exists(left) {
		t.Errorf("postgres:15, still listed, was recorded as dropped\n%s", out.String())
	}
}

// Docker failing midway stops a removal with an error, so the release
// stays for the next one, which finishes it.
func TestRemoveDockerFails(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	e := newEnv(t, d, "app-prod", filepath.Join(t.TempDir(), LeftFile), &out)
	d.pull("app:1", "sha256:1")
	id := e.newRelease("app:1")
	e.set.Bookmark(e.rel(id))
	d.fail = map[string]bool{"Tags": true}
	if err := e.set.Remove(e.rel(id)); err == nil {
		t.Fatalf("no error with Docker failing\n%s", out.String())
	}
	if err := e.set.Remove(e.rel(id)); err != nil || len(d.images) != 0 {
		t.Errorf("the next removal did not finish it (%v): %v\n%s", err, d.names, out.String())
	}
}

// An image a release ran under a name in another repository than the
// release holding it (a mirror) is recorded when the release goes, and
// removed once nothing holds it.
func TestRemoveOtherRepository(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	e := newEnv(t, d, "app-prod", filepath.Join(t.TempDir(), LeftFile), &out)
	d.pull("bitnami/redis:7", "sha256:1")
	d.pull("mirror.example/bitnami/redis:7", "sha256:1")
	older := e.newRelease("bitnami/redis:7")
	e.set.Bookmark(e.rel(older))
	newer := e.newRelease("mirror.example/bitnami/redis:7")
	e.set.Bookmark(e.rel(newer))
	for _, id := range []string{newer, older} { // pruning: newest first
		if err := e.set.Remove(e.rel(id)); err != nil {
			t.Fatal(err)
		}
		e.set.Prune()
	}
	if len(d.images) != 0 {
		t.Errorf("left: %v\n%s", d.names, out.String())
	}
}

// A release whose digest's image is gone is not restored: the fetch
// pulls it before the switch, never the run step after.
func TestRestoreDigestGone(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	e := newEnv(t, d, "app-prod", filepath.Join(t.TempDir(), LeftFile), &out)
	d.pull("nginx@sha256:abc", "sha256:1")
	id := e.newRelease("nginx@sha256:abc")
	e.set.Bookmark(e.rel(id))
	for n := range d.names {
		delete(d.names, n)
	}
	delete(d.images, "sha256:1")
	if e.set.Restore(e.rel(id)) {
		t.Errorf("restored with its image gone\n%s", out.String())
	}
}

// A name rewritten the same to Docker (docker.io/library/nginx:1 and
// nginx:1) between two listings is not recorded as dropped.
func TestBookmarkRespelled(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	left := filepath.Join(t.TempDir(), LeftFile)
	e := newEnv(t, d, "app-prod", left, &out)
	d.pull("nginx:1", "sha256:1")
	id := e.newRelease("docker.io/library/nginx:1")
	e.set.Bookmark(e.rel(id))
	e.lists[id] = []string{"nginx:1"}
	e.set.Bookmark(e.rel(id))
	if exists(left) {
		t.Errorf("recorded as dropped\n%s", out.String())
	}
}

func TestIsBookmark(t *testing.T) {
	for tag, want := range map[string]bool{
		"app:devopsy-app-prod-1":          true,
		"reg:5000/app:devopsy-app-prod-1": true,
		"devopsy-docs-prod-app":           false,
		"devopsy-docs-prod-app:latest":    false,
		"reg:5000/devopsy-x":              false,
	} {
		if isBookmark(tag) != want {
			t.Errorf("isBookmark(%q) = %v", tag, !want)
		}
	}
}

func TestDigestNames(t *testing.T) {
	if got := familiar("docker.io/library/nginx:1.27@sha256:abc"); got != "nginx@sha256:abc" {
		t.Errorf("familiar: %q", got)
	}
	if got := digestRef("nginx:1.27@sha256:abc"); got != "nginx@sha256:abc" {
		t.Errorf("digestRef: %q", got)
	}
	if got := familiar("nginx"); got != "nginx:latest" {
		t.Errorf("familiar: %q", got)
	}
}
