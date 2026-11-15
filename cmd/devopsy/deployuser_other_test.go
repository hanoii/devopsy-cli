package main

import (
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/howto"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

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
//
// BUG (review round 3): otherDeployUser looks for a member's uid claims and environments in its
// home only, while both live under its release root. A deploy user set up on a volume, with
// every environment of the server, reads as "bare": the login is offered `sudo rm` of its
// setup record as an account set up by mistake, after which the login is recorded and the real
// deploy user refused (W0009); with no setup record it is not seen at all, and the login
// recorded at once, by --upgrade too.
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
//
// BUG (review round 3): otherDeployUser takes root for another deploy user as any docker
// member, so the login's setup is refused with "run it as root" as its first fix, a setup that
// records no one (deployUserFact, as root with no record), while a release root of root's is
// already no evidence.
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
//
// BUG (review round 3): homeReleases takes any <a>/<b>/releases beside current or shared/ for
// an environment of devopsy's, so such a member refuses the real deploy user's record for good,
// its only ways out "run it as" that member or moving that member's apps (the topic's steps),
// and "both hold environments" once the deploy user has released.
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
//
// BUG (review round 3): otherWire joins its fields with tabs and otherLine splits on them, so
// an environment whose directory name holds a tab shifts the fields: the rest of its path is
// read as a bare setup record, and the refusal offers `sudo rm` of it.
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
//
// BUG (review round 3): where no command names the other user (no host in the session, or an
// owner with no name, "uid N"), otherDeployRefusal keeps "run it as <other>" as its text and
// fixWords joins it to the topic's command: "run it as uid 1001; this prints the steps:" with
// `devopsy --server-setup-howto deploy-user`, steps that make this login the deploy user.
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
