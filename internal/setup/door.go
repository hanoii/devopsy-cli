package setup

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// DoorHeader is the first line of every file --project-users setup writes whole, each at a
// path of its own, as Header is --server-setup's; the mounts drop-in, which both keep, keeps
// Header.
const DoorHeader = "# Written by devopsy (--project-users setup)."

// DefaultSpace is the door's space --project-users setup makes when none is named.
const DefaultSpace = "/var/lib/devopsy-door"

// ProcGroup is the group that sees every process once /proc hides them, the deploy user in it.
const ProcGroup = "devopsy-proc"

// unitDir is where the units devopsy writes sit.
const unitDir = "/etc/systemd/system"

// ProcUnitName remounts /proc with hidepid at every boot; ProcUnit is its file, ProcUnitLink
// what enables it.
const (
	ProcUnitName = "devopsy-proc.service"
	ProcUnit     = unitDir + "/" + ProcUnitName
	ProcUnitLink = unitDir + "/sysinit.target.wants/" + ProcUnitName
)

// PolkitDropIn gives polkit the group, where the server has polkit: it reads its callers'
// processes to answer them. Its name is the group's, a path of its own: a drop-in named for
// devopsy alone is one another tool may write.
const PolkitDropIn = unitDir + "/polkit.service.d/90-" + ProcGroup + ".conf"

// GIDMark stands for the group's number in ProcUnitText's text as the root run gets it: the
// group may be made by that very run, which then puts its number there.
const GIDMark = "@gid@"

// PlainPath refuses a path devopsy would write into a unit file, its name and the config
// other than as it is: absolute, clean, not /, and only letters, digits and . _ - / (a mount
// unit's Where= takes no quoting, and its name escapes every other character).
func PlainPath(p string) error {
	switch {
	case p == "/":
		return errors.New("/ cannot be the directory")
	case !path.IsAbs(p):
		return fmt.Errorf("%s is not an absolute path", p)
	case path.Clean(p) != p:
		return fmt.Errorf("%s is not written plainly, as %s", p, path.Clean(p))
	}
	for _, r := range p {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-/", r)) {
			return fmt.Errorf("%s holds %q, and devopsy takes only letters, digits and . _ - /", p, r)
		}
	}
	return nil
}

// MountUnitName is the mount unit systemd names for path p, as systemd-escape --path --suffix
// mount writes it: the slashes at its ends dropped, the others as -, a leading . and every
// character but letters, digits, : _ . as \x and its hex (a - included).
func MountUnitName(p string) string {
	p = strings.Trim(path.Clean(p), "/")
	if p == "" {
		return "-.mount"
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '/':
			b.WriteByte('-')
		case c == '.' && i == 0:
			fmt.Fprintf(&b, `\x%02x`, c)
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == ':', c == '_', c == '.':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	return b.String() + ".mount"
}

// SpaceUnit is the space's mount unit's file, SpaceUnitLink what enables it at every boot.
func SpaceUnit(space string) string { return unitDir + "/" + MountUnitName(space) }
func SpaceUnitLink(space string) string {
	return unitDir + "/local-fs.target.wants/" + MountUnitName(space)
}

// SpaceUnitText is the space's mount unit: the directory bound on itself with nosuid and nodev,
// at every boot. space is a PlainPath.
func SpaceUnitText(space string) string {
	return DoorHeader + "\n[Mount]\nWhat=" + space + "\nWhere=" + space + "\nType=none\nOptions=bind,nosuid,nodev\n\n[Install]\nWantedBy=local-fs.target\n"
}

// ProcUnitText is ProcUnit's text for the group's number gid (GIDMark for the root run's): /proc
// remounted with hidepid=invisible and that gid early at every boot. The kernel takes the group
// by number only.
func ProcUnitText(gid string) string {
	return DoorHeader + "\n[Unit]\nDefaultDependencies=no\nAfter=systemd-remount-fs.service\nBefore=sysinit.target\n\n" +
		"[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/usr/bin/mount -o remount,hidepid=invisible,gid=" + gid + " /proc\n\n" +
		"[Install]\nWantedBy=sysinit.target\n"
}

// PolkitDropInText is PolkitDropIn's text.
func PolkitDropInText() string {
	return DoorHeader + "\n[Service]\nSupplementaryGroups=" + ProcGroup + "\n"
}

// DoorConfigLines are what --project-users setup appends to the deploy user's config.yaml to
// name space, the door's switch.
func DoorConfigLines(space string) string { return "door:\n  space: " + space + "\n" }

// errDoorThere is AppendDoorConfig's for a config with door: already.
var errDoorThere = errors.New("it has a door: already")

// AppendDoorConfig is the config's text have (nil for no file) with DoorConfigLines appended,
// as text, the rest as it is (comments, order): after a newline the text lacks at its end.
// The result is read back as the config's loader reads it (remote's readConfigFile: the first
// YAML document, its top mapping by key), and refused unless door: space: reads as space
// there: a file whose YAML ends before its last line (a flow mapping, an indented one, a ...
// or a second document) would take the lines where nothing reads them. A text that is no
// YAML, or holds a door: key, is an error too: only the admin changes a door:.
func AppendDoorConfig(have []byte, space string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(have, &doc); err != nil {
		return nil, fmt.Errorf("it is no YAML devopsy reads (%w)", err)
	}
	if len(doc.Content) > 0 {
		top := doc.Content[0]
		if top.Kind != yaml.MappingNode {
			return nil, errors.New("it is no YAML mapping")
		}
		for i := 0; i+1 < len(top.Content); i += 2 {
			if top.Content[i].Value == "door" {
				return nil, errDoorThere
			}
		}
	}
	out := slices.Clone(have)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, DoorConfigLines(space)...)
	var top map[string]yaml.Node
	var door struct {
		Space string `yaml:"space"`
	}
	if err := yaml.Unmarshal(out, &top); err != nil {
		return nil, errNotRead
	}
	if n, ok := top["door"]; !ok || n.Decode(&door) != nil || door.Space != space {
		return nil, errNotRead
	}
	return out, nil
}

