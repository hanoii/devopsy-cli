package cli

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// CheckNetworksFlag is the hidden flag the release script runs in a new
// release before it goes live: NetworkConflicts.
const CheckNetworksFlag = "--check-networks"

// NetworkOwner returns the compose project a Docker network of that name
// belongs to (its com.docker.compose.project label), and whether the
// network exists. Replaceable in tests.
var NetworkOwner = func(name string) (owner string, exists bool) {
	out, err := exec.Command("docker", "network", "inspect", "-f", `{{index .Labels "com.docker.compose.project"}}`, name).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// NetworkConflicts checks that no network the project at cwd declares as
// its own (not external) exists already as another compose project's: two
// projects naming the same network would share it, their containers
// reaching each other by service name, which Compose allows without a
// word. A project that needs another's network joins it as external. A
// compose configuration that cannot be read is no conflict: whatever is
// wrong with it, the release's up says so.
func NetworkConflicts(cwd string, environ []string) error {
	out, err := ComposeConfig(cwd, environ)
	if err != nil {
		return nil
	}
	var cfg struct {
		Name     string `json:"name"`
		Networks map[string]struct {
			Name     string `json:"name"`
			External bool   `json:"external"`
		} `json:"networks"`
	}
	if err := json.Unmarshal(out, &cfg); err != nil {
		return nil
	}
	var names []string
	for _, n := range cfg.Networks {
		if !n.External && n.Name != "" {
			names = append(names, n.Name)
		}
	}
	sort.Strings(names)
	var conflicts []string
	for _, name := range names {
		owner, exists := NetworkOwner(name)
		if exists && owner != "" && owner != cfg.Name {
			conflicts = append(conflicts, fmt.Sprintf("network %s is %s's", name, owner))
		}
	}
	if len(conflicts) == 0 {
		return nil
	}
	return fmt.Errorf("%s: a project owns the networks it names (name them from COMPOSE_PROJECT_NAME); to reach another project, join its network as external", strings.Join(conflicts, ", "))
}
