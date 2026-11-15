package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/howto"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// recordAt writes the deploy user's record under a new host root, holding text ("" for none).
func recordAt(t *testing.T, text string) setupHost {
	t.Helper()
	h := setupHost{root: t.TempDir()}
	if text != "" {
		write(t, h.path(setup.DeployUserFile), text, 0o644)
	}
	return h
}

// Every session refuses a login the record does not name, before anything (W0009), naming both
// users and the deploy user's address on the session's host; root, a project user, the user it
// names and a server with no record pass; a record that names no one user refuses whoever logs
// in but root, since guessing would let any docker user through.
func TestCheckDeployUser(t *testing.T) {
	t.Parallel()
	me := login{uid: 1001, name: "admin"}
	no := func() bool { return false }
	yes := func() bool { return true }
	for _, c := range []struct {
		name   string
		text   string
		me     login
		pu     func() bool
		passes bool
	}{
		{"no record", "", me, no, true},
		{"its user", setup.DeployUserText("admin"), me, no, true},
		{"root", setup.DeployUserText("deploy"), login{uid: 0, name: "root"}, no, true},
		{"a project user", setup.DeployUserText("deploy"), login{uid: 1002, name: "shop"}, yes, true},
		{"another user", setup.DeployUserText("deploy"), me, no, false},
		{"a broken record", "deploy\nadmin\n", me, no, false},
		{"a broken record, root", "deploy\nadmin\n", login{uid: 0, name: "root"}, no, true},
		// Told apart before the record is read: a broken one never refuses a project user.
		{"a broken record, a project user", "deploy\nadmin\n", login{uid: 1002, name: "shop"}, yes, true},
		// A name no address takes is no one's: a command naming it would not parse.
		{"a name no address takes", "dep;loy\n", me, no, false},
		{"a name starting with -", "-oProxy\n", me, no, false},
	} {
		err := checkDeployUser(recordAt(t, c.text), c.me, c.pu, remote.Session{Host: "admin@vm1:5369"})
		if (err == nil) != c.passes {
			t.Errorf("%s: %v", c.name, err)
		}
	}

	// Another user: who logged in and who the deploy user is, and the refused command itself
	// offered again as the deploy user.
	err := checkDeployUser(recordAt(t, setup.DeployUserText("deploy")), me, no, remote.Session{Host: "admin@vm1:5369", Command: "devopsy @admin@vm1:5369: --upgrade"})
	var f *report.Failure
	if !errors.As(err, &f) || f.Msg != report.NotDeployUser.Msg() || len(f.Details) != 2 ||
		f.Details[0] != "logged in as admin, but this server's deploy user is deploy ("+setup.DeployUserFile+")" {
		t.Fatalf("another user: %#v", err)
	}
	if len(f.Remedies) != 2 || f.Remedies[0].Text != "run it as deploy:" || len(f.Remedies[0].Cmds) != 1 || f.Remedies[0].Cmds[0] != "devopsy @deploy@vm1:5369: --upgrade" || !f.Remedies[1].Hint {
		t.Errorf("another user's remedies: %#v", f.Remedies)
	}
	// A command whose address names no server: the deploy user's address, for example.
	err = checkDeployUser(recordAt(t, setup.DeployUserText("deploy")), me, no, remote.Session{Host: "admin@vm1:5369", Command: "devopsy @prod --release"})
	if !errors.As(err, &f) || len(f.Remedies[0].Cmds) != 1 || f.Remedies[0].Cmds[0] != "devopsy @deploy@vm1:5369: --server-info" {
		t.Errorf("an alias: %#v", err)
	}
	// Without the session's host, the way out in words.
	err = checkDeployUser(recordAt(t, setup.DeployUserText("deploy")), me, no, remote.Session{})
	if !errors.As(err, &f) || len(f.Remedies[0].Cmds) != 0 || !strings.Contains(f.Remedies[0].Text, "as deploy") {
		t.Errorf("no host: %#v", err)
	}
	// A record that names no one: W0010, with why and the fix as root.
	err = checkDeployUser(recordAt(t, "\n# none\n"), me, no, remote.Session{})
	if !errors.As(err, &f) || f.Msg != report.DeployUserUnread.Msg() || len(f.Details) != 1 || f.Details[0] != setup.DeployUserFile+" should hold the deploy user's name alone on a line, and it holds none" ||
		len(f.Remedies) != 1 || !strings.Contains(f.Remedies[0].Text, "as root") {
		t.Errorf("a record naming no one: %#v", err)
	}
}

