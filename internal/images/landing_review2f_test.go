package images

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// A copy's check refuses before anything stops what its landing would
// fail on (CheckLanding, then Land: one policy). But a data service held
// on another image whose compose no longer gives it (a profile a variable
// turned off since the release) passes the check: it is judged by the
// listing's own claim and nothing in copyNeeds minds a service compose
// lacks. Then derive, in Land, finds no service for the held entry and
// refuses ("compose no longer gives it"): the sync's copy is complete, the
// environment is marked, and what the sync stopped stays down. The check
// should refuse that sync (or an import, a --no-code restore) first, with
// nothing changed.
func TestLandingRefusesHeldServiceComposeLacksBeforeStop(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	prod, dev := newSim(t, d, "app-prod", &out), newSim(t, d, "app-dev", &out)
	d.publish("mariadb:11.8", "I1")
	prod.release(false, db118)
	dev.release(false, db118)
	r1, _ := dev.release(false, db118)
	d.publish("mariadb:12.1", "J1")
	dev.failRun(false, dbSvc("mariadb:12.1"))
	if dev.runs("database") != "J1" || heldIn(dev.e.rel(r1))["database"] == "" {
		dev.fail("setup: %s runs %s, held %v", r1, dev.runs("database"), heldIn(dev.e.rel(r1)))
	}
	// The database leaves compose's config under the same release.
	dev.e.svcs[r1] = []Service{proxySvc}
	data := prod.e.set.ParentData(prod.base(), prod.e.rel(prod.cur))
	rel := dev.e.rel(r1)
	out.Reset()
	checked := dev.e.set.CheckLanding(rel, dev.e.set.SyncLanding(data, ""))
	landed := dev.e.set.Land(dev.base(), rel, dev.e.set.SyncLanding(data, "vm1:prod"))
	if checked == nil && landed != nil {
		t.Errorf("the check let the sync through (nothing changed yet), and its landing failed once the copy was complete: %v\n%s", landed, out.String())
	}
	if landed != nil && !errors.Is(landed, ErrRefused) {
		t.Errorf("the landing's error: %v", landed)
	}
	if checked != nil && !strings.Contains(out.String(), "database") {
		t.Errorf("the refusal names no service:\n%s", out.String())
	}
}
