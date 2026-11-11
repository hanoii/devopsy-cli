package setup

import (
	"slices"
	"testing"
)

// A refusal stops only the steps that need what it refuses: Docker every one (each is a root
// run), daemon.json devopsy cannot judge or Docker does not read and a broken record its parts
// and a restart, the release root its making and the mounts drop-in, a root no unit file
// holds the drop-in alone, flock and the firewall's own the firewall; Docker's plugins and
// tar, which releases need, none.
func TestStepsBlockedByRefusals(t *testing.T) {
	t.Parallel()
	all := []Step{StepRoot, StepBuffers, StepMounts, StepParts, StepRestart, StepFirewall}
	for _, c := range []struct {
		name    string
		f       Facts
		blocked []Step
	}{
		{"none", Facts{}, nil},
		{"Docker", Facts{Refusals: []Refusal{{Item: RefusalDocker}}}, all},
		{"daemon.json unjudged", Facts{DaemonError: "not a JSON object"}, []Step{StepParts, StepRestart}},
		{"a broken record", Facts{Refusals: []Refusal{{Item: RefusalRecord}}}, []Step{StepParts, StepRestart}},
		{"the release root", Facts{Refusals: []Refusal{{Item: RefusalRoot}}}, []Step{StepRoot, StepMounts}},
		{"flock", Facts{Refusals: []Refusal{{Item: RefusalTools, Stops: RefusalFlock}}}, []Step{StepFirewall}},
		{"tar", Facts{Refusals: []Refusal{{Item: RefusalTools, Stops: RefusalTar}}}, nil},
		{"Docker's plugins", Facts{Refusals: []Refusal{{Item: RefusalDocker, Stops: RefusalPlugins}}}, nil},
		{"Docker's config file", Facts{Refusals: []Refusal{{Item: RefusalDocker, Stops: RefusalDaemon}}}, []Step{StepParts, StepRestart}},
		{"a root no unit holds", Facts{Refusals: []Refusal{{Item: RefusalRoot, Stops: RefusalMountsPath}}}, []Step{StepMounts}},
		{"the firewall's own", Facts{Refusals: []Refusal{{Item: FirewallItemName}}}, []Step{StepFirewall}},
	} {
		var blocked []Step
		for _, s := range Steps {
			if !c.f.Goes(s) {
				blocked = append(blocked, s)
			}
		}
		if !slices.Equal(blocked, c.blocked) {
			t.Errorf("%s: %v blocked, want %v", c.name, blocked, c.blocked)
		}
	}
}

// What an apply would change counts only the steps that go: a daemon.json part to put beside a
// broken record is nothing to apply, a firewall to load beside a daemon.json devopsy cannot
// judge is; that daemon.json's parts, unknown, count as a change its refusal stopped.
func TestChangesCountsTheStepsThatGo(t *testing.T) {
	t.Parallel()
	parts := Facts{Refusals: []Refusal{{Item: RefusalRecord}}, Parts: []PartFact{{Puts: true}}}
	if parts.Changes() {
		t.Error("a part to put, its record broken: changes")
	}
	fw := Facts{DaemonError: "x", Firewall: &FirewallFact{State: FirewallReload}}
	if !fw.Changes() || !slices.Equal(fw.Changing(), []Step{StepParts, StepFirewall}) || !slices.Equal(fw.Stopped(), []string{string(StepParts)}) {
		t.Errorf("a firewall to load beside daemon.json unjudged: %v, stopped %v", fw.Changing(), fw.Stopped())
	}
	if (Facts{}).NoneGoes() || !(Facts{Refusals: []Refusal{{Item: RefusalDocker}}}).NoneGoes() {
		t.Error("NoneGoes")
	}
}
