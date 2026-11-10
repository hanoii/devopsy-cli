package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// BUG: --project-users add closes "the deploy user's projects" in the release root, and takes
// every directory there for one, where the rest of devopsy (serverProjects, envDirs) takes
// none whose name starts with a dot. With the default root, the deploy user's home, add
// closes and names .cache, .config, .docker and the like as projects it closed.
func TestReview4bCloseDeployProjectsClosesProjectsOnly(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for _, name := range []string{"shop", ".cache"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	closed, err := closeDeployProjects(root, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(closed, "shop") {
		t.Fatalf("the control: shop not closed: %q", closed)
	}
	if projects := serverProjects(nil, root); slices.Contains(projects, ".cache") {
		t.Fatalf("the control: .cache is a project to serverProjects: %q", projects)
	}
	if slices.Contains(closed, ".cache") {
		t.Errorf("closed as the deploy user's projects: %q", closed)
	}
}
