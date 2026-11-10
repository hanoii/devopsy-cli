package door

import (
	"encoding/json"
	"testing"
)

// Keys and forms the rules leave out: deny by default says a key not listed refuses, and this
// says which ones that had better stay, each with what it would reach. svc alone is taken.
func TestCheckFileRefusesWhatIsNotListed(t *testing.T) {
	t.Parallel()
	svc := func(extra string) string { return `{"name":"shop-prod","services":{"web":` + safe(extra) + `}}` }
	if errs := CheckFile(parse(t, svc("")), testPolicy, "up", &fakePaths{}); len(errs) != 0 {
		t.Fatalf("the control is refused, so nothing below says why it refuses: %v", errs)
	}
	bind := `{"type":"bind","source":"/space/mnt/shop-prod/data","target":"/d"`
	tests := []struct{ name, file, want string }{
		{"include, read as whoever runs compose", `{"name":"shop-prod","include":["/x"]}`, "top: include"},
		{"configs, a host's file", `{"name":"shop-prod","configs":{"c":{"file":"/etc/shadow"}}}`, "top: configs"},
		{"models", `{"name":"shop-prod","models":{"m":{}}}`, "top: models"},
		{"a service's secrets", svc(`,"secrets":["k"]`), "web: secrets"},
		{"a service's configs", svc(`,"configs":["c"]`), "web: configs"},
		{"the socket, by compose's own key", svc(`,"use_api_socket":true`), "web: use_api_socket"},
		{"userns_mode", svc(`,"userns_mode":"host"`), "web: userns_mode"},
		{"uts", svc(`,"uts":"host"`), "web: uts"},
		{"cgroup", svc(`,"cgroup":"host"`), "web: cgroup"},
		{"device rules", svc(`,"device_cgroup_rules":["a *:* rwm"]`), "web: device_cgroup_rules"},
		{"gpus", svc(`,"gpus":"all"`), "web: gpus"},
		{"the OOM killer's score", svc(`,"oom_score_adj":-1000`), "web: oom_score_adj"},
		{"the OOM killer off", svc(`,"oom_kill_disable":true`), "web: oom_kill_disable"},
		{"real time cpu", svc(`,"cpu_rt_runtime":950000`), "web: cpu_rt_runtime"},
		{"a storage driver's options", svc(`,"storage_opt":{"size":"1T"}`), "web: storage_opt"},
		{"a name of its own", svc(`,"container_name":"other-prod-web-1"`), "web: container_name"},
		{"links", svc(`,"links":["x"]`), "web: links"},
		{"external links", svc(`,"external_links":["other-prod-web-1"]`), "web: external_links"},
		{"dns", svc(`,"dns":["1.1.1.1"]`), "web: dns"},
		{"a develop block, which syncs host files", svc(`,"develop":{"watch":[]}`), "web: develop"},
		{"a stop hook as root", svc(`,"pre_stop":[{"command":["x"],"user":"0"}]`), "hook may hold only"},
		{"a hook, privileged", svc(`,"post_start":[{"command":["x"],"privileged":true}]`), "hook may hold only"},
		{"a hook's own directory", svc(`,"post_start":[{"command":["x"],"working_dir":"/"}]`), "hook may hold only"},
		{"a compose label by its longer name", svc(`,"labels":{"com.docker.compose.project.working_dir":"/x"}`), "label com.docker.compose.project.working_dir"},
		{"a role of no string", svc(`,"labels":{"devopsy.role":1}`), "the role"},
		{"a role of nothing", svc(`,"labels":{"devopsy.role":null}`), "the role"},
		{"a mount of an image, another's files", svc(`,"volumes":[{"type":"image","source":"other-prod-app","target":"/d"}]`), "a mount of type image"},
		{"a mount with no type", svc(`,"volumes":[{"source":"/","target":"/d"}]`), "a mount of type"},
		{"a mount in two words", svc(`,"volumes":["/:/h"]`), "a mount must be a map"},
		{"a volume mount's driver options", svc(`,"volumes":[{"type":"volume","source":"v","target":"/d","volume":{"driver_opts":{"device":"/"}}}]`), "a mount's volume: driver_opts"},
		{"a tmpfs mount's owner", svc(`,"volumes":[{"type":"tmpfs","target":"/d","tmpfs":{"uid":0}}]`), "a mount's tmpfs: uid"},
		{"a mount's bind in another form", svc(`,"volumes":[` + bind + `,"bind":"rshared"}]`), "a mount's bind must be a map"},
		{"a bind read-only by a word", svc(`,"volumes":[{"type":"bind","source":"/home/devopsy-shop/shop/prod/x","target":"/d","read_only":"true"}]`), "a host path"},
		{"an external volume by a word", `{"name":"shop-prod","volumes":{"v":{"name":"shop-prod_v","external":"true"}}}`, "volume v"},
		{"a volume with no name", `{"name":"shop-prod","volumes":{"v":null}}`, "volume v"},
		{"a network with no name", `{"name":"shop-prod","networks":{"n":null}}`, "plain network"},
		{"a network of the host's driver", `{"name":"shop-prod","networks":{"n":{"name":"shop-prod_n","driver":"host"}}}`, "plain network"},
		// Compose's and devopsy's labels are theirs to set, as on a volume.
		{"compose's label on a network", `{"name":"shop-prod","networks":{"n":{"name":"shop-prod_n","labels":{"com.docker.compose.project":"other-prod"}}}}`, "a label of compose's or devopsy's"},
		{"a shared network's name, not external", `{"name":"shop-prod","networks":{"n":{"name":"traefik"}}}`, "plain network"},
		{"an external network by a word, not its own", `{"name":"shop-prod","networks":{"n":{"name":"other-prod_default","external":"true"}}}`, "external must be true or false"},
		{"IPv6 of its own", `{"name":"shop-prod","networks":{"n":{"name":"shop-prod_n","enable_ipv6":true}}}`, "plain network"},
		// Compose takes a word for a boolean, IPv6 by "true", "yes" or "on" (v5.5.0).
		{"IPv6 of its own by a word", `{"name":"shop-prod","networks":{"n":{"name":"shop-prod_n","enable_ipv6":"yes"}}}`, "enable_ipv6 must be true or false"},
		{"another container's namespace", svc(`,"network_mode":"service:x"`), "network_mode service:x"},
		{"a capability by its other name", svc(`,"cap_add":["CAP_SYS_ADMIN"]`), "cap_add CAP_SYS_ADMIN"},
		{"a capability in lower case", svc(`,"cap_add":["sys_admin"]`), "cap_add sys_admin"},
		{"all capabilities", svc(`,"cap_add":["ALL"]`), "cap_add ALL"},
		{"a log driver of no string", svc(`,"logging":"syslog"`), "logging must be a map"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if errs := CheckFile(parse(t, tt.file), testPolicy, "up", &fakePaths{}); !anyHolds(errs, tt.want) {
				t.Errorf("want a refusal holding %q, got %v", tt.want, errs)
			}
		})
	}
	// A build's keys, on its own verb: each writes or reads with the deploy user's rights.
	ctx := "/home/devopsy-shop/shop/prod/code"
	for key, value := range map[string]string{
		"tags":              `["other-prod-app:1"]`,
		"cache_to":          `["type=local,dest=/home/devopsy/x"]`,
		"cache_from":        `["type=local,src=/home/devopsy/x"]`,
		"ulimits":           `{"nofile":1}`,
		"extra_hosts":       `["a:1.1.1.1"]`,
		"labels":            `{"a":"b"}`,
		"platforms":         `["linux/arm64"]`,
		"dockerfile_inline": `"FROM x"`,
	} {
		file := parse(t, `{"name":"shop-prod","services":{"web":`+safe(`,"image":"shop-prod-web","build":{"context":"`+ctx+`","`+key+`":`+value+`}`)+`}}`)
		if errs := CheckFile(file, testPolicy, "build", &fakePaths{}); !anyHolds(errs, "build: "+key) {
			t.Errorf("build: %s: %v", key, errs)
		}
	}
}

