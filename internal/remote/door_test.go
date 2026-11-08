package remote

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// A project user's entry is a file of its own beside the config, which devopsy writes and
// devopsy-server's rewrite of config.yaml leaves alone; door: users: in config.yaml still
// counts, and a user in both is refused, as is an entry on a server with no door.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv).
func TestServerDoorReadsUserEntries(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEVOPSY_HOME", home)
	write := func(name, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(home, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := testfile.Write(filepath.Join(home, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entry := filepath.Join("door", "users", "devopsy-blog.yaml")
	write(entry, "project: blog\njoinable: [tunnel]\n")
	if _, err := ServerDoor(); err == nil || !strings.Contains(err.Error(), "no door:") {
		t.Errorf("an entry with no door: %v", err)
	}
	write(ConfigFile, "door:\n  space: /space\n  users:\n    devopsy-shop: {project: shop}\n")
	u, err := DoorSettingsOf("devopsy-blog")
	if err != nil {
		t.Fatal(err)
	}
	if u.Project != "blog" || u.Space != "/space" || !slices.Equal(u.Joinable, []string{"tunnel"}) || !slices.Contains(u.Others, "shop") {
		t.Errorf("the entry's user: %+v", u)
	}
	if u, err := DoorSettingsOf("devopsy-shop"); err != nil || !slices.Contains(u.Others, "blog") {
		t.Errorf("config.yaml's user, beside the entry's: %+v %v", u, err)
	}
	write(filepath.Join("door", "users", "devopsy-shop.yaml"), "project: shop\n")
	if _, err := ServerDoor(); err == nil || !strings.Contains(err.Error(), "too") {
		t.Errorf("a user in both: %v", err)
	}
	write(filepath.Join("door", "users", "devopsy-shop.yaml"), "project: shop\nshell: /bin/bash\n")
	write(ConfigFile, "door:\n  space: /space\n")
	if _, err := ServerDoor(); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Errorf("an entry with a key nobody knows: %v", err)
	}
}
