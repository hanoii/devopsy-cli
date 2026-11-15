package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/hanoii/devopsy-cli/internal/howto"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
	"github.com/hanoii/devopsy-cli/internal/uids"
	"github.com/hanoii/devopsy-cli/internal/update"
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

// currentUser is the user this process runs as, by its uid in the system's user database, for
// whatever decides who it is (its name, its home, its ids). Never os/user's own lookups of it on
// Linux: without cgo (every release), user.Current, user.LookupId of its own uid and user.Lookup
// of the name in $USER all answer from $USER and $HOME where the passwd file does not list the
// uid (a login from LDAP or sssd), values the login sets itself, so a name it chose would pass
// for another's (the deploy user's, a project user's). The passwd file first, then getent, which
// asks the system's other sources (NSS).
func currentUser() (*user.User, error) {
	uid := strconv.Itoa(os.Getuid())
	if runtime.GOOS != "linux" {
		return user.LookupId(uid)
	}
	find := func(lines string) *user.User {
		for line := range strings.SplitSeq(lines, "\n") {
			// name:password:uid:gid:gecos:home:shell
			if f := strings.Split(line, ":"); len(f) >= 6 && f[2] == uid && f[0] != "" {
				return &user.User{Username: f[0], Uid: f[2], Gid: f[3], Name: f[4], HomeDir: f[5]}
			}
		}
		return nil
	}
	if passwd, err := os.ReadFile("/etc/passwd"); err == nil {
		if u := find(string(passwd)); u != nil {
			return u, nil
		}
	}
	if out, err := exec.Command("getent", "passwd", uid).Output(); err == nil {
		if u := find(string(out)); u != nil {
			return u, nil
		}
	}
	return nil, user.UnknownUserIdError(os.Getuid())
}

