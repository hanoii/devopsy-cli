package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// Bug (review round 2): on a server with no record where two docker users each have devopsy's
// setup record (an admin's own account devopsy once took for the deploy user ran --server-setup,
// what the record is for), each one's setup is refused with the command to run it as the other,
// and that command is refused the same way, sending the operator back: neither login can ever
// be recorded by a command devopsy offers, the real deploy user's included, whose every
// --upgrade then warns the same. A fix devopsy offers must not be one it refuses.
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
	if asDevopsy.Command == "devopsy @admin@vm1: --server-setup" && asAdmin.Command == "devopsy @devopsy@vm1: --server-setup" {
		t.Errorf("each user's refusal offers the setup as the other, which is refused too:\nas devopsy: %s\n  $ %s\nas admin: %s\n  $ %s", asDevopsy.Message, asDevopsy.Command, asAdmin.Message, asAdmin.Command)
	}
}

// Bug (review round 2): the root run names a release root's owner the passwd file does not list
// by its uid alone (remote.SetupOther, "other 1001 the release root ..."), where the plan's own
// read says "uid 1001" (otherDeployUser): the refusal then takes the number for a login and
// offers a command addressed as it, which no one can run, where the plan says the way in words.
func TestRootRunsOtherOwnerWithNoNameIsNoLogin(t *testing.T) {
	t.Parallel()
	user, evidence, ok := otherLine(remote.SetupOther + " 1001 the release root /srv/devopsy is its\n")
	if !ok {
		t.Fatal("no other line read")
	}
	r := otherDeployRefusal(user, evidence, login{uid: 1000, name: "admin"}, remote.Session{Host: "admin@vm1"})
	if r.Command != "" || !strings.Contains(r.Message, "uid 1001") {
		t.Errorf("an owner with no name is offered as a login: %q\n  $ %s", r.Message, r.Command)
	}
}
