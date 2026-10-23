package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// SyncServices reads compose's configuration: data and stop from the
// labels, binds under mnt as their top-level entries, profile services
// included, depends_on and the restart policy.
func TestSyncServices(t *testing.T) {
	root := project(t, "app", map[string]string{"compose.yaml": minimalCompose})
	dot := filepath.Join(root, ProjectDirName)
	// On servers mnt is a link to shared/mnt, which compose may resolve.
	shared := filepath.Join(filepath.Dir(root), "shared", "mnt")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(shared, filepath.Join(dot, "mnt")); err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(shared)
	bind := func(src string) map[string]string {
		return map[string]string{"type": "bind", "source": src, "target": "/x"}
	}
	config := map[string]any{"services": map[string]any{
		"app": map[string]any{
			"labels":     map[string]string{DataLabel: "mnt", DataStopLabel: "import", DataExcludeLabel: "logs, cache"},
			"volumes":    []any{bind(dot + "/mnt/storage/files"), bind(dot + "/mnt/logs"), bind("/etc/hosts"), map[string]string{"type": "volume", "source": "cache"}},
			"depends_on": map[string]any{"database": map[string]any{"condition": "service_healthy"}},
			"restart":    "always",
		},
		// The exclude label is ignored on a stream service, not an error.
		"database": map[string]any{
			"labels":  map[string]string{DataLabel: "stream", DataStopLabel: "never", DataExcludeLabel: "database/x"},
			"volumes": []any{bind(dot + "/mnt/database")},
		},
		"goaccess": map[string]any{"profiles": []string{"tools"}, "labels": map[string]string{DataLabel: "mnt"}, "volumes": []any{bind(resolved + "/report")}},
		"mntnot":   map[string]any{"volumes": []any{bind(dot + "/mntx/a")}},
		// none: no data, its own (from a variable) ignored, and its exclude.
		"plain":  map[string]any{"labels": map[string]string{DataLabel: "none", DataStopLabel: "own", DataExcludeLabel: "a/b"}, "volumes": []any{bind(dot + "/mnt/plain")}},
		"search": map[string]any{"labels": map[string]string{DataLabel: "stream", DataStopLabel: "own"}, "volumes": []any{bind(dot + "/mnt/index")}, "depends_on": map[string]any{"database": map[string]any{}}},
		// No data and a stop: an app without data of its own.
		"worker": map[string]any{"labels": map[string]string{DataStopLabel: "always"}},
	}}
	out, _ := json.Marshal(config)
	var gotEnv []string
	old := ComposeConfig
	defer func() { ComposeConfig = old }()
	ComposeConfig = func(cwd string, environ []string) ([]byte, error) { gotEnv = environ; return out, nil }

	dir, services, err := SyncServices(root, []string{"A=1"})
	if err != nil || dir != dot || len(gotEnv) != 1 {
		t.Fatalf("%v %s %v", err, dir, gotEnv)
	}
	want := []SyncService{
		// logs excluded by its label.
		{Name: "app", Transfer: DataMnt, Stop: StopImport, StopLabeled: true, Dirs: []string{"storage"}, HasData: true, DependsOn: []string{"database"}, Restart: "always"},
		{Name: "database", Transfer: DataStream, Stop: StopNever, StopLabeled: true, Dirs: []string{"database"}, HasData: true},
		{Name: "goaccess", Transfer: DataMnt, Stop: StopNever, Dirs: []string{"report"}, HasData: true, Profiles: []string{"tools"}},
		{Name: "mntnot", Transfer: DataNone, Stop: StopNever},
		{Name: "plain", Transfer: DataNone, Stop: StopNever, StopLabeled: true, Dirs: []string{"plain"}},
		{Name: "search", Transfer: DataStream, Stop: StopOwn, StopLabeled: true, Dirs: []string{"index"}, HasData: true, DependsOn: []string{"database"}},
		{Name: "worker", Transfer: DataNone, Stop: StopAlways, StopLabeled: true},
	}
	if len(services) != len(want) {
		t.Fatalf("services: %+v", services)
	}
	for i, s := range services {
		w := want[i]
		if s.Name != w.Name || s.Transfer != w.Transfer || s.Stop != w.Stop || s.StopLabeled != w.StopLabeled || s.HasData != w.HasData || s.Restart != w.Restart || strings.Join(s.Dirs, ",") != strings.Join(w.Dirs, ",") || strings.Join(s.DependsOn, ",") != strings.Join(w.DependsOn, ",") || strings.Join(s.Profiles, ",") != strings.Join(w.Profiles, ",") {
			t.Errorf("%d: %+v, want %+v", i, s, w)
		}
	}
	// A service in a profile is up only with it (COMPOSE_PROFILES).
	goaccess, _ := SyncFind(services, "goaccess")
	app, _ := SyncFind(services, "app")
	for profiles, want := range map[string]bool{"": false, "web": false, "web, tools": true, "*": true} {
		if goaccess.Active(profiles) != want || !app.Active(profiles) {
			t.Errorf("active with %q: goaccess %v, app %v", profiles, goaccess.Active(profiles), app.Active(profiles))
		}
	}
	if got := strings.Join(SyncDirs(services), ","); got != "report,storage" {
		t.Errorf("dirs, stream, none and excluded left out: %s", got)
	}
	if got := strings.Join(DataMntServices(services), ","); got != "app,goaccess" {
		t.Errorf("mnt: %s", got)
	}
	if s, ok := SyncFind(services, "search"); !ok || s.Transfer != DataStream {
		t.Errorf("find: %+v %v", s, ok)
	}
	mnt, stream, stop := DataLists(services, []string{"worker", "app"})
	if mnt != "app goaccess" || stream != "database search" || stop != "worker app" {
		t.Errorf("lists: %q %q %q", mnt, stream, stop)
	}

	// mnt whole is no entry: refused as a mnt service's, ignored on one
	// without data (a read-only tool).
	whole := `"volumes":[{"type":"bind","source":"` + dot + `/mnt"}]`
	if _, err := parseSyncServices([]byte(`{"services":{"x":{"labels":{"devopsy.data":"mnt"},`+whole+`}}}`), dot); err == nil || !strings.Contains(err.Error(), "service x: devopsy.data=mnt mounts ./mnt whole: mount its entries (./mnt/<name>)") {
		t.Errorf("mnt whole as data: %v", err)
	}
	if got, err := parseSyncServices([]byte(`{"services":{"x":{`+whole+`}}}`), dot); err != nil || len(got[0].Dirs) != 0 {
		t.Errorf("mnt whole without data: %+v %v", got, err)
	}
	// An entry with whitespace in its name would split in the lists and
	// modules that name it: refused on a mnt service, fine elsewhere.
	spaced := `"volumes":[{"type":"bind","source":"` + dot + `/mnt/my files"}]`
	if _, err := parseSyncServices([]byte(`{"services":{"x":{"labels":{"devopsy.data":"mnt"},`+spaced+`}}}`), dot); err == nil || !strings.Contains(err.Error(), "service x: ./mnt/my files: an entry of mnt a copy carries cannot have whitespace") {
		t.Errorf("an entry with a space: %v", err)
	}
	if _, err := parseSyncServices([]byte(`{"services":{"x":{"labels":{"devopsy.data":"stream"},`+spaced+`}}}`), dot); err != nil {
		t.Errorf("an entry with a space on a stream service: %v", err)
	}

	// Values devopsy does not know are errors naming the service; the
	// labels and values before devopsy.data name what replaces them; an
	// empty devopsy.data (a variable left unset) is no way to turn
	// copying off; own needs data to stop for.
	for label, want := range map[string]string{
		`"devopsy.data":"pause"`:                               "service db: devopsy.data=pause: mnt, stream or none",
		`"devopsy.data":""`:                                    "service db: devopsy.data is empty: mnt, stream, or none for no data",
		`"devopsy.data":"rsync"`:                               "service db: devopsy.data=rsync: mnt now",
		`"devopsy.data":"files"`:                               "service db: devopsy.data=files: mnt now",
		`"devopsy.data":"skip"`:                                "service db: devopsy.data=skip: none now",
		`"devopsy.data":"mnt","devopsy.data.stop":"yes"`:       "service db: devopsy.data.stop=yes: never, own, import or always",
		`"devopsy.data":"mnt","devopsy.data.stop":""`:          "service db: devopsy.data.stop=: never, own, import or always",
		`"devopsy.data":"mnt","devopsy.data.stop":"true"`:      "(true was own, false never)",
		`"devopsy.data.stop":"own"`:                            "service db: devopsy.data.stop=own needs a devopsy.data label",
		`"devopsy.data":"mnt","devopsy.data.exclude":"a/b"`:    "service db: devopsy.data.exclude=a/b: entries of mnt only",
		`"devopsy.sync":"rsync"`:                               "service db: devopsy.sync=rsync: that label is now devopsy.data (rsync is mnt, no label or skip is none)",
		`"devopsy.data":"mnt","devopsy.sync.stop":"true"`:      "service db: devopsy.sync.stop=true: that label is now devopsy.data.stop (true is own",
		`"devopsy.data":"mnt","devopsy.sync.rsync.exclude":""`: "service db: devopsy.sync.rsync.exclude=: that label is now devopsy.data.exclude",
	} {
		if _, err := parseSyncServices([]byte(`{"services":{"db":{"labels":{`+label+`}}}}`), dot); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", label, err)
		}
	}

	// What is left in the sync capability's directory names the new one.
	write(t, filepath.Join(dot, "capabilities", "sync", "export"), "#!/bin/sh\n")
	if _, _, err := SyncServices(root, nil); err == nil || !strings.Contains(err.Error(), ".devopsy/capabilities/sync/ is now .devopsy/capabilities/data/: move its files (export) there") {
		t.Errorf("capabilities/sync: %v", err)
	}
}

