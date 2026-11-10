package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
)

// toolVersion is a tool's version as it prints it: its first word of three numbers, a leading v
// aside (compose's `2.40.0`, buildx's `github.com/docker/buildx v0.38.2 <commit>`).
var toolVersion = regexp.MustCompile(`(?:^|\s)v?(\d+)\.(\d+)\.(\d+)`)

// versionAtLeast reads out's version and says whether it is at least floor: a version that
// cannot be read is not, so a feature that needs one refuses rather than guessing. It gives
// the version as read, for the refusal's words.
func versionAtLeast(out string, floor [3]int) (string, bool) {
	m := toolVersion.FindStringSubmatch(out)
	if m == nil {
		return strings.TrimSpace(out), false
	}
	var got [3]int
	for i := range got {
		fmt.Sscanf(m[i+1], "%d", &got[i])
	}
	return strings.Join(m[1:], "."), slices.Compare(got[:], floor[:]) >= 0
}

// floorWords is floor as a version, for a refusal's words.
func floorWords(floor [3]int) string {
	return fmt.Sprintf("%d.%d.%d", floor[0], floor[1], floor[2])
}

// buildxOCILayout is the first buildx whose imagetools writes an OCI layout
// (`--tag oci-layout://<dir>:<tag>`), what a claim copies a registry's image into; an older
// one answers "invalid reference format", which says nothing.
var buildxOCILayout = [3]int{0, 33, 0}

// containerdStore is how docker info's DriverStatus names Docker's containerd image store,
// the one a claim's load leaves as a pull by digest does: the classic store refuses an OCI
// layout, and given one records no registry digest.
const containerdStore = "io.containerd.snapshotter.v1"

// doorHostReady is what a server with project users needs of its Docker, beyond a current one:
// the containerd image store and buildx's OCI layout, read with docker (the real client) in
// env, nil when both are there. --project-users add refuses without them, the door's claims
// do, and --server-info warns. buildx is read under config, a DOCKER_CONFIG with no plugins of
// its own (an empty one of its own when ""), as the door's copy runs: Docker finds a plugin in
// $DOCKER_CONFIG/cli-plugins first, so the deploy user's ~/.docker could hold another buildx
// than the one that copies.
func doorHostReady(docker string, env []string, config string) error {
	if config == "" {
		dir, err := os.MkdirTemp("", "devopsy-docker-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		config = dir
	}
	env = append(slices.Clone(env), "DOCKER_CONFIG="+config)
	run := func(args ...string) (string, error) {
		c := exec.Command(docker, args...)
		c.Env = env
		out, err := c.Output()
		return string(out), err
	}
	info, err := run("info", "--format", "{{.DriverStatus}}")
	if err != nil {
		return fmt.Errorf("Docker's image store could not be read: %v", err)
	}
	if !strings.Contains(info, containerdStore) {
		return fmt.Errorf("Docker runs the classic image store, and project users need its containerd image store (%s)", containerdStore)
	}
	out, _ := run("buildx", "version")
	if v, ok := versionAtLeast(out, buildxOCILayout); !ok {
		return fmt.Errorf("project users need buildx %s or later, which copies a registry's image as it is (this server's: %q)", floorWords(buildxOCILayout), v)
	}
	return nil
}
