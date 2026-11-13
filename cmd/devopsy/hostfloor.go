package main

import (
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// toolVersion is a tool's version as it prints it: its first word of three numbers, a leading v
// aside (compose's `2.40.0`, buildx's `github.com/docker/buildx v0.38.2 <commit>`).
var toolVersion = regexp.MustCompile(`(?:^|\s)v?(\d+)\.(\d+)\.(\d+)`)

// versionAtLeast reads out's version and says whether it is at least floor: a version that
// cannot be read is not, so a floor refuses rather than guessing. It gives the version as
// read, for the refusal's words.
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

// versionOf is out's version as versionAtLeast reads it.
func versionOf(out string) string {
	v, _ := versionAtLeast(out, [3]int{})
	return v
}

// floorWords is floor as a version, for a refusal's words.
func floorWords(floor [3]int) string {
	return fmt.Sprintf("%d.%d.%d", floor[0], floor[1], floor[2])
}

// floorPart is one thing of a Docker the floor reads.
type floorPart int

const (
	floorStore floorPart = iota
	floorBuildx
	floorCompose
)

// floorParts are every part of the floor, in the table's order.
var floorParts = []floorPart{floorStore, floorBuildx, floorCompose}

// floor is one row of dockerFloors.
type floor struct {
	part floorPart
	// name is the plugin's, in a refusal's words; version its oldest. The image store has
	// neither.
	name    string
	version [3]int
	// fix is the way out of a Docker that misses it, by the docker topic's steps.
	fix string
}

// containerdStore is how docker info's DriverStatus names Docker's containerd image store.
const containerdStore = "io.containerd.snapshotter.v1"

// dockerFloors is the Docker every server runs, its one table, and no engine number: the
// store and the two plugins are what devopsy's features read, and a Docker installed from its
// repository has all three; one upgraded from an older version keeps the classic store.
//   - the containerd image store: a load of an OCI layout (the door's claims) and registry
//     digests for every image; the classic store refuses a layout and, given one, records no
//     digest;
//   - buildx 0.33.0: its imagetools writes an OCI layout (`--tag oci-layout://<dir>:<tag>`),
//     what a claim copies a registry's image into; an older one answers "invalid reference
//     format", which says nothing;
//   - compose 2.33.0: it reads a build context service:<name> as another service's build; one
//     before reads it as a path in the project's directory (the door's checkContexts).
var dockerFloors = []floor{
	{part: floorStore, fix: "set up Docker with the containerd image store, on a new server if this one already runs projects"},
	{part: floorBuildx, name: "buildx", version: [3]int{0, 33, 0}, fix: "upgrade Docker's buildx plugin"},
	{part: floorCompose, name: "compose", version: [3]int{2, 33, 0}, fix: "upgrade Docker's compose plugin"},
}

// floorOf is part's row of dockerFloors.
func floorOf(part floorPart) floor {
	for _, f := range dockerFloors {
		if f.part == part {
			return f
		}
	}
	panic("no floor for part " + fmt.Sprint(part))
}

// floorRead is what a Docker answered of its floor: whether docker info's driver status names
// the containerd image store (storeErr why docker info said nothing), and what buildx version
// and compose version --short printed ("" for nothing).
type floorRead struct {
	containerd bool
	storeErr   string
	buildx     string
	compose    string
}

// floorMiss is one floor a Docker misses, in words.
type floorMiss struct {
	floor floor
	msg   string
}

func (m floorMiss) Error() string { return m.msg }

// floorMissed is the one judgment of the floor: each of parts' rows r misses, in the table's
// order, a version that cannot be read missing its floor.
func floorMissed(r floorRead, parts ...floorPart) []floorMiss {
	var misses []floorMiss
	for _, f := range dockerFloors {
		if !slices.Contains(parts, f.part) {
			continue
		}
		var msg string
		switch f.part {
		case floorStore:
			switch {
			case r.storeErr != "":
				msg = "Docker's image store could not be read: " + r.storeErr
			case !r.containerd:
				msg = "Docker here uses the classic image store, and devopsy needs the containerd image store"
			}
		default:
			out := r.buildx
			if f.part == floorCompose {
				out = r.compose
			}
			needs := ", and devopsy needs " + floorWords(f.version) + " or later"
			switch v, ok := versionAtLeast(out, f.version); {
			case ok:
			case !toolVersion.MatchString(out):
				msg = f.name + " here answers no version" + needs
			default:
				msg = f.name + " here is " + v + needs
			}
		}
		if msg != "" {
			misses = append(misses, floorMiss{floor: f, msg: msg})
		}
	}
	return misses
}

// readFloor asks docker, in env, for parts of its floor, each by its own call, all at once:
// docker info's driver status, buildx version, compose version --short.
func readFloor(docker string, env []string, parts ...floorPart) floorRead {
	var r floorRead
	run := func(args ...string) (string, error) {
		c := exec.Command(docker, args...)
		c.Env = env
		out, err := c.Output()
		return string(out), err
	}
	var wg sync.WaitGroup
	for _, p := range parts {
		wg.Go(func() {
			switch p {
			case floorStore:
				out, err := run("info", "--format", "{{.DriverStatus}}")
				if err != nil {
					r.storeErr = cmdError(err)
					return
				}
				r.containerd = strings.Contains(out, containerdStore)
			case floorBuildx:
				r.buildx, _ = run("buildx", "version")
			case floorCompose:
				r.compose, _ = run("compose", "version", "--short")
			}
		})
	}
	wg.Wait()
	return r
}

// dockerFloor is readFloor then floorMissed, for a reader with an environment of its own (the
// door's DOCKER_CONFIG, a build's).
func dockerFloor(docker string, env []string, parts ...floorPart) []floorMiss {
	return floorMissed(readFloor(docker, env, parts...), parts...)
}

// floorError is misses as one error, nil for none.
func floorError(misses []floorMiss) error {
	if len(misses) == 0 {
		return nil
	}
	words := make([]string, len(misses))
	for i, m := range misses {
		words[i] = m.msg
	}
	return errors.New(strings.Join(words, "; "))
}
