package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/hanoii/devopsy-cli/internal/howto"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// deployUserRecord reads the server's record of its deploy user (setup.DeployUserFile, under
// h's root): the user it names and its text, "" and nil when there is none; a record that
// cannot be read or names no one user is an error.
func deployUserRecord(h setupHost) (name string, text []byte, err error) {
	text, err = os.ReadFile(h.path(setup.DeployUserFile))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, nil
	}
	if err == nil {
		if name, err = setup.ParseDeployUser(string(text), remote.LoginName); err == nil {
			return name, text, nil
		}
	}
	return "", nil, fmt.Errorf("%s cannot tell the server's deploy user: %w", setup.DeployUserFile, err)
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

// currentLogin is this process's login, its name the system's for its uid. Never user.Current:
// without cgo, a uid the passwd file does not list falls back to $USER, which the login sets
// itself, and a name it chose would pass for the deploy user's.
func currentLogin() login {
	l := login{uid: os.Getuid()}
	if u, err := user.LookupId(strconv.Itoa(l.uid)); err == nil {
		l.name = u.Username
	}
	return l
}

// checkDeployUser is the check of who a session runs as, before anything (--release-settings,
// every prelude's first call; --upgrade's, which has none; the --ssh-keys scripts' gate): nil
// for root, which is not checked, and for a project user (projectUser), told apart before the
// record is read, whose devopsy asks the door; nil with no record and for the user it names;
// else W0009, naming both users, with the address of the deploy user on the session's host
// (s.Host), in words without one. A record that cannot be read refuses with W0010: only root
// writes it, and guessing would let any docker user through.
func checkDeployUser(h setupHost, me login, projectUser func() bool, s remote.Session) error {
	if me.uid == 0 || projectUser() {
		return nil
	}
	deploy, _, err := deployUserRecord(h)
	switch {
	case err != nil:
		return &report.Failure{Msg: report.DeployUserUnread.Msg(), Details: []string{err.Error()},
			Remedies: []report.Remedy{report.Fix("correct it as root: the deploy user's name alone on a line, the file root's with mode 644")}}
	case deploy == "" || deploy == me.name:
		return nil
	}
	f := &report.Failure{Msg: report.NotDeployUser.Msg(),
		Details: []string{"this login is " + me.words() + ", and " + setup.DeployUserFile + " names " + deploy + " the deploy user: devopsy runs every release, uid claim and setup of a server as that one user"}}
	if s.Host != "" {
		f.Remedies = append(f.Remedies, report.Fix("address the server as "+deploy+", for example:", "devopsy @"+remote.AsUser(s.Host, deploy)+" --server-info"))
	} else {
		f.Remedies = append(f.Remedies, report.Fix("address the server as "+deploy))
	}
	f.Remedies = append(f.Remedies, stepsHint("to make "+me.words()+" the deploy user instead, move what is "+deploy+"'s by hand", howto.DeployUser))
	return f
}

// deployUserFact is the setup's item of the deploy user's record for a setup run as me: the
// record as a file devopsy writes whole, naming me (setup.FileFact.User); a refusal, which stops
// every step, where it names another user or cannot be read, since a setup as another user would
// make that user's release root and config; nothing as root with no record, since root is no
// deploy user to record, its sessions never checked, nor for a login with no name. With no
// record, a server that shows another deploy user (otherDeployUser, root the release root)
// refuses too, with the way to run it as that user on the session's host (s.Host).
func deployUserFact(h setupHost, me login, root string, s remote.Session) (*setup.FileFact, *setup.Refusal) {
	deploy, text, err := deployUserRecord(h)
	switch {
	case err != nil:
		return nil, &setup.Refusal{Item: setup.RefusalDeployUser, Message: err.Error(),
			Fix: "correct it as root, naming the deploy user alone on a line, the file root's with mode 644", Howto: howto.DeployUser}
	case deploy != "" && deploy != me.name:
		return nil, &setup.Refusal{Item: setup.RefusalDeployUser, Message: setup.DeployUserFile + " names " + deploy + " the server's deploy user, and this setup runs as " + me.words(),
			Fix: "run --server-setup as " + deploy + ", or make " + me.words() + " the deploy user by hand", Howto: howto.DeployUser}
	case me.uid == 0 && deploy == "", me.name == "":
		return nil, nil
	case deploy == "":
		if other, evidence := otherDeployUser(h, me, root); other != "" {
			r := otherDeployRefusal(other, evidence, me, s)
			return nil, &r
		}
	}
	return &setup.FileFact{Item: setup.ItemDeployUser, Path: setup.DeployUserFile, InSync: string(text) == setup.DeployUserText(me.name), Missing: deploy == "", User: me.name}, nil
}

