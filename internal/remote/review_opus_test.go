package remote

import (
	"slices"
	"strings"
	"testing"
)

// BUG: the end of a project user's processes kills them from a container (EndUserArgs), root
// under the host's pids, but under Docker's own AppArmor profile: on a host with AppArmor on,
// as Debian and Ubuntu ship it and --server-info expects (its apparmor_parser check), Docker
// confines every container but a privileged one with docker-default, whose signal rules let a
// container send only to its own peers ("signal (send,receive) peer=docker-default"; the host's
// processes are unconfined, from which it may only receive). So the child's kill(-1) and the
// parent's kill by pid reach none of the user's processes on such a host: every round finds
// them, and after 50 the key's removal fails with them still running, the door still theirs
// while a key is left. The end-to-end servers run Docker inside a container, with no
// AppArmor, so TestDoorKeyRemovalEndsTheUsersProcesses passes there. The fix reaches the cause
// with --security-opt apparmor=unconfined on this one container, already root on the host's
// pids; a check on a real AppArmor host (an OrbStack machine has none) should confirm it.
func TestReviewOpusEndSignalsPastAppArmor(t *testing.T) {
	t.Parallel()
	args := EndUserArgs("50001", "devopsy-shop", "/usr/local/bin/devopsy")
	unconfined := slices.Contains(args, "--privileged")
	for i, a := range args {
		if a == "--security-opt" && i+1 < len(args) && strings.HasPrefix(args[i+1], "apparmor=unconfined") {
			unconfined = true
		}
		if strings.HasPrefix(a, "--security-opt=apparmor=unconfined") {
			unconfined = true
		}
	}
	if !unconfined {
		t.Errorf("BUG: the end's container runs under docker-default's AppArmor, which signals no host process: %q", args)
	}
}
