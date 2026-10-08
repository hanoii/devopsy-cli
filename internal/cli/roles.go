package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Labels for sharing facts between projects on a host, read like
// devopsy.shell: a role is a slot only one compose project per host holds
// (devopsy-traefik's is proxy); exports are facts on a project's running
// containers; imports copy one into a release's target.env.
const (
	RoleLabel    = "devopsy.role"
	ExportPrefix = "devopsy.export."
	ImportPrefix = "devopsy.import."
)

// PrepareReleaseFlag is the hidden flag the release script runs in a new
// release, before it becomes current.
const PrepareReleaseFlag = "--prepare-release"

var (
	// roleName is a role or a compose project name: imports split on "/".
	roleName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	varName  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Container is a running container: its compose project and labels.
type Container struct {
	Project string
	Labels  map[string]string
}

// Containers lists the running containers with a label ("key" or
// "key=value"), through the local docker; replaceable in tests.
var Containers = func(label string) ([]Container, error) {
	out, err := exec.Command("docker", "ps", "-q", "--filter", "label="+label).Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil, nil
	}
	out, err = exec.Command("docker", append([]string{"inspect", "--format", "{{json .Config.Labels}}"}, ids...)...).Output()
	if err != nil {
		return nil, fmt.Errorf("docker inspect: %w", err)
	}
	var list []Container
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var labels map[string]string
		if err := json.Unmarshal([]byte(line), &labels); err != nil {
			return nil, fmt.Errorf("docker inspect: %w", err)
		}
		list = append(list, Container{Project: labels["com.docker.compose.project"], Labels: labels})
	}
	return list, nil
}

// Import is a devopsy.import.<Var>=<Source>/<Key>[?] label.
type Import struct {
	Var, Source, Key string
	Optional         bool
}

func (i Import) String() string {
	s := ImportPrefix + i.Var + "=" + i.Source + "/" + i.Key
	if i.Optional {
		s += "?"
	}
	return s
}

// ProjectRoles reads the project's role and imports from its compose files
// (projectDir is its .devopsy/), raw: they must not depend on the
// environment.
func ProjectRoles(projectDir string) (role string, imports []Import, err error) {
	labels, err := DevopsyLabels(projectDir)
	if err != nil {
		return "", nil, err
	}
	services := make([]string, 0, len(labels))
	for s := range labels {
		services = append(services, s)
	}
	sort.Strings(services)
	byVar := map[string]Import{}
	for _, service := range services {
		keys := make([]string, 0, len(labels[service]))
		for k := range labels[service] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := labels[service][k]
			switch {
			case k == RoleLabel:
				if !roleName.MatchString(v) {
					return "", nil, fmt.Errorf("%s: %s=%s: a role is lowercase letters, digits, - and _", service, k, v)
				}
				if role != "" && role != v {
					return "", nil, fmt.Errorf("two roles, %s and %s: a project holds one", role, v)
				}
				role = v
			case strings.HasPrefix(k, ImportPrefix):
				imp, err := parseImport(strings.TrimPrefix(k, ImportPrefix), v)
				if err != nil {
					return "", nil, fmt.Errorf("%s: %s=%s: %w", service, k, v, err)
				}
				if prev, ok := byVar[imp.Var]; ok && prev != imp {
					return "", nil, fmt.Errorf("%s is imported twice: %s and %s", imp.Var, prev, imp)
				}
				byVar[imp.Var] = imp
			}
		}
	}
	for _, imp := range byVar {
		imports = append(imports, imp)
	}
	sort.Slice(imports, func(a, b int) bool { return imports[a].Var < imports[b].Var })
	return role, imports, nil
}

func parseImport(name, value string) (Import, error) {
	imp := Import{Var: name}
	value, imp.Optional = strings.CutSuffix(value, "?")
	source, key, ok := strings.Cut(value, "/")
	imp.Source, imp.Key = source, key
	switch {
	case !varName.MatchString(name):
		return imp, fmt.Errorf("%q is not a variable name", name)
	case !ok || !roleName.MatchString(source) || !varName.MatchString(key):
		return imp, fmt.Errorf("want <role or compose project>/<KEY>, optionally ending in ?")
	}
	return imp, nil
}

