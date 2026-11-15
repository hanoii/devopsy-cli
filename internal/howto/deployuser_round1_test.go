package howto

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// Bug (deploy-user review round 1): the deploy-user topic's record step writes the file with
// root's umask, and a hardened root's (027, 077) leaves it unreadable to the new deploy user,
// whose every session then refuses itself ("cannot tell the server's deploy user: permission
// denied") before the --server-setup of the next step could set its mode back.
func TestDeployUserRecordStepLeavesItReadable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	record := filepath.Join(dir, "deploy-user")
	s := stepWith(t, DeployUser, "/etc/devopsy/deploy-user")
	out := pasted(t, s, "/usr/bin:/bin", "/etc/devopsy/deploy-user", record, "new=deploy", "umask 077; new=deploy")
	fi, err := os.Stat(record)
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("the record written under umask 077: %v, %v\n%s", fi, err, out)
	}
}

// Bug (deploy-user review round 1): the owners step gives the new user everything the old one
// owns under the old user's home, when that is the release root: its ~/.ssh and dotfiles too.
// With an admin's own account as the old deploy user (one recorded by mistake, the case this
// topic is for), sshd's StrictModes then refuses that admin's keys, and the deploy user holds
// their files.
func TestDeployUserOwnersStepLeavesTheOldUsersLogin(t *testing.T) {
	t.Parallel()
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	for _, f := range []string{filepath.Join(home, ".ssh", "authorized_keys"), filepath.Join(home, "shop", "prod", "current")} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(t.TempDir(), "log")
	logged := "shift 2\nfor a; do echo \"$a\"; done >> " + log + "\n"
	path := fakes(t, map[string]string{
		"getent": "echo " + me.Username + ":x:1000:1000::" + home + ":/bin/sh\n",
		"chown":  logged, "chgrp": logged,
	}, "getent", "chown", "chgrp")
	s := stepWith(t, DeployUser, `-exec chown -h "$new"`)
	out := pasted(t, s, path, "old=devopsy new=deploy", "old="+me.Username+" new=deploy")
	got, _ := os.ReadFile(log)
	for _, l := range strings.Split(string(got), "\n") {
		if strings.HasPrefix(l, filepath.Join(home, ".ssh")) {
			t.Errorf("the owners step handed the old user's ~/.ssh to the new one:\n%s\n%s", got, out)
			break
		}
	}
}

// Bug (deploy-user review round 1): the topic never gives the new user the devopsy binary,
// which devopsy-server installs as the deploy user's (/usr/local/lib/devopsy) so that it
// upgrades itself: the new deploy user's --upgrade then fails, the door's sudo rules and sshd's
// keys command run a binary the old user can still replace, and a project user's refusal
// (askedAsProjectUser, by the binary's owner) still names the old user.
func TestDeployUserTopicHandsOverTheBinary(t *testing.T) {
	t.Parallel()
	tp, _ := Find(string(DeployUser))
	for _, s := range tp.Steps {
		for _, c := range s.Cmds {
			if strings.Contains(c, "command -v devopsy") && strings.Contains(c, "chown") {
				return
			}
		}
	}
	t.Error("no step gives the new user the devopsy binary the old one owns")
}
