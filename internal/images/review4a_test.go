package images

import (
	"bytes"
	"strings"
	"testing"
)

// BUG: --status says "something outside devopsy's compose file made it" of a built service
// a plain up moved to another environment's build. The file pins pulled services only: an
// own image runs by its name, which another environment of the project building under the
// same image: moves, and a plain up through devopsy, with devopsy's file, then follows it.
// The line named that cause (the name moved, a plain up follows names) until the pins, and
// now sends the operator looking for an override or a container made by hand.
func TestStatusLinesOwnNameMovedIsNoOutsideHand(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	web := Service{Name: "web", Ref: "shop-web", Build: true}
	prod := newSim(t, d, "app-prod", &out)
	stage := newSim(t, d, "app-stage", &out)
	if _, err := prod.release(false, web); err != nil {
		t.Fatal(err)
	}
	was := prod.runs("web")
	// The stage's release builds under the same name: it moves.
	if _, err := stage.release(false, web); err != nil {
		t.Fatal(err)
	}
	prod.up()
	if prod.runs("web") == was {
		t.Fatalf("the plain up kept %s: nothing to say", was)
	}
	got := statusTexts(StatusLines(d, "app-prod", prod.e.rel(prod.cur)))
	if len(got) != 1 || !strings.HasPrefix(got[0], "web runs ") {
		t.Fatalf("status lines: %q", got)
	}
	if strings.Contains(got[0], "outside devopsy's compose file") {
		t.Errorf("a plain up that followed a built image's name, blamed on something outside devopsy's file:\n%s", got[0])
	}
}
