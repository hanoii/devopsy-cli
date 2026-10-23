package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode"
)

// DataLabel says the service has data and how it moves, in syncs and
// --export and --import: DataMnt, DataStream or DataNone. Without it the
// service has none, like DataNone.
const DataLabel = "devopsy.data"

// DataStopLabel says when devopsy stops the service while data moves:
// StopNever (the default), StopOwn, StopImport or StopAlways (DataStops).
const DataStopLabel = "devopsy.data.stop"

// DataExcludeLabel names entries of mnt a DataMnt service mounts that its
// copies leave out anyway (comma or space separated): an access log next
// to the files, say. Ignored on stream and none, so a project can flip
// DataLabel through a variable and keep it.
const DataExcludeLabel = "devopsy.data.exclude"

// How a service's data moves, DataLabel's values.
const (
	// DataMnt: the entries of mnt it bind-mounts, rsynced between servers
	// and a tar for --export and --import.
	DataMnt = "mnt"
	// DataStream: the data capability's export writes the data to stdout,
	// and its import reads it from stdin.
	DataStream = "stream"
	// DataNone (and no label): the service has no data: neither copied
	// nor touched.
	DataNone = "none"
)

// When devopsy stops a service while data moves, DataStopLabel's values:
// not a scale, own and import overlap only on the service's own import.
const (
	// StopNever (the default): it keeps running.
	StopNever = "never"
	// StopOwn: while its own data is exported or imported (a database
	// copied as files).
	StopOwn = "own"
	// StopImport: while any data is imported (an app, a worker).
	StopImport = "import"
	// StopAlways: while any data is exported or imported.
	StopAlways = "always"
)

// oldLabels are the labels and capability before devopsy.data, refused
// where labels are read, with what replaces them (no migration).
var oldLabels = []struct{ label, now string }{
	{"devopsy.sync", DataLabel + " (rsync is " + DataMnt + ", no label or skip is " + DataNone + ")"},
	{"devopsy.sync.stop", DataStopLabel + " (true is " + StopOwn + ", or " + StopImport + " or " + StopAlways + "; false is " + StopNever + ", the default)"},
	{"devopsy.sync.rsync.exclude", DataExcludeLabel},
}

// DataCapability is the capability that moves a stream service's data
// (export, import) and hooks into syncs (prepare, finish).
const DataCapability = "data"

// The hidden flags of a sync, run on servers.
const (
	SyncPushFlag     = "--sync-push"
	SyncExecFlag     = "--sync-exec"
	SyncServicesFlag = "--sync-services"
)

// DestroyDataFlag is --destroy's part where the project is, hidden: run in
// a server's release by DestroyScript, and locally by --destroy itself.
const DestroyDataFlag = "--destroy-data"

// ReleaseStepsFlag runs a rollback's or a restart's steps on a server from
// the release's own config, hidden: run by ActivateScript.
const ReleaseStepsFlag = "--release-steps"

// --export and --import move one service's data, the way its DataLabel
// says, locally or with @<target> on a server.
const (
	ExportFlag = "--export"
	ImportFlag = "--import"
)

// SyncService is a service of the project as its data labels say: how
// its data moves (Transfer, DataNone without any), when it is stopped
// (Stop, and StopLabeled whether DataStopLabel is set), the entries of
// mnt its binds hold ("." for mnt itself; a DataMnt service's without
// its excluded ones, named in Excluded), the services it depends on and
// its restart policy. HasData is whether Transfer is not DataNone.
type SyncService struct {
	Name, Transfer string
	Stop           string
	StopLabeled    bool
	Dirs, Excluded []string
	HasData        bool
	DependsOn      []string
	Restart        string
	// Profiles are the service's compose profiles: none means always on.
	Profiles []string
}

// Active reports whether s is in a profile that is up, profiles being
// what COMPOSE_PROFILES names (comma separated, * for all): an up naming
// a service starts it whatever its profiles, so devopsy leaves out those
// a plain up would.
func (s SyncService) Active(profiles string) bool {
	if len(s.Profiles) == 0 {
		return true
	}
	for _, p := range strings.Split(profiles, ",") {
		if p = strings.TrimSpace(p); p == "*" || slices.Contains(s.Profiles, p) {
			return true
		}
	}
	return false
}