// The setup's item: the record missing or in sync for the user it runs as, a record the admin
// wrote by hand for that user rewritten as devopsy writes it; one naming another user or none
// refuses, stopping every step; root records nothing with no record, and is refused one that
// names another.
func TestDeployUserFact(t *testing.T) {
	t.Parallel()
	me := login{uid: 1001, name: "deploy"}
	root := login{uid: 0, name: "root"}
	for _, c := range []struct {
		name            string
		text            string
		me              login
		fact, inSync    bool
		missing, refuse bool
	}{
		{"missing", "", me, true, false, true, false},
		{"in sync", setup.DeployUserText("deploy"), me, true, true, false, false},
		{"by hand", "deploy\n", me, true, false, false, false},
		{"another user", setup.DeployUserText("admin"), me, false, false, false, true},
		{"broken", "a b\n", me, false, false, false, true},
		{"root, none", "", root, false, false, false, false},
		{"root, another", setup.DeployUserText("deploy"), root, false, false, false, true},
	} {
		x, r := deployUserFact(recordAt(t, c.text), c.me, "", remote.Session{})
		if (x != nil) != c.fact || (r != nil) != c.refuse {
			t.Errorf("%s: fact %v, refusal %v", c.name, x, r)
			continue
		}
		if x != nil && (x.InSync != c.inSync || x.Missing != c.missing || x.Item != setup.ItemDeployUser || x.Path != setup.DeployUserFile || x.User != c.me.name) {
			t.Errorf("%s: %+v", c.name, *x)
		}
		if r != nil && (r.Item != setup.RefusalDeployUser || r.Howto == "") {
			t.Errorf("%s: %+v", c.name, *r)
		}
	}
}

// With no record, a server that shows another deploy user refuses to record this login, in the
// plan before anything: a docker member (listed, or by its primary group) with devopsy's setup
// record in its home, or a release root another user than root owns; a docker member with no
// record, one outside the group with one, this login's own record and a root of its own are
// none. The refusal names that user, the way to run it as that user on the session's host, and
// the steps to change the deploy user.
func TestDeployUserFactSeesAnotherDeployUser(t *testing.T) {
	t.Parallel()
	me := login{uid: os.Getuid(), name: "admin"}
	server := func(t *testing.T, records ...string) setupHost {
		t.Helper()
		h := setupHost{root: t.TempDir()}
		write(t, h.path("/etc/group"), "sudo:x:27:admin\ndocker:x:990:admin,ops\nstaff:x:50:\n", 0o644)
		write(t, h.path("/etc/passwd"), "root:x:0:0::/root:/bin/sh\nadmin:x:61000:61000::/home/admin:/bin/sh\nops:x:61001:61001::/home/ops:/bin/sh\n"+
			"devopsy:x:61002:990::/home/devopsy:/bin/sh\nother:x:61003:61003::/home/other:/bin/sh\n", 0o644)
		for _, u := range records {
			write(t, h.path("/home/"+u+"/.config/devopsy/"+setup.RecordFile), "{}\n", 0o600)
		}
		return h
	}
	for _, c := range []struct {
		name    string
		records []string
		other   string
	}{
		{"none", nil, ""},
		{"its own record", []string{"admin"}, ""},
		{"a member's", []string{"ops"}, "ops"},
		{"by its primary group", []string{"devopsy"}, "devopsy"},
		{"no member", []string{"other"}, ""},
	} {
		x, r := deployUserFact(server(t, c.records...), me, "", remote.Session{Host: "admin@vm1"})
		switch {
		case c.other == "" && (r != nil || x == nil || !x.Missing):
			t.Errorf("%s: %v, %v", c.name, x, r)
		// Its setup record alone: two ways out, running it as that user, or the record's removal.
		case c.other != "" && (x != nil || r == nil || r.Item != setup.RefusalDeployUser || !strings.HasPrefix(r.Message, c.other+" is set up on this server too (its setup record /home/"+c.other+"/.config/devopsy/server.yaml)") ||
			r.Command != "devopsy @"+c.other+"@vm1: --server-setup" || r.OrCommand != "sudo rm /home/"+c.other+"/.config/devopsy/server.yaml" || r.Howto != ""):
			t.Errorf("%s: %v, %+v", c.name, x, r)
		}
	}
	// A member that holds claims or environments, the login none: run it as that user, or the
	// steps, never a removal.
	h := server(t, "ops")
	write(t, h.path("/home/ops/.devopsy-uids/env:shop:prod"), "x\n", 0o644)
	if _, r := deployUserFact(h, me, "", remote.Session{Host: "admin@vm1"}); r == nil || !strings.HasPrefix(r.Message, "ops is set up as this server's deploy user already (its uid claims /home/ops/.devopsy-uids)") ||
		r.Command != "devopsy @ops@vm1: --server-setup" || r.Or != "" || r.Howto == "" {
		t.Errorf("a member holding claims: %+v", r)
	}
	// Both hold environments or claims: a bigger issue, said as such, with no command and no
	// removal, only the steps by hand.
	mine := t.TempDir()
	write(t, filepath.Join(mine, "blog", "prod", "releases", "1", "x"), "x\n", 0o644)
	write(t, filepath.Join(mine, "blog", "prod", ".lock"), "", 0o644)
	if _, r := deployUserFact(h, me, mine, remote.Session{Host: "admin@vm1"}); r == nil || !strings.HasPrefix(r.Message, "both admin and ops hold environments on this server (its uid claims /home/ops/.devopsy-uids)") ||
		r.Command != "" || r.Or != "" || r.Howto == "" {
		t.Errorf("both holding environments: %+v", r)
	}
	if ws := fixWords(*deployUserFactRefusal(t, h, me, mine)); len(ws) != 1 || ws[0].Hint || len(ws[0].Cmds) != 1 || ws[0].Cmds[0] != howto.Command(howto.DeployUser) {
		t.Errorf("both holding environments, its fixes: %+v", ws)
	}
	// A release root another user owns: its owner, by the passwd file.
	root := t.TempDir()
	if _, r := deployUserFact(server(t), login{uid: os.Getuid() + 1, name: "admin"}, root, remote.Session{}); r == nil || !strings.Contains(r.Message, "the release root "+root+" is its") || r.Command != "" {
		t.Errorf("a release root of another's: %+v", r)
	}
	if x, r := deployUserFact(server(t), me, root, remote.Session{}); r != nil || x == nil {
		t.Errorf("a release root of its own: %v, %+v", x, r)
	}
}

