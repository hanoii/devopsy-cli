package cli

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// CheckNamesFlag is the hidden flag the release script runs in a new
// release before it goes live, with the environment's directory:
// ProjectConflict, then NetworkConflicts.
const CheckNamesFlag = "--check-names"

// ProjectDirs returns the working directories of the containers of
// compose project name, running or not (their
// com.docker.compose.project.working_dir labels). Replaceable in tests.
var ProjectDirs = func(name string) []string {
	out, err := exec.Command("docker", "ps", "-a", "--filter", "label=com.docker.compose.project="+name, "--format", `{{.Label "com.docker.compose.project.working_dir"}}`).Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}

// ProjectConflict checks that the compose project of the release at cwd
// has no containers from outside dir, its environment's directory. Names
// are joined with -, so two environments can end up with one
// (<project>-<instance>-<environment>: instance a-b of c and instance a of
// b-c), and Compose would take the other's containers, volumes and
// network as its own. A compose configuration that cannot be read is no
// conflict: whatever is wrong with it, the release's up says so.
func ProjectConflict(cwd, dir string, environ []string) error {
	out, err := ComposeConfig(cwd, environ)
	if err != nil {
		return nil
	}
	var cfg struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(out, &cfg) != nil || cfg.Name == "" {
		return nil
	}
	for _, d := range ProjectDirs(cfg.Name) {
		if d != "" && !within(d, dir) {
			return fmt.Errorf("compose project %s is another environment's (its containers are in %s): names come from project, instance and environment joined with -, so rename one of them", cfg.Name, d)
		}
	}
	return nil
}

// within reports whether path is dir or under it, compared as written and
// with symbolic links resolved (a release root reached through a link).
func within(path, dir string) bool {
	under := func(p, d string) bool {
		rel, err := filepath.Rel(d, p)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
	}
	if under(filepath.Clean(path), filepath.Clean(dir)) {
		return true
	}
	p, err1 := filepath.EvalSymlinks(path)
	d, err2 := filepath.EvalSymlinks(dir)
	return err1 == nil && err2 == nil && under(p, d)
}

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
