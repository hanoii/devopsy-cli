package main

import (
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// TestDeployUserStepsRefuseAProjectUser keeps every step of the whole server refusing a
// project user itself, with its own words, whatever called it: the session's script is not
// the only way in, and the door must never be what stops one. A step a project user runs for
// its own environments (the release's readiness check) never even asks who runs it.
func TestDeployUserStepsRefuseAProjectUser(t *testing.T) {
	t.Parallel()
	for flag, want := range map[string]string{
		remote.GCStepFlag:         "what --gc removes is the server's deploy user's, never a project user's: nothing removed",
		remote.RebootStepFlag:     "a reboot is the server's deploy user's, never a project user's: nothing rebooted",
		remote.ServerSetupFlag:    "the server's setup is its deploy user's, never a project user's: nothing changed",
		remote.ProjectUsersFlag:   "project users are the server's deploy user's, never a project user's: nothing changed",
		remote.EndProjectUserFlag: "ending a project user is the server's deploy user's, never a project user's: nothing ended",
	} {
		if err := deployUserStep(flag, func() bool { return true }); err == nil || err.Error() != want {
			t.Errorf("%s as a project user: %v", flag, err)
		}
		if err := deployUserStep(flag, func() bool { return false }); err != nil {
			t.Errorf("%s as the deploy user: %v", flag, err)
		}
	}
	for _, flag := range []string{remote.ServerReadyFlag, remote.MntReadyFlag, remote.SyncKeyFlag, "--server-info", "--status"} {
		if err := deployUserStep(flag, func() bool { t.Errorf("%s asked who runs it", flag); return true }); err != nil {
			t.Errorf("%s: %v", flag, err)
		}
	}
}
