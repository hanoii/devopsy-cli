package door

import (
	"testing"
)

// BUG: a build writes a name another project runs by. A project's own names are any word
// under "<project>-" (OwnName), and Docker Hub's official images share that namespace:
// project mongo's build may write mongo-express:1, the image another project (shop) pulls
// and runs by that name. shop's start claims mongo-express:1 and checks its labels under
// shop's image lock, and compose then creates shop's container by the name; mongo's build
// takes mongo's lock only (imageLock is per project), so a build of mongo's between shop's
// check and its create (mongo may poll the name, its own by OwnsImage, to time it) has shop
// run mongo's image, with shop's environment, entries, volumes and networks. The same holds
// for eclipse-mosquitto (project eclipse), open-liberty (open), api-firewall (api).
func TestOpusReviewBuildWritesANameAnotherProjectRuns(t *testing.T) {
	t.Parallel()
	mongo := Policy{Project: "mongo", User: "devopsy-mongo", Space: "/space", Tree: "/home/devopsy-mongo/mongo", ForbiddenIDs: []int{1000}}
	shop := testPolicy
	ctx := "/home/devopsy-mongo/mongo/prod/releases/1/code"
	build := func(image string) []error {
		file := parse(t, `{"name":"mongo-prod","services":{"web":{"image":"`+image+`","user":"10001:10001","security_opt":["no-new-privileges"],"build":{"context":"`+ctx+`"}}}}`)
		return CheckFile(file, mongo, "build", &fakePaths{})
	}
	// Controls, which hold now: mongo's build of another project's name is refused, and
	// shop cannot claim a hold of mongo's.
	if errs := build("shop-prod-app:1"); !anyHolds(errs, "an image not this project's") {
		t.Fatalf("control: mongo's build of shop's name: %v", errs)
	}
	if err := shop.Claimable("mongo-express:devopsy-mongo-prod-1"); err == nil {
		t.Fatal("control: shop claims a hold of mongo's")
	}
	// The overlap: a name mongo's build writes, and shop claims and runs by that name.
	errs := build("mongo-express:1")
	ownedWithoutClaim := mongo.OwnsImage(fakeDaemon{}, "mongo-express:1", testKnown)
	if len(errs) == 0 && shop.Claimable("mongo-express:1") == nil && !shop.OwnsImage(fakeDaemon{}, "mongo-express:1", testKnown) {
		t.Errorf("BUG: mongo's build writes mongo-express:1 (Docker Hub's official image), which shop claims and runs by name under its own image lock only; mongo owns it without a claim: %v", ownedWithoutClaim)
	}
}

// BUG: a sync's receive reaches any port on the host's loopback. The form takes
// rsync://127.0.0.1:<any port>/, on the host's network, as root: the project users' firewall
// rejects their own processes' loopback connections by their group, but this container's
// rsync is root's, so a project user asks the door for a connection to any local service
// (sshd, a database another project publishes on 127.0.0.1, the deploy user's own) and reads
// the first line it answers in rsync's error ("server sent ... rather than greeting"). Only the
// caller's own forward, the port its sync session's sshd listens on, is a sync's receive.
func TestOpusReviewReceiveReachesAnyLoopbackPort(t *testing.T) {
	t.Parallel()
	recv := func(src string) error {
		_, err := decide(t, "run", "--network", "host", "-e", "RSYNC_PASSWORD", "-v", "/space/mnt/shop-prod:/mnt", "util:1", "rsync", "-a", src, "/mnt/data/")
		return err
	}
	// Control, which holds now: another host is refused.
	if err := recv("rsync://localhost:22/data/"); err == nil {
		t.Fatal("control: a receive from localhost by name taken")
	}
	// The host's sshd, nobody's sync forward.
	if err := recv("rsync://127.0.0.1:22/data/"); err == nil {
		t.Error("BUG: a receive from 127.0.0.1:22, no forward of the caller's, runs as root on the host's network past the project users' firewall")
	}
}