// Who a container runs as, in forms that read as something else, and the settings that take
// back what no-new-privileges gives.
func TestCheckFileIdentityForms(t *testing.T) {
	t.Parallel()
	check := func(service string) []error {
		return CheckFile(parse(t, `{"name":"shop-prod","services":{"web":`+service+`}}`), testPolicy, "up", &fakePaths{})
	}
	if errs := check(`{"user":"10001:10001","security_opt":["no-new-privileges"]}`); len(errs) != 0 {
		t.Fatalf("the control: %v", errs)
	}
	for _, user := range []string{"root", "0", "0:0", "00:1", "1:00", "01000:10001", "10001:0999", "10001:10001\n", " 10001:10001", "10001:10001:1", "-1:-1", "+1:1", "0x0:1", ""} {
		encoded, err := json.Marshal(user)
		if err != nil {
			t.Fatal(err)
		}
		if errs := check(`{"user":` + string(encoded) + `,"security_opt":["no-new-privileges"]}`); len(errs) == 0 {
			t.Errorf("user %q is taken", user)
		}
	}
	if errs := check(`{"user":10001,"security_opt":["no-new-privileges"]}`); len(errs) == 0 {
		t.Error("a user of no string is taken")
	}
	for _, opts := range []string{
		`["no-new-privileges:false"]`, `["no-new-privileges=false"]`, `["no-new-privileges=true"]`,
		`["no-new-privileges","no-new-privileges:false"]`, `["no-new-privileges","apparmor=unconfined"]`,
		`["no-new-privileges","label=disable"]`, `["seccomp=unconfined"]`, `[]`,
	} {
		if errs := check(`{"user":"10001:10001","security_opt":` + opts + `}`); len(errs) == 0 {
			t.Errorf("security_opt %s is taken", opts)
		}
	}
}

