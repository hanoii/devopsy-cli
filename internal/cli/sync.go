package cli

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// DataLabel says whether the service has data: true, or false (and no
// label) for none. How a copy carries it is DataCopyLabel's.
const DataLabel = "devopsy.data"

// DataCopyLabel says how a copy to another environment (a sync, --export
// and --import) carries a service's data: CopyVolumes (the default),
// CopyCustom or CopyNone. Ignored on a service without data, so a
// project can flip DataLabel through a variable and keep it.
const DataCopyLabel = "devopsy.data.copy"

// DataStopLabel says when devopsy stops the service while data moves:
// StopNever (the default), StopOwn, StopImport or StopAlways (DataStops).
const DataStopLabel = "devopsy.data.stop"

// DataExcludeLabel names entries of mnt a service with data mounts that
// its copies leave out anyway (comma or space separated): an access log
// next to the files, say. Ignored on a service without data, so a project
// can flip DataLabel through a variable and keep it.
const DataExcludeLabel = "devopsy.data.exclude"

// MntOwnerLabel is the owner of the entries of mnt a service mounts
// writable, <uid>:<gid> in numbers (names cannot be resolved from the
// host): devopsy creates them and gives them to it wherever it creates
// or writes entries (a release, a sync's rsync, an mnt --import, and
// locally before compose starts anything). Explicit, never a default
// from user: (changing that must not silently change the data's owner).
const MntOwnerLabel = "devopsy.mnt.owner"

// maxOwnerID is the largest uid or gid MntOwnerLabel takes: a uid_t
// holds 32 bits, and its largest value, 4294967295, is chown's -1 ("leave
// as is"), which coreutils refuses.
const maxOwnerID = 1<<32 - 2

// parseOwner reads MntOwnerLabel's value, <uid>:<gid> in decimal numbers
// up to maxOwnerID, and returns it normalized (010001 is 10001), so
// owners compare as numbers and every script and list says them alike.
func parseOwner(v string) (string, bool) {
	uid, gid, ok := strings.Cut(v, ":")
	var ids [2]uint64
	for i, n := range []string{uid, gid} {
		if !ok || n == "" || strings.Trim(n, "0123456789") != "" {
			return "", false
		}
		id, err := strconv.ParseUint(n, 10, 64)
		if err != nil || id > maxOwnerID {
			return "", false
		}
		ids[i] = id
	}
	return fmt.Sprintf("%d:%d", ids[0], ids[1]), true
}

// How a copy carries a service's data, DataCopyLabel's values.
const (
	// CopyVolumes (the default): the entries of mnt it bind-mounts, as
	// files, rsynced between servers and a tar for --export and --import.
	CopyVolumes = "volumes"
	// CopyCustom: the data capability's export writes the data to stdout,
	// and its import reads it from stdin, devopsy carrying the bytes.
	CopyCustom = "custom"
	// CopyNone: never copied (a sync skips it and the child keeps its
	// own, --export and --import refuse it); also what a service without
	// data has.
	CopyNone = "none"
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
	{"devopsy.sync", DataLabel + "=true (rsync is the default " + DataCopyLabel + "=" + CopyVolumes + "), or false for skip"},
	{"devopsy.sync.stop", DataStopLabel + " (true is " + StopOwn + ", or " + StopImport + " or " + StopAlways + "; false is " + StopNever + ", the default)"},
	{"devopsy.sync.rsync.exclude", DataExcludeLabel},
}

// DataCapability is the capability that moves a custom service's data
// (export, import) and hooks into syncs (prepare, finish).
const DataCapability = "data"

// The hidden flags of a sync, run on servers.
const (
	SyncPushFlag     = "--sync-push"
	SyncExecFlag     = "--sync-exec"
	SyncServicesFlag = "--sync-services"
)

// The hidden flags of a release's mounts, run by its activation:
// CheckMountsFlag in the new release before its switch (CheckMounts),
// MntOwnersFlag in current right after it (MntOwnerLabel).
const (
	CheckMountsFlag = "--check-mounts"
	MntOwnersFlag   = "--mnt-owners"
)

// DestroyDataFlag is --destroy's part where the project is, hidden: run in
// a server's release by DestroyScript, and locally by --destroy itself.
const DestroyDataFlag = "--destroy-data"

// ReleaseStepsFlag runs a rollback's or a restart's steps on a server from
// the release's own config, hidden: run by ActivateScript.
const ReleaseStepsFlag = "--release-steps"

