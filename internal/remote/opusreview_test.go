package remote

import "testing"

// An attack round on project users, by one reviewer.

// BUG: DoorFirewallMissing finds the rule's lines anywhere in OUTPUT, never whether a
// packet reaches them: a rule before them that accepts (a firewall's own chain, ufw's
// ufw-track-output accepting each new connection, or a plain ACCEPT) leaves every project
// user on Docker's bridges and the metadata service, and --server-info says nothing is
// missing. The parts count only before any rule that may accept a project user's packet.
func TestOpusReviewDoorFirewallMissingAfterAnAccept(t *testing.T) {
	t.Parallel()
	rules := "-P OUTPUT ACCEPT\n" +
		"-A OUTPUT -j ACCEPT\n" +
		"-A OUTPUT -o docker0 -m owner --gid-owner 1001 --suppl-groups -j REJECT --reject-with icmp-port-unreachable\n" +
		"-A OUTPUT -o br-+ -m owner --gid-owner 1001 --suppl-groups -j REJECT --reject-with icmp-port-unreachable\n" +
		"-A OUTPUT -d 169.254.169.254/32 -m owner --gid-owner 1001 --suppl-groups -j REJECT --reject-with icmp-port-unreachable\n"
	if got := DoorFirewallMissing(rules, "1001"); len(got) == 0 {
		t.Error("BUG: the rule's parts behind an ACCEPT count as there")
	}
}