// errNotRead is AppendDoorConfig's for a text the lines would land in unread.
var errNotRead = errors.New("lines added at its end would not be read as part of its settings")

// DoorFacts is what a server's devopsy finds of what project users need of it, for
// --project-users setup: each item's state, its refusals, and the files devopsy writes whole
// for it. The server's devopsy computes every state; the operator only words them.
type DoorFacts struct {
	// Space is the door's space, taken as named (a link in its path refuses); SpaceAction what an
	// apply does to the directory (RootMake, RootGive, "" for nothing), SpaceMode its mode
	// when it is not 0755 ("" in place).
	Space       string `json:"space"`
	SpaceAction string `json:"space_action,omitempty"`
	SpaceMode   string `json:"space_mode,omitempty"`
	// Guarded: the mount the space is on has nosuid and nodev now, whatever made it;
	// SpaceMounted: the space is a mount point of its own now, so enabling and starting its unit
	// mounts nothing.
	Guarded      bool `json:"guarded"`
	SpaceMounted bool `json:"space_mounted,omitempty"`
	// SpaceUnjudged: the space was refused before it was judged (its path one others could
	// swap), so what its step would change is not known, and counts as a change it stops.
	SpaceUnjudged bool `json:"space_unjudged,omitempty"`
	// DropInWithout: the mounts drop-in is as --server-setup writes it without the space, and
	// devopsy's want adds the space: only Docker's wait for it at boot differs.
	DropInWithout bool `json:"dropin_without,omitempty"`
	// ConfigFile is the deploy user's config; Config the lines an apply appends to it, "" for
	// none.
	ConfigFile string `json:"config_file"`
	Config     string `json:"config,omitempty"`
	// Processes is /proc's hiding as found.
	Processes ProcFact `json:"processes"`
	// Files are the files and links devopsy writes for these items, each in sync or not.
	Files    []FileFact `json:"files"`
	Refusals []Refusal  `json:"refusals,omitempty"`
}

// ProcFact is /proc's hiding of other users' processes: hidepid as /proc's mount says it and
// its gid= ("" for none); Other when it hides them and no unit of devopsy's does it (an fstab
// line), which is then judged by its own group; Group devopsy-proc's number, "" while it is
// missing; Member whether the deploy user is in the group that sees every process (Other: the
// one /proc's gid= names; else devopsy-proc), by the group file, never by its own process,
// whose groups count from its login; Polkit whether polkit.service is there.
type ProcFact struct {
	HidePID string `json:"hidepid,omitempty"`
	GID     string `json:"gid,omitempty"`
	Other   bool   `json:"other,omitempty"`
	Group   string `json:"group,omitempty"`
	Member  bool   `json:"member,omitempty"`
	Polkit  bool   `json:"polkit,omitempty"`
}

// The items --project-users setup sets besides the mounts drop-in (ItemMounts), each a
// FileFact's item and a refusal's.
const (
	ItemSpace     = "project users' directory"
	ItemProcesses = "processes"
	ItemConfig    = "config"
)

// What a refusal of --project-users setup stops, beside RefusalDocker, RefusalPlugins and
// RefusalMountsPath: the door's tools missing stop every step, the space refused stops the
// space and what names it (the drop-in, the config).
const (
	RefusalDoorTools = "door's tools"
	RefusalSpace     = ItemSpace
	RefusalProcesses = ItemProcesses
)