// --export and --import move one service's data, the way its
// DataCopyLabel says, locally or with @<target> on a server.
const (
	ExportFlag = "--export"
	ImportFlag = "--import"
)

// SyncService is a service of the project as its data labels say:
// whether it has data (HasData, DataLabel), how a copy carries it (Copy,
// CopyNone without data), when it is stopped (Stop, and StopLabeled
// whether DataStopLabel is set), the entries of mnt its binds hold (a
// service with data's without its excluded ones, named in Excluded), the
// services it depends on and its restart policy.
type SyncService struct {
	Name, Copy     string
	Stop           string
	StopLabeled    bool
	Dirs, Excluded []string
	HasData        bool
	DependsOn      []string
	Restart        string
	// Profiles are the service's compose profiles: none means always on.
	Profiles []string
	// Owner is its MntOwnerLabel, <uid>:<gid>, "" without one; Writable
	// the entries of mnt it mounts writable (excluded ones too), which
	// an owner covers: a read-only mount never claims an entry.
	Owner    string
	Writable []string
	// TreeBinds are its writable bind mounts from inside the project's
	// tree outside mnt, relative to the tree: they live in the release,
	// and go with the next one (CheckMounts refuses them).
	TreeBinds []string
	// OutsideBinds are its writable bind mounts from outside both the
	// tree and mnt, as resolved (/var/lib/app): the project's own
	// business, which no copy or snapshot carries (a snapshot refuses one
	// on a service with data).
	OutsideBinds []string
	// Binds are the sources of its bind mounts under mnt, relative to it
	// (storage, storage/files), read-only ones too: one below an owned
	// entry is refused (CheckMounts).
	Binds []string
	// LeavingBinds are its bind sources below an entry of mnt that a
	// symbolic link in the entry leads out of it (storage/public -> /),
	// as "<source> -> <where it leads>": Docker mounts that of the host
	// (CheckMounts refuses them). A link to the entry itself (an entry on
	// another disk) is the entry's place, never one.
	LeavingBinds []string
	// WholeMnt is whether it mounts ./mnt whole writable, which a service
	// without data or an owner may as far as parsing goes, and
	// CheckMounts refuses: it would write every entry, which the child
	// guard and the stop sets never see.
	WholeMnt bool
	// Volumes are the named volumes it mounts, by compose key, sorted (a
	// service with data's without the ones its exclude label names, in
	// Excluded with its entries): a service with data's are its data as
	// its entries of mnt are, copied as files the same way.
	Volumes []Volume
}

// Volume is a named volume of the project: its compose key, the name
// Docker knows it by (always <compose project>_<key>: a fixed name: or
// external: on a service with data is refused, it would be another
// project's or every environment's), and what Docker creates it with
// (driver:, driver_opts:, labels:), for devopsy to make it as compose
// would where a copy writes into one missing.
type Volume struct {
	Key, Name, Driver string
	Opts, Labels      map[string]string
}

// VolumeKeys are the keys of vs.
func VolumeKeys(vs []Volume) []string {
	keys := make([]string, 0, len(vs))
	for _, v := range vs {
		keys = append(keys, v.Key)
	}
	return keys
}

// Copies reports whether a copy carries s's data: it has data, and its
// DataCopyLabel is not CopyNone.
func (s SyncService) Copies() bool { return s.Copy != CopyNone }

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

