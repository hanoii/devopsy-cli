package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// Bug: resolveRoot drops a .. by the text before it follows the link ahead of it, so a root,
// or a link's target, that goes up from a link names another directory than the system's own
// resolution: the root made and the mounts drop-in are then for a path no release uses.
func TestResolveRootGoesUpFromALinksTarget(t *testing.T) {
	t.Parallel()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "real", "sub", ".keep"), "", 0o644)
	if err := os.Symlink(filepath.Join(dir, "real", "sub"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir+"/link/../other", filepath.Join(dir, "up")); err != nil {
		t.Fatal(err)
	}
	for root, want := range map[string]string{
		dir + "/link/../made": filepath.Join(dir, "real", "made"),
		dir + "/up/srv":       filepath.Join(dir, "real", "other", "srv"),
	} {
		if got := resolveRoot(root); got != want {
			t.Errorf("resolveRoot(%s) = %s, want %s", root, got, want)
		}
	}
	// The system's own answer for the first, once it is there.
	if err := os.Mkdir(filepath.Join(dir, "real", "made"), 0o755); err != nil {
		t.Fatal(err)
	}
	if real, err := filepath.EvalSymlinks(dir + "/link/../made"); err != nil || real != filepath.Join(dir, "real", "made") {
		t.Fatalf("the system resolves it to %s, %v", real, err)
	}
}

// Bug: a release root that is a link to / gets past the refusal of / (remote.ReleaseSettings
// reads the text): the facts give / as the root with no refusal, and for any deploy user but
// root its action is to give / to the deploy user, which the apply's root run then does.
func TestServerSetupRefusesARootLinkedToSlash(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("root owns /: nothing to give")
	}
	s, _ := setupServer(t)
	link := filepath.Join(filepath.Dir(s.root), "slash")
	if err := os.Symlink("/", link); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(filepath.Dir(s.root), "home", "config.yaml"), "releases:\n  root: "+link+"\n", 0o644)
	out, _ := s.run(nil, remote.ServerSetupFlag, "facts")
	i := strings.Index(out, "{")
	if i < 0 {
		return // refused before any facts
	}
	var f setup.Facts
	if err := json.Unmarshal([]byte(out[i:]), &f); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if f.Root == "/" && len(f.Refusals) == 0 {
		t.Errorf("the release root is / with no refusal, its action %q:\n%s", f.RootAction, out)
	}
}
