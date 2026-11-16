package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
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
	if err != nil {
		return "", nil, fmt.Errorf("%s cannot be read: %w", setup.DeployUserFile, err)
	}
	if name, err = setup.ParseDeployUser(string(text), remote.LoginName); err == nil && !loginKnown(h, name) {
		err = errors.New("names " + name + ", but this server has no such user")
	}
	if err != nil {
		return "", nil, fmt.Errorf("%s %w; it should hold only the deploy user's name, on one line", setup.DeployUserFile, err)
	}
	return name, text, nil
}

// deployRecordWrite is what root writes in a record that cannot be read, wherever its fix is
// said (W0010, the setup's refusal).
const deployRecordWrite = "write the deploy user's name in " + setup.DeployUserFile + ", on a line by itself (owner root, mode 644)"

// loginKnown says whether name is a user of h's (lookupLogin), always where h is a host root of
// the tests' with no passwd file of its own, which knows no user to judge by.
func loginKnown(h setupHost, name string) bool {
	if _, err := os.Stat(h.path("/etc/passwd")); err != nil && h.root != "" {
		return true
	}
	return lookupLogin(h, name, 0) != nil
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
// for another's (the deploy user's, a project user's).
func currentUser() (*user.User, error) {
	uid := strconv.Itoa(os.Getuid())
	if runtime.GOOS != "linux" {
		return user.LookupId(uid)
	}
	if u := lookupLogin(setupHost{}, uid, 2); u != nil {
		return u, nil
	}
	return nil, user.UnknownUserIdError(os.Getuid())
}

// lookupLogin is the user whose passwd field (0 its name, 2 its uid) is key: in h's passwd file
// first, then, on the host itself (h's root ""), through getent, which asks the system's other
// sources (NSS); nil for none.
func lookupLogin(h setupHost, key string, field int) *user.User {
	find := func(lines string) *user.User {
		for line := range strings.SplitSeq(lines, "\n") {
			// name:password:uid:gid:gecos:home:shell
			if f := strings.Split(line, ":"); len(f) >= 6 && f[field] == key && f[0] != "" {
				return &user.User{Username: f[0], Uid: f[2], Gid: f[3], Name: f[4], HomeDir: f[5]}
			}
		}
		return nil
	}
	if passwd, err := os.ReadFile(h.path("/etc/passwd")); err == nil {
		if u := find(string(passwd)); u != nil {
			return u
		}
	}
	if h.root == "" {
		if out, err := exec.Command("getent", "passwd", key).Output(); err == nil {
			return find(string(out))
		}
	}
	return nil
}

// loginOn is this process's login as the host root h's user database names it, the one every
// judgment of h reads (its passwd file; the host's own where h has none, currentLogin): its uid
// and that file's name for it, so the login and the users a judgment compares it with never
// come from two databases.
func loginOn(h setupHost) login {
	if h.root == "" || !exists(h.path("/etc/passwd")) {
		return currentLogin()
	}
	l := login{uid: os.Getuid()}
	if u := lookupLogin(h, strconv.Itoa(l.uid), 2); u != nil {
		l.name = u.Username
	}
	return l
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
		fix := report.Fix("open root's shell (--shell-host --root on this server) and " + deployRecordWrite)
		if c := s.OnServer("--shell-host --root"); c != "" {
			fix = report.Fix("open root's shell and "+deployRecordWrite+":", c)
		}
		return &report.Failure{Msg: report.DeployUserUnread.Msg(), Details: []string{err.Error()}, Remedies: []report.Remedy{fix}}
	case deploy == "" || deploy == me.name:
		return nil
	}
	f := &report.Failure{Msg: report.NotDeployUser.Msg(), Details: []string{
		"logged in as " + me.words() + ", but this server's deploy user is " + deploy + " (" + setup.DeployUserFile + ")",
		"devopsy works on a server as one user only, so that every release, uid claim and setup there stays in one place"}}
	switch c := s.CommandAs(deploy); {
	case c != "":
		f.Remedies = append(f.Remedies, report.Fix("run it as "+deploy+":", c))
	case s.Host != "":
		f.Remedies = append(f.Remedies, report.Fix("address the server as "+deploy+", for example:", "devopsy @"+remote.AsUser(s.Host, deploy)+" --server-info"))
	default:
		f.Remedies = append(f.Remedies, report.Fix("address the server as "+deploy))
	}
	f.Remedies = append(f.Remedies, followHint("to make "+me.words()+" the deploy user instead", howto.DeployUser))
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
		return nil, &setup.Refusal{Item: setup.RefusalDeployUser, Message: "this server's deploy user is " + deploy + " (" + setup.DeployUserFile + "), and this setup runs as " + me.words(),
			Info: "this server's deploy user is " + deploy + " (" + setup.DeployUserFile + "): a --server-setup as " + me.words() + " is refused",
			Fix:  "run --server-setup as " + deploy + ", or make " + me.words() + " the deploy user by hand", Howto: howto.DeployUser}
	case me.uid == 0 && deploy == "", me.name == "":
		return nil, nil
	case deploy == "":
		o, unseen := otherDeployUser(h, me, root)
		// What this login cannot read is asked as root where the caller can (rootRead), the
		// same judgment's line read back: never "would record" on a home it could not see.
		if o.name == "" && len(unseen) > 0 && h.rootRead != nil {
			out, err := h.rootRead(me, root)
			if err != nil {
				r := unseenRefusal(unseen, me, err)
				return nil, &r
			}
			if r := rootDeployRefusal(h, out, s); r != nil {
				return nil, r
			}
		}
		if o.name != "" {
			r := otherDeployRefusal(o, me, s)
			return nil, &r
		}
	}
	return &setup.FileFact{Item: setup.ItemDeployUser, Path: setup.DeployUserFile, InSync: string(text) == setup.DeployUserText(me.name), Missing: deploy == "", User: me.name}, nil
}

