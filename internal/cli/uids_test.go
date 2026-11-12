package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/uids"
)

// In a release, the two uids are the server's claims, over every layer and the caller's
// value: a uid set by hand could be another tenant's. The claims are made under the release
// root the release sits in.
func TestUIDsInRelease(t *testing.T) {
	t.Parallel()
	root := project(t, "shop", map[string]string{
		"target.env":   "DEVOPSY_INSTANCE='eu'\n",
		"compose.yaml": minimalCompose,
		".env":         "DEVOPSY_UID=10001\n",
	})
	p, err := loadProject(root, []string{"DEVOPSY_PROJECT_UID=10001"})
	if err != nil {
		t.Fatal(err)
	}
	releaseRoot := strings.TrimSuffix(filepath.Dir(filepath.Dir(filepath.Dir(root))), "/shop/eu/prod")
	store := uids.Store{Dir: filepath.Join(releaseRoot, UIDsDir)}
	env := uids.Owner{Project: "shop", Instance: "eu", Env: "prod"}
	for k, o := range map[string]uids.Owner{UIDEnv: env, ProjectUIDEnv: env.ProjectOf()} {
		claimed, ok, err := store.Read(o)
		if err != nil || !ok {
			t.Fatalf("%s: no claim of %s under %s: %v", k, o, store.Dir, err)
		}
		got, _ := p.env.Lookup(k)
		if got != strconv.FormatUint(uint64(claimed), 10) {
			t.Errorf("%s = %q, want the claim's %d", k, got, claimed)
		}
		if defs := p.env.trace(k); len(defs) < 2 || defs[0].Kind != EnvDevopsy {
			t.Errorf("%s: the claim does not win over the layer that set it: %+v", k, defs)
		}
	}
	if !p.env.marked[UIDEnv] || !p.env.marked[ProjectUIDEnv] {
		t.Error("the uids are not shown by --env")
	}
	// The same environment loaded again keeps its uids.
	again, err := loadProject(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{UIDEnv, ProjectUIDEnv} {
		a, _ := p.env.Lookup(k)
		b, _ := again.env.Lookup(k)
		if a != b {
			t.Errorf("%s moved: %s, then %s", k, a, b)
		}
	}
}

// A release whose place is not the one its target lines name is refused: its owner is read
// from where it is, never from a variable a .env could set.
func TestUIDsReleaseOutOfPlace(t *testing.T) {
	t.Parallel()
	root := project(t, "shop", map[string]string{"target.env": "", "compose.yaml": minimalCompose})
	write(t, releaseTargetEnv(root), "DEVOPSY_PROJECT='blog'\nDEVOPSY_ENVIRONMENT='prod'\n")
	if _, err := loadProject(root, nil); err == nil || !strings.Contains(err.Error(), "not at <root>/blog/prod") {
		t.Fatalf("a release of shop's place naming blog: %v", err)
	}
}

// Locally the uids are the plain hashes, the caller's or the .env's own value first; the
// project's comes from config.yaml's project: alone, so a worktree's local project keeps the
// uid every server gives, while its environment uid follows its own compose name.
func TestUIDsLocal(t *testing.T) {
	t.Parallel()
	root := project(t, "shop", map[string]string{
		"compose.yaml":      minimalCompose,
		"config.yaml":       "project: shop\n",
		"config.local.yaml": "project: shop-wt\n",
	})
	p, err := loadProject(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := p.env.Lookup(ProjectUIDEnv); got != strconv.FormatUint(uint64(uids.HashIn(uids.ProjectDomain, "project:shop")), 10) {
		t.Errorf("project uid %s, want the committed project's hash", got)
	}
	if got, _ := p.env.Lookup(UIDEnv); got != strconv.FormatUint(uint64(uids.HashIn(uids.EnvDomain, "env:shop-wt")), 10) {
		t.Errorf("uid %s, want the local compose name's hash", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), UIDsDir)); err == nil {
		t.Error("a claim made locally")
	}
	mine, err := loadProject(root, []string{"DEVOPSY_UID=1000"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := mine.env.Lookup(UIDEnv); got != "1000" {
		t.Errorf("the caller's uid overridden locally: %s", got)
	}
}
