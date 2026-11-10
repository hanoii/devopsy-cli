package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/images"
)

// Review round 1 (Opus). Bug: the door's pull claims a service under pull_policy missing (or
// if_not_present) whose image is here, the project's: compose's pull skips it ("Skipped Image
// is already present locally", compose 5.5), asking no registry, so a release's fetch keeps
// the image it has. The door reads the manifest and, the tag moved in its registry, copies
// and runs the new one, and a tag gone from its registry fails the release.
func TestReviewOpusPullKeepsAPresentImageUnderMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	doorImageFake(t, dir)
	write(t, filepath.Join(dir, "sudo"), "#!/bin/sh\nshift 5\n[ \"$1\" = real ] || exit 1\nprintf '%s\\n' \"$2\"\n", 0o755)
	private := "registry.example.com/blog/private:1"
	d := fakeManifest(t, dir, plainManifest("private"), private)
	// Here, by its tag and its digest, and the project's.
	repo := images.RepoOf(private)
	write(t, filepath.Join(dir, "store"), private+"|"+d+"|"+repo+"@"+d+"|"+private+"|{}\n"+repo+"@"+d+"|"+d+"|"+repo+"@"+d+"|"+private+"|{}\n", 0o644)
	side := door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}
	write(t, side.ImagesFile(), d+"\n", 0o600)
	// Its registry moved the tag since.
	fakeManifest(t, dir, plainManifest("private2"), private)
	file := `{"name":"shop-prod","services":{"web":{"image":"` + private + `","pull_policy":"missing","user":"10001:10001","security_opt":["no-new-privileges"]}}}`
	c := exec.Command(os.Args[0], "-test.run", "^$")
	c.Env = append(os.Environ(), "DEVOPSY_TEST_DOOR_COMPOSE="+dir, "DEVOPSY_TEST_DOOR_VERB=pull", "DEVOPSY_TEST_DOOR_FILE="+file,
		"DEVOPSY_TEST_DOOR_LOG="+filepath.Join(dir, "refused.log"), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := c.CombinedOutput()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	if calls := readString(filepath.Join(dir, "calls")); strings.Contains(calls, "imagetools") {
		t.Errorf("a pull under pull_policy missing of an image here asked its registry, as compose's does not (exit %d):\n%s\n%s", c.ProcessState.ExitCode(), out, calls)
	}
}

// Review round 1 (Opus). Bug: a claim checks buildx's version (hostReady, doorHostReady) with
// the door's environment, no DOCKER_CONFIG, so Docker finds buildx in the deploy user's
// ~/.docker/cli-plugins first, but runs the copy with DOCKER_CONFIG the logins' directory,
// where Docker finds the system's buildx: the check reads another buildx than the one that
// copies (composeKnowsServiceContexts reads compose under the build's own environment for
// this reason), and an older one there fails the copy with "invalid reference format", the
// words the floor is there to replace.
func TestReviewOpusHostCheckReadsTheBuildxThatCopies(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := doorImageFake(t, dir)
	envCalls := filepath.Join(dir, "envcalls")
	wrap := filepath.Join(dir, "docker-wrap")
	write(t, wrap, "#!/bin/sh\necho \"${DOCKER_CONFIG-unset}|$*\" >>"+envCalls+"\nexec "+fake+" \"$@\"\n", 0o755)
	private := "registry.example.com/blog/private"
	fakeManifest(t, dir, plainManifest("private"), private+":1")
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}, real: wrap}
	if _, _, err := side.claim(private+":1", filepath.Join(dir, "none.json")); err != nil {
		t.Fatal(err)
	}
	config := map[string]string{}
	for _, line := range strings.Split(readString(envCalls), "\n") {
		cfg, args, _ := strings.Cut(line, "|")
		switch {
		case args == "buildx version":
			config["version"] = cfg
		case strings.HasPrefix(args, "buildx imagetools create "):
			config["copy"] = cfg
		}
	}
	if config["version"] != config["copy"] {
		t.Errorf("buildx's version read with DOCKER_CONFIG %q, its copy run with %q:\n%s", config["version"], config["copy"], readString(envCalls))
	}
}