// otherDeployUser is the user other than me a server with no record shows set up by devopsy,
// and what shows it, by the strongest evidence first: a member of the docker group (h's group
// and passwd files, a member listed or its primary group), root never, holding uid claims or an
// environment (devopsyEnvironment) under its release root (its config's releases: root, else
// its home), a deploy user that never ran --server-setup included; then one whose evidence is
// weak, its setup record alone in its home's config (~/.config/devopsy/server.yaml, bare), or
// the server's devopsy its own (binaryOwner, binary), a member too; else the owner of the
// release root root, another user's but root's, by name, else "uid N", no login. Its zero value
// for none. unseen are the members whose home, config or release root this process may not read
// (a home of mode 700, as Debian makes them), which read as no evidence: where nothing is found
// and some are unseen, the facts ask the same as root (rootRead). The one judgment of it: the
// facts read it as the login can, and the root run asks it again as root before it makes the
// record (DeployUserOtherFlag).
func otherDeployUser(h setupHost, me login, root string) (o otherUser, unseen []string) {
	users, members := dockerMembers(h)
	var weak otherUser
	for _, m := range members {
		i := slices.IndexFunc(users, func(u passwdEntry) bool { return u.name == m })
		if m == me.name || i < 0 || users[i].home == "" || users[i].uid == strconv.Itoa(me.uid) || users[i].uid == "0" {
			continue
		}
		home := users[i].home
		config := filepath.Join(home, ".config", "devopsy", remote.ConfigFile)
		if denied(h.path(home)) || denied(h.path(config)) {
			unseen = append(unseen, m)
			continue
		}
		record := filepath.Join(home, ".config", "devopsy", setup.RecordFile)
		theirs := remote.ReleaseRootIn(h.path(config), home)
		if theirs != home && denied(h.path(theirs)) {
			unseen = append(unseen, m)
			continue
		}
		claims := filepath.Join(theirs, uids.DirName)
		env := devopsyEnvironment(h.path(theirs))
		switch {
		case exists(h.path(claims)):
			return otherUser{name: m, evidence: "its uid claims " + claims, both: holdsEnvironments(root)}, unseen
		case env != "":
			return otherUser{name: m, evidence: "its environment " + filepath.Join(theirs, env), both: holdsEnvironments(root)}, unseen
		case exists(h.path(record)) && weak.name == "":
			weak = otherUser{name: m, evidence: "its setup record " + record, bare: record}
		}
	}
	if weak.name != "" {
		return weak, unseen
	}
	if path, uid, ok := h.binaryOwner(); ok && uid != 0 && uid != me.uid {
		if i := slices.IndexFunc(users, func(u passwdEntry) bool { return u.uid == strconv.Itoa(uid) }); i >= 0 && slices.Contains(members, users[i].name) {
			return otherUser{name: users[i].name, evidence: path, binary: filepath.Dir(path)}, unseen
		}
	}
	var st syscall.Stat_t
	if root != "" && syscall.Stat(root, &st) == nil && st.Uid != 0 && int(st.Uid) != me.uid {
		owner := fmt.Sprint("uid ", st.Uid)
		if i := slices.IndexFunc(users, func(u passwdEntry) bool { return u.uid == strconv.Itoa(int(st.Uid)) }); i >= 0 {
			owner = users[i].name
		}
		return otherUser{name: owner, evidence: "it owns the release root " + root}, unseen
	}
	return otherUser{}, unseen
}

