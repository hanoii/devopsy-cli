package main

import (
	"errors"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// deployUserSteps are the hidden steps that act on the whole server, its deploy user's alone,
// each with the words of its refusal of a project user: whose it is, and what was not done.
var deployUserSteps = map[string][2]string{
	remote.GCStepFlag:         {"what --gc removes is the server's deploy user's", "nothing removed"},
	remote.RebootStepFlag:     {"a reboot is the server's deploy user's", "nothing rebooted"},
	remote.ServerSetupFlag:    {"the server's setup is its deploy user's", "nothing changed"},
	remote.ProjectUsersFlag:   {"project users are the server's deploy user's", "nothing changed"},
	remote.EndProjectUserFlag: {"ending a project user is the server's deploy user's", "nothing ended"},
	builderIdleFlag:           {"stopping a project user's builder is the server's deploy user's", "nothing stopped"},
}

// deployUserStep refuses flag's step to a project user (projectUser, asked only for one of
// deployUserSteps), before it reads or runs anything: its session's script refuses one first
// (askedAsProjectUser), but a call to the step itself goes through no script, and a project
// user's docker is the door, whose refusal of a root run or of Docker's reads must never be
// what stops a step of the whole server.
func deployUserStep(flag string, projectUser func() bool) error {
	w, ok := deployUserSteps[flag]
	if !ok || !projectUser() {
		return nil
	}
	return errors.New(w[0] + ", never a project user's: " + w[1])
}

// stepAsProjectUser says whether a step of the whole server runs as a project user: a login in
// the project users' group (projectUser) that is not the server's deploy user, by its record
// (record, deployUserRecord's name). The group can hold the deploy user by mistake, which the
// steps refuse in their own words, with the fix (--project-users: sshd then ignores its keys);
// a record that does not read, or none, leaves the group to decide.
func stepAsProjectUser(projectUser func() bool, me login, record func() (string, error)) bool {
	if !projectUser() {
		return false
	}
	deploy, err := record()
	return err != nil || deploy == "" || deploy != me.name
}

// asProjectUserStep is stepAsProjectUser of this login, on this host's record.
func asProjectUserStep() bool {
	h := deployUserHost()
	return stepAsProjectUser(asProjectUser, loginOn(h), func() (string, error) {
		name, _, err := deployUserRecord(h)
		return name, err
	})
}
