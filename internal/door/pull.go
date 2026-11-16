package door

import (
	"slices"
	"strings"
)

// A Pull is what compose's pull verb asks of the door for a project user: the images to claim,
// each once, in the order of their services' names (the file is a map), and how to say it.
type Pull struct {
	Images []string
	// IfMissing are the images every service selecting them pulls only when missing
	// (pull_policy missing or if_not_present), which the door skips when the project has
	// them, as compose skips an image it has.
	IfMissing map[string]bool
	// Quiet says nothing but failures (-q).
	Quiet bool
	// IgnoreFailures says every failure and still succeeds (--ignore-pull-failures).
	IgnoreFailures bool
}

// PlanPull reads argv (a pull and its flags, CheckVerb's) against the checked file, as
// compose's pull selects: the services named, active under the profiles or not, else the
// active ones (compose's own reading of the profiles), their dependencies only with
// --include-deps. A service is skipped when it names no image, when its image is the
// project's own (OwnRef: a build's name, a hold), when the project set pull_policy never,
// and with --ignore-buildable when it builds; one that builds is otherwise claimed, as
// compose pulls it (a release in image mode relies on a missing tag failing). Under
// pull_policy missing or if_not_present an image is one of IfMissing. A service named that
// the file does not have is refused.
func PlanPull(file map[string]any, active []string, argv []string, p Policy) (Pull, error) {
	services, _ := file["services"].(map[string]any)
	var pull Pull
	var named []string
	includeDeps, ignoreBuildable := false, false
	for _, a := range argv[1:] {
		switch a {
		case "-q", "--quiet":
			pull.Quiet = true
		case "--ignore-pull-failures":
			pull.IgnoreFailures = true
		case "--include-deps":
			includeDeps = true
		case "--ignore-buildable":
			ignoreBuildable = true
		default:
			if _, ok := services[a]; !ok {
				return Pull{}, refuse("pull: no such service: %s", a)
			}
			named = append(named, a)
		}
	}
	selected := named
	if len(named) == 0 {
		selected = slices.Clone(active)
	}
	if includeDeps {
		selected = withDependencies(services, selected)
	}
	slices.Sort(selected)
	pulled := map[string]bool{}
	for _, name := range slices.Compact(selected) {
		s, _ := services[name].(map[string]any)
		image, _ := s["image"].(string)
		policy, _ := s["pull_policy"].(string)
		if image == "" || p.OwnRef(image) || policy == "never" || ignoreBuildable && s["build"] != nil {
			continue
		}
		if !slices.Contains(pull.Images, image) {
			pull.Images = append(pull.Images, image)
		}
		if policy != "missing" && policy != "if_not_present" {
			pulled[image] = true
		}
	}
	for _, image := range pull.Images {
		if !pulled[image] {
			if pull.IfMissing == nil {
				pull.IfMissing = map[string]bool{}
			}
			pull.IfMissing[image] = true
		}
	}
	return pull, nil
}

// withDependencies is selected and every service of services they depend on, through
// depends_on, as far as it goes: what compose starts or pulls with them.
func withDependencies(services map[string]any, selected []string) []string {
	selected = slices.Clone(selected)
	for i := 0; i < len(selected); i++ {
		s, _ := services[selected[i]].(map[string]any)
		for _, dep := range dependencies(s["depends_on"]) {
			if _, ok := services[dep]; ok && !slices.Contains(selected, dep) {
				selected = append(selected, dep)
			}
		}
	}
	return selected
}

// MayStart is the services of file a start verb (argv, StartsContainers) can start, given
// those compose takes as active under its profiles: the active ones, every one argv names
// (any word of it that is a service's name, so a value or a command that happens to be one
// only widens it), and what they depend on. Compose starts nothing outside it, so an image a
// start judges is one of these.
func MayStart(file map[string]any, active, argv []string) map[string]bool {
	services, _ := file["services"].(map[string]any)
	selected := slices.Clone(active)
	for _, a := range argv[min(1, len(argv)):] {
		if _, ok := services[a]; ok {
			selected = append(selected, a)
		}
	}
	may := map[string]bool{}
	for _, s := range withDependencies(services, selected) {
		may[s] = true
	}
	return may
}

// Profiled says whether any service of file is under a profile: without one, every service
// is active, whatever the profiles a command names.
func Profiled(file map[string]any) bool {
	services, _ := file["services"].(map[string]any)
	for _, svc := range services {
		m, _ := svc.(map[string]any)
		if p, _ := m["profiles"].([]any); len(p) > 0 {
			return true
		}
	}
	return false
}

// dependencies are the services a depends_on names, as a list or as compose renders it, a map.
func dependencies(v any) []string {
	var deps []string
	switch d := v.(type) {
	case []any:
		for _, x := range d {
			if s, ok := x.(string); ok {
				deps = append(deps, s)
			}
		}
	case map[string]any:
		deps = sortedKeys(d)
	}
	return slices.DeleteFunc(deps, func(s string) bool { return strings.TrimSpace(s) == "" })
}
