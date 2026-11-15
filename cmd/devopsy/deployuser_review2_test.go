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
