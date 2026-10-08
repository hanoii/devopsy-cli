package cli

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/compose-spec/compose-go/v2/dotenv"
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
	ID      string
	Project string
	Labels  map[string]string
}

// Name is the container's compose project, or says it has none.
func (c Container) Name() string {
	if c.Project != "" {
		return c.Project
	}
	id := c.ID
	if len(id) > 12 {
		id = id[:12]
	}
	return "a container not started by compose (" + id + ")"
}

// Host is a snapshot of the running containers, so one lookup sees one
// state of the host.
type Host []Container

// Running lists the host's running containers, through the local docker;
// replaceable in tests.
var Running = func() (Host, error) {
	out, err := exec.Command("docker", "ps", "-q", "--no-trunc").Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil, nil
	}
	out, err = exec.Command("docker", append([]string{"inspect", "--format", "{{.Id}} {{json .Config.Labels}}"}, ids...)...).Output()
	if err != nil {
		return nil, fmt.Errorf("docker inspect: %w", err)
	}
	return parseInspect(out)
}

// parseInspect reads lines of "<id> <labels as JSON>" (null without labels).
func parseInspect(out []byte) (Host, error) {
	var host Host
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		id, data, _ := strings.Cut(line, " ")
		var labels map[string]string
		if err := json.Unmarshal([]byte(data), &labels); err != nil {
			return nil, fmt.Errorf("docker inspect: %w", err)
		}
		if labels == nil {
			labels = map[string]string{}
		}
		host = append(host, Container{ID: id, Project: labels["com.docker.compose.project"], Labels: labels})
	}
	return host, nil
}

// Holders are the containers holding role.
func (h Host) Holders(role string) Host {
	var out Host
	for _, c := range h {
		if c.Labels[RoleLabel] == role {
			out = append(out, c)
		}
	}
	return out
}

// Project are the containers of a compose project.
func (h Host) Project(name string) Host {
	var out Host
	for _, c := range h {
		if c.Project == name && name != "" {
			out = append(out, c)
		}
	}
	return out
}

