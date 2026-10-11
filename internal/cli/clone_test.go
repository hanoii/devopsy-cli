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
			"labels":  map[string]string{CloneLabel: "pause"},
			"volumes": []any{bind(dot + "/mnt/storage/files"), bind(dot + "/mnt/logs"), bind("/etc/hosts"), map[string]string{"type": "volume", "source": "cache"}},
		},
		"database": map[string]any{
			"labels":  map[string]string{CloneLabel: "skip"},
			"volumes": []any{bind(dot + "/mnt/database")},
		},
		"goaccess": map[string]any{"profiles": []string{"tools"}, "volumes": []any{bind(resolved + "/report")}},
		"mntnot":   map[string]any{"volumes": []any{bind(dot + "/mntx/a")}},
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
		{Name: "app", Policy: ClonePause, Dirs: []string{"logs", "storage"}},
		{Name: "database", Policy: CloneSkip, Dirs: []string{"database"}},
		{Name: "goaccess", Policy: CloneLive, Dirs: []string{"report"}},
		{Name: "mntnot", Policy: CloneLive},
	}
	for i, s := range services {
		if s.Name != want[i].Name || s.Policy != want[i].Policy || strings.Join(s.Dirs, ",") != strings.Join(want[i].Dirs, ",") {
			t.Errorf("%d: %+v, want %+v", i, s, want[i])
		}
	}
	if got := strings.Join(CloneDirs(services), ","); got != "logs,report,storage" {
		t.Errorf("dirs, skip left out: %s", got)
	}
	if got := ClonePolicy(services, ClonePause); len(got) != 1 || got[0] != "app" {
		t.Errorf("pause: %v", got)
	}

	// mnt itself subsumes every entry.
	services = append(services, CloneService{Name: "all", Policy: CloneStop, Dirs: []string{"."}})
	if got := strings.Join(CloneDirs(services), ","); got != "." {
		t.Errorf("whole mnt: %s", got)
	}
	if got, err := parseCloneServices([]byte(`{"services":{"x":{"volumes":[{"type":"bind","source":"`+dot+`/mnt"}]}}}`), dot); err != nil || got[0].Dirs[0] != "." {
		t.Errorf("mnt bind: %+v %v", got, err)
	}

	// A value devopsy does not know is an error naming the service.
	if _, err := parseCloneServices([]byte(`{"services":{"db":{"labels":{"devopsy.clone":"freeze"}}}}`), dot); err == nil || !strings.Contains(err.Error(), "service db: devopsy.clone=freeze") {
		t.Errorf("bad label: %v", err)
	}
}
