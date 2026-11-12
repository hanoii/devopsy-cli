package setup

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"slices"
)

// What a refusal stops (Refusal.Stops, else its Item), each stopping the steps that need it
// (stepNeeds).
const (
	// RefusalTools is the tools' refusal's item; it stops by the tool missing (RefusalFlock,
	// RefusalTar).
	RefusalTools = "tools"
	// RefusalDocker: Docker not answering, not this host's, no systemd, its unit inactive.
	RefusalDocker = "Docker"
	// RefusalRoot: the release root refused (another user's, a file, /).
	RefusalRoot = "release root"
	// RefusalRecord: the record unreadable.
	RefusalRecord = "record"
	// RefusalDaemon: daemon.json devopsy cannot judge (Facts.DaemonError), or not what Docker
	// reads (its unit's --config-file).
	RefusalDaemon = "daemon.json"
	// RefusalPlugins: Docker without its compose or buildx plugin, which releases need and no
	// step of the setup does.
	RefusalPlugins = "plugins"
	// RefusalFlock: no flock, which the firewall's loader takes its lock with.
	RefusalFlock = "flock"
	// RefusalTar: no tar, which releases need and no step of the setup does.
	RefusalTar = "tar"
	// RefusalMountsPath: a release root a unit file cannot hold, which the mounts drop-in names.
	RefusalMountsPath = "mounts path"
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

// stepNeeds is what each step needs, by what a refusal stops: every step is a root run, so
// Docker refused (not answering, not this host's) stops them all; daemon.json devopsy cannot
// judge or Docker does not read stops its parts and a restart for them, and so does a broken
// record, which keeps the parts' entries; the release root refused stops its making and the
// mounts drop-in that names it, a root a unit file cannot hold the drop-in alone; the
// firewall's loader takes its lock with flock, the one tool a step runs (the apply's own locks
// are devopsy's), and its own refusal (allow where the door's is wanted) stops it alone.
// Docker's plugins and tar stop no step: releases need them.
var stepNeeds = map[Step][]string{
	StepRoot:     {RefusalDocker, RefusalRoot},
	StepBuffers:  {RefusalDocker},
	StepMounts:   {RefusalDocker, RefusalRoot, RefusalMountsPath},
	StepParts:    {RefusalDocker, RefusalDaemon, RefusalRecord},
	StepRestart:  {RefusalDocker, RefusalDaemon, RefusalRecord},
	StepFirewall: {RefusalDocker, RefusalFlock, FirewallItemName},
}

// Refused is every item that refuses in f, a daemon.json devopsy cannot judge among them.
func (f Facts) Refused() []string {
	var items []string
	for _, r := range f.Refusals {
		if item := cmp.Or(r.Stops, r.Item); !slices.Contains(items, item) {
			items = append(items, item)
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

// NoneGoes says whether no step goes in f (Docker refused): nothing is applied at all.
func (f Facts) NoneGoes() bool { return !slices.ContainsFunc(Steps, f.Goes) }

// Stopped is what f's refusals stop of the steps with a change to make, by name, "" for
// nothing: the words of the first session's question, of a run with nothing to apply, and of
// an apply's end.
func (f Facts) Stopped() []string {
	var words []string
	for _, s := range f.Changing() {
		if !f.Goes(s) {
			words = append(words, string(s))
		}
	}
	return words
}

// Changing is the steps an apply would change, unasked, whether they go or not: those with a
// change to make, and those a refusal stops whose difference f cannot hold (a daemon.json
// devopsy cannot judge, a release root refused or one no unit file holds), counted as a change
// stopped so the refusal's line names them and --upgrade notes no version.
func (f Facts) Changing() []Step {
	var steps []Step
	for _, s := range Steps {
		if changes, known := f.change(s); changes || !known && !f.Goes(s) {
			steps = append(steps, s)
		}
	}
	return steps
}

// change is whether step s has a change to make, and known whether f holds what decides it.
func (f Facts) change(s Step) (changes, known bool) {
	parts := slices.ContainsFunc(f.Parts, func(p PartFact) bool { return p.Puts })
	switch s {
	case StepRoot:
		return f.RootAction != "", f.RootAction != "" || !slices.Contains(f.Refused(), RefusalRoot)
	case StepBuffers:
		return slices.ContainsFunc(f.Files, func(x FileFact) bool { return x.Item == ItemBuffers && !x.InSync }) || slices.ContainsFunc(f.Buffers, func(b BufferFact) bool { return b.Low }), true
	case StepMounts:
		return slices.ContainsFunc(f.Files, func(x FileFact) bool { return x.Item == ItemMounts && !x.InSync }), slices.ContainsFunc(f.Files, func(x FileFact) bool { return x.Item == ItemMounts })
	case StepParts:
		return parts, f.DaemonError == ""
	case StepRestart:
		// A restart for what Docker has yet to take, or for the parts to put.
		return f.Pending || parts, f.DaemonError == ""
	case StepFirewall:
		return f.Firewall != nil && f.Firewall.State.Changes(), true
	}
	return false, true
}

// ID is a refusal's identity, its item's and message's digest: what --server-setup's first
// session says it said, so its apply says only a refusal new since. Hex, nothing a shell reads.
func (r Refusal) ID() string {
	sum := sha256.Sum256([]byte(r.Item + "\x00" + r.Message))
	return hex.EncodeToString(sum[:6])
}

// AllRefusals are f's refusals, a daemon.json devopsy cannot judge among them as one of
// RefusalDaemon's, each once.
func (f Facts) AllRefusals() []Refusal {
	rs := slices.Clone(f.Refusals)
	if f.DaemonError != "" {
		rs = append(rs, Refusal{Item: RefusalDaemon, Message: f.DaemonError})
	}
	return rs
}
