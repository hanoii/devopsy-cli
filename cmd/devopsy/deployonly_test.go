package main

import (
	"errors"
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

// A step of the whole server is a project user's only for a login in the project users' group
// that the server's record does not name its deploy user: the deploy user in the group by
// mistake is still the deploy user, refused by the step in its own words; with no record, or
// one that does not read, the group decides; a login outside it is never asked about.
func TestStepAsProjectUserSparesTheRecordedDeployUser(t *testing.T) {
	t.Parallel()
	me := login{uid: 61001, name: "devopsy"}
	record := func(name string, err error) func() (string, error) {
		return func() (string, error) { return name, err }
	}
	for _, c := range []struct {
		inGroup bool
		record  func() (string, error)
		want    bool
	}{
		{true, record("devopsy", nil), false},
		{true, record("admin", nil), true},
		{true, record("", nil), true},
		{true, record("", errors.New("cannot be read")), true},
		{false, func() (string, error) { t.Error("the record read for a login outside the group"); return "", nil }, false},
	} {
		if got := stepAsProjectUser(func() bool { return c.inGroup }, me, c.record); got != c.want {
			name, err := "", error(nil)
			if c.inGroup {
				name, err = c.record()
			}
			t.Errorf("in the group %v, record %q (%v): %v, want %v", c.inGroup, name, err, got, c.want)
		}
	}
}
