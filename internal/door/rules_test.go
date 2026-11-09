package door

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

var testPolicy = Policy{
	Project: "shop", User: "devopsy-shop", Space: "/space", Tree: "/home/devopsy-shop/shop",
	Joinable: []string{"traefik"}, ForbiddenIDs: []int{1000, 1001, 999},
}

// fakePaths answers as a project user's tree would: links lists where a path
// ends, and anything else ends where it says.
type fakePaths struct {
	links  map[string]string
	sealed []string
}

func (f *fakePaths) Real(path string) (string, error) {
	if r, ok := f.links[path]; ok {
		return r, nil
	}
	return filepath.Clean(path), nil
}

func (f *fakePaths) Seal(path string) (string, error) {
	f.sealed = append(f.sealed, path)
	return "/space/sealed/shop/abc/x", nil
}

func parse(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// safe is a service the rules take, with the given keys added.
func safe(extra string) string {
	return `{"image":"busybox:1.37","user":"10001:10001","security_opt":["no-new-privileges"]` + extra + `}`
}

func TestCheckFileTakesWhatTemplatesUse(t *testing.T) {
	t.Parallel()
	file := parse(t, `{"name":"shop-prod","x-any":1,
	  "networks":{"default":{"name":"shop-prod_default","internal":true},"proxy":{"name":"shop-prod_proxy"},"tunnel":{"name":"traefik","external":true}},
	  "volumes":{"data":{"name":"shop-prod_data"}},
	  "services":{"shop-web":`+safe(`,"read_only":true,"cap_add":["NET_BIND_SERVICE"],"cap_drop":["ALL"],"tmpfs":["/tmp"],
	    "labels":{"traefik.enable":"true","devopsy.data":"volumes","":""},
	    "environment":{"A":"b"},"networks":{"default":null,"tunnel":null},"logging":{"driver":"local"},
	    "deploy":{"resources":{"limits":{"memory":"1g"}}},"post_start":[{"command":["true"]}],
	    "volumes":[{"type":"volume","source":"data","target":"/d"},{"type":"tmpfs","target":"/t"}]`)+`}}`)
	if errs := CheckFile(file, testPolicy, "up", &fakePaths{}); len(errs) != 0 {
		t.Fatalf("refused: %v", errs)
	}
	labels := file["services"].(map[string]any)["shop-web"].(map[string]any)["labels"].(map[string]any)
	if _, ok := labels[""]; ok {
		t.Error("a label that interpolated to nothing stayed")
	}
}

// A rendered file escapes its `$` by doubling: what compose then makes of it is a `$`.
func TestCheckFileTakesEscapedDollars(t *testing.T) {
	t.Parallel()
	file := parse(t, `{"name":"shop-prod","services":{"web":`+safe(`,"command":["sh","-c","echo $$HOME"],"environment":{"A":"p$$w"}`)+`}}`)
	if errs := CheckFile(file, testPolicy, "up", &fakePaths{}); len(errs) != 0 {
		t.Errorf("refused: %v", errs)
	}
}

// Each is a way a compose file reaches past its project, and what refuses it.
func TestCheckFileRefuses(t *testing.T) {
	t.Parallel()
	svc := func(extra string) string { return `{"name":"shop-prod","services":{"web":` + safe(extra) + `}}` }
	tests := []struct{ name, file, want string }{
		{"another project's name", `{"name":"other-prod","services":{}}`, "another project's name"},
		{"a name that only starts the same", `{"name":"shopx-prod","services":{}}`, "another project's name"},
		{"an unknown top key", `{"name":"shop-prod","secrets":{}}`, "top: secrets"},
		{"privileged", svc(`,"privileged":true`), "web: privileged"},
		{"env_file, read as whoever runs compose", svc(`,"env_file":["/x"]`), "web: env_file"},
		{"label_file", svc(`,"label_file":["/x"]`), "web: label_file"},
		{"extends", svc(`,"extends":{"file":"/x","service":"y"}`), "web: extends"},
		{"provider, a program run as whoever runs compose", svc(`,"provider":{"type":"/tmp/evil"}`), "web: provider"},
		{"pid", svc(`,"pid":"host"`), "web: pid"},
		{"ipc", svc(`,"ipc":"host"`), "web: ipc"},
		{"devices", svc(`,"devices":["/dev/sda"]`), "web: devices"},
		{"volumes_from", svc(`,"volumes_from":["x"]`), "web: volumes_from"},
		{"runtime", svc(`,"runtime":"x"`), "web: runtime"},
		{"cgroup_parent", svc(`,"cgroup_parent":"x"`), "web: cgroup_parent"},
		{"a key nobody knows yet", svc(`,"new_in_compose_9":1`), "web: new_in_compose_9"},
		{"the host's network", svc(`,"network_mode":"host"`), "network_mode host"},
		{"another container's network", svc(`,"network_mode":"container:victim"`), "network_mode container:victim"},
		// Docker's default bridge, docker0, which every service with it shares, its
		// containers talking to each other's ports: none of the project's names.
		{"the default bridge", svc(`,"network_mode":"bridge"`), "network_mode bridge"},
		{"ports", svc(`,"ports":[{"target":80}]`), "web: ports"},
		{"a capability", svc(`,"cap_add":["SYS_ADMIN"]`), "cap_add SYS_ADMIN"},
		{"sysctls", svc(`,"sysctls":{"net.ipv4.ip_forward":"1"}`), "sysctls"},
		{"group_add", svc(`,"group_add":["999"]`), "group_add"},
		{"a log driver reaching out", svc(`,"logging":{"driver":"syslog"}`), "a log driver"},
		{"deploy past resources", svc(`,"deploy":{"mode":"host"}`), "deploy holds only"},
		{"a hook as root", svc(`,"post_start":[{"command":["x"],"user":"0"}]`), "hook may hold only"},
		{"a compose label", svc(`,"labels":{"com.docker.compose.project":"other-prod"}`), "label com.docker.compose.project"},
		{"devopsy's container label", svc(`,"labels":{"devopsy.env":"other-prod"}`), "label devopsy.env"},
		{"the door's label", svc(`,"labels":{"devopsy.door":"devopsy-shop"}`), "label devopsy.door"},
		{"a role not given", svc(`,"labels":{"devopsy.role":"proxy"}`), "the role proxy"},
		{"a bind mount of type npipe", svc(`,"volumes":[{"type":"npipe","source":"x"}]`), "a mount of type npipe"},
		{"a bind of the host", svc(`,"volumes":[{"type":"bind","source":"/","target":"/h"}]`), "a host path"},
		{"a bind of the socket", svc(`,"volumes":[{"type":"bind","source":"/var/run/docker.sock","target":"/s"}]`), "a host path"},
		{"another's entry", svc(`,"volumes":[{"type":"bind","source":"/space/mnt/other-prod/data","target":"/d"}]`), "a host path"},
		{"its mnt itself, not an entry", svc(`,"volumes":[{"type":"bind","source":"/space/mnt/shop-prod","target":"/d"}]`), "a host path"},
		{"its tree, writable", svc(`,"volumes":[{"type":"bind","source":"/home/devopsy-shop/shop/prod/x","target":"/d"}]`), "a host path"},
		{"a volume with options", `{"name":"shop-prod","volumes":{"v":{"name":"shop-prod_v","driver_opts":{"device":"/"}}}}`, "volume v"},
		{"another's volume", `{"name":"shop-prod","volumes":{"v":{"name":"other-prod_v","external":true}}}`, "volume v"},
		{"an external network not given", `{"name":"shop-prod","networks":{"n":{"name":"other-prod_default","external":true}}}`, "another's"},
		{"a macvlan network", `{"name":"shop-prod","networks":{"n":{"name":"shop-prod_n","driver":"macvlan"}}}`, "plain network"},
		{"bind propagation, reaching the host's mounts", svc(`,"volumes":[{"type":"bind","source":"/space/mnt/shop-prod/data","target":"/d","bind":{"propagation":"rshared"}}]`), "a mount's bind: propagation"},
		{"selinux relabeling the source", svc(`,"volumes":[{"type":"bind","source":"/space/mnt/shop-prod/data","target":"/d","bind":{"selinux":"z"}}]`), "a mount's bind: selinux"},
		{"a mount of a key nobody knows", svc(`,"volumes":[{"type":"tmpfs","target":"/d","x":1}]`), "a mount: x"},
		{"a network's driver options on a service", svc(`,"networks":{"default":{"driver_opts":{"a":"b"}}}`), "network default: driver_opts"},
		{"a mac address", svc(`,"networks":{"default":{"mac_address":"x"}}`), "network default: mac_address"},
		{"a volume's labels and driver", `{"name":"shop-prod","volumes":{"v":{"name":"shop-prod_v","driver":"x"}}}`, "volume v"},
		// IPv6 a project's network does without: fewer paths across its server to keep shut.
		{"a network with IPv6", `{"name":"shop-prod","networks":{"n":{"name":"shop-prod_n","enable_ipv6":true}}}`, "plain network"},
		{"a network's address space", `{"name":"shop-prod","networks":{"n":{"name":"shop-prod_n","ipam":{"config":[{"subnet":"10.0.0.0/8"}]}}}}`, "plain network"},
		{"labels in another form", svc(`,"labels":"a=b"`), "labels must be a map"},
		{"mounts in another form", svc(`,"volumes":{"a":"b"}`), "volumes must be a list"},
		{"ports in another form", svc(`,"ports":"80:80"`), "ports must be a list"},
		{"a capability in another form", svc(`,"cap_add":"SYS_ADMIN"`), "cap_add must be a list"},
		{"a variable in a service", svc(`,"image":"${X}"`), "a variable"},
		{"a variable in a key", `{"name":"shop-prod","services":{"${X}":{}}}`, "a variable"},
		// A start runs the image the door read: compose pulling or building one then would run another.
		{"a pull at every start", svc(`,"pull_policy":"always"`), "pull_policy always"},
		{"a build at every start", svc(`,"pull_policy":"build"`), "pull_policy build"},
		{"an alias on a shared network", `{"name":"shop-prod","networks":{"t":{"name":"traefik","external":true}},"services":{"shop-web":` + safe(`,"networks":{"t":{"aliases":["victim"]}}`) + `}}`, "an alias on the shared network"},
		// Compose makes a service's name an alias on each of its networks.
		{"another's name on a shared network", `{"name":"shop-prod","networks":{"t":{"name":"traefik","external":true}},"services":{"traefik":` + safe(`,"networks":{"t":null}`) + `}}`, "a service is named shop-<name>"},
		{"another's name on a shared default", `{"name":"shop-prod","networks":{"default":{"name":"traefik","external":true}},"services":{"traefik":` + safe(``) + `}}`, "a service is named shop-<name>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			errs := CheckFile(parse(t, tt.file), testPolicy, "up", &fakePaths{})
			if !anyHolds(errs, tt.want) {
				t.Errorf("want a refusal holding %q, got %v", tt.want, errs)
			}
		})
	}
}