// The steps of --project-users setup's apply, in its order: the root run's (the space, the
// drop-in, the processes), then the config, the deploy user's own file, once the space is.
const (
	StepSpace     Step = "the project users' directory"
	StepDoorDrop  Step = "Docker's wait at boot for that directory"
	StepProcesses Step = "process hiding"
	StepConfig    Step = "door: space: in the config"
)

// DoorSteps are --project-users setup's steps, in its order.
var DoorSteps = []Step{StepSpace, StepDoorDrop, StepProcesses, StepConfig}

// doorNeeds is what each of DoorSteps needs, by what a refusal stops: the tools, Docker and
// the release root refuse first, with nothing applied (a setup half done by a missing package
// is a state nobody chose, and a server --project-users add then refuses is not ready for
// project users), Docker's plugins and floor included.
var doorNeeds = map[Step][]string{
	StepSpace:     {RefusalDoorTools, RefusalDocker, RefusalPlugins, RefusalRoot, RefusalSpace},
	StepDoorDrop:  {RefusalDoorTools, RefusalDocker, RefusalPlugins, RefusalRoot, RefusalSpace, RefusalMountsPath},
	StepProcesses: {RefusalDoorTools, RefusalDocker, RefusalPlugins, RefusalRoot, RefusalProcesses},
	StepConfig:    {RefusalDoorTools, RefusalDocker, RefusalPlugins, RefusalRoot, RefusalSpace},
}

// Refused is every item that refuses in f, by what each stops.
func (f DoorFacts) Refused() []string { return refusedItems(f.Refusals) }

// Goes says whether step s goes in f: none of what it needs refuses.
func (f DoorFacts) Goes(s Step) bool { return len(blocks(f.Refused(), doorNeeds[s])) == 0 }

// NoneGoes says whether no step goes in f.
func (f DoorFacts) NoneGoes() bool { return !slices.ContainsFunc(DoorSteps, f.Goes) }

// Changing is the steps with a change to make, whether they go or not.
func (f DoorFacts) Changing() []Step {
	var steps []Step
	for _, s := range DoorSteps {
		if f.Changes(s) {
			steps = append(steps, s)
		}
	}
	return steps
}

// Stopped is what f's refusals stop of the steps with a change to make, by name.
func (f DoorFacts) Stopped() []string {
	var words []string
	for _, s := range f.Changing() {
		if !f.Goes(s) {
			words = append(words, string(s))
		}
	}
	return words
}

// Applies says whether an apply would change anything: a step with a change that goes.
func (f DoorFacts) Applies() bool {
	return slices.ContainsFunc(f.Changing(), f.Goes)
}

// Changes says whether step s has a change to make in f.
func (f DoorFacts) Changes(s Step) bool {
	switch s {
	case StepSpace:
		return f.SpaceUnjudged || f.SpaceAction != "" || f.SpaceMode != "" || f.Mounts()
	case StepDoorDrop:
		return f.FilesDiffer(ItemMounts)
	case StepProcesses:
		p := f.Processes
		return !p.Other && (p.Group == "" || !p.Member || !p.Hides() || f.FilesDiffer(ItemProcesses))
	case StepConfig:
		return f.Config != ""
	}
	return false
}

// Mounts says whether the space step mounts the space: it is not on a mount with nosuid and
// nodev, or its unit or the unit's link are not as devopsy writes them; the apply puts the unit
// and enables it by this, the one source beside Changes.
func (f DoorFacts) Mounts() bool { return !f.Guarded || f.FilesDiffer(ItemSpace) }

// Hides says whether /proc hides other users' processes from all but the group devopsy wants:
// hidepid with gid= devopsy-proc's number (another way's group being judged by its refusal).
func (p ProcFact) Hides() bool {
	return Hiding(p.HidePID) && p.GID != "" && (p.Other || p.GID == p.Group)
}

// Hiding says whether a hidepid value, as /proc's mount says it, keeps a user from what other
// users' processes run: invisible (2) and ptraceable (4) hide them, and noaccess (1) shows only
// that each is there and whose, closing everything under /proc/<pid>, its command line
// included.
func Hiding(hidepid string) bool {
	switch hidepid {
	case "invisible", "2", "ptraceable", "4", "noaccess", "1":
		return true
	}
	return false
}

// FilesDiffer says whether a file or link of item is not as devopsy writes it.
func (f DoorFacts) FilesDiffer(item string) bool {
	return slices.ContainsFunc(f.Files, func(x FileFact) bool { return x.Item == item && !x.InSync })
}
