package cli

import (
	"strings"
	"testing"
)

// The data capability's catalog names every command whose sync runs its
// hooks: what --debug capabilities shows a project author.
func TestDataCapabilityCalledBy(t *testing.T) {
	t.Parallel()
	for _, c := range Capabilities {
		if c.Name != DataCapability {
			continue
		}
		for _, flag := range []string{"--sync", "--reset", "--release-sync", "--export", "--import"} {
			if !strings.Contains(c.CalledBy, flag) {
				t.Errorf("the data capability's caller list lacks %s: %s", flag, c.CalledBy)
			}
		}
		return
	}
	t.Fatal("no data capability in the catalog")
}