// ulimits take only limits, never rtprio or nice, which are the opposite: with them a uid
// 10001 under no-new-privileges takes SCHED_FIFO or nice -20, whose spin starves every project
// on the server (`docker run --ulimit rtprio=99 --user 10001:10001 --security-opt
// no-new-privileges` then `chrt -f 50 id -u` works, and is refused without it).
func TestCheckFileUlimitsGiveNoPriority(t *testing.T) {
	t.Parallel()
	check := func(ulimits string) []error {
		return CheckFile(parse(t, `{"name":"shop-prod","services":{"web":`+safe(`,"ulimits":`+ulimits)+`}}`), testPolicy, "up", &fakePaths{})
	}
	if errs := check(`{"nofile":65536}`); len(errs) != 0 {
		t.Fatalf("the control: %v", errs)
	}
	for _, ulimits := range []string{`{"rtprio":99}`, `{"nice":40}`, `{"rtprio":{"soft":99,"hard":99}}`} {
		if errs := check(ulimits); len(errs) == 0 {
			t.Errorf("%s is taken", ulimits)
		}
	}
}

// A service takes no fixed address on a shared network, as it takes no alias there
// (checkAliases): an alias would answer for another's name, and an address does the same to
// who looks by address, taken for good while its holder is stopped. Nothing asks for one on a
// shared network.
func TestCheckFileTakesNoFixedAddressOnASharedNetwork(t *testing.T) {
	t.Parallel()
	file := func(net, opts string) map[string]any {
		return parse(t, `{"name":"shop-prod","networks":`+net+`,"services":{"shop-web":`+safe(`,"networks":{"n":`+opts+`}`)+`}}`)
	}
	own := `{"n":{"name":"shop-prod_n"}}`
	shared := `{"n":{"name":"traefik","external":true}}`
	if errs := CheckFile(file(own, `{"ipv4_address":"172.30.0.9"}`), testPolicy, "up", &fakePaths{}); len(errs) != 0 {
		t.Fatalf("the control, its own network: %v", errs)
	}
	if errs := CheckFile(file(shared, `null`), testPolicy, "up", &fakePaths{}); len(errs) != 0 {
		t.Fatalf("the control, the shared network: %v", errs)
	}
	for _, key := range []string{"ipv4_address", "ipv6_address"} {
		if errs := CheckFile(file(shared, `{"`+key+`":"172.30.0.9"}`), testPolicy, "up", &fakePaths{}); len(errs) == 0 {
			t.Errorf("%s on a shared network is taken", key)
		}
	}
}

// deploy's resources hold no devices, as gpus is refused: a reservation's devices are the
// same request by another key, which compose takes (`docker compose config`) and asks the
// daemon for. Not run on a GPU.
func TestCheckFileResourcesHoldNoDevices(t *testing.T) {
	t.Parallel()
	check := func(deploy string) []error {
		return CheckFile(parse(t, `{"name":"shop-prod","services":{"web":`+safe(`,"deploy":`+deploy)+`}}`), testPolicy, "up", &fakePaths{})
	}
	if errs := check(`{"resources":{"limits":{"memory":"1g"}}}`); len(errs) != 0 {
		t.Fatalf("the control: %v", errs)
	}
	if errs := check(`{"resources":{"reservations":{"devices":[{"capabilities":["gpu"],"driver":"nvidia"}]}}}`); len(errs) == 0 {
		t.Error("devices, past gpus")
	}
}