// --release-settings, every script's first call, refuses a login the record does not name with
// remote.SettingsRefusedExit, saying W0009, so the script stops before it changes anything.
func TestReleaseSettingsRefusesAnotherLogin(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("root is never checked")
	}
	home := t.TempDir()
	h := recordAt(t, setup.DeployUserText("someone-else"))
	cmd := exec.Command(binary, "--release-settings")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "DEVOPSY_HOME=" + home, remote.HostRootEnv + "=" + h.root, remote.HostEnv + "=vm1"}
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != remote.SettingsRefusedExit || !strings.Contains(string(out), "error: "+report.NotDeployUser.Msg()) ||
		!strings.Contains(string(out), "devopsy @someone-else@vm1: --server-info") || strings.Contains(string(out), "root=") {
		t.Errorf("another login: %v\n%s", err, out)
	}
	if err := os.Remove(filepath.Join(h.root, setup.DeployUserFile)); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(binary, "--release-settings")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + home, "DEVOPSY_HOME=" + home, remote.HostRootEnv + "=" + h.root}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("no record: %v\n%s", err, out)
	}
}

// A project user's refusal names the deploy user the server answered in the command offered,
// on the host as typed; with no name (no record, no owner of its devopsy) or one no address
// takes, it says the way in words, never a placeholder.
func TestProjectUserRefusalNamesTheDeployUser(t *testing.T) {
	t.Parallel()
	tg := &remote.Target{Host: "devopsy-shop@vm1"}
	if f := projectUserRefusal(tg, "deploy\n", "x", "--gc"); f.Msg != "devopsy-shop is a project user, and x" || len(f.Remedies[0].Cmds) != 1 || f.Remedies[0].Cmds[0] != "devopsy @deploy@vm1: --gc" {
		t.Errorf("a deploy user named: %#v", f)
	}
	for _, out := range []string{"", "\n", "a b\n"} {
		if f := projectUserRefusal(tg, out, "x", "--gc"); len(f.Remedies[0].Cmds) != 0 || strings.Contains(f.Remedies[0].Text, "<") {
			t.Errorf("%q: %#v", out, f)
		}
	}
}

