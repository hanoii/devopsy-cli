package cli

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// SyncLabel says how a sync, --export and --import move a service's data
// (SyncRsync, SyncStream). Without it the service is SyncSkip.
const SyncLabel = "devopsy.sync"

// SyncStopLabel, true or false (the default), says whether the service is
// stopped meanwhile: on both sides for a whole sync, around --export and
// --import.
const SyncStopLabel = "devopsy.sync.stop"

// SyncExcludeLabel names entries of mnt an rsync service mounts that its
// copy leaves out anyway (comma or space separated): an access log next
// to the files, say. Ignored on a stream service, so a project can flip
// SyncLabel through a variable and keep it.
const SyncExcludeLabel = "devopsy.sync.rsync.exclude"

// How a service's data moves, SyncLabel's values. A service opts in:
// without the label it is SyncSkip.
const (
	// SyncSkip (no label): the service is neither copied nor touched.
	SyncSkip = "skip"
	// SyncRsync: devopsy rsyncs the entries of mnt it bind-mounts (a tar of
	// them for --export and --import).
	SyncRsync = "rsync"
	// SyncStream: the sync capability's export, on the parent, writes the
	// data to stdout, and its import, on the child, reads it from stdin.
	SyncStream = "stream"
)

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

// --export and --import move one service's data, the way its SyncLabel
// says, locally or with @<target> on a server.
const (
	ExportFlag = "--export"
	ImportFlag = "--import"
)

// SyncService is a service of the project as a sync sees it: how its data
// moves (Transfer), whether it is stopped meanwhile, the entries of mnt
// its binds hold ("." for mnt itself; an rsync service's without its
// excluded ones, named in Excluded) and the services it depends on.
// Labeled is whether it has a non-empty SyncLabel.
type SyncService struct {
	Name, Transfer string
	Stop           bool
	Dirs, Excluded []string
	Labeled        bool
	DependsOn      []string
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
// their transfer (SyncLabel, default SyncSkip) and the entries of
// .devopsy/mnt their bind mounts hold. projectDir is the project's
// .devopsy/.
func SyncServices(cwd string, environ []string) (projectDir string, services []SyncService, err error) {
	projectDir, err = FindProjectDir(cwd)
	if err != nil {
		return "", nil, err
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
		cs := SyncService{Name: name, Transfer: s.Labels[SyncLabel]}
		// An empty value is no label: a variable can turn a service off.
		switch cs.Transfer {
		case "":
			cs.Transfer = SyncSkip
		case SyncRsync, SyncStream:
			cs.Labeled = true
		default:
			return nil, fmt.Errorf("service %s: %s=%s: %s or %s (no label: neither copied nor touched)", name, SyncLabel, cs.Transfer, SyncRsync, SyncStream)
		}
		switch v := s.Labels[SyncStopLabel]; v {
		case "", "false":
		case "true":
			// Only a labeled service is touched.
			cs.Stop = cs.Labeled
		default:
			return nil, fmt.Errorf("service %s: %s=%s: true or false", name, SyncStopLabel, v)
		}
		var exclude []string
		if cs.Transfer == SyncRsync {
			exclude = strings.FieldsFunc(s.Labels[SyncExcludeLabel], func(r rune) bool { return r == ',' || r == ' ' })
			for _, e := range exclude {
				if e == "." || e == ".." || strings.ContainsAny(e, "/\\") {
					return nil, fmt.Errorf("service %s: %s=%s: entries of mnt only, like files or logs", name, SyncExcludeLabel, s.Labels[SyncExcludeLabel])
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
// labeled rsync, sorted; "." (all of mnt) subsumes the rest.
func SyncDirs(services []SyncService) []string {
	var dirs []string
	for _, s := range services {
		if s.Transfer != SyncRsync {
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

// SyncWholeNotice is a notice for a copy of mnt itself (an entry "."):
// it includes the entries of the services without a label or labeled
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
		case s.Transfer == SyncRsync:
			if slices.Contains(s.Dirs, ".") {
				whole = append(whole, s.Name)
			}
		case len(s.Dirs) > 0:
			included = append(included, s.Name+" ("+s.Transfer+": "+strings.Join(s.Dirs, ", ")+")")
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
	msg := fmt.Sprintf("devopsy: %s copies mnt whole, %s included", strings.Join(whole, ", "), strings.Join(included, ", "))
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

// SyncRsyncServices names the services labeled rsync.
func SyncRsyncServices(services []SyncService) []string {
	var names []string
	for _, s := range services {
		if s.Transfer == SyncRsync {
			names = append(names, s.Name)
		}
	}
	return names
}

// SyncStopped names the services labeled devopsy.sync.stop=true.
func SyncStopped(services []SyncService) []string {
	var names []string
	for _, s := range services {
		if s.Stop {
			names = append(names, s.Name)
		}
	}
	return names
}

// SyncStreamServices names the services labeled stream in compose's
// depends_on order, dependencies first, ties alphabetical: a database
// imported before what reads it. The order is over every service, so a
// dependency through an unlabeled one counts too; a cycle (compose
// refuses one already) is an error.
func SyncStreamServices(services []SyncService) ([]string, error) {
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
	var names []string
	for _, n := range order {
		if s, _ := SyncFind(services, n); s.Transfer == SyncStream {
			names = append(names, n)
		}
	}
	return names, nil
}

// SyncLists are the values of DEVOPSY_SYNC_RSYNC, DEVOPSY_SYNC_STREAM and
// DEVOPSY_SYNC_STOP for the sync capability: this side's services by
// label, space-separated (stream in depends_on order when it has one). A
// stopped stream service is in both stream and stop, so a script knows
// to run --rm rather than exec.
func SyncLists(services []SyncService) (rsync, stream, stop string) {
	streams, err := SyncStreamServices(services)
	if err != nil {
		for _, s := range services {
			if s.Transfer == SyncStream {
				streams = append(streams, s.Name)
			}
		}
	}
	return strings.Join(SyncRsyncServices(services), " "), strings.Join(streams, " "), strings.Join(SyncStopped(services), " ")
}