// An image's name never starts with a dash, as Decide's own pull refuses one: it goes to
// `docker image inspect` and `docker pull -q` as the deploy user (checkImageLabels), one word
// of a file's, and a dash makes it a flag there.
func TestCheckFileImageIsNoFlag(t *testing.T) {
	t.Parallel()
	check := func(image string) []error {
		return CheckFile(parse(t, `{"name":"shop-prod","services":{"web":`+safe(`,"image":"`+image+`"`)+`}}`), testPolicy, "up", &fakePaths{})
	}
	if errs := check("busybox:1.37"); len(errs) != 0 {
		t.Fatalf("the control: %v", errs)
	}
	for _, image := range []string{"--all-tags", "--platform=linux/arm64", "-a"} {
		if errs := check(image); len(errs) == 0 {
			t.Errorf("%s is taken", image)
		}
	}
}

// What a request outside the forms may still word another way, each with its control.
func TestDecideRefusesOtherSpellings(t *testing.T) {
	t.Parallel()
	entry := "/space/mnt/shop-prod/data"
	own := []string{"-v", entry + ":/mnt/data", "util:1", "sh", "-c", "own script", "sh", "data", "1:2"}
	if _, err := decide(t, append([]string{"run"}, own...)...); err != nil {
		t.Fatalf("the control: %v", err)
	}
	for _, flag := range [][]string{
		{"--entrypoint", "/bin/sh"}, {"--env-file", "/x"}, {"--volumes-from", "victim"}, {"--device", "/dev/sda"},
		{"--ipc", "host"}, {"--uts", "host"}, {"--userns", "host"}, {"--cgroupns", "host"}, {"--add-host", "a:1.1.1.1"},
		{"--tmpfs", "/x"}, {"--security-opt", "seccomp=unconfined"}, {"--restart", "always"}, {"--workdir", "/"},
		{"--network=host"}, {"--mount=type=bind,source=/,target=/mnt/data"}, {"--volume=/:/mnt/data"}, {"--name=x"},
		{"-eA=1"}, {"-u0"}, {"-p127.0.0.1::873"}, {"-v/:/mnt/data"}, {"--init"}, {"--pull", "always"}, {"--platform", "x"},
		{"--cidfile", "/home/devopsy/x"}, {"--read-only=false"}, {"--runtime", "x"}, {"--cap-drop", "ALL"},
	} {
		if a, err := decide(t, append(append([]string{"run"}, flag...), own...)...); err == nil {
			t.Errorf("%q: %q", flag, a.Docker)
		}
	}
	for _, argv := range [][]string{
		{"ps", "--size"},
		{"ps", "-l"},
		{"ps", "-n", "1"},
		{"ps", "-a=true"},
		{"inspect", "-s", "mine"},
		{"inspect", "--size", "mine"},
		{"network", "ls"},
		{"network", "rm", "shop-prod_default"},
		{"network", "create", "shop-prod_x"},
		{"network", "disconnect", "traefik", "mine"},
		{"network", "inspect", "--verbose", "shop-prod_default"},
		{"network", "prune", "-f"},
		{"image", "ls"},
		{"image", "save", "busybox"},
		{"image", "load"},
		{"image", "tag", "x", "busybox:1.37"},
		{"image", "rm", "--force", "shop-prod-app"},
		{"image", "rm", "--no-prune", "shop-prod-app"},

		{"volume", "ls"},
		{"volume", "prune", "-f"},
		{"volume", "create", "--driver", "x", "--label", "com.docker.compose.project=shop-prod", "--label", "com.docker.compose.volume=d", "shop-prod_d"},
		{"volume", "create", "--label", "com.docker.compose.project=shop-prod", "--label", "com.docker.compose.volume=d", "--driver", "x", "shop-prod_d"},
		{"volume", "create", "--label", "com.docker.compose.project=shop-prod", "--label", "com.docker.compose.volume=d", "-o", "device=/", "shop-prod_d"},
		{"volume", "create", "--label", "com.docker.compose.project=shop-prod", "--label", "com.docker.compose.volume=d", "--label", "devopsy.env=other-prod", "shop-prod_d"},
		{"volume", "create", "--label", "com.docker.compose.project=shop-prod", "--label", "com.docker.compose.volume=d", "shop-prod_other"},
		{"volume", "create", "--label", "com.docker.compose.project=shop-prod", "--label", "com.docker.compose.volume=../d", "shop-prod_../d"},
		{"volume", "inspect", "--format", "x", "other-prod_data"},
		{"tag", "x"},
		{"tag", "x", "y", "busybox:devopsy-shop-prod-1"},
		{"rm", "--link", "mine"},
		{"rm", "-v", "mine"},
		{"rm", "mine", "victim"},
		{"pull", "busybox"},
		{"pull", "-q"},
		{"pull", "-q", "--platform=x"},
		{"pull", "-a", "busybox"},
		{"pull", "-q", "busybox", "busybox"},
		{"port", "mine"},
		{"port", "mine", "22"},
		{"port", "--help", "873"},
		{"exec", "-i", "-u", "0", "mine", "rsync", "--server", "--daemon", "--config=/etc/rsyncd.conf", "."},
		{"exec", "-i", "mine", "rsync", "--server", "--daemon", "--config=/etc/rsyncd.conf", ".", "--help"},
		{"exec", "mine", "rsync", "--server", "--daemon", "--config=/etc/rsyncd.conf", "."},
		{"exec", "-i", "--privileged", "mine", "rsync", "--server", "--daemon", "--config=/etc/rsyncd.conf", "."},
		{"exec", "-i", "mine", "rsync", "rsync://127.0.0.1/"},
		{"logs", "mine"},
		{"cp", "mine:/x", "/y"},
		{"stop", "mine"},
		{"kill", "mine"},
		{"restart", "mine"},
		{"start", "mine"},
		{"update", "--memory", "1g", "mine"},
		{"commit", "mine", "x"},
		{"export", "mine"},
		{"save", "busybox"},
		{"load"},
		{"import", "-"},
		{"login"},
		{"context", "ls"},
		{"plugin", "install", "x"},
		{"build", "-t", "util:1", "--file", "/x", "-"},
		{"build", "--network", "host", "-t", "util:1", "-"},
		{"build", "-q", "-t", "util:1"},
		{"build", "-"},
	} {
		if a, err := decide(t, argv...); err == nil {
			t.Errorf("%q: %+v", argv, a)
		}
	}
}

