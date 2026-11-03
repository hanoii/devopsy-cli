package images

import (
	"bytes"
	"testing"
)

// Bug: the fix for a rollback under a shared name stops at the rollback. A
// landing's derive leaves a data service it does not hold on its name, which
// another environment's newer build holds, so the run step after a sync runs
// that build on the copy: stage's 12.1 on prod's 11.8 data, its label judged,
// never that image, and img ran writes it down as 11.8. Its bookmark holds the
// right one, as in Restore and unhold. An import and a --no-code restore go
// through the same derive.
func TestLandingSharedNameRunsOthersBuildOnData(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	d := newFake()
	d.publish("mariadb:11.8", "M1")
	db := func(version string) Service {
		return Service{Name: "database", Ref: "shop-db:latest", Build: true, Data: true, Method: CopyVolumes, DataVersion: version}
	}
	base := newSim(t, d, "app-base", &out)
	if _, err := base.release(false, dbAt("mariadb:11.8", "11.8")); err != nil {
		base.fail("base: %v", err)
	}
	prod := newSim(t, d, "app-prod", &out)
	if _, err := prod.release(false, db("11.8")); err != nil {
		prod.fail("prod: %v", err)
	}
	own := prod.runs("database")
	stage := newSim(t, d, "app-stage", &out)
	stage.n = 100
	if _, err := stage.release(false, db("12.1")); err != nil {
		stage.fail("stage: %v", err)
	}
	newer := stage.runs("database")
	out.Reset()
	if err := syncInto(t, base, prod, false); err != nil {
		prod.fail("sync: %v", err)
	}
	if got := prod.runs("database"); got != own {
		t.Errorf("prod's run step after a sync runs %s, stage's 12.1 build %s, on 11.8 data, not its own %s: history %v\n%s", got, newer, own, prod.history("database"), out.String())
	}
}