// ErrComposeConfig is ComposeConfig's failure: compose could not read
// the project's files, which its own commands then say (no label's
// fault: --check-mounts lets it through).
var ErrComposeConfig = errors.New("docker compose config")

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
		return nil, fmt.Errorf("%w: %v %s", ErrComposeConfig, err, strings.TrimSpace(stderr.String()))
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
		Name    string `json:"name"`
		Volumes map[string]struct {
			Name       string            `json:"name"`
			External   bool              `json:"external"`
			Driver     string            `json:"driver"`
			DriverOpts map[string]string `json:"driver_opts"`
			Labels     map[string]string `json:"labels"`
		} `json:"volumes"`
		Services map[string]struct {
			Labels    map[string]string          `json:"labels"`
			DependsOn map[string]json.RawMessage `json:"depends_on"`
			Restart   string                     `json:"restart"`
			Profiles  []string                   `json:"profiles"`
			Volumes   []struct {
				Type     string `json:"type"`
				Source   string `json:"source"`
				ReadOnly bool   `json:"read_only"`
			} `json:"volumes"`
		} `json:"services"`
	}
	if err := json.Unmarshal(out, &config); err != nil {
		return nil, fmt.Errorf("docker compose config: %w", err)
	}
	// Resolved as far as it exists, as each source is below: a mnt not
	// made yet is still where the sources resolve to.
	mnts := []string{filepath.Join(projectDir, "mnt")}
	if r := resolvePath(mnts[0]); r != mnts[0] {
		mnts = append(mnts, r)
	}
	// The project's tree, its links resolved: on servers a release's
	// code/, where shared/'s entries are links out of it.
	tree := resolvePath(filepath.Dir(projectDir))
	var services []SyncService
	for name, s := range config.Services {
		for _, o := range oldLabels {
			if v, ok := s.Labels[o.label]; ok {
				return nil, fmt.Errorf("service %s: %s=%s: that label is now %s", name, o.label, v, o.now)
			}
		}
		cs := SyncService{Name: name, Copy: CopyNone, Stop: StopNever, DependsOn: []string{}, Restart: s.Restart, Profiles: s.Profiles}
		data, labeled := s.Labels[DataLabel]
		if labeled {
			// An empty value is an error: a variable left unset by accident
			// must not stop copying silently. The values before the copy
			// label are errors naming what replaces them.
			switch data {
			case "true":
				cs.HasData = true
			case "false":
			case "":
				return nil, fmt.Errorf("service %s: %s is empty: true, or false for no data (a variable left unset?)", name, DataLabel)
			case "mnt", "rsync", "files":
				return nil, fmt.Errorf("service %s: %s=%s: %s=true now (its entries of mnt are the default %s=%s)", name, DataLabel, data, DataLabel, DataCopyLabel, CopyVolumes)
			case "stream":
				return nil, fmt.Errorf("service %s: %s=%s: %s=true and %s=%s now", name, DataLabel, data, DataLabel, DataCopyLabel, CopyCustom)
			case "none", "skip":
				return nil, fmt.Errorf("service %s: %s=%s: %s=false now (no data), or %s=true and %s=%s for data no copy carries", name, DataLabel, data, DataLabel, DataLabel, DataCopyLabel, CopyNone)
			default:
				return nil, fmt.Errorf("service %s: %s=%s: true or false", name, DataLabel, data)
			}
		}
		if v, ok := s.Labels[DataCopyLabel]; ok && cs.HasData {
			switch v {
			case CopyVolumes, CopyCustom, CopyNone:
				cs.Copy = v
			case "":
				return nil, fmt.Errorf("service %s: %s is empty: %s, %s or %s (a variable left unset?)", name, DataCopyLabel, CopyVolumes, CopyCustom, CopyNone)
			default:
				return nil, fmt.Errorf("service %s: %s=%s: %s, %s or %s", name, DataCopyLabel, v, CopyVolumes, CopyCustom, CopyNone)
			}
		} else if cs.HasData {
			cs.Copy = CopyVolumes
		}
		if v, ok := s.Labels[DataStopLabel]; ok {
			switch v {
			case StopNever, StopImport, StopAlways:
			case StopOwn:
				// own stops it for its own data: with none chosen (false,
				// or a copy of none, through a variable), no copy moves
				// it; without any label it would never stop, which is not
				// what own says.
				if !labeled {
					return nil, fmt.Errorf("service %s: %s=%s needs a %s label: own stops it while its own data moves (%s=true); %s=%s stops it while any data is imported", name, DataStopLabel, v, DataLabel, DataLabel, DataStopLabel, StopImport)
				}
				if !cs.Copies() {
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
		if cs.HasData {
			exclude = strings.FieldsFunc(s.Labels[DataExcludeLabel], func(r rune) bool { return r == ',' || r == ' ' })
			for _, e := range exclude {
				if e == "." || e == ".." || strings.ContainsAny(e, "/\\") {
					return nil, fmt.Errorf("service %s: %s=%s: entries of mnt or named volumes only, like files or logs", name, DataExcludeLabel, s.Labels[DataExcludeLabel])
				}
			}
		}
		if v, ok := s.Labels[MntOwnerLabel]; ok {
			owner, ok := parseOwner(v)
			if !ok {
				return nil, fmt.Errorf("service %s: %s=%s: <uid>:<gid> in numbers from 0 to %d, like 10001:10001 (names cannot be resolved from the host; a variable left unset?)", name, MntOwnerLabel, v, maxOwnerID)
			}
			cs.Owner = owner
		}
		for _, v := range s.Volumes {
			if v.Type == "volume" && v.Source != "" {
				// A named volume (an anonymous one has no source, and dies
				// with its container): data on a service with data, by its
				// key; refused there when it is not the environment's own.
				if slices.ContainsFunc(cs.Volumes, func(x Volume) bool { return x.Key == v.Source }) || slices.Contains(cs.Excluded, v.Source) {
					continue
				}
				if slices.Contains(exclude, v.Source) {
					cs.Excluded = append(cs.Excluded, v.Source)
					continue
				}
				top := config.Volumes[v.Source]
				whose := ""
				switch {
				case top.External:
					whose = "external"
				case top.Name != "" && config.Name != "" && top.Name != config.Name+"_"+v.Source:
					whose = "named " + top.Name
				}
				if cs.HasData && whose != "" {
					return nil, fmt.Errorf("service %s: %s=true mounts the volume %s, which is %s: a copy into it would write another project's data, or every environment's: give the environment its own (no name:, not external:), or leave it out (%s=%s)", name, DataLabel, v.Source, whose, DataExcludeLabel, v.Source)
				}
				cs.Volumes = append(cs.Volumes, Volume{Key: v.Source, Name: cmp.Or(top.Name, v.Source), Driver: top.Driver, Opts: top.DriverOpts, Labels: top.Labels})
				continue
			}
			if v.Type != "bind" {
				continue
			}
			// What a bind is in mnt goes by its source as written (on
			// servers through .devopsy/mnt, the link to shared/mnt); what
			// the mounts check refuses goes by that and by where its
			// links lead too (Docker mounts that), so a link in the
			// release to below an owned entry, or one in shared/mnt into
			// the release, is judged by where it leads.
			src := filepath.Clean(v.Source)
			r := resolvePath(src)
			d, rd := mntEntry(src, mnts), mntEntry(r, mnts)
			if rel := mntPath(src, mnts); d != "" && d != "." && rel != d {
				if er := resolvePath(filepath.Join(mntRoot(src, mnts), d)); !within(r, er) {
					if l := rel + " -> " + r; !slices.Contains(cs.LeavingBinds, l) {
						cs.LeavingBinds = append(cs.LeavingBinds, l)
					}
				}
			}
			for _, rel := range []string{mntPath(src, mnts), mntPath(r, mnts)} {
				if rel != "" && rel != "." && !slices.Contains(cs.Binds, rel) {
					cs.Binds = append(cs.Binds, rel)
				}
			}
			if rd == "" && !v.ReadOnly {
				if rel, err := filepath.Rel(tree, r); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
					if !slices.Contains(cs.TreeBinds, rel) {
						cs.TreeBinds = append(cs.TreeBinds, rel)
					}
				} else if !slices.Contains(cs.OutsideBinds, r) {
					cs.OutsideBinds = append(cs.OutsideBinds, r)
				}
			}
			if rd == "." && d != "." {
				cs.WholeMnt = cs.WholeMnt || !v.ReadOnly
				continue
			}
			if d == "." {
				// mnt whole is no entry: it would carry every other
				// service's data, and give them one owner. A service
				// without data may still mount it (a read-only tool):
				// devopsy ignores that mount.
				if cs.HasData {
					return nil, fmt.Errorf("service %s: %s=true mounts ./mnt whole: mount its entries (./mnt/<name>), each for what it holds", name, DataLabel)
				}
				if cs.Owner != "" && !v.ReadOnly {
					return nil, fmt.Errorf("service %s: %s=%s mounts ./mnt whole writable: mount its entries (./mnt/<name>), each for what it holds", name, MntOwnerLabel, cs.Owner)
				}
				cs.WholeMnt = cs.WholeMnt || !v.ReadOnly
				continue
			}
			if d != "" && !v.ReadOnly && !slices.Contains(cs.Writable, d) {
				cs.Writable = append(cs.Writable, d)
			}
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
		sort.Slice(cs.Volumes, func(i, j int) bool { return cs.Volumes[i].Key < cs.Volumes[j].Key })
		sort.Strings(cs.Writable)
		sort.Strings(cs.TreeBinds)
		sort.Strings(cs.Binds)
		// The owners' list (the child's, for a sync) names entries split
		// by spaces too, and find and chown take them as arguments: one
		// starting with a dash would be an option.
		if cs.Owner != "" {
			for _, d := range cs.Writable {
				if strings.ContainsFunc(d, unicode.IsSpace) || strings.HasPrefix(d, "-") {
					return nil, fmt.Errorf("service %s: ./mnt/%s: an entry of mnt %s owns cannot have whitespace in its name or start with a dash: rename it", name, d, MntOwnerLabel)
				}
			}
		}
		// Copies name entries in space-separated lists (the child's,
		// exclude) and rsync modules: a name with whitespace would split.
		// The tools that carry them (find for a snapshot's tar, tar for
		// --export) take entries as arguments: a name starting with a
		// dash would be an option (find: unknown predicate).
		if cs.HasData {
			for _, d := range append(slices.Clone(cs.Dirs), cs.Excluded...) {
				if strings.ContainsFunc(d, unicode.IsSpace) || strings.HasPrefix(d, "-") {
					return nil, fmt.Errorf("service %s: ./mnt/%s: an entry of mnt a copy carries cannot have whitespace in its name or start with a dash: rename it", name, d)
				}
			}
			// A named volume's key, the same: a take's find and --export's
			// tar name it as an argument (compose takes -data as a key).
			for _, v := range cs.Volumes {
				if strings.ContainsFunc(v.Key, unicode.IsSpace) || strings.HasPrefix(v.Key, "-") {
					return nil, fmt.Errorf("service %s: the volume %s: a named volume a copy carries cannot have whitespace in its key or start with a dash: rename it", name, v.Key)
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
	// A copy names entries and volumes side by side (a tar's top level,
	// the rsync modules): a volume of a service with data named like an
	// entry of mnt a service with data mounts would be both.
	entries := map[string]string{}
	for _, cs := range services {
		if cs.HasData {
			for _, d := range cs.Dirs {
				entries[d] = cs.Name
			}
		}
	}
	for _, cs := range services {
		if !cs.HasData {
			continue
		}
		for _, v := range cs.Volumes {
			if by, ok := entries[v.Key]; ok {
				return nil, fmt.Errorf("service %s: the volume %s is named like the entry ./mnt/%s %s mounts: a copy carries both by that name: rename one", cs.Name, v.Key, v.Key, by)
			}
		}
	}
	return services, nil
}

// mntEntry is the top-level entry of mnt that source is or lies in, "."
// for mnt itself (never an entry: parseSyncServices refuses or ignores
// it), "" outside it.
func mntEntry(source string, mnts []string) string {
	rel := mntPath(source, mnts)
	if rel == "" || rel == "." {
		return rel
	}
	return strings.SplitN(rel, string(filepath.Separator), 2)[0]
}

// mntRoot is the one of mnts source is in, "" outside them.
func mntRoot(source string, mnts []string) string {
	for _, m := range mnts {
		if within(source, m) {
			return m
		}
	}
	return ""
}

// mntPath is source relative to mnt, "." for mnt itself, "" outside it.
func mntPath(source string, mnts []string) string {
	for _, m := range mnts {
		rel, err := filepath.Rel(m, source)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		return rel
	}
	return ""
}

// resolvePath is p with its symbolic links resolved, as far as it
// exists: what is missing (Docker creates a bind's source) is joined as
// written, and a link whose target is missing is followed to where it
// leads all the same (what it points to may be made later).
func resolvePath(p string) string {
	return resolveLinks(p, 0)
}

// resolveLinks is resolvePath, depth links followed so far: past 40 (a
// loop) the path stays as it is.
func resolveLinks(p string, depth int) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	parent := filepath.Dir(p)
	if parent == p {
		return p
	}
	rp := resolveLinks(parent, depth)
	full := filepath.Join(rp, filepath.Base(p))
	if t, err := os.Readlink(full); err == nil && depth < 40 {
		if !filepath.IsAbs(t) {
			t = filepath.Join(rp, t)
		}
		return resolveLinks(t, depth+1)
	}
	return full
}

// MntOwners are the owners of the entries of mnt, entry to <uid>:<gid>
// (MntOwnerLabel), from the services of every profile: each entry a
// service with an owner mounts writable. Two services whose owners
// differ on one entry are an error; one without an owner writing an
// owned entry (a tool) is none.
func MntOwners(services []SyncService) (map[string]string, error) {
	owners := map[string]string{}
	by := map[string]string{}
	for _, s := range services {
		if s.Owner == "" {
			continue
		}
		for _, d := range s.Writable {
			if o, ok := owners[d]; ok && o != s.Owner {
				return nil, fmt.Errorf("./mnt/%s is written by %s (%s=%s) and %s (%s): one owner per entry", d, by[d], MntOwnerLabel, o, s.Name, s.Owner)
			}
			owners[d], by[d] = s.Owner, s.Name
		}
	}
	return owners, nil
}

// CheckLeavingBinds refuses the bind sources a symbolic link in their
// entry leads out of it (SyncService.LeavingBinds), alone: a rollback's
// check (--check-mounts restored), whose labels otherwise refuse
// nothing, since Docker would mount that of the host whatever release
// runs (a restored snapshot's link outlives the code it was checked in:
// review).
// A restore's (restore) skips the entries its snapshot carries (carried):
// they are replaced, their links checked in its tars.
func CheckLeavingBinds(services []SyncService, carried []string, restore bool) error {
	var bad []string
	for _, s := range services {
		for _, l := range s.LeavingBinds {
			if e, _, _ := strings.Cut(l, string(filepath.Separator)); slices.Contains(carried, e) {
				continue
			}
			bad = append(bad, fmt.Sprintf("%s mounts ./mnt/%s through a symbolic link out of its entry: Docker would mount that of the host", s.Name, l))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	what := "the rollback"
	if restore {
		what = "the restore"
	}
	return &report.Failure{Msg: "the mounts check refuses " + what + ": it does not go live", Details: bad, Fixes: []report.Remedy{report.Fix("remove the link in shared/mnt (a restored snapshot's, or one made by hand), or bind-mount where it leads explicitly")}}
}

// CheckMounts refuses, before a release goes live, the mounts whose data
// would not be what the labels say (services as SyncServices read them,
// so ./mnt whole as data or an owner's is refused already): ./mnt whole
// mounted writable by any other service, which writes every entry while
// the child guard and the stop sets see none of it (read-only, a tool,
// it is ignored); a writable bind mount from inside the project's tree
// outside mnt, which goes with the next release; a service copied as
// CopyVolumes that copies no entry of mnt (none mounted, or all excluded),
// which a sync would take for nothing to copy; an MntOwnerLabel on one
// that mounts no entry writable; an entry two owners write (MntOwners);
// and a bind source below an owned entry (./mnt/storage/files), by any
// service, read-only too: the entry's owner can replace that path with a
// symbolic link, and Docker resolves a bind source on the host (a link
// to / would mount the host's root); and a bind source a link in its
// entry leads out of it (LeavingBinds), whoever made it.
func CheckMounts(services []SyncService) error {
	var bad []string
	var fixes []report.Remedy
	tree, data, owner, whole, below, leaving := false, false, false, false, false, false
	owners, ownersErr := MntOwners(services)
	for _, s := range services {
		for _, l := range s.LeavingBinds {
			bad = append(bad, fmt.Sprintf("%s mounts ./mnt/%s through a symbolic link out of its entry: Docker would mount that of the host", s.Name, l))
			leaving = true
		}
		for _, b := range s.Binds {
			if e, _, under := strings.Cut(b, string(filepath.Separator)); under && owners[e] != "" {
				bad = append(bad, fmt.Sprintf("%s mounts ./mnt/%s, below the owned entry ./mnt/%s (%s=%s): its owner can make that path a symbolic link, and Docker would mount what it points to on the host", s.Name, b, e, MntOwnerLabel, owners[e]))
				below = true
			}
		}
		if s.WholeMnt {
			bad = append(bad, fmt.Sprintf("%s mounts ./mnt whole writable: it would write every entry, and no sync or import sees it there", s.Name))
			whole = true
		}
		for _, b := range s.TreeBinds {
			bad = append(bad, fmt.Sprintf("%s mounts ./%s writable: it is in the release, and the next one replaces it", s.Name, b))
			tree = true
		}
		if s.Copy == CopyVolumes && len(s.Dirs) == 0 && len(s.Volumes) == 0 {
			bad = append(bad, fmt.Sprintf("%s has data (%s=true, %s=%s) and copies no entry of mnt or named volume: a sync would copy nothing of it", s.Name, DataLabel, DataCopyLabel, CopyVolumes))
			data = true
		}
		if s.Owner != "" && len(s.Writable) == 0 {
			bad = append(bad, fmt.Sprintf("%s has %s=%s and mounts no entry of mnt writable: it owns nothing", s.Name, MntOwnerLabel, s.Owner))
			owner = true
		}
	}
	if err := ownersErr; err != nil {
		bad = append(bad, err.Error())
		fixes = append(fixes, report.Fix("give the services writing an entry one "+MntOwnerLabel+" (a variable keeps them alike), or mount it read-only where it is only read (:ro)"))
	}
	if len(bad) == 0 {
		return nil
	}
	if tree {
		fixes = append(fixes, report.Fix("mount data from an entry of mnt (./mnt/<name>), which outlives releases, or read-only (:ro) what ships with the release"))
	}
	if data {
		fixes = append(fixes, report.Fix("bind-mount its data from ./mnt/<name> or a named volume, or label it "+DataCopyLabel+"="+CopyCustom+" or "+CopyNone+", or "+DataLabel+"=false"))
	}
	if owner {
		fixes = append(fixes, report.Fix("remove the label, or mount its data writable from ./mnt/<name>"))
	}
	if whole {
		fixes = append(fixes, report.Fix("mount the entries it writes (./mnt/<name>), or ./mnt read-only (:ro)"))
	}
	if below {
		fixes = append(fixes, report.Fix("bind-mount the entry itself (./mnt/<name>) and use the path below it inside the container"))
	}
	if leaving {
		fixes = append(fixes, report.Fix("remove the link in shared/mnt (a restored snapshot's, or one made by hand), or bind-mount where it leads explicitly"))
	}
	return &report.Failure{Msg: "the mounts check refuses the release: it does not go live", Details: bad, Fixes: fixes}
}

// SyncDirs are the entries of mnt a sync rsyncs: those of the services
// copied as CopyVolumes, sorted.
func SyncDirs(services []SyncService) []string {
	var dirs []string
	for _, s := range services {
		if s.Copy != CopyVolumes {
			continue
		}
		for _, d := range s.Dirs {
			if !slices.Contains(dirs, d) {
				dirs = append(dirs, d)
			}
		}
	}
	sort.Strings(dirs)
	return dirs
}

// SyncVolumes are the named volumes a sync rsyncs, by key: those of the
// services copied as CopyVolumes, sorted.
func SyncVolumes(services []SyncService) []string {
	var keys []string
	for _, s := range services {
		if s.Copy != CopyVolumes {
			continue
		}
		for _, v := range s.Volumes {
			if !slices.Contains(keys, v.Key) {
				keys = append(keys, v.Key)
			}
		}
	}
	sort.Strings(keys)
	return keys
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

// DataVolumesServices names the services copied as CopyVolumes.
func DataVolumesServices(services []SyncService) []string {
	var names []string
	for _, s := range services {
		if s.Copy == CopyVolumes {
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
			return (op == ExportOne || op == ImportOne) && s.Name == service || (op == SyncParent || op == SyncChild) && s.Copies()
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

// SyncCustomServices names the services copied as CopyCustom in
// compose's depends_on order, dependencies first, ties alphabetical: a
// database imported before what reads it. The order is over every
// service, so a dependency through one without data counts too; a cycle
// (compose refuses one already) is an error.
func SyncCustomServices(services []SyncService) ([]string, error) {
	order, err := dependsOn(services)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range order {
		if s, _ := SyncFind(services, n); s.Copy == CopyCustom {
			names = append(names, n)
		}
	}
	return names, nil
}

// DataLists are the values of DEVOPSY_DATA_VOLUMES, DEVOPSY_DATA_CUSTOM
// and DEVOPSY_DATA_STOP for the data capability: this side's services by
// how a copy carries them, space-separated (custom in depends_on order
// when it has one), and stop, the services this operation stops on this
// side. A stopped custom service is in both custom and stop, so a script
// knows to run --rm rather than exec.
func DataLists(services []SyncService, stop []string) (volumes, custom, stops string) {
	customs, err := SyncCustomServices(services)
	if err != nil {
		for _, s := range services {
			if s.Copy == CopyCustom {
				customs = append(customs, s.Name)
			}
		}
	}
	return strings.Join(DataVolumesServices(services), " "), strings.Join(customs, " "), strings.Join(stop, " ")
}
