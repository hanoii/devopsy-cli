package images

import (
	"bytes"
	"errors"
	"testing"
)

// Bug: Land writes every service's labels into the listing (withClaims),
// the ones it does not land included, whose image nothing checks
// (notFetched only looks at what lands). A variable set since the release
// (cache's image and label, from one variable, raised to 8) with an import
// of another service makes the listing say cache is at 8 while its release
// fetched, and its container runs, redis:7; img ran then writes that into
// its history, and a later release back at the label of the image that
// really ran is refused as lower.
func TestLandKeepsOtherServicesClaims(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-prod", &out)
	d.publish("mariadb:11.8", "I1")
	d.publish("redis:7", "R7")
	cache := func(ref, version string) Service {
		return Service{Name: "cache", Ref: ref, Data: true, Method: CopyVolumes, DataVersion: version}
	}
	if _, err := m.release(false, dbSvc("mariadb:11.8"), cache("redis:7", "7")); err != nil {
		m.fail("setup: %v", err)
	}
	rel := m.e.rel(m.cur)
	data := m.e.set.ExportData(m.base(), rel, "database")
	// --vars set CACHE_VERSION=8, nothing released.
	m.e.svcs[m.cur] = []Service{dbSvc("mariadb:11.8"), cache("redis:8", "8")}
	l := m.e.set.ImportLanding(data, "database")
	if err := m.e.set.CheckLanding(rel, l); err != nil {
		m.fail("the import's check: %v", err)
	}
	if err := m.e.set.Land(m.base(), rel, l); err != nil {
		m.fail("the import's landing: %v", err)
	}
	if err := m.e.set.Ran(m.base(), rel); err != nil {
		m.fail("img ran: %v", err)
	}
	if last, _ := readHistory(m.base()).last("cache"); last.Version != "7" || last.Tag != "7" {
		t.Errorf("cache's container runs redis:7 (%s), which nothing fetched anew, and its history now says version %q, tag %q:\n%s", m.runs("cache"), last.Version, last.Tag, out.String())
	}
	// The variable taken back: the image that ran all along.
	m.e.svcs[m.cur] = []Service{dbSvc("mariadb:11.8"), cache("redis:7", "7")}
	if _, err := m.release(false, dbSvc("mariadb:11.8"), cache("redis:7", "7")); errors.Is(err, ErrRefused) {
		t.Errorf("a release of the image that always ran is refused as lower:\n%s", out.String())
	}
}
