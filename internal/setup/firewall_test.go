package setup

import (
	"encoding/json"
	"testing"
)

// The metadata firewall's states, the rules taken in order: allow with a door refused, allow
// removing what devopsy put, a server never loaded loading it first, one loaded as devopsy
// loads it in sync, anything else loaded again.
func TestJudgeFirewall(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name                         string
		allow, door, applied, inSync bool
		want                         FirewallState
	}{
		{"allow with a door", true, true, true, true, FirewallRefused},
		{"allow with a door, never loaded", true, true, false, false, FirewallRefused},
		{"allow, devopsy's there", true, false, true, false, FirewallRemove},
		{"allow, nothing there", true, false, false, false, FirewallInSync},
		{"block, never loaded", false, false, false, false, FirewallLoad},
		{"block, loaded", false, false, true, true, FirewallInSync},
		{"block, a piece changed or a load failed", false, false, true, false, FirewallReload},
		{"block with a door, loaded", false, true, true, true, FirewallInSync},
	} {
		if got := JudgeFirewall(c.allow, c.door, c.applied, c.inSync); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	data, err := json.Marshal(FirewallFact{State: FirewallLoad})
	var back FirewallFact
	if err != nil || json.Unmarshal(data, &back) != nil || back.State != FirewallLoad || string(data) != `{"state":"to load"}` {
		t.Errorf("%s, %v, %+v", data, err, back)
	}
}

// An item whose first apply is confirmed: --upgrade applies it only once applied, so it never
// makes the first load; another item it always applies.
func TestItemUpgradeApplies(t *testing.T) {
	t.Parallel()
	if FirewallItem.UpgradeApplies(false) || !FirewallItem.UpgradeApplies(true) {
		t.Error("the metadata firewall")
	}
	if !(Item{Name: "socket buffers"}).UpgradeApplies(false) {
		t.Error("an item of devopsy's own")
	}
}
