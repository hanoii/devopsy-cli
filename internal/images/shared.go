package images

import (
	"fmt"
	"slices"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// ImageSpec is one service of a compose file as Shared reads it: its image: ("" for none)
// and whether it has a build: section.
type ImageSpec struct {
	Image  string
	Builds bool
}

// BuildName is the name compose gives service's build in the project composeName: its
// image:, else <compose name>-<service>.
func BuildName(composeName, service, image string) string {
	if image != "" {
		return image
	}
	return composeName + "-" + service
}

// Shared is the sharers of a build among services, the services of one compose file: each
// service with no build: whose image: is the name a service that builds is built under
// (BuildName), compared as Docker shows them (Familiar: shop-app is shop-app:latest), and
// the services that build it, sorted. A service that builds is never a sharer, whatever its
// image: names. More than one builder is the caller's to refuse (SharedTwice).
func Shared(composeName string, services map[string]ImageSpec) map[string][]string {
	builders := map[string][]string{}
	for name, s := range services {
		if s.Builds {
			ref := familiar(BuildName(composeName, name, s.Image))
			builders[ref] = append(builders[ref], name)
		}
	}
	shared := map[string][]string{}
	for name, s := range services {
		if s.Builds || s.Image == "" {
			continue
		}
		if b := builders[familiar(s.Image)]; len(b) > 0 {
			shared[name] = slices.Sorted(slices.Values(b))
		}
	}
	return shared
}

// SharedTwice refuses sharer, whose image several services build: each is an image of its
// own behind the door, so the name says none of them.
func SharedTwice(sharer, image string, builders []string) error {
	others := "the others"
	if len(builders) == 2 {
		others = "the other"
	}
	return &report.Failure{
		Msg:     fmt.Sprintf("%s runs %s, which %s build", sharer, image, both(builders)),
		Details: []string{fmt.Sprintf("for a project user each build is an image of its own, so devopsy cannot tell which one %s should run", sharer)},
		Remedies: []report.Remedy{report.Fix(fmt.Sprintf(
			"give %s different image: names, or keep build: on one of them and let %s use its image", report.JoinAnd(builders), others))},
	}
}

// SharedUnbuilt refuses sharer, whose builder this release does not build (it is only
// under a profile the release does not take): devopsy's name for a build is the
// environment's, not the release's, so an older build under it would run.
func SharedUnbuilt(sharer, builder string) error {
	return &report.Failure{
		Msg:      fmt.Sprintf("%s runs %s's image, but this release does not build %s", sharer, builder, builder),
		Details:  []string{fmt.Sprintf("%s is only under a profile this release does not use", builder)},
		Remedies: []report.Remedy{report.Fix(fmt.Sprintf("take %s out of its profile, put %s under the same profile, or give %s an image: of its own", builder, sharer, sharer))},
	}
}

// SharedTwiceLine is SharedTwice in the door's one line, for a compose command refused by
// hand (internal/door's render).
func SharedTwiceLine(sharer, image string, builders []string) string {
	return fmt.Sprintf("%s: its image %s is built by %s; give each build its own image: name", sharer, image, both(builders))
}

// both is builders joined for "which X build": "both a and b", or "a, b and c".
func both(names []string) string {
	if len(names) == 2 {
		return "both " + report.JoinAnd(names)
	}
	return report.JoinAnd(names)
}
