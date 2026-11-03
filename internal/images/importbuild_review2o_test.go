package images

import (
	"bytes"
	"errors"
	"testing"
)

// Bug: notFetched refuses a landing whose labels name an image its release
// did not fetch (the rule would judge the labels, and the listed image
// would run), but only by name; ReloadCheck, which a sync and a --no-code
// restore run first, compares a built service's build too. An import runs
// only notFetched, and for a database the project builds its name never
// changes: with the README's pattern (one variable for the build argument
// and the label) a variable raised to 12.1 and no release lets an export
// at 12.1 through, and the image its release built at 11.8 starts on it.
func TestImportRefusesBuildNotFetched(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	dp, d := newFake(), newFake()
	built := func(name, version string) Service {
		s := builtDB(name, version)
		s.BuildKey = "section-" + version + ";MARIADB_VERSION=" + version
		return s
	}
	prod := newSim(t, dp, "app-prod", &out)
	if _, err := prod.release(false, built("app-prod", "12.1")); err != nil {
		prod.fail("prod: %v", err)
	}
	data := prod.e.set.ExportData(prod.base(), prod.e.rel(prod.cur), "database")
	dev := newSim(t, d, "app-dev", &out)
	if _, err := dev.release(false, built("app-dev", "11.8")); err != nil {
		dev.fail("dev: %v", err)
	}
	// --vars set MARIADB_VERSION=12.1, nothing released: what a reload or
	// a sync refuses, needing a release.
	dev.e.svcs[dev.cur] = []Service{built("app-dev", "12.1")}
	rel := dev.e.rel(dev.cur)
	needs, err := dev.e.set.ReloadCheck(rel)
	if err != nil || len(needs) == 0 {
		dev.fail("setup: ReloadCheck says the variable needs a release: %v %v", needs, err)
	}
	out.Reset()
	if err := dev.e.set.CheckLanding(rel, dev.e.set.ImportLanding(data, "database")); !errors.Is(err, ErrRefused) {
		t.Errorf("the import is let through (%v), the release's 11.8 build (%s) to start on data at 12.1, though %v:\n%s", err, dev.runs("database"), needs, out.String())
	}
}
