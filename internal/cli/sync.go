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

// SyncLabel tunes how a sync treats a service (SyncLive and so on).
const SyncLabel = "devopsy.sync"

// SyncExcludeLabel names entries of mnt a service mounts that a sync
// leaves out anyway (comma or space separated): an access log next to
// the files, say. The service is still paused or stopped as its
// SyncLabel says; only those entries are not copied through it.
const SyncExcludeLabel = "devopsy.sync.exclude"

// Sync policies, SyncLabel's values. A service opts in: without the
// label it is SyncSkip.
const (
	// SyncSkip (the default): the service is neither copied nor touched.
	SyncSkip = "skip"
	// SyncLive: nothing happens to the service; its binds under mnt are
	// copied (after the flush hook).
	SyncLive = "live"
	// SyncCustom: devopsy copies nothing and touches nothing; the sync
	// capability's push, required then, copies it after the rest.
	SyncCustom = "custom"
	// SyncStop: stopped for the copy, a clean shutdown (a database's raw
	// files).
	SyncStop = "stop"
	// SyncPause: frozen for the copy, so its router stays and requests
	// wait (the routed app). Holding, not integrity: never a database.
	SyncPause = "pause"
)

// The hidden flags of a sync, run on servers.
const (
	SyncPushFlag     = "--sync-push"
	SyncRsyncFlag    = "--sync-rsync"
	SyncExecFlag     = "--sync-exec"
	SyncServicesFlag = "--sync-services"
)

// SyncService is a service of the project as a sync sees it: its policy
// and the entries of mnt its binds hold ("." for mnt itself). Labeled is
// whether it has SyncLabel.
type SyncService struct {
	Name, Policy string
	Dirs         []string
	Labeled      bool
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
// their policy (SyncLabel, default SyncSkip) and the entries of
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
// resolved): both count.
func parseSyncServices(out []byte, projectDir string) ([]SyncService, error) {
	var config struct {
		Services map[string]struct {
			Labels  map[string]string `json:"labels"`
			Volumes []struct {
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
		policy, labeled := s.Labels[SyncLabel]
		switch policy {
		case "":
			policy = SyncSkip
		case SyncLive, SyncSkip, SyncStop, SyncPause, SyncCustom:
		default:
			return nil, fmt.Errorf("service %s: %s=%s: %s, %s, %s, %s or %s", name, SyncLabel, policy, SyncSkip, SyncLive, SyncCustom, SyncStop, SyncPause)
		}
		cs := SyncService{Name: name, Policy: policy, Labeled: labeled}
		exclude := strings.FieldsFunc(s.Labels[SyncExcludeLabel], func(r rune) bool { return r == ',' || r == ' ' })
		for _, e := range exclude {
			if e == "." || e == ".." || strings.ContainsAny(e, "/\\") {
				return nil, fmt.Errorf("service %s: %s=%s: entries of mnt only, like files or logs", name, SyncExcludeLabel, s.Labels[SyncExcludeLabel])
			}
		}
		for _, v := range s.Volumes {
			if v.Type != "bind" {
				continue
			}
			if d := mntEntry(filepath.Clean(v.Source), mnts); d != "" && !slices.Contains(cs.Dirs, d) && !slices.Contains(exclude, d) {
				cs.Dirs = append(cs.Dirs, d)
			}
		}
		sort.Strings(cs.Dirs)
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

// SyncDirs are the entries of mnt a sync copies: those of the services
// labeled live, pause or stop, sorted; "." (all of mnt) subsumes the rest.
func SyncDirs(services []SyncService) []string {
	var dirs []string
	for _, s := range services {
		if s.Policy == SyncSkip || s.Policy == SyncCustom {
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
// it includes the entries of the services labeled skip or custom too,
// which their labels say not to copy, or to copy another way. Empty
// when nothing is copied against a label.
func SyncWholeNotice(services []SyncService) string {
	dirs := SyncDirs(services)
	if len(dirs) != 1 || dirs[0] != "." {
		return ""
	}
	var whole, included, exclude []string
	for _, s := range services {
		switch {
		case s.Policy != SyncSkip && s.Policy != SyncCustom:
			if slices.Contains(s.Dirs, ".") {
				whole = append(whole, s.Name)
			}
		case len(s.Dirs) > 0:
			included = append(included, s.Name+" ("+s.Policy+": "+strings.Join(s.Dirs, ", ")+")")
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
		msg += fmt.Sprintf(": %s=%s on %s leaves them out", SyncExcludeLabel, strings.Join(exclude, ","), strings.Join(whole, ", "))
	}
	return msg
}

// SyncPolicy lists the names of services with policy.
func SyncPolicy(services []SyncService, policy string) []string {
	var names []string
	for _, s := range services {
		if s.Policy == policy {
			names = append(names, s.Name)
		}
	}
	return names
}