// denied says whether reading path is refused to this process for its permissions (a home of
// mode 700, as Debian makes them), which os's own lookups and filepath.Glob read as nothing
// there.
func denied(path string) bool {
	f, err := os.Open(path)
	if err == nil {
		f.Close()
	}
	return errors.Is(err, fs.ErrPermission)
}

// binaryOwner is the server's devopsy, resolved (selfPath), and the uid it belongs to: who
// upgrades it, so its owner is the deploy user where devopsy-server installed it. Read under h's
// root, as every file the judgment reads, so its owner and the users it is compared with come
// from one host (a test's host root holds no binary). h.binary in its place where set, the
// tests'.
func (h setupHost) binaryOwner() (path string, uid int, ok bool) {
	if h.binary != nil {
		return h.binary()
	}
	path = selfPath()
	_, uid, ok = binaryOwnerOf(h.path(path))
	return path, uid, ok
}

// binaryOwnerOf is path and the uid it belongs to, ok false where it has none (no path, gone):
// the owner of the server's devopsy, which otherDeployUser weighs and the door's client takes
// for the deploy user (deployUser).
func binaryOwnerOf(path string) (string, int, bool) {
	if path == "" {
		return "", 0, false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return "", 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", 0, false
	}
	return path, int(st.Uid), true
}

// holding is what o holds, said after its name ("ops has ..."): an environment by its path, uid
// claims by where they are.
func (o otherUser) holding() string {
	if p, ok := strings.CutPrefix(o.evidence, "its environment "); ok {
		return p
	}
	if p, ok := strings.CutPrefix(o.evidence, "its uid claims "); ok {
		return "uid claims in " + p
	}
	return o.evidence
}

// otherUser is another deploy user a server shows (otherDeployUser): its name, what shows it,
// its setup record where that is all (bare, one it may hold by mistake), the directory of the
// server's devopsy where owning it is all (binary, evidence that binary's path), and both where
// it holds environments or claims and so does the login's own release root, a bigger issue
// than a choice.
type otherUser struct {
	name, evidence, bare, binary string
	both                         bool
}

// holdsEnvironments says whether the release root root holds devopsy's environments or uid
// claims, the login's own (otherDeployUser), by what only devopsy makes (devopsyEnvironment):
// unlike rootHolds, which errs toward held so that no root is moved under releases, a home's own
// .env is none here, so it never turns a choice into the error of two users with environments.
func holdsEnvironments(root string) bool {
	return root != "" && (exists(filepath.Join(root, uids.DirName)) || devopsyEnvironment(root) != "")
}

// passwdEntry is a passwd file's line, as otherDeployUser reads it.
type passwdEntry struct{ name, uid, gid, home string }