// A compose command's flags, in the spellings compose reads, past what TestCheckVerb lists.
func TestCheckVerbOtherSpellings(t *testing.T) {
	t.Parallel()
	for _, argv := range [][]string{
		{"exec", "-Tu0", "web", "sh"},
		{"exec", "-u=0", "web", "sh"},
		{"exec", "--user=0", "web", "sh"},
		{"exec", "--user", "", "web", "sh"},
		{"exec", "-u", "0:1", "web", "sh"},
		{"exec", "-u", "1:root", "web", "sh"},
		{"exec", "--index", "1", "web", "sh"},
		{"exec", "--", "web", "sh"},
		{"run", "-u", "0", "web", "sh"},
		{"run", "--user=0", "web", "sh"},
		{"run", "--entrypoint", "/x", "web"},
		{"run", "--name", "other-prod-web-1", "web"},
		{"run", "-l", "devopsy.role=proxy", "web"},
		{"run", "--label=devopsy.role=proxy", "web"},
		{"run", "--volume=/:/h", "web"},
		{"run", "--publish", "80:80", "web"},
		{"run", "--use-aliases", "web"},
		{"run", "--pull", "always", "web"},
		{"run", "--build", "web"},
		{"up", "--build"},
		{"up", "--watch"},
		{"up", "--scale", "web=99"},
		{"up", "--attach", "web", "--env-from-file", "/x"},
		{"create", "--build"},
		{"create", "--scale", "web=2"},
		{"build", "--ssh", "default"},
		{"build", "--build-arg", "A=1"},
		{"build", "--builder", "x"},
		{"build", "--push"},
		{"down", "--rmi"},
		{"down", "--rmi="},
		{"down", "--rmi", "ALL"},
		{"logs", "--until", "1"},
		{"rm", "--force=true"},
		{"cp", "web:/x", "/y"},
		{"attach", "web"},
		{"commit", "web", "x"},
		{"export", "web"},
		{"events"},
		{"ls"},
		{"pause", "web"},
		{"push"},
		{"scale", "web=9"},
		{"stats"},
		{"unpause", "web"},
		{"volumes"},
		{"wait", "web"},
		{"watch"},
		{"--project-directory", "/x", "up"},
		{"-f", "/x", "up"},
	} {
		if err := testPolicy.CheckVerb(argv); err == nil {
			t.Errorf("%q is taken", argv)
		}
	}
}
