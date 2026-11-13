package setup

import (
	"fmt"
	"slices"

	"github.com/hanoii/devopsy-cli/internal/howto"
)

// Facts is what a server's devopsy finds of its setup, read-only: the required items that
// refuse, every tunable item's state, pending, and what the operator words around them. The
// server's devopsy, which applies, computes every state; the operator only words them.
type Facts struct {
	// Root is the release root in its one form, its links followed as far as it is there;
	// RootAction what an apply does to it (RootMake, RootGive), "" for nothing.
	Root       string `json:"root"`
	RootAction string `json:"root_action,omitempty"`
	// Refusals are the required items that refuse, each said with its fix.
	Refusals []Refusal `json:"refusals,omitempty"`
	// Missing are the tools a server needs that are not there and refuse nothing (setsid,
	// mkfifo), each warned of, the door's (sudo) only on a server with project users.
	Missing []string `json:"missing,omitempty"`
	// DaemonError is why daemon.json could not be judged (not a JSON object, unreadable):
	// its parts are then unknown, and the file is never rewritten.
	DaemonError string     `json:"daemon_error,omitempty"`
	Parts       []PartFact `json:"parts,omitempty"`
	// Files are the files devopsy writes whole, each in sync or not.
	Files   []FileFact   `json:"files"`
	Buffers []BufferFact `json:"buffers"`
	// Firewall is the metadata firewall's, nil from a devopsy that judges none.
	Firewall *FirewallFact `json:"firewall,omitempty"`
	// Pending: Docker has yet to take daemon.json (Pending); PendingParts the parts docker info
	// shows it has yet to take, none when only the marker tells (log-opts).
	Pending      bool     `json:"pending,omitempty"`
	PendingParts []string `json:"pending_parts,omitempty"`
	// Reboot: the system's restart flag is raised, a reboot asked for, which restarts Docker.
	Reboot bool `json:"reboot,omitempty"`
	// Running is how many containers run; LiveRestore whether the daemon keeps them
	// through its restart.
	Running     int  `json:"running"`
	LiveRestore bool `json:"live_restore"`
	// Candidates are the volumes the root could be on, each offered or not (Candidate.Why);
	// Projects whether the root holds what devopsy keeps of a release already (a project's
	// environment, a uid claim, a project user added).
	Candidates []Candidate `json:"candidates,omitempty"`
	Projects   bool        `json:"projects"`
	// RootNamed: the deploy user's config (ConfigFile) names releases: root. NamesRoot: an apply
	// with --root names Root there first (facts --root of a root releases do not live in yet).
	RootNamed  bool   `json:"root_named,omitempty"`
	NamesRoot  bool   `json:"names_root,omitempty"`
	ConfigFile string `json:"config_file,omitempty"`
	// Record is the record's path, "" while there is none: the server is not set up by
	// devopsy yet.
	Record string `json:"record,omitempty"`
	// FloorNote: the record's note of the devopsy that found Docker at its floor (Record.Floor)
	// is not what this read finds, so an apply writes it, or takes it off.
	FloorNote bool `json:"floor_note,omitempty"`
}

// What an apply does to the release root.
const (
	RootMake = "make" // missing: made, then given to the deploy user
	RootGive = "give" // root's: given to the deploy user
)

// Refusal is a required item that refuses what depends on it, and its fix in words.
type Refusal struct {
	Item    string `json:"item"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
	// Howto is the topic of devopsy --server-setup-howto that has the fix's steps, "" for none:
	// the operator's devopsy offers its command after the fix's text (fixWords).
	Howto howto.Name `json:"howto,omitempty"`
	// Command is the fix's command, offered after its text, "" for none: a command of devopsy's
	// on the server's address as its session named it, the text saying it in words without one.
	Command string `json:"command,omitempty"`
	// Stops is what of the apply it stops (stepNeeds), Item when empty: a refusal said under
	// one item may stop less than that item's others (Docker's plugins, its unit's config
	// file).
	Stops string `json:"stops,omitempty"`
}

// PartFact is one daemon.json part as the server judged it, with what an apply would do
// with it unasked (PartState.Setup with no answer): put devopsy's value, or record.
type PartFact struct {
	Name     string `json:"name"`
	State    State  `json:"state"`
	Observed string `json:"observed"`
	Kept     string `json:"kept,omitempty"`
	Want     string `json:"want"`
	Puts     bool   `json:"puts,omitempty"`
	Records  bool   `json:"records,omitempty"`
}

// FactOf is s as the facts carry it.
func FactOf(s PartState) PartFact {
	d := s.Setup(Answer{})
	return PartFact{Name: s.Part.Name, State: s.State, Observed: s.Observed, Kept: s.Entry.Kept, Want: s.Part.Want, Puts: d.Put, Records: d.Record != nil}
}

// FileFact is a file devopsy writes whole: in sync when it holds its text exactly, Missing
// when it is not there (said only by the facts that word it so). With Link, it is a link that
// enables the unit Link names: in sync when it leads there.
type FileFact struct {
	Item    string `json:"item"`
	Path    string `json:"path"`
	InSync  bool   `json:"in_sync"`
	Missing bool   `json:"missing,omitempty"`
	Link    string `json:"link,omitempty"`
}

// BufferFact is a live socket buffer: Low when below BufferSize.
type BufferFact struct {
	Key  string `json:"key"`
	Live int64  `json:"live"`
	Low  bool   `json:"low,omitempty"`
}

// The tunable items besides daemon.json's parts.
const (
	ItemBuffers = "socket buffers"
	ItemMounts  = "mounts drop-in"
)

// MarshalText is s as Facts' JSON holds it.
func (s State) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText reads a State as MarshalText writes it.
func (s *State) UnmarshalText(b []byte) error {
	for st := InSync; st <= Outside; st++ {
		if st.String() == string(b) {
			*s = st
			return nil
		}
	}
	return fmt.Errorf("no part state %q", b)
}

// Records says whether an apply would change the record with no answer asked (a part in sync
// whose entry says otherwise, say), the host left as it is.
func (f Facts) Records() bool {
	return slices.ContainsFunc(f.Parts, func(p PartFact) bool { return p.Records })
}

// Changes says whether an apply would change the host with no answer asked: a step with a
// change to make (Changing) that goes (Goes), Docker's restart aside, which is asked apart.
func (f Facts) Changes() bool {
	return slices.ContainsFunc(f.Changing(), func(s Step) bool { return s != StepRestart && f.Goes(s) })
}

// AsksRoot says whether where releases live is still open: the config names no root and none
// is being named, nothing is released in the root, and a volume is offered. --server-setup
// asks it then at a terminal, and the volume hint shows it where it is not asked.
func (f Facts) AsksRoot() bool {
	return !f.RootNamed && !f.NamesRoot && !f.Projects && slices.ContainsFunc(f.Candidates, func(c Candidate) bool { return c.Why == "" })
}
