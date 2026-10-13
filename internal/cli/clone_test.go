package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CloneServices reads compose's configuration: policies from the label,
// binds under mnt as their top-level entries, profile services included.
func TestCloneServices(t *testing.T) {
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
			"labels":  map[string]string{CloneLabel: "pause", CloneExcludeLabel: "logs, cache"},
			"volumes": []any{bind(dot + "/mnt/storage/files"), bind(dot + "/mnt/logs"), bind("/etc/hosts"), map[string]string{"type": "volume", "source": "cache"}},
		},
		"database": map[string]any{
			"labels":  map[string]string{CloneLabel: "skip"},
			"volumes": []any{bind(dot + "/mnt/database")},
		},
		"goaccess": map[string]any{"profiles": []string{"tools"}, "labels": map[string]string{CloneLabel: "live"}, "volumes": []any{bind(resolved + "/report")}},
		"mntnot":   map[string]any{"volumes": []any{bind(dot + "/mntx/a")}},
		"plain":    map[string]any{"volumes": []any{bind(dot + "/mnt/plain")}},
		"search":   map[string]any{"labels": map[string]string{CloneLabel: "custom"}, "volumes": []any{bind(dot + "/mnt/index")}},
	}}
	out, _ := json.Marshal(config)
	var gotEnv []string
	old := ComposeConfig
	defer func() { ComposeConfig = old }()
	ComposeConfig = func(cwd string, environ []string) ([]byte, error) { gotEnv = environ; return out, nil }

	dir, services, err := CloneServices(root, []string{"A=1"})
	if err != nil || dir != dot || len(gotEnv) != 1 {
		t.Fatalf("%v %s %v", err, dir, gotEnv)
	}
	want := []CloneService{
		// logs excluded by its label.
		{Name: "app", Policy: ClonePause, Dirs: []string{"storage"}, Labeled: true},
		{Name: "database", Policy: CloneSkip, Dirs: []string{"database"}, Labeled: true},
		{Name: "goaccess", Policy: CloneLive, Dirs: []string{"report"}, Labeled: true},
		{Name: "mntnot", Policy: CloneSkip},
		// Without the label, skip: neither copied nor touched.
		{Name: "plain", Policy: CloneSkip, Dirs: []string{"plain"}},
		{Name: "search", Policy: CloneCustom, Dirs: []string{"index"}, Labeled: true},
	}
	if len(services) != len(want) {
		t.Fatalf("services: %+v", services)
	}
	for i, s := range services {
		if s.Name != want[i].Name || s.Policy != want[i].Policy || s.Labeled != want[i].Labeled || strings.Join(s.Dirs, ",") != strings.Join(want[i].Dirs, ",") {
			t.Errorf("%d: %+v, want %+v", i, s, want[i])
		}
	}
	if got := strings.Join(CloneDirs(services), ","); got != "report,storage" {
		t.Errorf("dirs, skip, custom and excluded left out: %s", got)
	}
	if got := ClonePolicy(services, ClonePause); len(got) != 1 || got[0] != "app" {
		t.Errorf("pause: %v", got)
	}

	// mnt itself subsumes every entry.
	if got := CloneWholeNotice(services); got != "" {
		t.Errorf("notice without mnt itself: %s", got)
	}
	services = append(services, CloneService{Name: "all", Policy: CloneStop, Dirs: []string{"."}})
	if got := strings.Join(CloneDirs(services), ","); got != "." {
		t.Errorf("whole mnt: %s", got)
	}
	// Then the skipped and custom services' entries go too: a notice.
	if got, want := CloneWholeNotice(services), "devopsy: all copies mnt whole, database (skip: database), plain (skip: plain), search (custom: index) included: devopsy.clone.exclude=database,plain,index on all leaves them out"; got != want {
		t.Errorf("whole mnt notice:\n%s\n%s", got, want)
	}
	if got, err := parseCloneServices([]byte(`{"services":{"x":{"labels":{"devopsy.clone":"live"},"volumes":[{"type":"bind","source":"`+dot+`/mnt"}]}}}`), dot); err != nil || got[0].Dirs[0] != "." || CloneDirs(got)[0] != "." {
		t.Errorf("mnt bind: %+v %v", got, err)
	}

	// A value devopsy does not know is an error naming the service.
	if _, err := parseCloneServices([]byte(`{"services":{"db":{"labels":{"devopsy.clone":"freeze"}}}}`), dot); err == nil || !strings.Contains(err.Error(), "service db: devopsy.clone=freeze") {
		t.Errorf("bad label: %v", err)
	}
	// Excluded entries are plain names of mnt's entries.
	if _, err := parseCloneServices([]byte(`{"services":{"app":{"labels":{"devopsy.clone":"live","devopsy.clone.exclude":"logs/old"}}}}`), dot); err == nil || !strings.Contains(err.Error(), "service app: devopsy.clone.exclude=logs/old") {
		t.Errorf("bad exclude: %v", err)
	}
}