// A setup refused by its root run as root (another deploy user its facts did not see, which
// appeared meanwhile or sits in a home the login cannot read) is a refusal of the facts' from
// then on: said once, with the facts' "deploy user:" lead, the metadata firewall not loaded, and
// no setup record of the login's written, not even a release build's floor note, which would
// make it a server set up by that login.
func TestServerSetupRefusedAsRootAppliesNothingElse(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	asRelease(t, s)
	me := currentLogin().name
	write(t, filepath.Join(host, "etc", "group"), "docker:x:990:"+me+",ops\n", 0o644)
	write(t, filepath.Join(host, "etc", "passwd"), loginLine(me)+"ops:x:61001:61001::/home/ops:/bin/sh\n", 0o644)
	// The other user's setup record appears once the facts were read, before the root run.
	waiting := filepath.Join(t.TempDir(), "waiting")
	outFile, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "@admin@one:", "--server-setup", "--yes")
	cmd.Dir = s.project
	cmd.Env = append(append([]string{}, s.env...), "FAKE_RUN_WAIT="+waiting, "FAKE_RUN_WAIT_ON=du_check()")
	cmd.Stdin = strings.NewReader("")
	cmd.Stdout, cmd.Stderr = outFile, outFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFile(t, waiting, "the setup's root run", startWait, func() string { return readString(outFile.Name()) })
	write(t, filepath.Join(host, "home", "ops", ".config", "devopsy", setup.RecordFile), "{}\n", 0o600)
	write(t, waiting+".go", "", 0o644)
	_ = cmd.Wait()
	out := readString(outFile.Name())
	if !strings.Contains(out, "ops is set up on this server too") {
		t.Fatalf("the root run's refusal not said (%d):\n%s", cmd.ProcessState.ExitCode(), out)
	}
	record := filepath.Join(filepath.Dir(s.root), "home", setup.RecordFile)
	if fileExists(record) || fileExists(filepath.Join(host, setup.DeployUserFile)) {
		t.Errorf("a login refused as root got a setup record (%q) or the deploy user's record:\n%s", readString(record), out)
	}
	if fileExists(filepath.Join(host, "iptables.log")) || fileExists(filepath.Join(host, remote.FirewallMark)) {
		t.Errorf("a login refused as root loaded the metadata firewall:\n%s", out)
	}
}

// A project user is told by its uid's name in the passwd file (currentLogin), never by $USER: a
// docker member whose uid the passwd file does not list, naming a project user in $USER, is
// refused W0009 as any other login. It runs only as such a uid beside a project user of the
// system's (in Linux, as root: groupadd devopsy-projects && useradd -G devopsy-projects
// devopsy-shop, then setpriv --reuid 4242 --regid 4242 --clear-groups).
func TestReleaseSettingsTellsAProjectUserByItsUID(t *testing.T) {
	t.Parallel()
	if _, err := user.LookupId(strconv.Itoa(os.Getuid())); err == nil || os.Getuid() == 0 {
		t.Skip("needs a uid the passwd file does not list")
	}
	pu := ""
	for line := range strings.SplitSeq(readString("/etc/passwd"), "\n") {
		if name, _, _ := strings.Cut(line, ":"); name != "" && inDoorGroup(name) {
			pu = name
			break
		}
	}
	if pu == "" {
		t.Skip("needs a project user of the system's, in " + remote.DoorGroup)
	}
	home := t.TempDir()
	h := recordAt(t, setup.DeployUserText("someone-else"))
	cmd := exec.Command(binary, "--release-settings")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "USER=" + pu, "HOME=" + home, "DEVOPSY_HOME=" + home, remote.HostRootEnv + "=" + h.root, remote.HostEnv + "=vm1"}
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != remote.SettingsRefusedExit || !strings.Contains(string(out), report.NotDeployUser.Msg()) {
		t.Errorf("a login of uid %d naming the project user %s in $USER passed the deploy user's check (%v):\n%s", os.Getuid(), pu, err, out)
	}
}

// On a server with no record where two docker members both have a setup record and nothing
// else, each one's setup is refused, and each refusal offers, besides running it as the other,
// the removal of the other's bare record as root: a way out from either side.
func TestDeployUserFactTwoSetupRecordsLeaveAWayOut(t *testing.T) {
	t.Parallel()
	h := setupHost{root: t.TempDir()}
	write(t, h.path("/etc/group"), "docker:x:990:admin,deploy\n", 0o644)
	write(t, h.path("/etc/passwd"), "admin:x:1000:1000::/home/admin:/bin/sh\ndeploy:x:1001:1001::/home/deploy:/bin/sh\n", 0o644)
	for _, u := range []string{"admin", "deploy"} {
		write(t, h.path("/home/"+u+"/.config/devopsy/"+setup.RecordFile), "{}\n", 0o600)
	}
	for me, other := range map[string]string{"admin": "deploy", "deploy": "admin"} {
		uid := map[string]int{"admin": 1000, "deploy": 1001}[me]
		_, r := deployUserFact(h, login{uid: uid, name: me}, "", remote.Session{Host: me + "@vm1"})
		if r == nil || !strings.HasPrefix(r.Message, other+" is set up on this server too") || r.OrCommand != "sudo rm /home/"+other+"/.config/devopsy/"+setup.RecordFile {
			t.Errorf("as %s: no way out by the removal of %s's bare record: %+v", me, other, r)
		}
	}
}

