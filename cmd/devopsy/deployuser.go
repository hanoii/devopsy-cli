package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"

	"github.com/hanoii/devopsy-cli/internal/howto"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// deployUserRecord is the user the server's record names (setup.DeployUserFile, under h's
// root), "" when there is none; a record that cannot be read or names no one user is an error.
func deployUserRecord(h setupHost) (string, error) {
	text, err := os.ReadFile(h.path(setup.DeployUserFile))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err == nil {
		var name string
		if name, err = setup.ParseDeployUser(string(text)); err == nil {
			return name, nil
		}
	}
	return "", fmt.Errorf("%s cannot tell the server's deploy user: %w", setup.DeployUserFile, err)
}

// login is who this devopsy runs as: its uid, and its name ("" when the system has none).
type login struct {
	uid  int
	name string
}

// words name l in a sentence: its name, else its uid.
func (l login) words() string {
	if l.name == "" {
		return fmt.Sprint("uid ", l.uid)
	}
	return l.name
}

// currentLogin is this process's login.
func currentLogin() login {
	l := login{uid: os.Getuid()}
	if u, err := user.Current(); err == nil {
		l.name = u.Username
	}
	return l
}

// checkDeployUser is every session's check of who it runs as (--release-settings, the
// prelude's first call): nil for root, which is not checked, with no record, for the user the
// record names, and for a project user (projectUser), whose devopsy asks the door; else W0009,
// naming both users, with the address of the deploy user on the session's host (s.Host), in
// words without one. A record that cannot be read refuses too: only root writes it, and
// guessing would let any docker user through.
func checkDeployUser(h setupHost, me login, projectUser func() bool, s remote.Session) error {
	if me.uid == 0 {
		return nil
	}
	deploy, err := deployUserRecord(h)
	switch {
	case err != nil:
		return &report.Failure{Msg: err.Error(), Remedies: []report.Remedy{stepsFix("correct it as root, naming the deploy user", howto.DeployUser)}}
	case deploy == "" || deploy == me.name || projectUser():
		return nil
	}
	who := me.words()
	f := &report.Failure{Msg: report.NotDeployUser.Msg(),
		Details: []string{"this login is " + who + ", and " + setup.DeployUserFile + " names " + deploy + " the deploy user: devopsy runs every release, uid claim and setup of a server as that one user"}}
	if s.Host != "" {
		f.Remedies = append(f.Remedies, report.Fix("address the server as "+deploy+", for example:", "devopsy @"+remote.AsUser(s.Host, deploy)+" --server-info"))
	} else {
		f.Remedies = append(f.Remedies, report.Fix("address the server as "+deploy))
	}
	f.Remedies = append(f.Remedies, stepsHint("to make "+who+" the deploy user instead, move what is "+deploy+"'s by hand", howto.DeployUser))
	return f
}

// deployUserFact is the setup's item of the deploy user's record for a setup run as me: the
// record as a file devopsy writes whole, naming me; a refusal, which stops every step, where it
// names another user or cannot be read, since a setup as another user would make that user's
// release root and config; nothing as root with no record, since root is no deploy user to
// record, its sessions never checked.
func deployUserFact(h setupHost, me login) (*setup.FileFact, *setup.Refusal) {
	deploy, err := deployUserRecord(h)
	who := me.words()
	switch {
	case err != nil:
		return nil, &setup.Refusal{Item: setup.RefusalDeployUser, Message: err.Error(), Fix: "correct it as root, naming the deploy user", Howto: howto.DeployUser}
	case deploy != "" && deploy != me.name:
		return nil, &setup.Refusal{Item: setup.RefusalDeployUser, Message: setup.DeployUserFile + " names " + deploy + " the server's deploy user, and this setup runs as " + who,
			Fix: "run --server-setup as " + deploy + ", or make " + who + " the deploy user by hand", Howto: howto.DeployUser}
	case me.uid == 0 && deploy == "", me.name == "":
		return nil, nil
	}
	have, _ := os.ReadFile(h.path(setup.DeployUserFile))
	return &setup.FileFact{Item: setup.ItemDeployUser, Path: setup.DeployUserFile, InSync: string(have) == setup.DeployUserText(me.name), Missing: deploy == ""}, nil
}
