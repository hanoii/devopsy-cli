package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		{Name: "goaccess", Transfer: DataMnt, Stop: StopNever, Dirs: []string{"report"}, HasData: true},
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
		if s.Name != w.Name || s.Transfer != w.Transfer || s.Stop != w.Stop || s.StopLabeled != w.StopLabeled || s.HasData != w.HasData || s.Restart != w.Restart || strings.Join(s.Dirs, ",") != strings.Join(w.Dirs, ",") || strings.Join(s.DependsOn, ",") != strings.Join(w.DependsOn, ",") {
			t.Errorf("%d: %+v, want %+v", i, s, w)
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

	// mnt itself subsumes every entry.
	if got := SyncWholeNotice(services); got != "" {
		t.Errorf("notice without mnt itself: %s", got)
	}
	services = append(services, SyncService{Name: "all", Transfer: DataMnt, Dirs: []string{"."}})
	if got := strings.Join(SyncDirs(services), ","); got != "." {
		t.Errorf("whole mnt: %s", got)
	}
	// Then the stream services' and those without data go too: a notice.
	if got, want := SyncWholeNotice(services), "all copies mnt whole, database (stream: database), plain (no data: plain), search (stream: index) included: mount mnt's entries (./mnt/database) on all instead of ./mnt, so each copies by its own label"; got != want {
		t.Errorf("whole mnt notice:\n%s\n%s", got, want)
	}
	if got, err := parseSyncServices([]byte(`{"services":{"x":{"labels":{"devopsy.data":"mnt"},"volumes":[{"type":"bind","source":"`+dot+`/mnt"}]}}}`), dot); err != nil || got[0].Dirs[0] != "." || SyncDirs(got)[0] != "." {
		t.Errorf("mnt bind: %+v %v", got, err)
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