// Exporter is who an import reads from, and what it found.
type Exporter struct {
	// Project is the compose project exporting, "" when none runs.
	Project string
	// Value is the export, when Found.
	Value string
	Found bool
}

// Resolve finds what imp reads: the compose project holding the role named
// by its source, else the compose project of that name; then the export on
// its running containers. Containers of one project exporting different
// values are an error.
func Resolve(imp Import) (Exporter, error) {
	var e Exporter
	holders, err := Containers(RoleLabel + "=" + imp.Source)
	if err != nil {
		return e, err
	}
	projects := map[string]bool{}
	for _, c := range holders {
		projects[c.Project] = true
	}
	switch len(projects) {
	case 0:
		e.Project = imp.Source
	case 1:
		e.Project = holders[0].Project
	default:
		return e, fmt.Errorf("role %s is held by several compose projects: %s", imp.Source, strings.Join(sortedKeys(projects), ", "))
	}
	list, err := Containers("com.docker.compose.project=" + e.Project)
	if err != nil {
		return e, err
	}
	if len(list) == 0 {
		e.Project = ""
		return e, nil
	}
	for _, c := range list {
		v, ok := c.Labels[ExportPrefix+imp.Key]
		if !ok {
			continue
		}
		if e.Found && v != e.Value {
			return e, fmt.Errorf("%s exports %s twice, %q and %q", e.Project, imp.Key, e.Value, v)
		}
		e.Value, e.Found = v, true
	}
	return e, nil
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// PrepareRelease implements the hidden `devopsy --prepare-release`, run by
// the release script in a new release (with shared/ linked) before it
// becomes current: it checks the project's role is free on this host, and
// writes its imports into the release's target.env, unless the release's
// environment already sets them (even empty). It returns what to tell the
// person releasing.
func PrepareRelease(cwd string, environ []string) ([]string, error) {
	p, err := loadProject(cwd, environ)
	if err != nil {
		return nil, err
	}
	targetEnv := filepath.Join(p.dir, "target.env")
	if _, err := os.Stat(targetEnv); err != nil {
		return nil, fmt.Errorf("%s only runs in a release (no %s)", PrepareReleaseFlag, targetEnv)
	}
	role, imports, err := ProjectRoles(p.dir)
	if err != nil {
		return nil, err
	}
	self, _ := p.env.Lookup("DEVOPSY_PROJECT_NAME")
	var said []string

	if role != "" {
		holders, err := Containers(RoleLabel + "=" + role)
		if err != nil {
			return nil, err
		}
		for _, c := range holders {
			if c.Project != self {
				return nil, fmt.Errorf("role %s is held by %s on this host: one compose project per role (%s=%s)", role, c.Project, RoleLabel, role)
			}
		}
	}

	var lines []string
	for _, imp := range imports {
		if _, ok := p.env.Lookup(imp.Var); ok {
			said = append(said, fmt.Sprintf("devopsy: %s: set by the target, not imported from %s", imp.Var, imp.Source))
			continue
		}
		e, err := Resolve(imp)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", imp, err)
		}
		if !e.Found {
			why := "nothing running is " + imp.Source
			if e.Project != "" {
				why = e.Project + " does not export " + imp.Key
			}
			if !imp.Optional {
				return nil, fmt.Errorf("%s: %s. Start it, or set %s for this target (empty for none)", imp, why, imp.Var)
			}
			said = append(said, fmt.Sprintf("devopsy: %s: %s, not imported (optional)", imp.Var, why))
			continue
		}
		lines = append(lines, DotenvLine(imp.Var, e.Value))
		from := imp.Source
		if e.Project != imp.Source {
			from += " (" + e.Project + ")"
		}
		said = append(said, fmt.Sprintf("devopsy: %s=%s, from %s", imp.Var, e.Value, from))
	}
	if len(lines) > 0 {
		f, err := os.OpenFile(targetEnv, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return nil, err
		}
		_, err = f.WriteString("# Imported at release (devopsy.import labels).\n" + strings.Join(lines, "\n") + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
	}
	return said, nil
}
