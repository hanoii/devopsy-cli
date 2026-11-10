package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
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

// BUG: --server-info lists a project user's environments only when its tree is in the deploy
// user's release root (doorTrees keeps those, environments walks that root alone). By default
// a project user's tree is in its own home, <home>/<project>, so on a server set up the
// default way the admin's --server-info lists none of its project users' environments, nor
// what a cut-off session left marked there.
func TestReview4bServerInfoListsAProjectUsersTreeAnywhere(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	root, tree := filepath.Join(tmp, "home", "devopsy"), filepath.Join(tmp, "home", "devopsy-shop", "shop")
	for _, d := range []string{filepath.Join(root, "app", "prod", "releases"), filepath.Join(tree, "prod", "releases")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, e := range environments(root, map[string]string{tree: "devopsy-shop"}, func(user, tree string) []remote.Environment {
		return []remote.Environment{{Path: "shop/prod", Status: "as " + user + "\n"}}
	}) {
		got = append(got, e.Path)
	}
	if !slices.Contains(got, "app/prod") {
		t.Fatalf("the control: the deploy user's own environment: %v", got)
	}
	if !slices.Contains(got, "shop/prod") {
		t.Errorf("a project user's tree in its own home is not listed: %v", got)
	}
}
