package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SyncServices reads compose's configuration: transfers and stop from
// the labels, binds under mnt as their top-level entries, profile services
// included, and depends_on.
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
			"labels":     map[string]string{SyncLabel: "rsync", SyncStopLabel: "true", SyncExcludeLabel: "logs, cache"},
			"volumes":    []any{bind(dot + "/mnt/storage/files"), bind(dot + "/mnt/logs"), bind("/etc/hosts"), map[string]string{"type": "volume", "source": "cache"}},
			"depends_on": map[string]any{"database": map[string]any{"condition": "service_healthy"}},
		},
		// The exclude label is ignored on a stream service, not an error.
		"database": map[string]any{
			"labels":  map[string]string{SyncLabel: "stream", SyncStopLabel: "false", SyncExcludeLabel: "database/x"},
			"volumes": []any{bind(dot + "/mnt/database")},
		},
		"goaccess": map[string]any{"profiles": []string{"tools"}, "labels": map[string]string{SyncLabel: "rsync"}, "volumes": []any{bind(resolved + "/report")}},
		"mntnot":   map[string]any{"volumes": []any{bind(dot + "/mntx/a")}},
		// Without a devopsy.sync label, stop is ignored: never touched.
		"plain":  map[string]any{"labels": map[string]string{SyncStopLabel: "true"}, "volumes": []any{bind(dot + "/mnt/plain")}},
		"search": map[string]any{"labels": map[string]string{SyncLabel: "stream", SyncStopLabel: "true"}, "volumes": []any{bind(dot + "/mnt/index")}, "depends_on": map[string]any{"database": map[string]any{}}},
		// An empty value (a variable unset) is no label.
		"worker": map[string]any{"labels": map[string]string{SyncLabel: ""}},
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
		{Name: "app", Transfer: SyncRsync, Stop: true, Dirs: []string{"storage"}, Labeled: true, DependsOn: []string{"database"}},
		{Name: "database", Transfer: SyncStream, Dirs: []string{"database"}, Labeled: true},
		{Name: "goaccess", Transfer: SyncRsync, Dirs: []string{"report"}, Labeled: true},
		{Name: "mntnot", Transfer: SyncSkip},
		{Name: "plain", Transfer: SyncSkip, Dirs: []string{"plain"}},
		{Name: "search", Transfer: SyncStream, Stop: true, Dirs: []string{"index"}, Labeled: true, DependsOn: []string{"database"}},
		{Name: "worker", Transfer: SyncSkip},
	}
	if len(services) != len(want) {
		t.Fatalf("services: %+v", services)
	}
	for i, s := range services {
		if s.Name != want[i].Name || s.Transfer != want[i].Transfer || s.Stop != want[i].Stop || s.Labeled != want[i].Labeled || strings.Join(s.Dirs, ",") != strings.Join(want[i].Dirs, ",") || strings.Join(s.DependsOn, ",") != strings.Join(want[i].DependsOn, ",") {
			t.Errorf("%d: %+v, want %+v", i, s, want[i])
		}
	}
	if got := strings.Join(SyncDirs(services), ","); got != "report,storage" {
		t.Errorf("dirs, stream, unlabeled and excluded left out: %s", got)
	}
	if got := strings.Join(SyncStopped(services), ","); got != "app,search" {
		t.Errorf("stopped: %s", got)
	}
	if got := strings.Join(SyncRsyncServices(services), ","); got != "app,goaccess" {
		t.Errorf("rsync: %s", got)
	}
	if s, ok := SyncFind(services, "search"); !ok || s.Transfer != SyncStream {
		t.Errorf("find: %+v %v", s, ok)
	}
	rsync, stream, stop := SyncLists(services)
	if rsync != "app goaccess" || stream != "database search" || stop != "app search" {
		t.Errorf("lists: %q %q %q", rsync, stream, stop)
	}

	// mnt itself subsumes every entry.
	if got := SyncWholeNotice(services); got != "" {
		t.Errorf("notice without mnt itself: %s", got)
	}
	services = append(services, SyncService{Name: "all", Transfer: SyncRsync, Dirs: []string{"."}})
	if got := strings.Join(SyncDirs(services), ","); got != "." {
		t.Errorf("whole mnt: %s", got)
	}
	// Then the unlabeled and stream services' entries go too: a notice.
	if got, want := SyncWholeNotice(services), "devopsy: all copies mnt whole, database (stream: database), plain (skip: plain), search (stream: index) included: devopsy.sync.rsync.exclude=database,plain,index on all leaves them out"; got != want {
		t.Errorf("whole mnt notice:\n%s\n%s", got, want)
	}
	if got, err := parseSyncServices([]byte(`{"services":{"x":{"labels":{"devopsy.sync":"rsync"},"volumes":[{"type":"bind","source":"`+dot+`/mnt"}]}}}`), dot); err != nil || got[0].Dirs[0] != "." || SyncDirs(got)[0] != "." {
		t.Errorf("mnt bind: %+v %v", got, err)
	}

	// Values devopsy does not know are errors naming the service: the old
	// ones too.
	for label, want := range map[string]string{
		`"devopsy.sync":"pause"`:                                    "service db: devopsy.sync=pause: rsync or stream",
		`"devopsy.sync":"skip"`:                                     "service db: devopsy.sync=skip: rsync or stream",
		`"devopsy.sync":"rsync","devopsy.sync.stop":"yes"`:          "service db: devopsy.sync.stop=yes: true or false",
		`"devopsy.sync":"rsync","devopsy.sync.rsync.exclude":"a/b"`: "service db: devopsy.sync.rsync.exclude=a/b: entries of mnt only",
	} {
		if _, err := parseSyncServices([]byte(`{"services":{"db":{"labels":{`+label+`}}}}`), dot); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", label, err)
		}
	}
}

// Stream services go in depends_on order, dependencies first, through
// services of any label, ties alphabetical; a cycle is an error.
func TestSyncStreamOrder(t *testing.T) {
	services := []SyncService{
		{Name: "a", Transfer: SyncStream, DependsOn: []string{"m"}},
		{Name: "b", Transfer: SyncStream},
		{Name: "c", Transfer: SyncStream},
		{Name: "m", Transfer: SyncRsync, DependsOn: []string{"z"}},
		{Name: "z", Transfer: SyncStream, DependsOn: []string{"missing"}},
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
	if _, stream, _ := SyncLists(services); stream != "a b c z" {
		t.Errorf("lists with a cycle: %q", stream)
	}
}