// currentLogin is this process's login (currentUser), its name "" where the system has none.
func currentLogin() login {
	l := login{uid: os.Getuid()}
	if u, err := currentUser(); err == nil {
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
		// Root's own shell is the way in: --shell-host as any other login is checked too.
		fix := report.Fix("correct it as root: the deploy user's name alone on a line, the file root's with mode 644, in root's shell (--shell-host --root on this server)")
		if c := s.OnServer("--shell-host --root"); c != "" {
			fix = report.Fix("correct it as root: the deploy user's name alone on a line, the file root's with mode 644, in root's shell:", c)
		}
		return &report.Failure{Msg: report.DeployUserUnread.Msg(), Details: []string{err.Error()}, Remedies: []report.Remedy{fix}}
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
		r := deployUnreadRefusal(err.Error())
		return nil, &r
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
// or its primary group) whose home holds devopsy's setup record in its config
// (~/.config/devopsy/server.yaml), uid claims (.devopsy-uids) or an environment's releases
// (homeReleases), a deploy user that never ran --server-setup included; else the owner of the
// release root root, another user's but root's, by name, else "uid N", no login. "" for none.
// The one judgment of it: the facts read it as the login can, and the root run asks it again
// as root before it makes the record (DeployUserOtherFlag).
func otherDeployUser(h setupHost, me login, root string) (user, evidence string) {
	group, _ := os.ReadFile(h.path("/etc/group"))
	passwd, _ := os.ReadFile(h.path("/etc/passwd"))
	type entry struct{ name, uid, gid, home string }
	var users []entry
	for line := range strings.SplitSeq(string(passwd), "\n") {
		// name:password:uid:gid:gecos:home:shell, a shell or more left out read as none.
		if f := strings.Split(line, ":"); len(f) >= 6 && f[0] != "" {
			users = append(users, entry{f[0], f[2], f[3], f[5]})
		}
	}
	gid, members := "", []string{}
	for line := range strings.SplitSeq(string(group), "\n") {
		if f := strings.Split(line, ":"); len(f) >= 4 && f[0] == "docker" {
			gid = f[2]
			for m := range strings.SplitSeq(f[3], ",") {
				if m = strings.TrimSpace(m); m != "" {
					members = append(members, m)
				}
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
		if m == me.name || i < 0 || users[i].home == "" || users[i].uid == strconv.Itoa(me.uid) {
			continue
		}
		home := users[i].home
		record := filepath.Join(home, ".config", "devopsy", setup.RecordFile)
		claims := filepath.Join(home, uids.DirName)
		switch {
		case exists(h.path(record)):
			return m, "its setup record " + record
		case exists(h.path(claims)):
			return m, "its uid claims " + claims
		}
		if env := homeReleases(h.path(home)); env != "" {
			return m, "its environment " + filepath.Join(home, env)
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

// homeReleases is an environment devopsy released under home, as its release root, relative
// to it, "" for none: <project>[/<instance>]/<environment> holding releases/ beside its current
// link, its release lock or its shared/, what only devopsy makes together.
func homeReleases(home string) string {
	for _, pattern := range []string{"*/*/releases", "*/*/*/releases"} {
		found, _ := filepath.Glob(filepath.Join(home, pattern))
		for _, rel := range found {
			env := filepath.Dir(rel)
			if fi, err := os.Stat(rel); err != nil || !fi.IsDir() {
				continue
			}
			for _, beside := range []string{"current", ".lock", "shared"} {
				if _, err := os.Lstat(filepath.Join(env, beside)); err == nil {
					r, _ := filepath.Rel(home, env)
					return r
				}
			}
		}
	}
	return ""
}

// exists says whether path is there, a link included.
func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// otherWire is the line --deploy-user-other prints, and otherLine reads back: other, the user
// and what shows it, tab separated, a user with no name ("uid N") holding a space.
func otherWire(user, evidence string) string {
	return remote.SetupOther + "\t" + user + "\t" + evidence
}

// runDeployUserOther implements the hidden remote.DeployUserOtherFlag <login> <uid> <root>, run
// by the setup's root run as root, under remote.HostRootEnv as it sets it, before it makes the
// deploy user's record: the record naming another user than login (its "other" line, the record
// as what shows it), one that cannot be read (its "unread" line), else otherDeployUser's "other"
// line where there is none, nothing when login may be recorded.
func runDeployUserOther(args []string) int {
	if len(args) != 3 {
		return fail("usage: devopsy " + remote.DeployUserOtherFlag + " <login> <uid> <root>")
	}
	uid, err := strconv.Atoi(args[1])
	if err != nil {
		return fail("not a uid: " + args[1])
	}
	me, h := login{uid: uid, name: args[0]}, setupHost{root: os.Getenv(remote.HostRootEnv)}
	deploy, _, err := deployUserRecord(h)
	switch {
	case err != nil:
		fmt.Println(remote.SetupUnread + "\t" + err.Error())
	case deploy != "" && deploy != me.name:
		fmt.Println(otherWire(deploy, "the record "+setup.DeployUserFile+" names it"))
	case deploy == "":
		if other, evidence := otherDeployUser(h, me, args[2]); other != "" {
			fmt.Println(otherWire(other, evidence))
		}
	}
	return 0
}

// deployUserHost is where the deploy user's check reads the record (checkDeployUser): the host's
// own root in a release build, remote.HostRootEnv's only in a development build, the tests': a
// login that set it would read a record of its own making and pass.
func deployUserHost() setupHost {
	if update.IsRelease(version) {
		return setupHost{}
	}
	return setupHost{root: os.Getenv(remote.HostRootEnv)}
}

// runDeployUserCheck implements the hidden remote.DeployUserCheckFlag: the deploy user's check
// of this login alone (checkDeployUser), for the scripts that run no prelude (the --ssh-keys
// scripts), which neither a config nor a release root of the login's may hold up.
func runDeployUserCheck() int {
	if err := checkDeployUser(deployUserHost(), currentLogin(), asProjectUser, remote.SessionFrom(os.Getenv)); err != nil {
		failErr(err)
		return remote.SettingsRefusedExit
	}
	return 0
}

// deployUnreadRefusal is the setup's refusal of a deploy user's record that cannot be read, why.
func deployUnreadRefusal(why string) setup.Refusal {
	return setup.Refusal{Item: setup.RefusalDeployUser, Message: why,
		Fix: "correct it as root, naming the deploy user alone on a line, the file root's with mode 644", Howto: howto.DeployUser}
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
