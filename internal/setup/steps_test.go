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
	if !fw.Changes() || !slices.Equal(fw.Changing(), []Step{StepParts, StepRestart, StepFirewall}) || !slices.Equal(fw.Stopped(), []string{string(StepParts), string(StepRestart)}) {
		t.Errorf("a firewall to load beside daemon.json unjudged: %v, stopped %v", fw.Changing(), fw.Stopped())
	}
	if (Facts{}).NoneGoes() || !(Facts{Refusals: []Refusal{{Item: RefusalDocker}}}).NoneGoes() {
		t.Error("NoneGoes")
	}
}

// A step a refusal stops whose difference the facts cannot hold counts as a change stopped:
// the mounts drop-in beside a root no unit file holds (no file judged), the release root
// beside its own refusal; one whose difference is known and none counts as nothing.
func TestChangingCountsWhatARefusalHides(t *testing.T) {
	t.Parallel()
	mounts := Facts{Refusals: []Refusal{{Item: RefusalRoot, Stops: RefusalMountsPath}}, Files: []FileFact{{Item: ItemBuffers, InSync: true}}}
	if !slices.Equal(mounts.Stopped(), []string{string(StepMounts)}) {
		t.Errorf("a root no unit holds: %v", mounts.Stopped())
	}
	root := Facts{Refusals: []Refusal{{Item: RefusalRoot}}, Files: []FileFact{{Item: ItemMounts, InSync: true}}}
	if !slices.Equal(root.Stopped(), []string{string(StepRoot)}) {
		t.Errorf("a root refused: %v", root.Stopped())
	}
	tools := Facts{Refusals: []Refusal{{Item: RefusalTools, Stops: RefusalFlock}}, Firewall: &FirewallFact{State: FirewallInSync}, Files: []FileFact{{Item: ItemMounts, InSync: true}}}
	if len(tools.Stopped()) != 0 {
		t.Errorf("flock missing, the firewall in sync: %v", tools.Stopped())
	}
}

