package setup

import "fmt"

// Item is one of the setup's items as the model rules it. ConfirmFirst: its first apply is
// --server-setup's, shown and confirmed, since it may cut what runs off from something; once
// applied, --upgrade keeps it as any of devopsy's own.
type Item struct {
	Name         string
	ConfirmFirst bool
}

// UpgradeApplies says whether --upgrade applies the item, applied saying it was before.
func (i Item) UpgradeApplies(applied bool) bool { return !i.ConfirmFirst || applied }

// FirewallItem is the firewall that keeps containers off the cloud's metadata services: its
// first load can cut a running app off from what it read there (its instance's credentials).
var FirewallItem = Item{Name: "metadata firewall", ConfirmFirst: true}

// FirewallState is the metadata firewall's state on a server.
type FirewallState int

const (
	// FirewallInSync: loaded as devopsy loads it, or with firewall: metadata: allow, nothing
	// of devopsy's there.
	FirewallInSync FirewallState = iota
	// FirewallLoad: never loaded here, the first load --server-setup's alone.
	FirewallLoad
	// FirewallReload: devopsy's, not as it loads it (a piece changed, a load failed): put back.
	FirewallReload
	// FirewallRemove: allow, and devopsy's pieces still there.
	FirewallRemove
	// FirewallRefused: allow with a door, whose project users' containers never reach a
	// metadata service.
	FirewallRefused
)

var firewallStates = [...]string{"in sync", "to load", "to load again", "to remove", "refused"}

func (s FirewallState) String() string { return firewallStates[s] }

// MarshalText is s as Facts' JSON holds it.
func (s FirewallState) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText reads a FirewallState as MarshalText writes it.
func (s *FirewallState) UnmarshalText(b []byte) error {
	for i, w := range firewallStates {
		if w == string(b) {
			*s = FirewallState(i)
			return nil
		}
	}
	return fmt.Errorf("no firewall state %q", b)
}

// JudgeFirewall is the metadata firewall's state from the server's want (allow, its config's
// firewall: metadata: allow; door, a door there), whether devopsy put it before (applied: its
// loader there, or its mark), and whether it is loaded as devopsy loads it (inSync: every piece
// as devopsy writes it, the loader's mark saying what it loads, its unit not failed).
func JudgeFirewall(allow, door, applied, inSync bool) FirewallState {
	switch {
	case allow && door:
		return FirewallRefused
	case allow && applied:
		return FirewallRemove
	case allow:
		return FirewallInSync
	case !applied:
		return FirewallLoad
	case inSync:
		return FirewallInSync
	}
	return FirewallReload
}

// FirewallFact is the metadata firewall as the server judged it, Door whether its loader
// loads the door's chains too, Unit its unit's failure ("" when none).
type FirewallFact struct {
	State FirewallState `json:"state"`
	Door  bool          `json:"door,omitempty"`
	Unit  string        `json:"unit,omitempty"`
}
