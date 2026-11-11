package setup

import "slices"

// The items a refusal names (Refusal.Item), the required ones and two of the tunables'.
const (
	RefusalTools  = "tools"
	RefusalDocker = "Docker"
	RefusalRoot   = "release root"
	RefusalRecord = "record"
	// RefusalDaemon stands for a daemon.json devopsy cannot judge (Facts.DaemonError).
	RefusalDaemon = "daemon.json"
)

// Step is one of an apply's steps, each applied or not apart from the others.
type Step string

const (
	StepRoot     Step = "the release root"
	StepParts    Step = "daemon.json's parts"
	StepRestart  Step = "Docker's restart"
	StepBuffers  Step = ItemBuffers
	StepMounts   Step = ItemMounts
	StepFirewall Step = FirewallItemName
)

// Steps are an apply's steps, in the order it takes them.
var Steps = []Step{StepRoot, StepBuffers, StepMounts, StepParts, StepRestart, StepFirewall}

// stepNeeds is what each step needs, by the items whose refusal stops it: every step is a root
// run, so Docker (its daemon, its util image) stops them all; daemon.json devopsy cannot judge
// stops its parts and a restart for them, and so does a broken record, which keeps the parts'
// entries; the release root refused stops its making and the mounts drop-in that names it;
// the firewall's loader takes its lock with flock (the tools), and its own refusal (allow
// where the door's is wanted) stops it alone.
var stepNeeds = map[Step][]string{
	StepRoot:     {RefusalDocker, RefusalRoot},
	StepBuffers:  {RefusalDocker},
	StepMounts:   {RefusalDocker, RefusalRoot},
	StepParts:    {RefusalDocker, RefusalDaemon, RefusalRecord},
	StepRestart:  {RefusalDocker, RefusalDaemon, RefusalRecord},
	StepFirewall: {RefusalDocker, RefusalTools, FirewallItemName},
}

// Refused is every item that refuses in f, a daemon.json devopsy cannot judge among them.
func (f Facts) Refused() []string {
	var items []string
	for _, r := range f.Refusals {
		if !slices.Contains(items, r.Item) {
			items = append(items, r.Item)
		}
	}
	if f.DaemonError != "" {
		items = append(items, RefusalDaemon)
	}
	return items
}

// Blocks is what in f stops step s: the refused items it needs, none when it goes.
func (f Facts) Blocks(s Step) []string {
	var by []string
	for _, item := range f.Refused() {
		if slices.Contains(stepNeeds[s], item) {
			by = append(by, item)
		}
	}
	return by
}

// Goes says whether step s goes in f: none of what it needs refuses.
func (f Facts) Goes(s Step) bool { return len(f.Blocks(s)) == 0 }

// Changing is what of each step an apply would change, unasked: the steps with a change to
// make, whether they go or not.
func (f Facts) Changing() []Step {
	var steps []Step
	for _, s := range Steps {
		var changes bool
		switch s {
		case StepRoot:
			changes = f.RootAction != ""
		case StepBuffers:
			changes = slices.ContainsFunc(f.Files, func(x FileFact) bool { return x.Item == ItemBuffers && !x.InSync }) || slices.ContainsFunc(f.Buffers, func(b BufferFact) bool { return b.Low })
		case StepMounts:
			changes = slices.ContainsFunc(f.Files, func(x FileFact) bool { return x.Item == ItemMounts && !x.InSync })
		case StepParts:
			changes = slices.ContainsFunc(f.Parts, func(p PartFact) bool { return p.Puts })
		case StepFirewall:
			changes = f.Firewall != nil && f.Firewall.State.Changes()
		}
		if changes {
			steps = append(steps, s)
		}
	}
	return steps
}
