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

// CloneLabel tunes how a clone treats a service (CloneLive and so on).
const CloneLabel = "devopsy.clone"

// Clone policies, CloneLabel's values. A service opts in: without the
// label it is CloneSkip.
const (
	// CloneSkip (the default): the service is neither copied nor touched.
	CloneSkip = "skip"
	// CloneLive: nothing happens to the service; its binds under mnt are
	// copied (after the flush hook).
	CloneLive = "live"
	// CloneCustom: devopsy copies nothing and touches nothing; the clone
	// capability's push, required then, copies it after the rest.
	CloneCustom = "custom"
	// CloneStop: stopped for the copy, a clean shutdown (a database's raw
	// files).
	CloneStop = "stop"
	// ClonePause: frozen for the copy, so its router stays and requests
	// wait (the routed app). Holding, not integrity: never a database.
	ClonePause = "pause"
)

// The hidden flags of a clone, run on servers.
const (
	ClonePushFlag     = "--clone-push"
	CloneRsyncFlag    = "--clone-rsync"
	CloneExecFlag     = "--clone-exec"
	CloneServicesFlag = "--clone-services"
)

// CloneService is a service of the project as a clone sees it: its policy
// and the entries of mnt its binds hold ("." for mnt itself). Labeled is
// whether it has CloneLabel.
type CloneService struct {
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

// CloneServices lists the services of the project at cwd, sorted, with
// their policy (CloneLabel, default CloneSkip) and the entries of
// .devopsy/mnt their bind mounts hold. projectDir is the project's
// .devopsy/.
func CloneServices(cwd string, environ []string) (projectDir string, services []CloneService, err error) {
	projectDir, err = FindProjectDir(cwd)
	if err != nil {
		return "", nil, err
	}
	out, err := ComposeConfig(cwd, environ)
	if err != nil {
		return "", nil, err
	}
	services, err = parseCloneServices(out, projectDir)
	return projectDir, services, err
}

// parseCloneServices reads compose's JSON configuration. Bind sources are
// absolute there, under the compose file's directory as written (on
// servers .devopsy/mnt is a link to shared/mnt, which compose may have
// resolved): both count.
func parseCloneServices(out []byte, projectDir string) ([]CloneService, error) {
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
	var services []CloneService
	for name, s := range config.Services {
		policy, labeled := s.Labels[CloneLabel]
		switch policy {
		case "":
			policy = CloneSkip
		case CloneLive, CloneSkip, CloneStop, ClonePause, CloneCustom:
		default:
			return nil, fmt.Errorf("service %s: %s=%s: %s, %s, %s, %s or %s", name, CloneLabel, policy, CloneSkip, CloneLive, CloneCustom, CloneStop, ClonePause)
		}
		cs := CloneService{Name: name, Policy: policy, Labeled: labeled}
		for _, v := range s.Volumes {
			if v.Type != "bind" {
				continue
			}
			if d := mntEntry(filepath.Clean(v.Source), mnts); d != "" && !slices.Contains(cs.Dirs, d) {
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

// CloneDirs are the entries of mnt a clone copies: those of the services
// labeled live, pause or stop, sorted; "." (all of mnt) subsumes the rest.
func CloneDirs(services []CloneService) []string {
	var dirs []string
	for _, s := range services {
		if s.Policy == CloneSkip || s.Policy == CloneCustom {
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

// ClonePolicy lists the names of services with policy.
func ClonePolicy(services []CloneService, policy string) []string {
	var names []string
	for _, s := range services {
		if s.Policy == policy {
			names = append(names, s.Name)
		}
	}
	return names
}