// otherDeployUser is the user other than me a server with no record shows set up by devopsy,
// and what shows it: a member of the docker group (h's group and passwd files, a member listed
// or its primary group) with devopsy's setup record in its home's config
// (~/.config/devopsy/server.yaml), else the owner of the release root root, another user's but
// root's. "" for none; what this login cannot see reads as none, and the root run reads the same
// again as root, before it writes the record (remote.SetupRootScript, its "other" line).
func otherDeployUser(h setupHost, me login, root string) (user, evidence string) {
	group, _ := os.ReadFile(h.path("/etc/group"))
	passwd, _ := os.ReadFile(h.path("/etc/passwd"))
	type entry struct{ name, uid, gid, home string }
	var users []entry
	for line := range strings.SplitSeq(string(passwd), "\n") {
		if f := strings.Split(line, ":"); len(f) >= 7 {
			users = append(users, entry{f[0], f[2], f[3], f[5]})
		}
	}
	gid, members := "", []string{}
	for line := range strings.SplitSeq(string(group), "\n") {
		if f := strings.Split(line, ":"); len(f) >= 4 && f[0] == "docker" {
			gid = f[2]
			if f[3] != "" {
				members = strings.Split(f[3], ",")
			}
		}
	}
	for _, u := range users {
		if gid != "" && u.gid == gid && !slices.Contains(members, u.name) {
			members = append(members, u.name)
		}
	}
	for _, m := range members {
		i := slices.IndexFunc(users, func(u entry) bool { return u.name == m })
		if m == me.name || i < 0 || users[i].home == "" {
			continue
		}
		record := filepath.Join(users[i].home, ".config", "devopsy", setup.RecordFile)
		if _, err := os.Stat(h.path(record)); err == nil {
			return m, "its setup record " + record
		}
	}
	var st syscall.Stat_t
	if root != "" && syscall.Stat(root, &st) == nil && st.Uid != 0 && int(st.Uid) != me.uid {
		owner := fmt.Sprint("uid ", st.Uid)
		if i := slices.IndexFunc(users, func(u entry) bool { return u.uid == strconv.Itoa(int(st.Uid)) }); i >= 0 {
			owner = users[i].name
		}
		return owner, "the release root " + root + " is its"
	}
	return "", ""
}

// otherDeployRefusal is the setup's refusal to record me where the server shows other set up by
// devopsy already (evidence, otherDeployUser's or the root run's), with the way to run it as
// other on the session's host, in words without one, and the steps to change the deploy user.
func otherDeployRefusal(other, evidence string, me login, s remote.Session) setup.Refusal {
	r := setup.Refusal{Item: setup.RefusalDeployUser, Howto: howto.DeployUser,
		Message: "devopsy was set up as " + other + " on this server (" + evidence + "), and this setup runs as " + me.words() + ": it would record " + me.words() + " as the deploy user, refusing " + other,
		Fix:     "run it as " + other}
	if s.Host != "" && remote.LoginName(other) {
		r.Fix, r.Command = "run it as "+other+":", "devopsy @"+remote.AsUser(s.Host, other)+" --server-setup"
	}
	return r
}
