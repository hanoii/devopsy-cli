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
// image, refused while a container, even a stopped one, uses it.
type fakeDocker struct {
	images     map[string]bool
	names      map[string]string
	containers []container
}

type container struct{ image, project string }

func newFake() *fakeDocker {
	return &fakeDocker{images: map[string]bool{}, names: map[string]string{}}
}

func (d *fakeDocker) ID(ref string) (string, error) {
	if id, ok := d.names[ref]; ok {
		return id, nil
	}
	if d.images[ref] {
		return ref, nil
	}
	return "", ErrNoImage
}

func (d *fakeDocker) Tags(id string) ([]string, error) {
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
	d.names[dst] = id
	return nil
}

func (d *fakeDocker) Remove(ref string) error {
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
	d.names[ref] = id
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
	if got := BookmarkPrefix("app-prod"); got != "devopsy-app-prod-" {
		t.Errorf("short: %q", got)
	}
	if got := BookmarkPrefix(""); got != "devopsy-" {
		t.Errorf("empty: %q", got)
	}
	long1 := strings.Repeat("a", 94)
	if BookmarkPrefix(long1) != "devopsy-"+long1+"-" {
		t.Errorf("94 characters are kept")
	}
	long2, long3 := strings.Repeat("b", 120)+"x", strings.Repeat("b", 120)+"y"
	p2, p3 := BookmarkPrefix(long2), BookmarkPrefix(long3)
	if p2 == p3 || len(p2) != len("devopsy-")+85+1+8+1 {
		t.Errorf("long names: %q %q", p2, p3)
	}
	// The longest tag: prefix, a 14-digit id, - and a 10-digit cksum.
	if n := len(p2) + 14 + 1 + 10; n > 128 {
		t.Errorf("a tag can reach %d characters", n)
	}
	if BookmarkPrefix(long2) != p2 {
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
		if got := Repo(ref); got != want {
			t.Errorf("Repo(%q) = %q, want %q", ref, got, want)
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
				es := ReadList(e.rel(id))
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
		shared := "mariadb:11"
		if e.set.Name == "app-b" && rng.Intn(3) == 0 {
			shared = "mariadb:11.4"
		}
		own := e.set.Name + "-app"
		switch op := rng.Intn(8); op {
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
			id := e.newRelease(own, shared)
			d.pull(own, newImage())
			switch {
			case shared == "mariadb:11.4":
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
			if e.set.Restore(e.rel(target)) {
				assertKept(t, d, before, log, &out)
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
				for _, b := range bookmarks(ReadList(e.rel(target)), e.set.Name, target) {
					delete(gone, b)
				}
			}
			e.current = target
			e.run(d)
			check(e.set.Name + " rollback " + target)
		case 4: // a release removed (pruned or by hand), never the current one
			for _, id := range e.releases() {
				if id != e.current {
					// Names whose image another release's bookmark holds stay.
					held := map[string]string{}
					for n, i := range d.names {
						if IsBookmark(n) {
							continue
						}
						tags, _ := d.Tags(i)
						for _, tg := range tags {
							if IsBookmark(tg) && !strings.HasSuffix(tg, "-"+id) && !strings.Contains(tg, "-"+id+"-") {
								held[n] = i
							}
						}
					}
					e.set.Remove(e.rel(id))
					for n, i := range held {
						if d.names[n] != i {
							t.Fatalf("after %v: removing %s took %s, which another release holds\n%s", log, id, n, out.String())
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
				bs := bookmarks(ReadList(e.rel(id)), e.set.Name, id)
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
		for _, e := range ReadList(other.rel(other.current)) {
			if e.ID != "" && d.names[e.Ref] == e.ID {
				used[e.Ref] = e.ID
			}
		}
	}
	for n, id := range d.names {
		for _, c := range d.containers {
			if c.project == other.set.Name && c.image == id && !IsBookmark(n) {
				used[n] = id
			}
		}
	}
	return used
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