// On a server with no record where two docker users each have devopsy's setup record alone, a
// fix devopsy offers is never only a setup it refuses the same way: each refusal offers the
// other's bare record's removal as a second way out, numbered after running it as the other.
func TestDeployUserRefusalNeverOffersARefusedSetup(t *testing.T) {
	t.Parallel()
	h := setupHost{root: t.TempDir()}
	write(t, h.path("/etc/group"), "docker:x:990:admin,devopsy\n", 0o644)
	write(t, h.path("/etc/passwd"), "admin:x:1000:1000::/home/admin:/bin/sh\ndevopsy:x:1001:1001::/home/devopsy:/bin/sh\n", 0o644)
	for _, u := range []string{"admin", "devopsy"} {
		write(t, h.path("/home/"+u+"/.config/devopsy/"+setup.RecordFile), "{}\n", 0o600)
	}
	s := remote.Session{Host: "vm1"}
	_, asDevopsy := deployUserFact(h, login{uid: 1001, name: "devopsy"}, "", s)
	_, asAdmin := deployUserFact(h, login{uid: 1000, name: "admin"}, "", s)
	if asDevopsy == nil || asAdmin == nil {
		t.Fatalf("two users set up, neither refused: %+v, %+v", asDevopsy, asAdmin)
	}
	for _, c := range []struct {
		r     *setup.Refusal
		other string
	}{{asDevopsy, "admin"}, {asAdmin, "devopsy"}} {
		ws := fixWords(*c.r)
		if len(ws) != 2 || ws[0].Hint || ws[1].Hint || len(ws[1].Cmds) != 1 || ws[1].Cmds[0] != "sudo rm /home/"+c.other+"/.config/devopsy/"+setup.RecordFile {
			t.Errorf("the refusal naming %s offers no removal of its bare record: %+v", c.other, ws)
		}
	}
}

