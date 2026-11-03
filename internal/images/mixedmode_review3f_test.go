package images

import (
	"bytes"
	"testing"
)

// A project moving between modes: a release in build mode followed by
// one in image mode (its database pushed under a commit tag), and the
// reverse. The restart after the newer one's failed run step holds the
// older one on the failed release's image as that release's listing has
// it (pulled by digest in image mode, by the build's name in build
// mode), and a release that fixes forward then goes.
func TestDataRestartHoldsAcrossModes(t *testing.T) {
	t.Parallel()
	for _, first := range []string{ModeBuild, ModeImage} {
		second := ModeImage
		if first == ModeImage {
			second = ModeBuild
		}
		t.Run(first+"-then-"+second, func(t *testing.T) {
			t.Parallel()
			d := newFake()
			var out bytes.Buffer
			m := newSim(t, d, "app-prod", &out)
			m.strict = true
			db := func(mode, commit, version string) Service {
				s := builtDB("app-prod", version)
				if mode == ModeImage {
					s.Ref = "registry.example.com/app-prod-database:" + commit
					d.publish(s.Ref, "I"+commit)
				}
				return s
			}
			m.e.mode = first
			r1, err := m.release(false, db(first, "c1", "11.8"))
			if err != nil {
				m.fail("r1: %v", err)
			}
			m.e.mode = second
			r2, err := m.failRun(false, db(second, "c2", "12.1"))
			if err != nil {
				m.fail("r2: %v", err)
			}
			failed, _ := match(readList(m.e.rel(r2)), entry{Service: "database"})
			imgs, labels, held := composeFacts(m.e.rel(r1))
			if m.cur != r1 || m.runs("database") != failed.ID || held["database"] != r2 {
				m.fail("the restart of %s (%s) after %s (%s): runs %s, want %s; compose images %v labels %v held %v", r1, first, r2, second, m.runs("database"), failed.ID, imgs, labels, held)
			}
			if second == ModeImage && digestOf(imgs["database"]) == "" {
				m.fail("a hold on an image the registry serves goes by its digest, not %q", imgs["database"])
			}
			if l, _ := readHistory(m.base()).last("database"); l.Version != "12.1" {
				m.fail("the history ends on %+v", l)
			}
			// A release that fixes forward, in the mode the project is in now.
			if _, err := m.release(false, db(second, "c3", "12.1")); err != nil {
				m.fail("fixing forward: %v", err)
			}
			if heldIn(m.e.rel(m.cur))["database"] != "" {
				m.fail("the release that fixes forward is held: %v", heldIn(m.e.rel(m.cur)))
			}
		})
	}
}
