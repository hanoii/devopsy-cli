package setup

import "fmt"

// FirewallItemName is the metadata firewall's item, the firewall that keeps containers off
// the cloud's metadata services.
const FirewallItemName = "metadata firewall"

// FirewallKind is what of devopsy's firewall a server has: none, the containers' chain alone,
// or the door's chains with it.
type FirewallKind int

const (
	NoFirewall FirewallKind = iota
	ContainersFirewall
	DoorFirewall
)

// FirewallWantsDoor says whether the firewall to load is the door's: door: in the server's
// config, or the door's loaded already, which devopsy never takes off a server.
func FirewallWantsDoor(door bool, loaded FirewallKind) bool {
	return door || loaded == DoorFirewall
}

// FirewallState is the metadata firewall's state on a server.
type FirewallState int

const (
	// FirewallInSync: loaded as devopsy loads it, or with firewall: metadata: allow, nothing
	// of devopsy's there.
	FirewallInSync FirewallState = iota
	// FirewallLoad: none loaded here, the first load either kind's.
	FirewallLoad
	// FirewallLoadDoor: the containers' chain loaded alone, and door: asks the door's too.
	FirewallLoadDoor
	// FirewallReload: loaded, not as devopsy loads it (a piece changed, a load failed): put
	// back, the same kind.
	FirewallReload
	// FirewallRemove: allow, and devopsy's containers' firewall still there.
	FirewallRemove
	// FirewallRefused: allow where the door's firewall is wanted (FirewallWantsDoor), whose
	// project users' containers never reach a metadata service.
	FirewallRefused
)

var firewallStates = [...]string{"in sync", "to load", "to load as the door's", "to load again", "to remove", "refused"}

func (s FirewallState) String() string { return firewallStates[s] }

// Changes says whether an apply in s changes the firewall.
func (s FirewallState) Changes() bool { return s != FirewallInSync && s != FirewallRefused }

// Upgrade says whether --upgrade applies s: a load again of the kind loaded only. A first
// load, a change of kind and a removal change what the server's apps reach, so they are
// --server-setup's, shown and confirmed (--yes confirms).
func (s FirewallState) Upgrade() bool { return s == FirewallReload }

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
// firewall: metadata: allow; door, door: in it), what of devopsy's firewall it has (loaded:
// its pieces, its loader's kind, the mark), and whether that is loaded as devopsy loads the
// kind FirewallWantsDoor gives (inSync: every piece as devopsy writes it, the mark saying the
// last load of that kind went through).
func JudgeFirewall(allow, door bool, loaded FirewallKind, inSync bool) FirewallState {
	wantDoor := FirewallWantsDoor(door, loaded)
	switch {
	case allow && wantDoor:
		return FirewallRefused
	case allow && loaded != NoFirewall:
		return FirewallRemove
	case allow:
		return FirewallInSync
	case loaded == NoFirewall:
		return FirewallLoad
	case wantDoor && loaded == ContainersFirewall:
		return FirewallLoadDoor
	case inSync:
		return FirewallInSync
	}
	return FirewallReload
}

// FirewallFact is the metadata firewall as the server judged it, Door whether the loader to
// load is the door's (FirewallWantsDoor), for one to load again Unit its unit's failure (""
// otherwise).
type FirewallFact struct {
	State FirewallState `json:"state"`
	Door  bool          `json:"door,omitempty"`
	Unit  string        `json:"unit,omitempty"`
}
