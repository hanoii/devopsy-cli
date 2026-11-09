package images

import (
	"os"
	"path/filepath"
	"testing"
)

// A door project's build has one name, devopsy's, and it reads back: the door's render
// and the listing give the same one, and nothing else is taken for it.
func TestDoorBuildName(t *testing.T) {
	t.Parallel()
	if got := DoorBuild("shop-prod", "web"); got != "devopsy.invalid/shop-prod/web" {
		t.Fatalf("DoorBuild: %s", got)
	}
	for ref, want := range map[string][2]string{
		"devopsy.invalid/shop-prod/web":                                  {"shop-prod", "web"},
		"devopsy.invalid/shop-prod/web:1":                                {"shop-prod", "web"},
		"devopsy.invalid/shop-prod/web:devopsy-shop-prod-20260101000000": {"shop-prod", "web"},
		"devopsy.invalid/shop-eu-prod/my_worker":                         {"shop-eu-prod", "my_worker"},
	} {
		name, svc, ok := IsDoorBuild(ref)
		if !ok || name != want[0] || svc != want[1] {
			t.Errorf("%s: %q %q %v", ref, name, svc, ok)
		}
	}
	for _, ref := range []string{
		"shop-prod-web", "mongo-express:1", "docker.io/devopsy.invalid/shop-prod/web",
		"devopsy.invalid/shop-prod", "devopsy.invalid/shop-prod/web/x", "devopsy.invalid//web",
		"devopsy.invalid/shop-prod/web@sha256:abc", "devopsy.invalid:5000/shop-prod/web",
	} {
		if _, _, ok := IsDoorBuild(ref); ok {
			t.Errorf("%s taken for a door build", ref)
		}
	}
}

// Behind the door the listing names a build by DoorBuild, whatever image: says, as the
// door's render does, so the record, the bookmarks and the holds go by the name compose
// runs; a service only pulled keeps its image, and so does every service of a release in
// image mode, whose builds a registry serves. The deploy user's listing is compose's.
func TestListingNamesADoorProjectsBuilds(t *testing.T) {
	t.Parallel()
	config := []byte(`{"name":"shop-prod","services":{
		"app":{"build":{"context":"."}},
		"named":{"build":{"context":"."},"image":"mongo-express:1"},
		"db":{"image":"mariadb:11.8"}}}`)
	for _, tt := range []struct {
		door bool
		want map[string]string
	}{
		{true, map[string]string{"app": "devopsy.invalid/shop-prod/app", "named": "devopsy.invalid/shop-prod/named", "db": "mariadb:11.8"}},
		{false, map[string]string{"app": "shop-prod-app", "named": "mongo-express:1", "db": "mariadb:11.8"}},
	} {
		svcs, err := ParseComposeConfig(config, tt.door)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range svcs {
			if s.Ref != tt.want[s.Name] {
				t.Errorf("door %v: %s listed as %s, want %s", tt.door, s.Name, s.Ref, tt.want[s.Name])
			}
		}
	}
	build, image := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(image, RecordFile), []byte(`{"mode":"image"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		rel        string
		door, want bool
	}{{build, true, true}, {image, true, false}, {build, false, false}} {
		if got := doorBuildsIn(tt.rel, tt.door); got != tt.want {
			t.Errorf("%s, door %v: %v", ModeOf(tt.rel), tt.door, got)
		}
	}
}
