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
