package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Not parallel: it sets ComposeConfig and NetworkOwner.
func TestNetworkConflicts(t *testing.T) {
	defer func(c func(string, []string) ([]byte, error), o func(string) (string, bool)) {
		ComposeConfig, NetworkOwner = c, o
	}(ComposeConfig, NetworkOwner)
	ComposeConfig = func(string, []string) ([]byte, error) {
		return []byte(`{"name":"shop-prod","networks":{"default":{"name":"shop-prod_default"},"traefik":{"name":"traefik","external":true},"backend":{"name":"backend"}}}`), nil
	}
	owners := map[string]string{"traefik": "traefik-main", "backend": "other-prod", "shop-prod_default": "shop-prod"}
	NetworkOwner = func(name string) (string, bool) { o, ok := owners[name]; return o, ok }
	err := NetworkConflicts(".", nil)
	if err == nil || !strings.Contains(err.Error(), "network backend is other-prod's") || strings.Contains(err.Error(), "traefik") || strings.Contains(err.Error(), "default") {
		t.Errorf("conflicts: %v", err)
	}
	// Own, absent, or unlabeled (made by hand): fine.
	owners = map[string]string{"shop-prod_default": "shop-prod", "backend": ""}
	if err := NetworkConflicts(".", nil); err != nil {
		t.Errorf("no conflict: %v", err)
	}
	// An unreadable configuration is the up's problem, not this check's.
	ComposeConfig = func(string, []string) ([]byte, error) { return []byte("not json"), nil }
	if err := NetworkConflicts(".", nil); err != nil {
		t.Errorf("unparsable: %v", err)
	}
}

// Not parallel: it sets ComposeConfig and ProjectDirs.
func TestProjectConflict(t *testing.T) {
	defer func(c func(string, []string) ([]byte, error), d func(string) []string) {
		ComposeConfig, ProjectDirs = c, d
	}(ComposeConfig, ProjectDirs)
	ComposeConfig = func(string, []string) ([]byte, error) { return []byte(`{"name":"shop-a-b-c"}`), nil }
	dirs := []string{"/srv/shop/a/b-c/current/.devopsy", "/srv/shop/a/b-c/releases/1/.devopsy"}
	ProjectDirs = func(name string) []string {
		if name != "shop-a-b-c" {
			t.Errorf("name: %s", name)
		}
		return dirs
	}
	if err := ProjectConflict(".", "/srv/shop/a/b-c", nil); err != nil {
		t.Errorf("own: %v", err)
	}
	if err := ProjectConflict(".", "/srv/shop/a-b/c", nil); err == nil || !strings.Contains(err.Error(), "compose project shop-a-b-c is another environment's (its containers are in /srv/shop/a/b-c/current/.devopsy)") {
		t.Errorf("another environment's: %v", err)
	}
	// A sibling whose name starts with the directory's is not inside it.
	dirs = []string{"/srv/shop/a/b-cd/current/.devopsy"}
	if err := ProjectConflict(".", "/srv/shop/a/b-c", nil); err == nil {
		t.Error("sibling")
	}
	// Through a link to the release root.
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(filepath.Join(real, "env"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	dirs = []string{filepath.Join(real, "env")}
	if err := ProjectConflict(".", filepath.Join(link, "env"), nil); err != nil {
		t.Errorf("through a link: %v", err)
	}
	dirs = nil
	if err := ProjectConflict(".", "/srv/x", nil); err != nil {
		t.Errorf("no containers: %v", err)
	}
}
