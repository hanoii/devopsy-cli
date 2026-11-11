package setup

// State is a daemon.json part's state, from its observed value (O), devopsy's want (W)
// and the record's entry (R).
type State int

const (
	// InSync: O is W.
	InSync State = iota
	// Kept: the admin kept O, as recorded.
	Kept
	// Devopsys: O is what devopsy applied, or there is no record and no value: devopsy
	// may put W.
	Devopsys
	// Changed: anything else, someone's value devopsy never puts over unasked.
	Changed
)

func (s State) String() string {
	return [...]string{"in sync", "kept", "devopsy's", "changed"}[s]
}

// Entry is a part's entry in the record: what devopsy applied, or what the admin kept,
// in canonical JSON. The zero Entry is none.
type Entry struct {
	Applied string `yaml:"applied,omitempty"`
	Kept    string `yaml:"kept,omitempty"`
}

// StateOf is a part's state, the rules taken in order.
func StateOf(o, w string, r Entry) State {
	switch {
	case o == w:
		return InSync
	case r.Kept != "" && o == r.Kept:
		return Kept
	case r.Applied != "" && o == r.Applied, r == Entry{} && o == "{}":
		return Devopsys
	}
	return Changed
}

// PartState is one part as the server has it.
type PartState struct {
	Part     Part
	Observed string
	Entry    Entry
	State    State
}

// Judge is every part devopsy wants, as d and the record have it, in Parts' order.
func Judge(d Daemon, r Record) []PartState {
	out := make([]PartState, 0, len(Parts))
	for _, p := range Parts {
		s := PartState{Part: p, Observed: d.Observed(p), Entry: r.Daemon[p.Name]}
		s.State = StateOf(s.Observed, p.Want, s.Entry)
		out = append(out, s)
	}
	return out
}

// Decision is what a command does with one part: put its want in daemon.json, record an
// entry (nil: the record stays as it is), or leave a changed part as it is, unrecorded,
// said with --server-setup as its fix.
type Decision struct {
	Put    bool
	Record *Entry
	Left   bool
}

// Answer is the admin's answer for a changed part: keep or replace the value whose
// Digest it names. The zero Answer is none.
type Answer struct {
	Replace bool
	Digest  string
}

// Upgrade is what --upgrade does with p, which nobody is asked about: devopsy's value
// is put, a changed one left.
func (p PartState) Upgrade() Decision {
	switch p.State {
	case InSync:
		return p.recordWant()
	case Devopsys:
		return p.putWant()
	case Changed:
		return Decision{Left: true}
	}
	return Decision{}
}

// Setup is what --server-setup's apply does with p, a changed part by its answer, which
// acts only while the part still holds the value answered about: else, or with no
// answer, it is left, to be asked at the next run.
func (p PartState) Setup(a Answer) Decision {
	if p.State != Changed {
		return p.Upgrade()
	}
	if a.Digest == "" || a.Digest != Digest(p.Observed) {
		return Decision{Left: true}
	}
	if a.Replace {
		return p.putWant()
	}
	return Decision{Record: &Entry{Kept: p.Observed}}
}

// recordWant records W applied, only when the record says otherwise.
func (p PartState) recordWant() Decision {
	if p.Entry == (Entry{Applied: p.Part.Want}) {
		return Decision{}
	}
	return Decision{Record: &Entry{Applied: p.Part.Want}}
}

// putWant puts W and records it applied.
func (p PartState) putWant() Decision {
	return Decision{Put: true, Record: &Entry{Applied: p.Part.Want}}
}

// Show is how a command shows a part: not at all, as kept by the admin, among the
// differences (--server-info's a warning fixed by --server-setup), or as a question,
// keep or replace.
type Show int

const (
	Silent Show = iota
	ShowKept
	Difference
	Question
)

// ShowInfo is how --server-info shows a part in state s.
func ShowInfo(s State) Show {
	switch s {
	case Kept:
		return ShowKept
	case Devopsys, Changed:
		return Difference
	}
	return Silent
}

// ShowSetup is how --server-setup shows a part in state s before it asks.
func ShowSetup(s State) Show {
	switch s {
	case Kept:
		return ShowKept
	case Devopsys:
		return Difference
	case Changed:
		return Question
	}
	return Silent
}
