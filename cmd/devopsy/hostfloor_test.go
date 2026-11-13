package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/howto"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/setup"
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
		floor := floorOf(floorCompose).version
		if strings.Contains(out, "buildx") {
			floor = floorOf(floorBuildx).version
		}
		if _, got := versionAtLeast(out, floor); got != want {
			t.Errorf("%q: %v, want %v", out, got, want)
		}
	}
}

// Recorded from real daemons: Docker 29's containerd store, the classic store's overlay2, and
// the plugins' versions as they print them.
const (
	containerdStatus = "[[driver-type io.containerd.snapshotter.v1]]"
	classicStatus    = "[[Backing Filesystem extfs] [Supports d_type true] [Using metacopy false] [Native Overlay Diff true] [userxattr false]]"
	buildxCurrent    = "github.com/docker/buildx v0.38.2 e25e2b7a4b6a8c3f"
)

// Every server's Docker runs the floor, one table read by one helper (dockerFloor): each floor
// missed, a version that cannot be read and an image store that cannot be read refuse, each
// in its own words, only the parts asked read and judged.
func TestDockerFloor(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		info, buildx, compose string
		parts                 []floorPart
		want                  []string
	}{
		"ready":         {containerdStatus, buildxCurrent, "2.40.0", floorParts, nil},
		"classic store": {classicStatus, buildxCurrent, "2.40.0", floorParts, []string{"Docker runs its classic image store, and devopsy supports only a current Docker installed fresh, on its containerd image store (io.containerd.snapshotter.v1)"}},
		"store unread":  {"", buildxCurrent, "2.40.0", floorParts, []string{"Docker's image store could not be read: Cannot connect to the Docker daemon"}},
		"old buildx":    {containerdStatus, "github.com/docker/buildx v0.29.1 a8e5ac0f2e6c", "2.40.0", floorParts, []string{"Docker's buildx is 0.29.1, and devopsy needs 0.33.0 or later"}},
		"no buildx":     {containerdStatus, "", "2.40.0", floorParts, []string{"Docker's buildx answers no version, and devopsy needs 0.33.0 or later"}},
		"buildx unread": {containerdStatus, "buildx, a build", "2.40.0", floorParts, []string{`Docker's buildx answers no version ("buildx, a build"), and devopsy needs 0.33.0 or later`}},
		"old compose":   {containerdStatus, buildxCurrent, "v2.20.2", floorParts, []string{"Docker's compose is 2.20.2, and devopsy needs 2.33.0 or later"}},
		"all three":     {classicStatus, "github.com/docker/buildx v0.29.1 a8e5ac0f2e6c", "2.32.4", floorParts, []string{"classic image store", "buildx is 0.29.1", "compose is 2.32.4"}},
		// The door's build reads compose alone: an old buildx there is not its to judge.
		"compose alone": {classicStatus, "", "2.40.0", []floorPart{floorCompose}, nil},
	} {
		dir := t.TempDir()
		docker := filepath.Join(dir, "docker")
		write(t, docker, `#!/bin/sh
echo "$*" >>`+filepath.Join(dir, "calls")+`
case "$1" in
  info) [ -n '`+c.info+`' ] || { echo 'Cannot connect to the Docker daemon' >&2; exit 1; }; echo '`+c.info+`' ;;
  buildx) [ -n '`+c.buildx+`' ] || { echo "docker: 'buildx' is not a docker command." >&2; exit 1; }; echo '`+c.buildx+`' ;;
  compose) echo '`+c.compose+`' ;;
esac
`, 0o755)
		misses := dockerFloor(docker, nil, c.parts...)
		if len(misses) != len(c.want) {
			t.Errorf("%s: %v, want %q", name, misses, c.want)
			continue
		}
		for i, want := range c.want {
			if !strings.Contains(misses[i].Error(), want) {
				t.Errorf("%s: %q, want %q", name, misses[i], want)
			}
		}
		if calls := readString(filepath.Join(dir, "calls")); len(c.parts) == 1 && strings.Count(calls, "\n") != 1 {
			t.Errorf("%s: read more than the part asked:\n%s", name, calls)
		}
	}
}

// The setup's Docker item refuses each floor missed with the docker topic's fix, and a plugin
// missing once, as missing, never again as below its floor; the floor stops no step of the
// setup, as the plugins do not: releases need them, and no step does.
func TestDockerAnswersRefuseTheFloor(t *testing.T) {
	t.Parallel()
	ready := remote.DockerFacts{Compose: "2.40.0", Buildx: "0.38.2", Containerd: true}
	if rs := dockerAnswers(ready); len(rs) != 0 {
		t.Errorf("a Docker at its floor refused: %+v", rs)
	}
	for name, c := range map[string]struct {
		change func(*remote.DockerFacts)
		want   []string
		fix    string
	}{
		"classic store": {func(d *remote.DockerFacts) { d.Containerd = false }, []string{"Docker runs its classic image store"}, "install a current Docker fresh, on a new server where this one runs projects, as root"},
		"old buildx":    {func(d *remote.DockerFacts) { d.Buildx = "0.29.1" }, []string{"Docker's buildx is 0.29.1, and devopsy needs 0.33.0 or later"}, "upgrade Docker's buildx plugin alone, as root"},
		"old compose":   {func(d *remote.DockerFacts) { d.Compose = "2.32.4" }, []string{"Docker's compose is 2.32.4, and devopsy needs 2.33.0 or later"}, "upgrade Docker's compose plugin alone, as root"},
		"no buildx":     {func(d *remote.DockerFacts) { d.Buildx = "" }, []string{"Docker has no buildx plugin"}, dockerFix},
	} {
		d := ready
		c.change(&d)
		rs := dockerAnswers(d)
		if len(rs) != len(c.want) {
			t.Errorf("%s: %+v, want %q", name, rs, c.want)
			continue
		}
		for i, r := range rs {
			if !strings.Contains(r.Message, c.want[i]) || r.Item != setup.RefusalDocker || r.Howto != howto.Docker || r.Fix != c.fix {
				t.Errorf("%s: %+v, want %q fixed by %q, the docker topic", name, r, c.want[i], c.fix)
			}
		}
		f := setup.Facts{Refusals: rs}
		for _, step := range setup.Steps {
			if !f.Goes(step) {
				t.Errorf("%s: stops %s", name, step)
			}
		}
	}
}