// dockerMembers are h's passwd file's users and the docker group's members by name: those its
// group line lists, then those whose primary group it is.
func dockerMembers(h setupHost) ([]passwdEntry, []string) {
	group, _ := os.ReadFile(h.path("/etc/group"))
	passwd, _ := os.ReadFile(h.path("/etc/passwd"))
	var users []passwdEntry
	for line := range strings.SplitSeq(string(passwd), "\n") {
		// name:password:uid:gid:gecos:home:shell, a shell or more left out read as none.
		if f := strings.Split(line, ":"); len(f) >= 6 && f[0] != "" {
			users = append(users, passwdEntry{f[0], f[2], f[3], f[5]})
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
	return users, members
}

// devopsyEnvironment is an environment devopsy released under the release root root, relative
// to it, "" for none: <project>[/<instance>]/<environment> holding releases/ beside its release
// lock (.lock), or a release holding its devopsy-target.env, what only devopsy makes. Another
// tool's layout of releases/ beside current and shared/ (Capistrano's, Deployer's) is none. The
// deploy-user topic's move step tests the same in shell (TestDevopsyEnvironmentIsTheTopicsRule).
func devopsyEnvironment(root string) string {
	for _, pattern := range []string{"*/*/releases", "*/*/*/releases"} {
		found, _ := filepath.Glob(filepath.Join(root, pattern))
		for _, rel := range found {
			env := filepath.Dir(rel)
			if fi, err := os.Stat(rel); err != nil || !fi.IsDir() {
				continue
			}
			targets, _ := filepath.Glob(filepath.Join(rel, "*", remote.TargetEnvFile))
			if _, err := os.Lstat(filepath.Join(env, ".lock")); err == nil || len(targets) > 0 {
				r, _ := filepath.Rel(root, env)
				return r
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

// otherWire is the line --deploy-user-other prints, and otherLine reads back: other, then the
// user, what shows it, its bare setup record and the directory of the devopsy it owns ("" for
// none), each quoted as Go quotes a string (strconv.Quote), so a tab or a newline in a path never
// shifts a field, and both (1 or ""), tab separated.
func otherWire(o otherUser) string {
	both := ""
	if o.both {
		both = "1"
	}
	return remote.SetupOther + "\t" + strconv.Quote(o.name) + "\t" + strconv.Quote(o.evidence) + "\t" + strconv.Quote(o.bare) + "\t" + strconv.Quote(o.binary) + "\t" + both
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
		fmt.Println(otherWire(otherUser{name: deploy, evidence: "the record " + setup.DeployUserFile + " names it"}))
	case deploy == "":
		if o, _ := otherDeployUser(h, me, args[2]); o.name != "" {
			fmt.Println(otherWire(o))
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
	if err := checkDeployUser(deployUserHost(), loginOn(deployUserHost()), asProjectUser, remote.SessionFrom(os.Getenv)); err != nil {
		failErr(err)
		return remote.SettingsRefusedExit
	}
	return 0
}

// deployUnreadRefusal is the setup's refusal of a deploy user's record that cannot be read, why.
func deployUnreadRefusal(why string) setup.Refusal {
	return setup.Refusal{Item: setup.RefusalDeployUser, Message: why,
		Fix: "as root, " + deployRecordWrite, Howto: howto.DeployUser}
}

// unseenRefusal is the setup's refusal where what the docker members unseen keep could not be
// read as me (otherDeployUser), nor as root (rootRead, err): whether one of them is the deploy
// user already is not known, so nothing is recorded.
func unseenRefusal(unseen []string, me login, err error) setup.Refusal {
	whose, it := "its home", "it"
	if len(unseen) > 1 {
		whose, it = "their homes", "them"
	}
	r := setup.Refusal{Item: setup.RefusalDeployUser,
		Message: "devopsy cannot tell whether " + report.JoinOr(unseen) + " is already this server's deploy user: " + whose + " cannot be read as " + me.words() + ", nor as root (" + err.Error() + ")",
		Fix:     "run --server-setup again once root can read " + it + ", or run it as the user who should be the deploy user"}
	if errors.Is(err, errNoUtilImage) {
		r.Fix = "run --server-setup, which builds devopsy's util image and reads those homes as root"
	}
	return r
}

// wouldRecord is what a setup as who would do where the server shows other set up, as
// --server-info says it to a reader who runs no setup (setup.Refusal.Info).
func wouldRecord(who, other string) string {
	return "a --server-setup as " + who + " would record " + who + " and lock " + other + " out"
}

// rootReadOther is a setupHost's rootRead for h: the hidden DeployUserOtherFlag run as root on
// the host, read-only and with no network, through a Docker that is this host's (hostRun.Here),
// its line as the root run reads it. build readies the util image first, as the setup's first
// facts do; without it the image must be there already (runIfReady), as --server-info and an
// apply read, which then say it unread.
func rootReadOther(h setupHost, build bool) func(me login, root string) (string, error) {
	return func(me login, root string) (string, error) {
		ctx := context.Background()
		if build {
			if !dockerHere(dockerInfoWith(ctx, "docker")) {
				return "", errNotThisHost
			}
			if err := ensureUtilImageSaying(ctx, "building devopsy's util image, to read the other docker users' homes as root", "docker"); err != nil {
				return "", err
			}
		}
		var out bytes.Buffer
		run := hostRun{Here: true, Script: remote.HostRootEnv + `="$1" exec "$2" ` + remote.DeployUserOtherFlag + ` "$3" "$4" "$5"`,
			Args: []string{h.root, selfPath(), me.name, strconv.Itoa(me.uid), root}}
		err := run.runIfReady(ctx, &out, func(err error) error { return fmt.Errorf("its run as root: %w", err) })
		return out.String(), err
	}
}

// otherDeployRefusal is the setup's refusal to record me where the server shows o set up by
// devopsy already (otherDeployUser's, or the root run's line), with the way to run it as o on
// the session's host, in words without one, and the steps to change the deploy user. Where all
// that shows o is its setup record (bare), two docker users each set up and unrecorded would
// refuse toward each other, so the second way out is that record's removal, as root, where o was
// set up by mistake. Where o and the login both hold environments or claims (both), no command
// settles it: only the steps, by hand.
func otherDeployRefusal(o otherUser, me login, s remote.Session) setup.Refusal {
	who := me.words()
	r := setup.Refusal{Item: setup.RefusalDeployUser, Howto: howto.DeployUser, HowtoAside: true,
		Message: o.name + " is set up as this server's deploy user already (" + o.evidence + "): recording " + who + " instead would lock " + o.name + " out",
		Info:    o.name + " is set up as this server's deploy user already (" + o.evidence + "): " + wouldRecord(who, o.name)}
	login := remote.LoginName(o.name)
	switch {
	case o.both:
		r.Message = who + " and " + o.name + " both have environments on this server (" + o.name + " has " + o.holding() + "): one of them has to take over the other's before either can be recorded as the deploy user"
		r.Fix, r.Command, r.Howto = "decide which of the two is the deploy user, and move the other's environments to it by hand, with the steps this prints:", howto.Command(howto.DeployUser), ""
		return r
	case !login:
		// An owner with no name in the passwd file: no login to run it as.
		r.Fix = "find which login " + o.name + " is, and run --server-setup as that user"
	case s.Host == "":
		r.Fix = "run --server-setup as " + o.name
	default:
		r.Fix, r.Command = "run it as "+o.name+":", "devopsy @"+remote.AsUser(s.Host, o.name)+" --server-setup"
	}
	switch {
	case o.bare != "":
		r.Howto = ""
		r.Message = o.name + " is set up on this server too (" + o.evidence + "): recording " + who + " as the deploy user would lock " + o.name + " out"
		r.Info = o.name + " is set up on this server too (" + o.evidence + "): " + wouldRecord(who, o.name)
		r.Or, r.OrCommand = "if "+o.name+" was set up by mistake and holds no environments, remove its setup record as root:", "sudo rm "+remote.QuoteWord(o.bare)
	case o.binary != "":
		// The devopsy that upgrades itself as its owner: weak as a bare record is, so the way
		// out is the binary given over, never an error of two users.
		r.Howto = ""
		r.Message = "the server's devopsy belongs to " + o.name + " (" + o.evidence + "), and this setup runs as " + who + ": recording " + who + " would lock " + o.name + " out"
		r.Info = "the server's devopsy belongs to " + o.name + " (" + o.evidence + "): " + wouldRecord(who, o.name)
		r.Or, r.OrCommand = "if "+who+" should be the deploy user, make it the owner of the server's devopsy, as root:", "sudo chown -R "+remote.QuoteWord(who+":")+" "+remote.QuoteWord(o.binary)
		if !remote.LoginName(who) {
			r.Or, r.OrCommand = "if "+who+" should be the deploy user, make it the owner of the server's devopsy ("+o.binary+"), as root", ""
		}
	}
	return r
}
