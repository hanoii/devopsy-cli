package setup

import (
	"encoding/json"
	"errors"
	"testing"
)

// The metadata firewall's states, the rules taken in order: allow where the door's firewall
// is wanted (door: or the door's loaded) refused, allow removing what devopsy put, a server
// with none loading it first, the containers' alone loaded where door: asks the door's, one
// loaded as devopsy loads it in sync, anything else loaded again. A door's firewall loaded
// stays the door's with door: gone.
func TestJudgeFirewall(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name        string
		allow, door bool
		loaded      FirewallKind
		inSync      bool
		want        FirewallState
		wantsDoor   bool
		upgrade     bool
	}{
		{"allow with a door", true, true, DoorFirewall, true, FirewallRefused, true, false},
		{"allow with a door, none loaded", true, true, NoFirewall, false, FirewallRefused, true, false},
		{"allow, the door's loaded, door: gone", true, false, DoorFirewall, true, FirewallRefused, true, false},
		{"allow, devopsy's there", true, false, ContainersFirewall, false, FirewallRemove, false, false},
		{"allow, nothing there", true, false, NoFirewall, false, FirewallInSync, false, false},
		{"block, none loaded", false, false, NoFirewall, false, FirewallLoad, false, false},
		{"block with a door, none loaded", false, true, NoFirewall, false, FirewallLoad, true, false},
		{"block with a door, the containers' loaded", false, true, ContainersFirewall, true, FirewallLoadDoor, true, false},
		{"block, loaded", false, false, ContainersFirewall, true, FirewallInSync, false, false},
		{"block, a piece changed or a load failed", false, false, ContainersFirewall, false, FirewallReload, false, true},
		{"block with a door, loaded", false, true, DoorFirewall, true, FirewallInSync, true, false},
		{"block, the door's loaded, door: gone", false, false, DoorFirewall, true, FirewallInSync, true, false},
		{"block, the door's loaded not as devopsy loads it, door: gone", false, false, DoorFirewall, false, FirewallReload, true, true},
	} {
		got := JudgeFirewall(c.allow, c.door, c.loaded, c.inSync)
		if got != c.want || FirewallWantsDoor(c.door, c.loaded) != c.wantsDoor || got.Upgrade() != c.upgrade {
			t.Errorf("%s: %s (door's %v, --upgrade %v), want %s", c.name, got, FirewallWantsDoor(c.door, c.loaded), got.Upgrade(), c.want)
		}
	}
	// An apply changes the firewall in every state but in sync and refused.
	for st, changes := range map[FirewallState]bool{FirewallInSync: false, FirewallLoad: true, FirewallLoadDoor: true, FirewallReload: true, FirewallRemove: true, FirewallRefused: false} {
		if st.Changes() != changes {
			t.Errorf("%s: changes %v", st, st.Changes())
		}
	}
	data, err := json.Marshal(FirewallFact{State: FirewallLoadDoor})
	var back FirewallFact
	if err != nil || json.Unmarshal(data, &back) != nil || back.State != FirewallLoadDoor || string(data) != `{"state":"to load as the door's"}` {
		t.Errorf("%s, %v, %+v", data, err, back)
	}
}

// A root read's judgment of the chains: chains not as devopsy loads them make a firewall in
// sync by its files and mark one to load again, its findings kept for the words; another
// state stays as it is; a read that failed changes no state and says why.
func TestFirewallFactChains(t *testing.T) {
	t.Parallel()
	f := FirewallFact{State: FirewallInSync}
	f.Chains(nil, nil)
	if f.State != FirewallInSync || f.Wrong != nil {
		t.Errorf("chains right: %+v", f)
	}
	f.Chains([]string{"iptables: nothing in PREROUTING jumps to DEVOPSY-CONTAINERS"}, nil)
	if f.State != FirewallReload || len(f.Wrong) != 1 {
		t.Errorf("chains wiped: %+v", f)
	}
	g := FirewallFact{State: FirewallRefused}
	g.Chains([]string{"x"}, nil)
	if g.State != FirewallRefused {
		t.Errorf("refused, chains wiped: %+v", g)
	}
	h := FirewallFact{State: FirewallInSync}
	h.Chains(nil, errors.New("Docker did not answer"))
	if h.State != FirewallInSync || h.Unread != "Docker did not answer" {
		t.Errorf("unread: %+v", h)
	}
}