// A service's labels in a list are checked as in a map: the file is the caller's, and
// compose takes a list too (docker compose config turns it into these labels), where
// checkLabels and carriesLabel would leave a role, devopsy's own labels and the vet's
// prefix unread.
func TestCheckFileLabelsInListForm(t *testing.T) {
	t.Parallel()
	for _, label := range []string{"devopsy.role=proxy", "devopsy.env=other-prod", "devopsy.task=owners"} {
		file := parse(t, `{"name":"shop-prod","services":{"web":`+safe(`,"labels":["`+label+`"]`)+`}}`)
		if errs := CheckFile(file, testPolicy, "up", &fakePaths{}); len(errs) == 0 {
			t.Errorf("%s went through in a list", label)
		}
	}
}

// A source compose would interpolate is refused: the door checks words and compose then
// interpolates them, the file being the caller's with no `$$` guaranteed (docker compose
// config gives this source as /space/mnt/shop-prod/..). The daemon would then bind
// /space/mnt, every project's entries, and the container's uid is the caller's to choose.
func TestCheckFileBindSurvivesInterpolation(t *testing.T) {
	t.Parallel()
	file := parse(t, `{"name":"shop-prod","services":{"web":`+safe(`,"volumes":[{"type":"bind","source":"/space/mnt/shop-prod/${X:-..}","target":"/d"}]`)+`}}`)
	if errs := CheckFile(file, testPolicy, "up", &fakePaths{}); len(errs) == 0 {
		t.Errorf("an interpolated source went through: %v", file["services"].(map[string]any)["web"].(map[string]any)["volumes"])
	}
}