// Projects are the compose projects running, sorted.
func (h Host) Projects() []string {
	seen := map[string]bool{}
	for _, c := range h {
		if c.Project != "" {
			seen[c.Project] = true
		}
	}
	return sortedKeys(seen)
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

// Resolve finds what imp reads on host: the compose project holding the
// role named by its source, else the compose project of that name; then
// the export on its running containers. Containers of one project exporting
// different values are an error.
func (h Host) Resolve(imp Import) (Exporter, error) {
	var e Exporter
	holders := h.Holders(imp.Source)
	projects := map[string]bool{}
	for _, c := range holders {
		projects[c.Name()] = true
	}
	var list Host
	switch len(projects) {
	case 0:
		list = h.Project(imp.Source)
	case 1:
		if holders[0].Project == "" {
			list = holders
		} else {
			list = h.Project(holders[0].Project)
		}
	default:
		return e, fmt.Errorf("role %s is held by several: %s", imp.Source, strings.Join(sortedKeys(projects), ", "))
	}
	if len(list) == 0 {
		return e, nil
	}
	e.Project = list[0].Name()
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

// Why says why e has no value for imp.
func (e Exporter) Why(imp Import) string {
	if e.Project == "" {
		return fmt.Sprintf("no running container has %s=%s, and no compose project %s is running", RoleLabel, imp.Source, imp.Source)
	}
	return e.Project + " does not export " + imp.Key
}

// checkValue refuses what cannot be a variable's value on one line.
func checkValue(v string) error {
	for _, r := range v {
		if unicode.IsControl(r) {
			return fmt.Errorf("the exported value has control characters or newlines: %q", v)
		}
	}
	return nil
}

// WaitForExporters is how long a release waits for an import's source to
// run, as while the proxy restarts.
var WaitForExporters = 30 * time.Second

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
	host, err := Running()
	if err != nil {
		return nil, err
	}

	if role != "" {
		for _, c := range host.Holders(role) {
			if c.Project != self {
				return nil, fmt.Errorf("role %s is held by %s on this host: one compose project per role (%s=%s)", role, c.Name(), RoleLabel, role)
			}
		}
	}

	// Required sources not running yet are waited for a while: a proxy
	// restarting should not fail a release.
	deadline := time.Now().Add(WaitForExporters)
	for waited := false; ; {
		missing := ""
		for _, imp := range imports {
			if _, ok := p.env.Lookup(imp.Var); ok || imp.Optional {
				continue
			}
			if e, err := host.Resolve(imp); err == nil && e.Project == "" {
				missing = imp.Source
			}
		}
		if missing == "" || !time.Now().Before(deadline) {
			break
		}
		if !waited {
			said = append(said, fmt.Sprintf("devopsy: waiting up to %s for %s to run...", WaitForExporters, missing))
			waited = true
		}
		time.Sleep(2 * time.Second)
		if host, err = Running(); err != nil {
			return nil, err
		}
	}

	var lines []string
	for _, imp := range imports {
		if _, ok := p.env.Lookup(imp.Var); ok {
			said = append(said, fmt.Sprintf("devopsy: %s: set by the target, not imported from %s", imp.Var, imp.Source))
			continue
		}
		e, err := host.Resolve(imp)
		if err != nil {
			return said, fmt.Errorf("%s: %w", imp, err)
		}
		if !e.Found {
			if !imp.Optional {
				return said, fmt.Errorf("%s: %s. Start it, or set %s for this target (empty for none)", imp, e.Why(imp), imp.Var)
			}
			said = append(said, fmt.Sprintf("devopsy: %s: %s, not imported (optional)", imp.Var, e.Why(imp)))
			continue
		}
		if err := checkValue(e.Value); err != nil {
			return said, fmt.Errorf("%s: %w", imp, err)
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
		_, err = f.WriteString(ImportedMarker + "\n" + strings.Join(lines, "\n") + "\n")
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return nil, err
		}
	}
	return said, nil
}

// ImportedMarker starts the imported lines in a release's target.env.
const ImportedMarker = "# Imported at release (devopsy.import labels)."

// ImportState is one import of a running compose project and what its
// current release has, for `devopsy --debug imports`.
type ImportState struct {
	Project string
	Import  Import
	// Release is the value in the project's release: imported (Imported),
	// or set by the target itself (Set). Neither: not in a release, or not
	// imported.
	Release       string
	Imported, Set bool
	Released      bool
	Now           Exporter
	Err           error
}

// Stale reports whether the release imported a value its source no longer
// exports.
func (s ImportState) Stale() bool {
	return s.Imported && s.Err == nil && s.Now.Found && s.Now.Value != s.Release
}

// ImportStates lists the imports of every compose project running on the
// host, from its containers' labels, and the values in its current release
// (the target.env and .env next to its compose file).
func (h Host) ImportStates() []ImportState {
	var states []ImportState
	for _, project := range h.Projects() {
		imports := map[string]Import{}
		dir := ""
		for _, c := range h.Project(project) {
			if d := c.Labels["com.docker.compose.project.working_dir"]; d != "" {
				dir = d
			}
			for k, v := range c.Labels {
				if !strings.HasPrefix(k, ImportPrefix) {
					continue
				}
				if imp, err := parseImport(strings.TrimPrefix(k, ImportPrefix), v); err == nil {
					imports[imp.Var] = imp
				}
			}
		}
		if len(imports) == 0 {
			continue
		}
		set, imported, released := ReleaseEnv(dir)
		vars := make([]string, 0, len(imports))
		for v := range imports {
			vars = append(vars, v)
		}
		sort.Strings(vars)
		for _, v := range vars {
			s := ImportState{Project: project, Import: imports[v], Released: released}
			if val, ok := imported[v]; ok {
				s.Release, s.Imported = val, true
			} else if val, ok := set[v]; ok {
				s.Release, s.Set = val, true
			}
			s.Now, s.Err = h.Resolve(imports[v])
			states = append(states, s)
		}
	}
	return states
}

// ReleaseEnv reads a release's .devopsy/ (dir): what its target sets
// (target.env above the imported lines, and .env), what it imported, and
// whether it is a release at all.
func ReleaseEnv(dir string) (set, imported map[string]string, released bool) {
	set, imported = map[string]string{}, map[string]string{}
	data, err := os.ReadFile(filepath.Join(dir, "target.env"))
	if dir == "" || err != nil {
		return set, imported, false
	}
	own, after, _ := strings.Cut(string(data), ImportedMarker)
	if m, err := dotenv.UnmarshalWithLookup(own, nil); err == nil {
		maps.Copy(set, m)
	}
	if m, err := dotenv.UnmarshalWithLookup(after, nil); err == nil {
		maps.Copy(imported, m)
	}
	if env, err := os.ReadFile(filepath.Join(dir, ".env")); err == nil {
		if m, err := dotenv.UnmarshalWithLookup(string(env), nil); err == nil {
			maps.Copy(set, m)
		}
	}
	return set, imported, true
}
