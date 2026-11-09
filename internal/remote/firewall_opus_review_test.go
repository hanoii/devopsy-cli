package remote

import (
	"slices"
	"strings"
	"testing"
)

// BUG: Oracle Cloud's metadata service over IPv6 is not a metadata address. Its instance
// metadata (instance principals' credentials among them, IMDSv2's header being anyone's to
// send) answers at fd00:c1::a9fe:a9fe on an instance with IPv6, which neither chain holds a
// project user's process or its containers off: IPv6 is named address by address, and the
// list has AWS's, Google's and OpenStack's only. The control: AWS's is there.
func TestOpusReviewFirewallOracleIPv6Metadata(t *testing.T) {
	t.Parallel()
	var v6 firewallFamily
	for _, f := range firewallFamilies {
		if f.cmd == "ip6tables" {
			v6 = f
		}
	}
	rules := strings.Join(doorRules(v6, DoorChain), "\n") + "\n" + strings.Join(doorRules(v6, DoorContainerChain), "\n")
	if !slices.Contains(v6.metadata, "fd00:ec2::254/128") || !strings.Contains(rules, "-d fd00:ec2::254/128 -j REJECT") {
		t.Fatalf("control: AWS's IPv6 metadata address: %v", v6.metadata)
	}
	if !strings.Contains(rules, "fd00:c1::a9fe:a9fe") {
		t.Errorf("BUG: Oracle Cloud's IPv6 metadata address fd00:c1::a9fe:a9fe is reached by project users and their containers: %v", v6.metadata)
	}
}
