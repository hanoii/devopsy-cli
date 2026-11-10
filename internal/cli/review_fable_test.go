package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// BUG: the deploy user never reads a project user's tree (internals/door.md): what it needs
// comes from a read as that user. ImportStates (--debug imports, over every compose project
// running on the host) reads devopsy-target.env and .env where a container's
// com.docker.compose.project.working_dir label points, as whoever runs it. On a container the
// door made (devopsy.door), that label is the project user's word and the files are in its
// tree, through whatever links it put there: the deploy user's read follows them to any file
// it can read itself, and shows its lines as that project's values.
//
// Not parallel: it sets Running (fakeHost).
func TestReviewFableImportStatesReadNoDoorProjectsTree(t *testing.T) {
	// The project user's tree, and a file of the deploy user's it links to.
	tree := filepath.Join(t.TempDir(), "shop", "prod", "releases", "1")
	dir := filepath.Join(tree, remote.CodeDir, ProjectDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(t.TempDir(), "devopsy-target.env")
	write(t, own, "TOKEN='deploys'\n")
	if err := os.Symlink(own, releaseTargetEnv(filepath.Join(tree, remote.CodeDir))); err != nil {
		t.Fatal(err)
	}
	fakeHost(t,
		container("traefik", "devopsy.role=proxy", "devopsy.export.HOST=smtp"),
		container("shop-prod", "devopsy.door=devopsy-shop", "com.docker.compose.project.working_dir="+dir, "devopsy.import.TOKEN=proxy/HOST"),
	)
	host, _ := Running()
	states := host.ImportStates()
	if len(states) != 1 {
		t.Fatalf("%+v", states)
	}
	if s := states[0]; s.Released || s.Set || s.Release != "" {
		t.Errorf("BUG: a door project's tree is read by whoever lists imports, through its links: %+v", s)
	}
}
