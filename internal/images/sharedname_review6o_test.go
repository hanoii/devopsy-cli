package images

import (
	"bytes"
	"testing"
)

// Bug: a rollback of a built data service whose name another environment's
// newer build holds keeps the name, as it must, and runs that image: "stays on
// its newer image ... runs it too". For a data service the rule judged the
// release's own label, never that image: here stage's 12 build runs on prod's
// 11.8 data, upgrading it in place, and img ran writes it down as 11.8, so
// nothing refuses the older image after it. The bookmark that a hold going
// away runs by in this same case (unhold, ownNameBack) holds the right one.
func TestRollbackSharedNameRunsOthersBuildOnData(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	d := newFake()
	db := func(version string) Service {
		return Service{Name: "database", Ref: "shop-db:latest", Build: true, Data: true, Method: CopyVolumes, DataVersion: version}
	}
	prod := newSim(t, d, "app-prod", &out)
	r1, err := prod.release(false, db("11.8"))
	if err != nil {
		prod.fail("prod: %v", err)
	}
	own := prod.runs("database")
	if _, err := prod.release(false, db("11.8")); err != nil {
		prod.fail("prod: %v", err)
	}
	stage := newSim(t, d, "app-stage", &out)
	stage.n = 100
	if _, err := stage.release(false, db("12.1")); err != nil {
		stage.fail("stage: %v", err)
	}
	newer := stage.runs("database")
	out.Reset()
	if err := prod.rollback(r1, false); err != nil {
		prod.fail("rollback: %v", err)
	}
	if got := prod.runs("database"); got != own {
		t.Errorf("prod's 11.8 data runs %s, stage's 12.1 build %s, not %s, its own, which its bookmark holds: history %v\n%s", got, newer, own, prod.history("database"), out.String())
	}
}
