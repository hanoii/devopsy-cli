package main

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Review round 1 (Opus), bug: --server-info's "--gc would remove" never counts a release
// snapshot whose release is gone on a server reached over SSH. ServerInfoScript runs
// --server-facts after basePrelude, which exports the environment's snapshot directory and
// base (images.SnapshotsEnv, images.BaseEnv) for the target's path, "" for a server named
// alone: $root//snapshots. leftFacts then runs gcScope.find, whose envSnapshotPaths goes
// through snapshotsOf, which takes that variable over every environment's own directory, so
// every environment's snapshots are read from $root//snapshots, and none is found. GCScript
// unsets both for --gc itself; ServerInfoScript does not.
func TestReview1ServerFactsCountGoneReleaseSnapshots(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	base := filepath.Join(root, "shop", "prod")
	write(t, filepath.Join(base, "releases", "20261101000000", "code", "x"), "", 0o644)
	write(t, filepath.Join(base, "snapshots", "20261020000001", remote.SnapshotManifest), "format\t1\nkind\trelease\nrelease\t20261001000000\n", 0o600)
	write(t, filepath.Join(base, "snapshots", "20261020000001", "data", "mnt.x.tar.zst"), "xxx", 0o600)
	bin := t.TempDir()
	write(t, filepath.Join(bin, "docker"), "#!/bin/sh\ncase \"$*\" in\n  'system df -v --format {{json .}}') echo '{}' ;;\nesac\nexit 0\n", 0o755)
	facts := func(extra ...string) *remote.GCFacts {
		t.Helper()
		cmd := exec.Command(binary, remote.ServerFactsFlag)
		cmd.Dir = root
		cmd.Env = append([]string{"HOME=" + root, "DEVOPSY_HOME=" + t.TempDir(), remote.HostRootEnv + "=" + t.TempDir(),
			"PATH=" + bin + ":" + fakeBin(t) + ":/usr/bin:/bin"}, extra...)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("--server-facts: %v", err)
		}
		var info remote.ServerInfo
		if err := json.Unmarshal(out, &info); err != nil || info.GC == nil {
			t.Fatalf("--server-facts: %v, gc %v:\n%s", err, info.GC, out)
		}
		return info.GC
	}
	if f := facts(); f.Snapshots != 1 {
		t.Fatalf("with no prelude's variables: %d release snapshots to remove, want 1", f.Snapshots)
	}
	// As ServerInfoScript's prelude exports them for @<server>: (t.Path empty).
	if f := facts(images.SnapshotsEnv+"="+root+"//snapshots", images.BaseEnv+"="+root+"/"); f.Snapshots != 1 {
		t.Errorf("under ServerInfoScript's prelude: %d release snapshots to remove, want 1 (shop/prod 20261020000001)", f.Snapshots)
	}
}

// Review round 1 (Opus), bug: --gc's listed removal prunes a builder's cache the listing said
// stays. The list shows a project user's builder that is not running as "not running, its
// cache stays until it runs again", and the operator confirms that; the builder starts (a build
// of that user's) before the answer, and gcPlan.only keeps a builder by its name alone, so the
// second session finds it running and prunes it: a removal the listing never showed.
func TestReview1GCRemovesNoCacheListedAsKept(t *testing.T) {
	t.Parallel()
	listing := gcPlan{Builders: []gcBuilder{{Name: "devopsy-blog", Project: "blog", Stopped: true}}}
	now := gcPlan{Builders: []gcBuilder{{Name: "devopsy-blog", Project: "blog", Records: 3, Size: 10e6}}}
	if got := now.only(listing); got.count() != 0 {
		t.Errorf("the listed removal prunes %+v, which the listing said keeps its cache", got.Builders)
	}
}