func anyHolds(errs []error, want string) bool {
	for _, err := range errs {
		var r *Refusal
		if errors.As(err, &r) && strings.Contains(r.Why, want) {
			return true
		}
	}
	return false
}

func TestCheckFileIdentity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, service string
		refused       bool
	}{
		{"numbers", `{"user":"10001:10001","security_opt":["no-new-privileges"]}`, false},
		{"no-new-privileges:true", `{"user":"10001:10001","security_opt":["no-new-privileges:true"]}`, false},
		{"no user", `{"security_opt":["no-new-privileges"]}`, true},
		{"root", `{"user":"0:0","security_opt":["no-new-privileges"]}`, true},
		{"root's group", `{"user":"10001:0","security_opt":["no-new-privileges"]}`, true},
		{"a name, which an image's passwd can make root", `{"user":"app:app","security_opt":["no-new-privileges"]}`, true},
		{"a uid alone, leaving the image's group", `{"user":"10001","security_opt":["no-new-privileges"]}`, true},
		{"a host user's uid", `{"user":"1000:10001","security_opt":["no-new-privileges"]}`, true},
		{"the socket's group", `{"user":"10001:999","security_opt":["no-new-privileges"]}`, true},
		{"no no-new-privileges", `{"user":"10001:10001"}`, true},
		{"seccomp off beside it", `{"user":"10001:10001","security_opt":["no-new-privileges","seccomp=unconfined"]}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			errs := CheckFile(parse(t, `{"name":"shop-prod","services":{"web":`+tt.service+`}}`), testPolicy, "up", &fakePaths{})
			if (len(errs) != 0) != tt.refused {
				t.Errorf("refused %v, want %v: %v", len(errs) != 0, tt.refused, errs)
			}
		})
	}
}

// A path that ends in an entry is pinned to where it ends, so a link swapped in
// later reaches no restart; and one that ends elsewhere is refused, whatever it says.
func TestCheckFilePinsBinds(t *testing.T) {
	t.Parallel()
	through := "/home/devopsy-shop/shop/prod/current/.devopsy/mnt/data"
	entry := "/space/mnt/shop-prod/data"
	conf := "/home/devopsy-shop/shop/prod/current/.devopsy/conf"
	confReal := "/home/devopsy-shop/shop/prod/releases/1/code/.devopsy/conf"
	paths := &fakePaths{links: map[string]string{through: entry, conf: confReal}}
	file := parse(t, `{"name":"shop-prod","services":{"web":`+safe(`,"volumes":[
	  {"type":"bind","source":"`+through+`","target":"/d"},
	  {"type":"bind","source":"`+conf+`","target":"/c","read_only":true}]`)+`}}`)
	if errs := CheckFile(file, testPolicy, "up", paths); len(errs) != 0 {
		t.Fatal(errs)
	}
	vols := file["services"].(map[string]any)["web"].(map[string]any)["volumes"].([]any)
	if got := vols[0].(map[string]any)["source"]; got != entry {
		t.Errorf("the entry holds %v, want %s", got, entry)
	}
	if got := vols[1].(map[string]any)["source"]; got != "/space/sealed/shop/abc/x" {
		t.Errorf("the release file holds %v, want its sealed copy", got)
	}
	if len(paths.sealed) != 1 || paths.sealed[0] != confReal {
		t.Errorf("sealed %v, want its real path", paths.sealed)
	}
	// shared/mnt swapped for a link to another's entries.
	paths = &fakePaths{links: map[string]string{through: "/space/mnt/other-prod/data"}}
	file = parse(t, `{"name":"shop-prod","services":{"web":`+safe(`,"volumes":[{"type":"bind","source":"`+through+`","target":"/d"}]`)+`}}`)
	if errs := CheckFile(file, testPolicy, "up", paths); !anyHolds(errs, "a host path") {
		t.Errorf("a swapped shared/mnt: %v", errs)
	}
}

func TestCheckFileBuild(t *testing.T) {
	t.Parallel()
	ctx := "/home/devopsy-shop/shop/prod/releases/1/code"
	build := func(b string) string {
		return `{"name":"shop-prod","services":{"web":{"user":"10001:10001","security_opt":["no-new-privileges"],"build":` + b + `}}}`
	}
	service := func(file map[string]any) map[string]any {
		return file["services"].(map[string]any)["web"].(map[string]any)
	}
	file := parse(t, build(`{"context":"`+ctx+`","dockerfile":"docker/Dockerfile","args":{"A":"b"}}`))
	if errs := CheckFile(file, testPolicy, "build", &fakePaths{}); len(errs) != 0 {
		t.Fatal(errs)
	}
	b := service(file)["build"].(map[string]any)
	if b["context"] != "/space/sealed/shop/abc/x" || b["dockerfile"] != "docker/Dockerfile" {
		t.Errorf("not built from a sealed copy: %v", b)
	}
	// Any other verb never reads the block.
	file = parse(t, build(`{"context":"/etc","ssh":["default"]}`))
	if errs := CheckFile(file, testPolicy, "up", &fakePaths{}); len(errs) != 0 {
		t.Fatal(errs)
	}
	if s := service(file); s["build"] != nil {
		t.Errorf("an up keeps the build: %v", s["build"])
	}
	file = parse(t, build(`{"context":"/etc"}`))
	if errs := CheckFile(file, testPolicy, "pull", &fakePaths{}); len(errs) != 0 {
		t.Fatal(errs)
	}
	if ctx := service(file)["build"].(map[string]any)["context"]; ctx != "." {
		t.Errorf("a pull keeps the context %v", ctx)
	}
	tests := []struct{ name, block, want string }{
		{"a context outside", `{"context":"/home/devopsy"}`, "a context outside"},
		{"a Dockerfile outside", `{"context":"` + ctx + `","dockerfile":"../../../../../../home/devopsy/.ssh/id"}`, "a Dockerfile outside"},
		{"ssh, the deploy user's agent", `{"context":"` + ctx + `","ssh":["default"]}`, "build: ssh"},
		{"secrets", `{"context":"` + ctx + `","secrets":["k"]}`, "build: secrets"},
		{"another context", `{"context":"` + ctx + `","additional_contexts":{"a":"/etc"}}`, "build: additional_contexts"},
		{"the host's network", `{"context":"` + ctx + `","network":"host"}`, "build: network"},
		{"privileged", `{"context":"` + ctx + `","privileged":true}`, "build: privileged"},
		{"an entitlement", `{"context":"` + ctx + `","entitlements":["network.host"]}`, "build: entitlements"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if errs := CheckFile(parse(t, build(tt.block)), testPolicy, "build", &fakePaths{}); !anyHolds(errs, tt.want) {
				t.Errorf("want %q, got %v", tt.want, errs)
			}
		})
	}
	// A build writes the tag it names, in the one image store.
	for _, image := range []string{"other-prod-app", "busybox:1.37", "shop-prod-app:devopsy-other-prod-1"} {
		file = parse(t, `{"name":"shop-prod","services":{"web":{"image":"`+image+`","user":"10001:10001","security_opt":["no-new-privileges"],"build":{"context":"`+ctx+`"}}}}`)
		if errs := CheckFile(file, testPolicy, "build", &fakePaths{}); !anyHolds(errs, "an image not this project's") {
			t.Errorf("%s: %v", image, errs)
		}
	}
	file = parse(t, `{"name":"shop-prod","services":{"web":{"image":"shop-prod-app:1","user":"10001:10001","security_opt":["no-new-privileges"],"build":{"context":"`+ctx+`"}}}}`)
	if errs := CheckFile(file, testPolicy, "build", &fakePaths{}); len(errs) != 0 {
		t.Errorf("its own image: %v", errs)
	}
	// A context that is a link out of the tree.
	paths := &fakePaths{links: map[string]string{ctx + "/ctx": "/home/devopsy"}}
	if errs := CheckFile(parse(t, build(`{"context":"`+ctx+`/ctx"}`)), testPolicy, "build", paths); !anyHolds(errs, "a context outside") {
		t.Errorf("a linked context: %v", errs)
	}
}

func TestCheckVerb(t *testing.T) {
	t.Parallel()
	tests := []struct {
		argv string
		ok   bool
	}{
		{"up -d --wait --remove-orphans", true},
		// A start runs the image the door read: compose pulling one then would run another.
		{"up --pull never web", true},
		{"up --pull always web", false},
		{"up -d --pull=always", false},
		{"create --pull always", false},
		{"up --wait-timeout=30", true},
		{"down -v --rmi local", true},
		{"down --rmi=local", true},
		{"logs --tail 10 -f web", true},
		{"ps -q web --all", true},
		{"pull --ignore-buildable", true},
		{"exec -T web sh -c --anything", true},
		{"run --rm -T web --privileged", true},
		{"build --pull", true},
		{"down --rmi all", false},
		{"down --rmi=all", false},
		{"up --build", false},
		{"up --env-file /x", false},
		{"exec -u 0 web sh", false},
		{"exec --privileged web sh", false},
		// Who a shell runs as is held as a service's user: is: numbers, never root or a
		// host id (ForbiddenIDs), and never a name, which the image maps to any uid.
		{"exec -u 10001:10001 web sh", true},
		{"exec -u 1000 web sh", false},
		{"exec -u 10001:999 web sh", false},
		{"exec --user app web sh", false},
		{"exec -u admin web sh", false},
		{"exec --user=admin:admin web sh", false},
		{"exec -u root:root web sh", false},
		{"exec -u 1:0 web sh", false},
		{"exec -u 00 web sh", false},
		{"run -v /:/h web", false},
		{"run --cap-add ALL web", false},
		{"run --service-ports web", false},
		{"cp web:/x /y", false},
		{"config", false},
		{"publish x", false},
		{"bridge convert", false},
		{"up --wait-timeout", false},
		{"up -d=true", false},
		{"web --privileged", false},
		{"logs web --unknown", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.argv, func(t *testing.T) {
			t.Parallel()
			err := testPolicy.CheckVerb(strings.Fields(tt.argv))
			if (err == nil) != tt.ok {
				t.Errorf("ok %v, want %v: %v", err == nil, tt.ok, err)
			}
		})
	}
}

func TestNameOf(t *testing.T) {
	t.Parallel()
	tree := testPolicy.Tree
	tests := []struct{ path, want string }{
		{tree + "/prod", "shop-prod"},
		{tree + "/eu/prod", "shop-eu-prod"},
		{tree, ""},
		{tree + "/../other/prod", ""},
		{tree + "/a/b/c", ""},
		{tree + "/Prod", ""},
		{tree + "/.hidden", ""},
		{"/home/devopsy-other/other/prod", ""},
		{tree + "x/prod", ""},
	}
	for _, tt := range tests {
		got, err := testPolicy.NameOf(tt.path)
		if got != tt.want || (err == nil) != (tt.want != "") {
			t.Errorf("%s: %q, %v, want %q", tt.path, got, err, tt.want)
		}
	}
}

// A compose name of this project's is one word: OwnName goes by a prefix and Mnt by
// filepath.Join, so a name holding `..` would put its mnt anywhere. CheckFile would take a
// bind of it, and the door's `entries`, which runs on a file not yet checked, would set an
// ACL and make a directory there as the deploy user (TestDoorEntriesStayInItsMnt).
func TestCheckFileNameIsOneWord(t *testing.T) {
	t.Parallel()
	name := "shop-x/../../../home/deploy/.docker"
	if testPolicy.OwnName(name) {
		t.Errorf("%s is this project's", name)
	}
	file := parse(t, `{"name":"`+name+`","services":{"web":`+safe(`,"volumes":[{"type":"bind","source":"/home/deploy/.docker/cli-plugins","target":"/x"}]`)+`}}`)
	if errs := CheckFile(file, testPolicy, "up", &fakePaths{}); len(errs) == 0 {
		t.Errorf("a bind of the deploy user's home taken: %v", file["services"])
	}
}

// NameOf leaves out Others, as OwnName does: <tree>/eu-prod is the compose name of the
// project shop-eu's prod, and the door's `mnt` would open that project's
// <space>/mnt/shop-eu-prod to this user by an ACL (closedDir), no longer "closed to every
// other user", its data readable.
func TestNameOfLeavesLongerProjects(t *testing.T) {
	t.Parallel()
	p := testPolicy
	p.Others = []string{"shop-eu"}
	for _, env := range []string{"/eu-prod", "/eu/prod"} {
		if got, err := p.NameOf(p.Tree + env); err == nil {
			t.Errorf("%s: %s, another project's", env, got)
		}
	}
}

// A build takes no labels of its own, as checkLabels reads a service's only: a build's go
// on the image and from there on every container of it, as an image's LABEL does (`docker
// inspect` shows them in .Config.Labels), which is what imports read for a role and what a
// proxy reads for its routes, and the vet (carriesLabel) sees neither. A Dockerfile's LABEL
// needs a check at the daemon's side, which this test does not ask.
func TestCheckFileBuildLabels(t *testing.T) {
	t.Parallel()
	file := parse(t, `{"name":"shop-prod","services":{"web":`+safe(`,"build":{"context":"/home/devopsy-shop/shop/prod/code","labels":{"devopsy.role":"proxy"}}`)+`}}`)
	if errs := CheckFile(file, testPolicy, "build", &fakePaths{}); len(errs) == 0 {
		t.Error("a role claimed through a build's labels")
	}
}

// Compose's pull verb writes no hold of another project's, as the door's own pull and a
// claim do not (Claimable): that project runs its hold by name, so whoever serves the tag
// would choose what its next start, rollback or restart runs.
func TestCheckFileRefusesAPullOverAnotherHold(t *testing.T) {
	t.Parallel()
	p := testPolicy
	p.Others = []string{"blog"}
	hold := "registry.example.com/blog/app:devopsy-blog-prod-20240101000000"
	if _, err := Decide([]string{"pull", "-q", hold}, p, testDaemon, &fakeHost{}, testKnown); err == nil {
		t.Fatal("the control: the door's own pull takes another's hold")
	}
	file := parse(t, `{"name":"shop-prod","services":{"web":{"image":"`+hold+`","user":"10001:10001","security_opt":["no-new-privileges"]}}}`)
	if errs := CheckFile(file, p, "pull", &fakePaths{}); len(errs) == 0 {
		t.Errorf("compose's pull of another project's hold %s is taken", hold)
	}
}
