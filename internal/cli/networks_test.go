package cli

import (
	"strings"
	"testing"
)

func TestNetworkConflicts(t *testing.T) {
	defer func(c func(string, []string) ([]byte, error), o func(string) (string, bool)) {
		ComposeConfig, NetworkOwner = c, o
	}(ComposeConfig, NetworkOwner)
	ComposeConfig = func(string, []string) ([]byte, error) {
		return []byte(`{"name":"shop-prod","networks":{"default":{"name":"shop-prod_default"},"traefik":{"name":"traefik","external":true},"backend":{"name":"backend"}}}`), nil
	}
	owners := map[string]string{"traefik": "traefik-main", "backend": "other-prod", "shop-prod_default": "shop-prod"}
	NetworkOwner = func(name string) (string, bool) { o, ok := owners[name]; return o, ok }
	err := NetworkConflicts(".", nil)
	if err == nil || !strings.Contains(err.Error(), "network backend is other-prod's") || strings.Contains(err.Error(), "traefik") || strings.Contains(err.Error(), "default") {
		t.Errorf("conflicts: %v", err)
	}
	// Own, absent, or unlabeled (made by hand): fine.
	owners = map[string]string{"shop-prod_default": "shop-prod", "backend": ""}
	if err := NetworkConflicts(".", nil); err != nil {
		t.Errorf("no conflict: %v", err)
	}
	// An unreadable configuration is the up's problem, not this check's.
	ComposeConfig = func(string, []string) ([]byte, error) { return []byte("not json"), nil }
	if err := NetworkConflicts(".", nil); err != nil {
		t.Errorf("unparsable: %v", err)
	}
}