// The root run's judgment is the server's devopsy's own (DeployUserOtherFlag), as root under the
// host root it sets: a record naming another user, one it cannot read, and with none a docker
// member whose home shows devopsy set it up (its setup record, its uid claims, an environment's
// releases) or a release root of another's, each on its line, a member's own work (a .env, a
// shared/ of its own) none; a release root's owner the passwd file does not list is named
// "uid N", which the refusal never offers as a login.
func TestDeployUserOtherIsOneJudgment(t *testing.T) {
	t.Parallel()
	other := func(t *testing.T, h setupHost, uid int, root string) string {
		t.Helper()
		cmd := exec.Command(binary, remote.DeployUserOtherFlag, "admin", strconv.Itoa(uid), root)
		cmd.Env = []string{"PATH=/usr/bin:/bin", remote.HostRootEnv + "=" + h.root}
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	server := func(t *testing.T, files ...string) setupHost {
		t.Helper()
		h := setupHost{root: t.TempDir()}
		write(t, h.path("/etc/group"), "docker:x:990:admin,ops\n", 0o644)
		// A passwd line with no shell is read as one with an empty shell.
		write(t, h.path("/etc/passwd"), "admin:x:61000:61000::/home/admin:/bin/sh\nops:x:61001:61001::/home/ops\n", 0o644)
		for _, f := range files {
			write(t, h.path(f), "x\n", 0o644)
		}
		return h
	}
	me := os.Getuid()
	for _, c := range []struct {
		name  string
		files []string
		want  string
	}{
		{"none", nil, ""},
		{"a member's own work", []string{"/home/ops/work/.env", "/home/ops/notes/app/shared/todo"}, ""},
		{"its setup record", []string{"/home/ops/.config/devopsy/" + setup.RecordFile}, otherWire(otherUser{name: "ops", evidence: "its setup record /home/ops/.config/devopsy/" + setup.RecordFile, bare: "/home/ops/.config/devopsy/" + setup.RecordFile}) + "\n"},
		{"its uid claims", []string{"/home/ops/.devopsy-uids/env:shop:prod"}, otherWire(otherUser{name: "ops", evidence: "its uid claims /home/ops/.devopsy-uids"}) + "\n"},
		{"an environment", []string{"/home/ops/shop/prod/releases/20261115000000/x", "/home/ops/shop/prod/.lock"}, otherWire(otherUser{name: "ops", evidence: "its environment /home/ops/shop/prod"}) + "\n"},
		{"the record naming another", []string{setup.DeployUserFile}, ""},
	} {
		h := server(t, c.files...)
		if c.name == "the record naming another" {
			write(t, h.path(setup.DeployUserFile), setup.DeployUserText("ops"), 0o644)
			c.want = otherWire(otherUser{name: "ops", evidence: "the record " + setup.DeployUserFile + " names it"}) + "\n"
		}
		if got := other(t, h, me, ""); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	h := server(t)
	write(t, h.path(setup.DeployUserFile), "one\ntwo\n", 0o644)
	if got := other(t, h, me, ""); !strings.HasPrefix(got, remote.SetupUnread+"\t"+setup.DeployUserFile+" should hold") {
		t.Errorf("a record that cannot be read: %q", got)
	}
	// The root, this test user's, read as another's by the uid given: no name in this passwd.
	root := t.TempDir()
	got := other(t, server(t), me+1, root)
	o, ok := otherLine(got)
	r := otherDeployRefusal(o, login{uid: me + 1, name: "admin"}, remote.Session{Host: "admin@vm1"})
	if !ok || o.name != fmt.Sprint("uid ", me) || r.Command != "" || !strings.Contains(r.Message, "uid "+strconv.Itoa(me)+" is set up as this server's deploy user already (the release root "+root+" is its)") {
		t.Errorf("an owner with no name: %q, %+v", got, r)
	}
}

// --shell-host as a login is checked as every session is: refused W0009 where the record names
// another user, W0010 where it cannot be read, its fix root's own shell (--shell-host --root);
// with no record it opens for whoever logs in.
func TestShellHostChecksTheLogin(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("root is never checked")
	}
	s, _ := setupServer(t)
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, code := s.run([]string{"SHELL=/bin/sh"}, "@admin@one:", "--shell-host"); code != 0 {
		t.Fatalf("no record (%d):\n%s", code, out)
	}
	for _, c := range []struct{ name, record, code, fix string }{
		{"another login", setup.DeployUserText("someone-else"), report.NotDeployUser.Msg(), "$ devopsy @someone-else@one: --server-info"},
		{"a record that cannot be read", "one\ntwo\n", report.DeployUserUnread.Msg(), "$ devopsy @admin@one: --shell-host --root"},
	} {
		s, host := setupServer(t)
		if err := os.MkdirAll(s.root, 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, filepath.Join(host, setup.DeployUserFile), c.record, 0o644)
		out, code := s.run([]string{"SHELL=/bin/sh"}, "@admin@one:", "--shell-host")
		if code == 0 || !strings.Contains(out, "error: "+c.code) || !strings.Contains(out, c.fix+"\n") {
			t.Errorf("%s: --shell-host (%d):\n%s", c.name, code, out)
		}
	}
}

// deployUserFactRefusal is deployUserFact's refusal on h for me with root, failing without one.
func deployUserFactRefusal(t *testing.T, h setupHost, me login, root string) *setup.Refusal {
	t.Helper()
	_, r := deployUserFact(h, me, root, remote.Session{Host: me.name + "@vm1"})
	if r == nil {
		t.Fatal("no refusal")
	}
	return r
}

// The deploy-user topic's move step finds devopsy's projects by the rule devopsyEnvironment
// judges environments by, written in shell: on each tree, the topic's loop lists a project
// exactly where devopsyEnvironment finds an environment of it.
func TestDevopsyEnvironmentIsTheTopicsRule(t *testing.T) {
	t.Parallel()
	tp, _ := howto.Find(string(howto.DeployUser))
	// The loop's lines, from its first to the one that ends it.
	var lines []string
	for _, s := range tp.Steps {
		for _, c := range s.Cmds {
			if strings.HasPrefix(c, `projects=$(cd "$from"`) || len(lines) > 0 && !strings.HasSuffix(lines[len(lines)-1], "| sort -u)") {
				lines = append(lines, c)
			}
		}
	}
	loop := strings.Join(lines, "\n")
	if loop == "" {
		t.Fatal("no projects= line in the move step")
	}
	for name, files := range map[string][]string{
		"a release lock":            {"shop/prod/releases/1/code/x", "shop/prod/.lock"},
		"a release's target env":    {"shop/prod/releases/1/" + remote.TargetEnvFile},
		"an instance's environment": {"shop/eu/prod/releases/1/x", "shop/eu/prod/.lock"},
		"another tool's releases":   {"site/app/releases/1/REVISION", "site/app/current/REVISION", "site/app/shared/x"},
		"a .env alone":              {"work/app/.env", "work/app/shared/x"},
	} {
		root := t.TempDir()
		for _, f := range files {
			write(t, filepath.Join(root, f), "x\n", 0o644)
		}
		cmd := exec.Command("sh", "-c", "from="+remote.QuoteWord(root)+"\n"+loop+"\nprintf '%s' \"$projects\"")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		env := devopsyEnvironment(root)
		want := ""
		if env != "" {
			want, _, _ = strings.Cut(env, "/")
		}
		if string(out) != want {
			t.Errorf("%s: the topic lists %q, devopsyEnvironment finds %q", name, out, env)
		}
	}
}

// otherServer is a host root whose docker group holds admin, the login, and ops, with files
// under it, each holding a line.
func otherServer(t *testing.T, group string, files ...string) setupHost {
	t.Helper()
	h := setupHost{root: t.TempDir()}
	write(t, h.path("/etc/group"), group, 0o644)
	write(t, h.path("/etc/passwd"), "root:x:0:0::/root:/bin/sh\nadmin:x:1000:1000::/home/admin:/bin/sh\nops:x:1001:1001::/home/ops:/bin/sh\n", 0o644)
	for _, f := range files {
		write(t, h.path(f), "x\n", 0o644)
	}
	return h
}

// A docker member whose config names a release root outside its home (releases: root, what
// --server-setup --root writes for a volume) holds its environments and uid claims there, so
// they count as its own: it is never judged as holding a setup record alone, whose removal the
// refusal would offer, and one that never ran --server-setup is seen too.

func TestOtherDeployUserHoldsWhatItsReleaseRootHolds(t *testing.T) {
	t.Parallel()
	me := login{uid: 1000, name: "admin"}
	const config = "/home/ops/.config/devopsy/config.yaml"
	held := []string{"/srv/releases/.devopsy-uids/env:shop:prod", "/srv/releases/shop/prod/releases/20261115000000/x", "/srv/releases/shop/prod/.lock"}

	h := otherServer(t, "docker:x:990:admin,ops\n", append([]string{"/home/ops/.config/devopsy/" + setup.RecordFile}, held...)...)
	write(t, h.path(config), "releases:\n  root: /srv/releases\n", 0o644)
	o := otherDeployUser(h, me, "")
	if o.name != "ops" || o.bare != "" {
		t.Errorf("a member set up on a release root of its config's, holding environments there, is judged bare (its record's removal offered): %+v", o)
	}
	if _, r := deployUserFact(h, me, "", remote.Session{Host: "admin@vm1"}); r == nil || r.OrCommand != "" {
		t.Errorf("its refusal offers the removal of its setup record: %+v", r)
	}

	h = otherServer(t, "docker:x:990:admin,ops\n", held...)
	write(t, h.path(config), "releases:\n  root: /srv/releases\n", 0o644)
	if o := otherDeployUser(h, me, ""); o.name != "ops" {
		t.Errorf("a member that never ran the setup, its environments on a release root of its config's, is not seen: %+v", o)
	}
}

// Root is no deploy user, whatever its home holds: listed in the docker group with a setup
// record of a setup once run as root, it refuses no login's record.

func TestOtherDeployUserIsNeverRoot(t *testing.T) {
	t.Parallel()
	h := otherServer(t, "docker:x:990:admin,root\n", "/root/.config/devopsy/"+setup.RecordFile)
	if o := otherDeployUser(h, login{uid: 1000, name: "admin"}, ""); o.name != "" {
		t.Errorf("root taken for another deploy user: %+v", o)
	}
}

// An environment of devopsy's is told from another tool's releases by what only devopsy makes:
// a docker member deploying its own apps with a tool of the same layout (releases/ beside a
// current link and shared/, as Capistrano and Deployer make them) is no deploy user.

func TestHomeReleasesAreDevopsysOwn(t *testing.T) {
	t.Parallel()
	h := otherServer(t, "docker:x:990:admin,ops\n", "/home/ops/apps/shop/releases/20261115000000/REVISION", "/home/ops/apps/shop/shared/config/database.yml")
	write(t, h.path("/home/ops/apps/shop/current/REVISION"), "x\n", 0o644)
	if o := otherDeployUser(h, login{uid: 1000, name: "admin"}, ""); o.name != "" {
		t.Errorf("another tool's releases taken for an environment of devopsy's: %+v", o)
	}
}

// What the root run says of another deploy user reads back as it was said, whatever its paths
// hold: the removal offered is only ever a bare setup record's.

func TestOtherWireKeepsItsFields(t *testing.T) {
	t.Parallel()
	said := otherUser{name: "ops", evidence: "its environment /home/ops/shop\tx/prod"}
	got, ok := otherLine(otherWire(said) + "\n")
	if !ok || got != said {
		t.Errorf("read back %+v (%v), said %+v", got, ok, said)
	}
	if r := otherDeployRefusal(got, login{uid: 1000, name: "admin"}, remote.Session{Host: "admin@vm1"}); r.OrCommand != "" {
		t.Errorf("a removal offered for a path that is no setup record: %q", r.OrCommand)
	}
}

// A refusal's fix never says to run the setup as the other user with, as its command, the one
// that prints the steps to change the deploy user: those steps do the opposite.

func TestOtherDeployRefusalWithNoCommandKeepsItsFixApartFromTheSteps(t *testing.T) {
	t.Parallel()
	for _, o := range []otherUser{
		{name: "uid 1001", evidence: "the release root /srv/releases is its"},
		{name: "ops", evidence: "its uid claims /home/ops/.devopsy-uids"},
	} {
		s := remote.Session{Host: "admin@vm1"}
		if o.name == "ops" {
			s = remote.Session{}
		}
		for _, w := range fixWords(otherDeployRefusal(o, login{uid: 1000, name: "admin"}, s)) {
			if strings.HasPrefix(w.Text, "run it as") && len(w.Cmds) == 1 && w.Cmds[0] == howto.Command(howto.DeployUser) {
				t.Errorf("%s: %q offered with %q", o.name, w.Text, w.Cmds[0])
			}
		}
	}
}

// A record naming a user the server does not have (a login removed, a name mistyped) is one
// that cannot tell the deploy user: W0010, as docs/codes.md says, never W0009 naming no one.
func TestCheckDeployUserOfNoLogin(t *testing.T) {
	t.Parallel()
	h := recordAt(t, setup.DeployUserText("ghost"))
	write(t, h.path("/etc/passwd"), "admin:x:1000:1000::/home/admin:/bin/sh\n", 0o644)
	err := checkDeployUser(h, login{uid: 1000, name: "admin"}, func() bool { return false }, remote.Session{})
	var f *report.Failure
	if !errors.As(err, &f) || f.Msg != report.DeployUserUnread.Msg() || len(f.Details) != 1 || !strings.Contains(f.Details[0], "it names ghost, and this server has no such user") {
		t.Errorf("a record naming no login: %#v", err)
	}
	write(t, h.path("/etc/passwd"), "admin:x:1000:1000::/home/admin:/bin/sh\nghost:x:1001:1001::/home/ghost:/bin/sh\n", 0o644)
	if err := checkDeployUser(h, login{uid: 1000, name: "admin"}, func() bool { return false }, remote.Session{}); !errors.As(err, &f) || f.Msg != report.NotDeployUser.Msg() {
		t.Errorf("a record naming a login of the server's: %#v", err)
	}
}

// loginLine is the passwd line of the login these tests run as, named me, by its own uid: a
// host root's user database names the login as the one every judgment of it reads (loginOn),
// so a fake one lists it with the uid it has.
func loginLine(me string) string {
	uid := strconv.Itoa(os.Getuid())
	return me + ":x:" + uid + ":" + uid + "::/home/" + me + ":/bin/sh\n"
}

// The login a judgment of a host root compares its users with comes from that root's own user
// database, by the process's uid: never the host's name for it beside a passwd file that gives
// that uid another name, which the judgment reads; a root with no passwd file of its own is the
// host's.
func TestLoginOnIsTheHostRootsUser(t *testing.T) {
	t.Parallel()
	h := setupHost{root: t.TempDir()}
	if got := loginOn(h); got != currentLogin() {
		t.Errorf("no passwd file: %+v, the host's %+v", got, currentLogin())
	}
	write(t, h.path("/etc/passwd"), "someone:x:"+strconv.Itoa(os.Getuid())+":0::/home/someone:/bin/sh\n", 0o644)
	if got := loginOn(h); got.name != "someone" || got.uid != os.Getuid() {
		t.Errorf("its own passwd file: %+v", got)
	}
}
