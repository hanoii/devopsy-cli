package setup

import (
	"slices"
	"testing"
)

// A refusal stops only the steps that need what it refuses: Docker every one (each is a root
// run), daemon.json devopsy cannot judge and a broken record its parts and a restart, the
// release root its making and the mounts drop-in, the tools and the firewall's own the
// firewall; the others go.
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
		{"the tools", Facts{Refusals: []Refusal{{Item: RefusalTools}}}, []Step{StepFirewall}},
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

// What an apply would change counts only the steps that go: a daemon.json part to put beside
// a daemon.json devopsy cannot judge is nothing to apply, a firewall to load beside it is.
func TestChangesCountsTheStepsThatGo(t *testing.T) {
	t.Parallel()
	parts := Facts{Refusals: []Refusal{{Item: RefusalRecord}}, Parts: []PartFact{{Puts: true}}}
	if parts.Changes() {
		t.Error("a part to put, its record broken: changes")
	}
	fw := Facts{DaemonError: "x", Firewall: &FirewallFact{State: FirewallReload}}
	if !fw.Changes() || !slices.Equal(fw.Changing(), []Step{StepFirewall}) {
		t.Errorf("a firewall to load beside daemon.json unjudged: %v", fw.Changing())
	}
}
