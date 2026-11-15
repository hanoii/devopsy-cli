package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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

	err := checkDeployUser(recordAt(t, setup.DeployUserText("deploy")), me, no, remote.Session{Host: "admin@vm1:5369"})
	var f *report.Failure
	if !errors.As(err, &f) || f.Msg != report.NotDeployUser.Msg() || len(f.Details) != 1 || !strings.Contains(f.Details[0], "this login is admin") || !strings.Contains(f.Details[0], "names deploy the deploy user") {
		t.Fatalf("another user: %#v", err)
	}
	if len(f.Remedies) != 2 || len(f.Remedies[0].Cmds) != 1 || f.Remedies[0].Cmds[0] != "devopsy @deploy@vm1:5369: --server-info" || !f.Remedies[1].Hint {
		t.Errorf("another user's remedies: %#v", f.Remedies)
	}
	// Without the session's host, the way out in words.
	err = checkDeployUser(recordAt(t, setup.DeployUserText("deploy")), me, no, remote.Session{})
	if !errors.As(err, &f) || len(f.Remedies[0].Cmds) != 0 || !strings.Contains(f.Remedies[0].Text, "as deploy") {
		t.Errorf("no host: %#v", err)
	}
	// A record that names no one: W0010, with why and the fix as root.
	err = checkDeployUser(recordAt(t, "\n# none\n"), me, no, remote.Session{})
	if !errors.As(err, &f) || f.Msg != report.DeployUserUnread.Msg() || len(f.Details) != 1 || f.Details[0] != setup.DeployUserFile+" cannot tell the server's deploy user: it names no user" ||
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
		write(t, h.path("/etc/passwd"), "root:x:0:0::/root:/bin/sh\nadmin:x:1000:1000::/home/admin:/bin/sh\nops:x:1001:1001::/home/ops:/bin/sh\n"+
			"devopsy:x:1002:990::/home/devopsy:/bin/sh\nother:x:1003:1003::/home/other:/bin/sh\n", 0o644)
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
		case c.other != "" && (x != nil || r == nil || r.Item != setup.RefusalDeployUser || !strings.HasPrefix(r.Message, "devopsy was set up as "+c.other+" on this server (its setup record /home/"+c.other+"/.config/devopsy/server.yaml)") ||
			r.Command != "devopsy @"+c.other+"@vm1: --server-setup" || r.Howto == ""):
			t.Errorf("%s: %v, %+v", c.name, x, r)
		}
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