// ComposeConfig prints the project's compose configuration as JSON, every
// profile's services included: the project found from cwd, with its
// environment over environ. Replaceable in tests.
var ComposeConfig = func(cwd string, environ []string) ([]byte, error) {
	plan, err := Build(cwd, []string{"--", "--profile", "*", "config", "--format", "json"}, environ)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("docker", plan.Args[1:]...)
	cmd.Env = plan.Env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker compose config: %v %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// SyncServices lists the services of the project at cwd, sorted, with
// their data labels and the entries of .devopsy/mnt their bind mounts
// hold. projectDir is the project's .devopsy/. The labels before
// devopsy.data, and the sync capability's directory, are errors naming
// what replaces them.
func SyncServices(cwd string, environ []string) (projectDir string, services []SyncService, err error) {
	projectDir, err = FindProjectDir(cwd)
	if err != nil {
		return "", nil, err
	}
	if left, _ := os.ReadDir(filepath.Join(projectDir, "capabilities", "sync")); len(left) > 0 {
		return "", nil, fmt.Errorf(".devopsy/capabilities/sync/ is now .devopsy/capabilities/%s/: move its files (%s) there", DataCapability, left[0].Name())
	}
	out, err := ComposeConfig(cwd, environ)
	if err != nil {
		return "", nil, err
	}
	services, err = parseSyncServices(out, projectDir)
	return projectDir, services, err
}

// parseSyncServices reads compose's JSON configuration. Bind sources are
// absolute there, under the compose file's directory as written (on
// servers .devopsy/mnt is a link to shared/mnt, which compose may have
// resolved): both count. depends_on is a map keyed by service.
func parseSyncServices(out []byte, projectDir string) ([]SyncService, error) {
	var config struct {
		Services map[string]struct {
			Labels    map[string]string          `json:"labels"`
			DependsOn map[string]json.RawMessage `json:"depends_on"`
			Restart   string                     `json:"restart"`
			Profiles  []string                   `json:"profiles"`
			Volumes   []struct {
				Type   string `json:"type"`
				Source string `json:"source"`
			} `json:"volumes"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &config); err != nil {
		return nil, fmt.Errorf("docker compose config: %w", err)
	}
	mnts := []string{filepath.Join(projectDir, "mnt")}
	if r, err := filepath.EvalSymlinks(mnts[0]); err == nil && r != mnts[0] {
		mnts = append(mnts, r)
	}
	var services []SyncService
	for name, s := range config.Services {
		for _, o := range oldLabels {
			if v, ok := s.Labels[o.label]; ok {
				return nil, fmt.Errorf("service %s: %s=%s: that label is now %s", name, o.label, v, o.now)
			}
		}
		cs := SyncService{Name: name, Transfer: DataNone, Stop: StopNever, DependsOn: []string{}, Restart: s.Restart, Profiles: s.Profiles}
		data, labeled := s.Labels[DataLabel]
		if labeled {
			// An empty value is an error: a variable left unset by accident
			// must not stop copying silently.
			switch data {
			case DataMnt, DataStream, DataNone:
				cs.Transfer = data
			case "":
				return nil, fmt.Errorf("service %s: %s is empty: %s, %s, or %s for no data (a variable left unset?)", name, DataLabel, DataMnt, DataStream, DataNone)
			case "rsync", "files":
				return nil, fmt.Errorf("service %s: %s=%s: %s now (its entries of mnt)", name, DataLabel, data, DataMnt)
			case "skip":
				return nil, fmt.Errorf("service %s: %s=%s: %s now (no data)", name, DataLabel, data, DataNone)
			default:
				return nil, fmt.Errorf("service %s: %s=%s: %s, %s or %s", name, DataLabel, data, DataMnt, DataStream, DataNone)
			}
		}
		cs.HasData = cs.Transfer != DataNone
		if v, ok := s.Labels[DataStopLabel]; ok {
			switch v {
			case StopNever, StopImport, StopAlways:
			case StopOwn:
				// own stops it for its own data: with none chosen (a
				// variable), it has none to stop for; without any label
				// it would never stop, which is not what own says.
				if !labeled {
					return nil, fmt.Errorf("service %s: %s=%s needs a %s label: own stops it while its own data moves (%s=%s or %s); %s=%s stops it while any data is imported", name, DataStopLabel, v, DataLabel, DataLabel, DataMnt, DataStream, DataStopLabel, StopImport)
				}
				if !cs.HasData {
					v = StopNever
				}
			case "true", "false":
				return nil, fmt.Errorf("service %s: %s=%s: %s, %s, %s or %s (true was %s, false %s)", name, DataStopLabel, v, StopNever, StopOwn, StopImport, StopAlways, StopOwn, StopNever)
			default:
				return nil, fmt.Errorf("service %s: %s=%s: %s, %s, %s or %s", name, DataStopLabel, v, StopNever, StopOwn, StopImport, StopAlways)
			}
			cs.Stop, cs.StopLabeled = v, true
		}
		var exclude []string
		if cs.Transfer == DataMnt {
			exclude = strings.FieldsFunc(s.Labels[DataExcludeLabel], func(r rune) bool { return r == ',' || r == ' ' })
			for _, e := range exclude {
				if e == "." || e == ".." || strings.ContainsAny(e, "/\\") {
					return nil, fmt.Errorf("service %s: %s=%s: entries of mnt only, like files or logs", name, DataExcludeLabel, s.Labels[DataExcludeLabel])
				}
			}
		}
		for _, v := range s.Volumes {
			if v.Type != "bind" {
				continue
			}
			d := mntEntry(filepath.Clean(v.Source), mnts)
			switch {
			case d == "" || slices.Contains(cs.Dirs, d) || slices.Contains(cs.Excluded, d):
			case slices.Contains(exclude, d):
				cs.Excluded = append(cs.Excluded, d)
			default:
				cs.Dirs = append(cs.Dirs, d)
			}
		}
		sort.Strings(cs.Dirs)
		sort.Strings(cs.Excluded)
		// Copies name entries in space-separated lists (the child's,
		// exclude) and rsync modules: a name with whitespace would split.
		if cs.Transfer == DataMnt {
			for _, d := range append(slices.Clone(cs.Dirs), cs.Excluded...) {
				if strings.ContainsFunc(d, unicode.IsSpace) {
					return nil, fmt.Errorf("service %s: ./mnt/%s: an entry of mnt a copy carries cannot have whitespace in its name: rename it", name, d)
				}
			}
		}
		for dep := range s.DependsOn {
			cs.DependsOn = append(cs.DependsOn, dep)
		}
		sort.Strings(cs.DependsOn)
		services = append(services, cs)
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	return services, nil
}

// mntEntry is the top-level entry of mnt that source is or lies in, "."
// for mnt itself, "" outside it.
func mntEntry(source string, mnts []string) string {
	for _, m := range mnts {
		rel, err := filepath.Rel(m, source)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		if rel == "." {
			return "."
		}
		return strings.SplitN(rel, string(filepath.Separator), 2)[0]
	}
	return ""
}

// SyncDirs are the entries of mnt a sync rsyncs: those of the services
// labeled mnt, sorted; "." (all of mnt) subsumes the rest.
func SyncDirs(services []SyncService) []string {
	var dirs []string
	for _, s := range services {
		if s.Transfer != DataMnt {
			continue
		}
		for _, d := range s.Dirs {
			if d == "." {
				return []string{"."}
			}
			if !slices.Contains(dirs, d) {
				dirs = append(dirs, d)
			}
		}
	}
	sort.Strings(dirs)
	return dirs
}

// transferWord is how a message names a service's data label: "no data"
// for none.
func transferWord(s SyncService) string {
	if s.Transfer == DataNone {
		return "no data"
	}
	return s.Transfer
}

// SyncWholeNotice is a notice for a copy of mnt itself (an entry "."):
// it includes the entries of the services without data or labeled
// stream too, which their labels say not to copy, or to copy another
// way. Empty when nothing is copied against a label.
func SyncWholeNotice(services []SyncService) string {
	dirs := SyncDirs(services)
	if len(dirs) != 1 || dirs[0] != "." {
		return ""
	}
	var whole, included, exclude []string
	for _, s := range services {
		switch {
		case s.Transfer == DataMnt:
			if slices.Contains(s.Dirs, ".") {
				whole = append(whole, s.Name)
			}
		case len(s.Dirs) > 0:
			included = append(included, s.Name+" ("+transferWord(s)+": "+strings.Join(s.Dirs, ", ")+")")
			for _, d := range s.Dirs {
				if d != "." && !slices.Contains(exclude, d) {
					exclude = append(exclude, d)
				}
			}
		}
	}
	if len(included) == 0 {
		return ""
	}
	msg := fmt.Sprintf("%s copies mnt whole, %s included", strings.Join(whole, ", "), strings.Join(included, ", "))
	if len(exclude) > 0 {
		// An exclude on the service mounting ./mnt never matches: its one
		// entry is "." itself.
		msg += fmt.Sprintf(": mount mnt's entries (./mnt/%s) on %s instead of ./mnt, so each copies by its own label", exclude[0], strings.Join(whole, ", "))
	}
	return msg
}

// SyncFind is the service named name.
func SyncFind(services []SyncService, name string) (SyncService, bool) {
	for _, s := range services {
		if s.Name == name {
			return s, true
		}
	}
	return SyncService{}, false
}

// DataMntServices names the services labeled mnt.
func DataMntServices(services []SyncService) []string {
	var names []string
	for _, s := range services {
		if s.Transfer == DataMnt {
			names = append(names, s.Name)
		}
	}
	return names
}

// DataOp is what devopsy does with data, for DataStops: every operation
// is an export (a read) or an import (a replacement), of one service or
// of all of them at once.
type DataOp int

const (
	// ExportOne is --export <service>.
	ExportOne DataOp = iota
	// ImportOne is --import <service>.
	ImportOne
	// SyncParent is a sync's parent side: every data service exported.
	SyncParent
	// SyncChild is a sync's child side: every data service imported.
	SyncChild
)

// Stopped is a service DataStops stops, and why: its stop word, or
// "depends on <service>".
type Stopped struct{ Name, Why string }

// DataStops is the set devopsy stops for op (service is ExportOne's and
// ImportOne's), from labels alone: callers stop only what runs. Each
// service's own row (StopOwn while its own data moves, StopImport while
// any is imported, StopAlways for every copy), the union for an
// operation over every data service (one stop and one start), then every
// service whose depends_on reaches one of them, transitively: it cannot
// work without it anyway. In depends_on order, dependencies first.
func DataStops(services []SyncService, op DataOp, service string) []Stopped {
	direct := func(s SyncService) bool {
		switch s.Stop {
		case StopAlways:
			return true
		case StopImport:
			return op == ImportOne || op == SyncChild
		case StopOwn:
			return (op == ExportOne || op == ImportOne) && s.Name == service || (op == SyncParent || op == SyncChild) && s.HasData
		}
		return false
	}
	why := map[string]string{}
	for _, s := range services {
		if direct(s) {
			why[s.Name] = s.Stop
		}
	}
	return closure(services, why)
}

// WithDependents is names (why: how) and every
// service whose depends_on reaches one of them, transitively, in
// depends_on order: what stops with them.
func WithDependents(services []SyncService, names []string, how string) []Stopped {
	why := map[string]string{}
	for _, n := range names {
		why[n] = how
	}
	return closure(services, why)
}

// closure adds to why ("" for none) every service whose depends_on
// reaches one there, transitively ("depends on <service>"), and lists
// them in depends_on order.
func closure(services []SyncService, why map[string]string) []Stopped {
	// To a fixpoint: depends_on order makes it one pass, and a cycle
	// (compose refuses one) still ends.
	for changed := true; changed; {
		changed = false
		for _, name := range dependsOrder(services) {
			s, _ := SyncFind(services, name)
			if why[name] != "" {
				continue
			}
			for _, d := range s.DependsOn {
				if why[d] != "" {
					why[name] = "depends on " + d
					changed = true
					break
				}
			}
		}
	}
	var set []Stopped
	for _, name := range dependsOrder(services) {
		if why[name] != "" {
			set = append(set, Stopped{name, why[name]})
		}
	}
	return set
}

// StoppedNames are the services of a set.
func StoppedNames(set []Stopped) []string {
	var names []string
	for _, s := range set {
		names = append(names, s.Name)
	}
	return names
}

// StoppingLine says what devopsy stops and why, for the services of set
// in names (those running): "stopping database (own), app (depends on
// database)".
func StoppingLine(set []Stopped, names []string) string {
	var parts []string
	for _, s := range set {
		if slices.Contains(names, s.Name) {
			parts = append(parts, s.Name+" ("+s.Why+")")
		}
	}
	return "stopping " + strings.Join(parts, ", ")
}

// Dependent is a service left running that depends on one being
// imported (On), transitively.
type Dependent struct{ Name, On string }

// UnlabeledDependents are the services whose depends_on reaches one of
// imported, transitively, that are not in set and carry no DataStopLabel:
// left running while it is replaced, which authors miss (a worker or a
// cron writing to a streamed database). An explicit never silences it.
func UnlabeledDependents(services []SyncService, imported []string, set []Stopped) []Dependent {
	on := map[string]string{}
	for _, name := range imported {
		on[name] = name
	}
	for changed := true; changed; {
		changed = false
		for _, s := range services {
			if on[s.Name] != "" {
				continue
			}
			for _, d := range s.DependsOn {
				if on[d] != "" {
					on[s.Name] = on[d]
					changed = true
					break
				}
			}
		}
	}
	stopped := StoppedNames(set)
	var out []Dependent
	for _, s := range services {
		if on[s.Name] == "" || slices.Contains(imported, s.Name) || slices.Contains(stopped, s.Name) || s.StopLabeled {
			continue
		}
		out = append(out, Dependent{s.Name, on[s.Name]})
	}
	return out
}

// DependentNotice is the notice for d, left running.
func DependentNotice(d Dependent) string {
	return fmt.Sprintf("%s depends on %s and keeps running while it is imported: label it %s=%s, or %s to keep it running", d.Name, d.On, DataStopLabel, StopImport, StopNever)
}

// dependsOrder is every service in compose's depends_on order,
// dependencies first, ties alphabetical (services is sorted); with a
// cycle, alphabetical.
func dependsOrder(services []SyncService) []string {
	order, err := dependsOn(services)
	if err != nil {
		order = nil
		for _, s := range services {
			order = append(order, s.Name)
		}
	}
	return order
}

// dependsOn orders services by depends_on, dependencies first, ties
// alphabetical: a cycle (compose refuses one already) is an error.
func dependsOn(services []SyncService) ([]string, error) {
	known := map[string]bool{}
	for _, s := range services {
		known[s.Name] = true
	}
	deps := map[string][]string{}
	for _, s := range services {
		for _, d := range s.DependsOn {
			if known[d] && d != s.Name {
				deps[s.Name] = append(deps[s.Name], d)
			}
		}
	}
	done := map[string]bool{}
	var order []string
	for len(order) < len(services) {
		// services is sorted: the first ready one is the alphabetical tie.
		progressed := false
		for _, s := range services {
			if done[s.Name] || slices.ContainsFunc(deps[s.Name], func(d string) bool { return !done[d] }) {
				continue
			}
			done[s.Name] = true
			order = append(order, s.Name)
			progressed = true
			break
		}
		if !progressed {
			var left []string
			for _, s := range services {
				if !done[s.Name] {
					left = append(left, s.Name)
				}
			}
			return nil, fmt.Errorf("depends_on has a cycle among %s", strings.Join(left, ", "))
		}
	}
	return order, nil
}

// SyncStreamServices names the services labeled stream in compose's
// depends_on order, dependencies first, ties alphabetical: a database
// imported before what reads it. The order is over every service, so a
// dependency through one without data counts too; a cycle (compose
// refuses one already) is an error.
func SyncStreamServices(services []SyncService) ([]string, error) {
	order, err := dependsOn(services)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range order {
		if s, _ := SyncFind(services, n); s.Transfer == DataStream {
			names = append(names, n)
		}
	}
	return names, nil
}

// DataLists are the values of DEVOPSY_DATA_MNT, DEVOPSY_DATA_STREAM and
// DEVOPSY_DATA_STOP for the data capability: this side's services by
// label, space-separated (stream in depends_on order when it has one),
// and stop, the services this operation stops on this side. A stopped
// stream service is in both stream and stop, so a script knows to run
// --rm rather than exec.
func DataLists(services []SyncService, stop []string) (mnt, stream, stops string) {
	streams, err := SyncStreamServices(services)
	if err != nil {
		for _, s := range services {
			if s.Transfer == DataStream {
				streams = append(streams, s.Name)
			}
		}
	}
	return strings.Join(DataMntServices(services), " "), strings.Join(streams, " "), strings.Join(stop, " ")
}
