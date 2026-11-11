package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// A tool's version is read from what it prints, compose's and buildx's alike, and one that
// cannot be read is never taken for one at the floor.
func TestVersionAtLeast(t *testing.T) {
	t.Parallel()
	for out, want := range map[string]bool{
		"2.40.0\n":  true,
		"v2.33.0\n": true,
		"2.32.4\n":  false,
		"5.7.0\n":   true,
		"github.com/docker/buildx v0.38.2 e25e2b7a4b6a8c3f\n": true,
		"github.com/docker/buildx v0.29.1 a8e5ac0f2e6c\n":     false,
		"":                false,
		"not a version\n": false,
	} {
		floor := serviceContexts
		if strings.Contains(out, "buildx") {
			floor = buildxOCILayout
		}
		if _, got := versionAtLeast(out, floor); got != want {
			t.Errorf("%q: %v, want %v", out, got, want)
		}
	}
}

// A server with project users needs Docker's containerd image store and buildx 0.33.0 or
// later: each missing, or a version that cannot be read, refuses with words. The outputs are
// recorded from real daemons: Docker 29's containerd store, the classic store's overlay2.
func TestDoorHostReady(t *testing.T) {
	t.Parallel()
	const containerd = "[[driver-type io.containerd.snapshotter.v1]]"
	const classic = "[[Backing Filesystem extfs] [Supports d_type true] [Using metacopy false] [Native Overlay Diff true] [userxattr false]]"
	for name, c := range map[string]struct {
		info, buildx, want string
	}{
		"ready":         {containerd, "github.com/docker/buildx v0.38.2 e25e2b7a4b6a8c3f", ""},
		"classic store": {classic, "github.com/docker/buildx v0.38.2 e25e2b7a4b6a8c3f", "classic image store"},
		"old buildx":    {containerd, "github.com/docker/buildx v0.29.1 a8e5ac0f2e6c", "buildx 0.33.0 or later"},
		"no buildx":     {containerd, "", "buildx 0.33.0 or later"},
	} {
		dir := t.TempDir()
		docker := filepath.Join(dir, "docker")
		write(t, docker, "#!/bin/sh\ncase \"$1\" in\n  info) echo '"+c.info+"' ;;\n  buildx) [ -n '"+c.buildx+"' ] && echo '"+c.buildx+"' || exit 1 ;;\nesac\n", 0o755)
		err := doorHostReady(docker, nil, "")
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}
