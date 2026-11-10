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

func (f *fakePaths) Within(root, rel string) error { return nil }

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

// The pull policies under which compose runs the image the door read, and no network at all.
func TestCheckFileTakesWhatPullsNothingAtAStart(t *testing.T) {
	t.Parallel()
	for _, extra := range []string{`,"pull_policy":"missing"`, `,"pull_policy":"never"`, `,"pull_policy":"if_not_present"`, `,"network_mode":"none"`} {
		file := parse(t, `{"name":"shop-prod","services":{"web":`+safe(extra)+`}}`)
		if errs := CheckFile(file, testPolicy, "up", &fakePaths{}); len(errs) != 0 {
			t.Errorf("%s refused: %v", extra, errs)
		}
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
		// What compose and devopsy read of a volume is theirs, as on the door's volume create.
		{"a volume labelled as compose's", `{"name":"shop-prod","volumes":{"v":{"name":"shop-prod_v","labels":{"com.docker.compose.project":"other-prod"}}}}`, "volume v"},
		{"a volume labelled as devopsy's", `{"name":"shop-prod","volumes":{"v":{"name":"shop-prod_v","labels":{"devopsy.data":"true"}}}}`, "volume v"},
		// IPv6 a project's network does without: fewer paths across its server to keep shut.
		{"a network with IPv6", `{"name":"shop-prod","networks":{"n":{"name":"shop-prod_n","enable_ipv6":true}}}`, "plain network"},
		{"a network's address space", `{"name":"shop-prod","networks":{"n":{"name":"shop-prod_n","ipam":{"config":[{"subnet":"10.0.0.0/8"}]}}}}`, "plain network"},
		// A bridge of another name than Docker's (br-+) is one the project users' firewall
		// does not match: only the deploy user renames one.
		{"a network's bridge renamed", `{"name":"shop-prod","networks":{"n":{"name":"shop-prod_n","driver_opts":{"com.docker.network.bridge.name":"eth9"}}}}`, "plain network"},
		{"labels in another form", svc(`,"labels":"a=b"`), "labels must be a map"},
		{"mounts in another form", svc(`,"volumes":{"a":"b"}`), "volumes must be a list"},
		{"ports in another form", svc(`,"ports":"80:80"`), "ports must be a list"},
		{"a capability in another form", svc(`,"cap_add":"SYS_ADMIN"`), "cap_add must be a list"},
		{"a variable in a service", svc(`,"image":"${X}"`), "a variable"},
		{"a variable in a key", `{"name":"shop-prod","services":{"${X}":{}}}`, "a variable"},
		// A start runs the image the door read: compose pulling or building one then would run another.
		{"a pull at every start", svc(`,"pull_policy":"always"`), "pull_policy always"},
		{"a pull once a day", svc(`,"pull_policy":"daily"`), "pull_policy daily"},
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
		{"another context, a path", `{"context":"` + ctx + `","additional_contexts":{"a":"/etc"}}`, "only another service's build"},
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
	// A build writes devopsy's name for it, whatever image: says (a registry's included),
	// so no name a project writes is one a registry serves; a start runs that name, unless
	// devopsy's own file holds the service on a pin or a hold of its own.
	named := func(image string) string {
		return `{"name":"shop-prod","services":{"web":{"image":"` + image + `","user":"10001:10001","security_opt":["no-new-privileges"],"build":{"context":"` + ctx + `"}}}}`
	}
	for _, image := range []string{"", "mongo-express:1", "other-prod-app", "shop-prod-app:1", "shop-prod-app:devopsy-other-prod-1", "registry.example.com/shop/app:1"} {
		for _, verb := range []string{"build", "up", "run", "down"} {
			file = parse(t, named(image))
			if errs := CheckFile(file, testPolicy, verb, &fakePaths{}); len(errs) != 0 {
				t.Fatalf("%s %s: %v", verb, image, errs)
			}
			if got := service(file)["image"]; got != "devopsy.internal/shop-prod/web" {
				t.Errorf("%s of image %q runs %v", verb, image, got)
			}
		}
	}
	for _, image := range []string{"registry.example.com/shop/app:1@sha256:abc", "devopsy.internal/shop-prod/web:devopsy-shop-prod-20260101000000", "devopsy.internal/shop-stage/web"} {
		file = parse(t, named(image))
		if errs := CheckFile(file, testPolicy, "up", &fakePaths{}); len(errs) != 0 {
			t.Fatalf("%s: %v", image, errs)
		}
		if got := service(file)["image"]; got != image {
			t.Errorf("a start held on %s runs %v", image, got)
		}
	}
	// A pull fetches what a registry serves under image: (image mode's builds): never renamed.
	file = parse(t, named("registry.example.com/shop/app:1"))
	if errs := CheckFile(file, testPolicy, "pull", &fakePaths{}); len(errs) != 0 {
		t.Fatal(errs)
	}
	if got := service(file)["image"]; got != "registry.example.com/shop/app:1" {
		t.Errorf("a pull renamed its image: %v", got)
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
		{"up --pull missing web", true},
		{"create --pull=never", true},
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

// A build takes another service's build of the same file as a context (compose's
// additional_contexts: service:<name>), its way to build one service on another: that build
// runs on the project's own builder from its own release, under the project's own name. Any
// other context is refused: a path the client would read with the deploy user's rights, an
// image (docker-image://) the project's builder would fetch past a claim, a URL, the service
// itself, a service with no build or none of the file, a name not one word.
func TestCheckFileBuildOnAnotherServicesBuild(t *testing.T) {
	t.Parallel()
	ctx := "/home/devopsy-shop/shop/prod/releases/1/code"
	file := func(contexts string) map[string]any {
		return parse(t, `{"name":"shop-prod","services":{`+
			`"base":`+safe(`,"build":{"context":"`+ctx+`/base"}`)+`,`+
			`"db":`+safe(``)+`,`+
			`"app":`+safe(`,"build":{"context":"`+ctx+`","additional_contexts":`+contexts+`}`)+`}}`)
	}
	if errs := CheckFile(file(`{"shop-base":"service:base"}`), testPolicy, "build", &fakePaths{}); len(errs) != 0 {
		t.Errorf("another service's build as a context: %v", errs)
	}
	for contexts, want := range map[string]string{
		`{"shop-base":"/etc"}`:                       "only another service's build",
		`{"shop-base":"../base"}`:                    "only another service's build",
		`{"shop-base":"docker-image://alpine:3.22"}`: "only another service's build",
		`{"shop-base":"https://example.com/x.git"}`:  "only another service's build",
		`{"shop-base":"target:base"}`:                "only another service's build",
		`{"shop-base":"service:app"}`:                "no other service of this file that builds",
		`{"shop-base":"service:db"}`:                 "no other service of this file that builds",
		`{"shop-base":"service:blog"}`:               "no other service of this file that builds",
		`{"shop base":"service:base"}`:               "a name not one word",
		`["shop-base=service:base"]`:                 "additional_contexts must be a map",
	} {
		if errs := CheckFile(file(contexts), testPolicy, "build", &fakePaths{}); !anyHolds(errs, want) {
			t.Errorf("%s: %v, want %q", contexts, errs, want)
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
	// A build's name is the door's to write, never a pull's: another project's is refused,
	// this project's own left out of the pull (pull_policy: never, which compose's pull
	// skips); a registry's name is anyone's to pull, a project's name at its start included
	// (no build is named so).
	p.Others = []string{"mongo"}
	for image, want := range map[string]string{"devopsy.internal/blog-prod/web": "refused", "devopsy.internal/blog-prod/web:1": "refused", "devopsy.internal/shop-prod/web": "skipped", "mongo-express:1": "pulled"} {
		file := parse(t, `{"name":"shop-prod","services":{"web":{"image":"`+image+`","user":"10001:10001","security_opt":["no-new-privileges"]}}}`)
		errs := CheckFile(file, p, "pull", &fakePaths{})
		got := "pulled"
		if len(errs) > 0 {
			got = "refused"
		} else if file["services"].(map[string]any)["web"].(map[string]any)["pull_policy"] == "never" {
			got = "skipped"
		}
		if got != want {
			t.Errorf("a pull of %s: %s, want %s (%v)", image, got, want, errs)
		}
	}
}

// One rule says what of the door's mnts a path is, for a mount's kind, a bind's source, and
// what a copy wrote to give back: this project's mnt, or one of its entries, a Word; never
// deeper, another project's, or a name a Word is not.
func TestMntPath(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]string{
		"/space/mnt/shop-prod":                "shop-prod ",
		"/space/mnt/shop-prod/db":             "shop-prod db",
		"/space/mnt/shop-eu-prod/app.ini":     "shop-eu-prod app.ini",
		"/space/mnt/shop-prod/db/sub":         "",
		"/space/mnt/blog-prod/db":             "",
		"/space/mnt/shop-prod/-x":             "",
		"/space/mnt/shop-prod/..":             "",
		"/space/mnt":                          "",
		"/space/clones/shop/abc/1/mnt/db":     "",
		"/home/devopsy-shop/shop/prod/mnt/db": "",
	} {
		got := ""
		if name, entry, ok := testPolicy.MntPath(path); ok {
			got = name + " " + entry
		}
		if got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
}

// A release in build mode runs each of its builds by its bookmark, which devopsy's own
// compose file names (images.ComposeFile, <build's name>:devopsy-<compose name>-<id>), so
// every render of a live release carries one per built service. A compose pull there (a
// run step's `devopsy pull <service>`, a restart command's, an operator's by hand) is about
// the services that pull: the built one is skipped (its build: kept for that), and the
// project's own bookmark is no name a pull writes.
func TestCheckFilePullInAReleaseRunningByBookmarks(t *testing.T) {
	t.Parallel()
	bookmark := "devopsy.internal/shop-prod/web:devopsy-shop-prod-20260101000000"
	if !testPolicy.OwnRef(bookmark) {
		t.Fatalf("the control: %s is no name of this project's own", bookmark)
	}
	file := parse(t, `{"name":"shop-prod","services":{`+
		`"web":{"image":"`+bookmark+`","build":{"context":"/home/devopsy-shop/shop/prod/releases/20260101000000/code"},"user":"10001:10001","security_opt":["no-new-privileges"]},`+
		`"proxy":`+safe(``)+`}}`)
	if errs := CheckFile(file, testPolicy, "pull", &fakePaths{}); len(errs) != 0 {
		t.Errorf("a pull in a release whose build runs by its bookmark: %v", errs)
	}
}

// What a build writes for a service is what the next start of it reads: for the deploy
// user's project compose tags the image: of the rendered file, the release's bookmark
// (images.ComposeFile), and the `up` after it runs that build.
func TestCheckFileBuildLandsWhereAStartReads(t *testing.T) {
	t.Parallel()
	bookmark := "devopsy.internal/shop-prod/web:devopsy-shop-prod-20260101000000"
	image := func(verb string) string {
		file := parse(t, `{"name":"shop-prod","services":{"web":{"image":"`+bookmark+`",`+
			`"build":{"context":"/home/devopsy-shop/shop/prod/releases/20260101000000/code"},"user":"10001:10001","security_opt":["no-new-privileges"]}}}`)
		if errs := CheckFile(file, testPolicy, verb, &fakePaths{}); len(errs) != 0 {
			t.Fatalf("%s: %v", verb, errs)
		}
		got, _ := file["services"].(map[string]any)["web"].(map[string]any)["image"].(string)
		return got
	}
	if built, started := image("build"), image("up"); built != started {
		t.Errorf("a build lands on %s, and the start after it runs %s", built, started)
	}
}