// devopsy.mnt.owner covers the entries a service mounts writable,
// excluded ones too, in every profile; a read-only mount claims none;
// numbers only; two owners on one entry conflict, an unlabeled writer
// does not.
func TestMntOwners(t *testing.T) {
	dot := filepath.Join(t.TempDir(), ProjectDirName)
	parse := func(services string) ([]SyncService, error) {
		return parseSyncServices([]byte(`{"services":{`+services+`}}`), dot)
	}
	bind := func(src string, ro bool) string {
		return fmt.Sprintf(`{"type":"bind","source":%q,"target":"/x","read_only":%v}`, dot+"/"+src, ro)
	}
	services, err := parse(`"app":{"labels":{"devopsy.data":"mnt","devopsy.data.exclude":"logs","devopsy.mnt.owner":"10001:10001"},"volumes":[` + bind("mnt/storage/files", false) + `,` + bind("mnt/logs", false) + `,` + bind("mnt/config", true) + `]},
"goaccess":{"labels":{"devopsy.mnt.owner":"20000:20000"},"volumes":[` + bind("mnt/logs", true) + `,` + bind("mnt/report", false) + `]},
"database":{"profiles":["db"],"labels":{"devopsy.data":"stream","devopsy.mnt.owner":"999:999"},"volumes":[` + bind("mnt/database", false) + `]},
"sqlite":{"profiles":["tools"],"volumes":[` + bind("mnt/storage", false) + `]}`)
	if err != nil {
		t.Fatal(err)
	}
	app, _ := SyncFind(services, "app")
	if app.Owner != "10001:10001" || strings.Join(app.Writable, ",") != "logs,storage" || strings.Join(app.Dirs, ",") != "config,storage" {
		t.Errorf("app: %+v", app)
	}
	owners, err := MntOwners(services)
	if err != nil || fmt.Sprint(owners) != "map[database:999:999 logs:10001:10001 report:20000:20000 storage:10001:10001]" {
		t.Errorf("owners: %v %v", owners, err)
	}
	if err := CheckMounts(services); err != nil {
		t.Errorf("an unlabeled writer of an owned entry, read-only readers: %v", err)
	}
	// Two owners on one entry.
	services, err = parse(`"app":{"labels":{"devopsy.mnt.owner":"10001:10001"},"volumes":[` + bind("mnt/logs", false) + `]},
"goaccess":{"labels":{"devopsy.mnt.owner":"20000:20000"},"volumes":[` + bind("mnt/logs", false) + `]}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MntOwners(services); err == nil || err.Error() != "./mnt/logs is written by app (devopsy.mnt.owner=10001:10001) and goaccess (20000:20000): one owner per entry" {
		t.Errorf("two owners: %v", err)
	}
	if err := CheckMounts(services); err == nil || !strings.Contains(strings.Join(report.Details(err), "\n"), "one owner per entry") {
		t.Errorf("the mounts check, two owners: %v", err)
	}
	for label, want := range map[string]string{
		`"devopsy.mnt.owner":"app"`:       "service x: devopsy.mnt.owner=app: <uid>:<gid> in numbers",
		`"devopsy.mnt.owner":""`:          "service x: devopsy.mnt.owner=: <uid>:<gid> in numbers",
		`"devopsy.mnt.owner":"10001"`:     "service x: devopsy.mnt.owner=10001: <uid>:<gid>",
		`"devopsy.mnt.owner":"10001:app"`: "service x: devopsy.mnt.owner=10001:app: <uid>:<gid>",
		`"devopsy.mnt.owner":"+1:1"`:      "service x: devopsy.mnt.owner=+1:1: <uid>:<gid> in numbers from 0 to 4294967294",
		// Owners no uid_t holds, which find and chown refuse after the
		// switch (review of 2026-10-23), and chown's -1.
		`"devopsy.mnt.owner":"4294967296:0"`:           "service x: devopsy.mnt.owner=4294967296:0: <uid>:<gid> in numbers from 0 to 4294967294",
		`"devopsy.mnt.owner":"0:4294967296"`:           "devopsy.mnt.owner=0:4294967296: <uid>:<gid>",
		`"devopsy.mnt.owner":"4294967295:4294967295"`:  "devopsy.mnt.owner=4294967295:4294967295: <uid>:<gid>",
		`"devopsy.mnt.owner":"99999999999999999999:1"`: "devopsy.mnt.owner=99999999999999999999:1: <uid>:<gid>",
	} {
		if _, err := parse(`"x":{"labels":{` + label + `}}`); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", label, err)
		}
	}
	// Owners are numbers: 010001 is 10001, one owner, said alike.
	services, err = parse(`"app":{"labels":{"devopsy.mnt.owner":"010001:0010001"},"volumes":[` + bind("mnt/logs", false) + `]},
"worker":{"labels":{"devopsy.mnt.owner":"10001:10001"},"volumes":[` + bind("mnt/logs", false) + `]},
"root":{"labels":{"devopsy.mnt.owner":"0:00"},"volumes":[` + bind("mnt/top", false) + `]},
"max":{"labels":{"devopsy.mnt.owner":"4294967294:4294967294"},"volumes":[` + bind("mnt/max", false) + `]}`)
	if err != nil {
		t.Fatal(err)
	}
	if owners, err := MntOwners(services); err != nil || fmt.Sprint(owners) != "map[logs:10001:10001 max:4294967294:4294967294 top:0:0]" {
		t.Errorf("owners as numbers: %v %v", owners, err)
	}
	// mnt whole: refused writable with an owner, ignored read-only.
	if _, err := parse(`"x":{"labels":{"devopsy.mnt.owner":"1:1"},"volumes":[` + bind("mnt", false) + `]}`); err == nil || !strings.Contains(err.Error(), "service x: devopsy.mnt.owner=1:1 mounts ./mnt whole writable") {
		t.Errorf("mnt whole owned: %v", err)
	}
	if _, err := parse(`"x":{"labels":{"devopsy.mnt.owner":"1:1"},"volumes":[` + bind("mnt", true) + `,` + bind("mnt/a", false) + `]}`); err != nil {
		t.Errorf("mnt whole read-only: %v", err)
	}
	if _, err := parse(`"x":{"labels":{"devopsy.mnt.owner":"1:1"},"volumes":[` + bind("mnt/my files", false) + `]}`); err == nil || !strings.Contains(err.Error(), "an entry of mnt devopsy.mnt.owner owns cannot have whitespace") {
		t.Errorf("an owned entry with a space: %v", err)
	}
}

// The mounts check refuses each mount whose data is not what the labels
// say: a writable bind from the release's tree outside mnt (read-only
// ones, and shared/'s links out of it, stay), mnt data with no entry,
// an owner of nothing.
func TestCheckMounts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "code")
	dot := filepath.Join(root, ProjectDirName)
	shared := filepath.Join(filepath.Dir(root), "shared")
	for _, d := range []string{filepath.Join(shared, "mnt"), filepath.Join(shared, "conf"), filepath.Join(root, "src"), dot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range []string{"mnt", "conf"} {
		if err := os.Symlink(filepath.Join(shared, l), filepath.Join(dot, l)); err != nil {
			t.Fatal(err)
		}
	}
	bind := func(src string, ro bool) string {
		return fmt.Sprintf(`{"type":"bind","source":%q,"target":"/x","read_only":%v}`, src, ro)
	}
	check := func(services string) error {
		t.Helper()
		parsed, err := parseSyncServices([]byte(`{"services":{`+services+`}}`), dot)
		if err != nil {
			t.Fatal(err)
		}
		return CheckMounts(parsed)
	}
	if err := check(`"app":{"labels":{"devopsy.data":"mnt"},"volumes":[` + bind(root+"/src", true) + `,` + bind(dot+"/Caddyfile", true) + `,` + bind(dot+"/conf/x", false) + `,` + bind(dot+"/mnt/storage", false) + `,` + bind("/var/run/docker.sock", false) + `]}`); err != nil {
		t.Errorf("read-only tree mounts, shared/'s links, mnt and absolute paths: %v", err)
	}
	for services, want := range map[string]string{
		`"app":{"volumes":[` + bind(root+"/src", false) + `]}`:                                      "app mounts ./src writable: it is in the release, and the next one replaces it",
		`"app":{"volumes":[` + bind(dot+"/cache/x", false) + `]}`:                                   "app mounts ./.devopsy/cache/x writable",
		`"app":{"labels":{"devopsy.data":"mnt"},"volumes":[` + bind(root+"/src", true) + `]}`:       "app is labeled devopsy.data=mnt and copies no entry of mnt",
		`"app":{"labels":{"devopsy.mnt.owner":"1:1"},"volumes":[` + bind(dot+"/mnt/a", true) + `]}`: "app has devopsy.mnt.owner=1:1 and mounts no entry of mnt writable: it owns nothing",
	} {
		err := check(services)
		if err == nil || err.Error() != "the mounts check refuses the release: it does not go live" || !strings.Contains(strings.Join(report.Details(err), "\n"), want) {
			t.Errorf("%s: %v %q", services, err, report.Details(err))
		}
	}
}

// Compose lists the implicit dependencies under depends_on too (as
// docker compose v5.5.0 prints a.network_mode service:db, b.links,
// c.volumes_from, and d's required: false), so they count for the
// services that stop with a stopped one.
func TestSyncServicesImplicitDependencies(t *testing.T) {
	out := []byte(`{"services":{
"a":{"network_mode":"service:db","depends_on":{"db":{"condition":"service_started","restart":true,"required":true}}},
"b":{"links":["db"],"depends_on":{"db":{"condition":"service_started","restart":true,"required":true}}},
"c":{"volumes_from":["db"],"depends_on":{"db":{"condition":"service_started","required":true}}},
"d":{"depends_on":{"db":{"condition":"service_healthy","required":false}}},
"db":{"labels":{"devopsy.data":"mnt","devopsy.data.stop":"own"}}}}`)
	services, err := parseSyncServices(out, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	set := DataStops(services, ExportOne, "db")
	if got := StoppingLine(set, StoppedNames(set)); got != "stopping db (own), a (depends on db), b (depends on db), c (depends on db), d (depends on db)" {
		t.Errorf("implicit dependencies: %s", got)
	}
}

// DataStops is the table of devopsy.data.stop per operation, the union
// for a sync, and the services depending on a stopped one, transitively.
func TestDataStops(t *testing.T) {
	svc := func(name, data, stop string, deps ...string) SyncService {
		return SyncService{Name: name, Transfer: data, Stop: stop, HasData: data != DataNone, DependsOn: deps}
	}
	names := func(set []Stopped) string { return strings.Join(StoppedNames(set), ",") }

	// The table: one service of each stop word, all with data, nothing
	// depending on another.
	table := []SyncService{svc("always", DataMnt, StopAlways), svc("import", DataMnt, StopImport), svc("never", DataMnt, StopNever), svc("own", DataMnt, StopOwn)}
	for _, c := range []struct {
		op      DataOp
		service string
		want    string
	}{
		// its own data exported: own and always down.
		{ExportOne, "own", "always,own"},
		{ExportOne, "import", "always"},
		{ExportOne, "never", "always"},
		// another's exported: always only.
		{ExportOne, "always", "always"},
		// its own imported: own, import and always.
		{ImportOne, "own", "always,import,own"},
		{ImportOne, "import", "always,import"},
		// another's imported: import and always, never own.
		{ImportOne, "never", "always,import"},
		// a sync: every own data service, and always, on the parent; and
		// import too on the child.
		{SyncParent, "", "always,own"},
		{SyncChild, "", "always,import,own"},
	} {
		if got := names(DataStops(table, c.op, c.service)); got != c.want {
			t.Errorf("op %d of %s: %s, want %s", c.op, c.service, got, c.want)
		}
	}

	// The union: two own databases, each down while the other is
	// exported too.
	two := []SyncService{svc("a", DataMnt, StopOwn), svc("b", DataStream, StopOwn), svc("app", DataNone, StopNever)}
	if got := names(DataStops(two, SyncParent, "")); got != "a,b" {
		t.Errorf("union: %s", got)
	}
	if got := names(DataStops(two, ExportOne, "a")); got != "a" {
		t.Errorf("one own: %s", got)
	}

	// Dependents follow: a database own, its app unlabeled; transitively
	// (worker on app on database); dependencies first, with why.
	shop := []SyncService{
		svc("app", DataMnt, StopNever, "database"),
		svc("cache", DataNone, StopNever),
		svc("database", DataMnt, StopOwn),
		svc("worker", DataNone, StopNever, "app"),
	}
	set := DataStops(shop, ExportOne, "database")
	if got := StoppingLine(set, StoppedNames(set)); got != "stopping database (own), app (depends on database), worker (depends on app)" {
		t.Errorf("dependents: %s", got)
	}
	// Only the running ones are said.
	if got := StoppingLine(set, []string{"app", "database"}); got != "stopping database (own), app (depends on database)" {
		t.Errorf("running only: %s", got)
	}
	if got := names(DataStops(shop, ExportOne, "app")); got != "" {
		t.Errorf("the app's storage exported, never: %s", got)
	}
	// A stream database (never) with the app import: the app and its
	// worker for an import, nothing for an export.
	shop[0].Stop, shop[2].Transfer, shop[2].Stop = StopImport, DataStream, StopNever
	if got := StoppingLine(DataStops(shop, ImportOne, "database"), []string{"app", "worker"}); got != "stopping app (import), worker (depends on app)" {
		t.Errorf("stream import: %s", got)
	}
	if got := names(DataStops(shop, ExportOne, "database")); got != "" {
		t.Errorf("stream export: %s", got)
	}
	if got := names(DataStops(shop, SyncParent, "")); got != "" {
		t.Errorf("stream, parent side: %s", got)
	}
	if got := names(DataStops(shop, SyncChild, "")); got != "app,worker" {
		t.Errorf("stream, child side: %s", got)
	}
}

// The services left running that depend on an imported one, unlabeled:
// named; an explicit never, one in the set, and the imported one itself
// are not.
func TestUnlabeledDependents(t *testing.T) {
	services := []SyncService{
		{Name: "app", DependsOn: []string{"database"}, Stop: StopImport, StopLabeled: true},
		{Name: "cron", DependsOn: []string{"worker"}},
		{Name: "database", Transfer: DataStream, HasData: true},
		{Name: "quiet", DependsOn: []string{"database"}, Stop: StopNever, StopLabeled: true},
		{Name: "solo"},
		{Name: "worker", DependsOn: []string{"database"}},
	}
	set := DataStops(services, ImportOne, "database")
	got := UnlabeledDependents(services, []string{"database"}, set)
	if len(got) != 2 || got[0] != (Dependent{"cron", "database"}) || got[1] != (Dependent{"worker", "database"}) {
		t.Errorf("dependents: %+v", got)
	}
	if msg := DependentNotice(got[1]); msg != "worker depends on database and keeps running while it is imported: label it devopsy.data.stop=import, or never to keep it running" {
		t.Errorf("notice: %s", msg)
	}
}

// Stream services go in depends_on order, dependencies first, through
// services of any label, ties alphabetical; a cycle is an error.
func TestSyncStreamOrder(t *testing.T) {
	services := []SyncService{
		{Name: "a", Transfer: DataStream, DependsOn: []string{"m"}},
		{Name: "b", Transfer: DataStream},
		{Name: "c", Transfer: DataStream},
		{Name: "m", Transfer: DataMnt, DependsOn: []string{"z"}},
		{Name: "z", Transfer: DataStream, DependsOn: []string{"missing"}},
	}
	got, err := SyncStreamServices(services)
	if err != nil || strings.Join(got, ",") != "b,c,z,a" {
		t.Errorf("order: %v %v", got, err)
	}
	services[1].DependsOn = []string{"c"}
	services[2].DependsOn = []string{"b"}
	if _, err := SyncStreamServices(services); err == nil || !strings.Contains(err.Error(), "cycle among b, c") {
		t.Errorf("cycle: %v", err)
	}
	// The lists still name them, alphabetically.
	if _, stream, _ := DataLists(services, nil); stream != "a b c z" {
		t.Errorf("lists with a cycle: %q", stream)
	}
	// And the stops still end.
	services[1].Stop = StopAlways
	if got := DataStops(services, ExportOne, "b"); len(got) != 2 {
		t.Errorf("stops with a cycle: %+v", got)
	}
}
