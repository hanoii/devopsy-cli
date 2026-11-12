package door

import (
	"errors"
	"testing"
)

// Bug (review round 2): the import form's script matcher (script) takes a call with fewer
// arguments than its patterns, and decideForm then reads the map's uids at their fixed places
// (form.mapped, 6 and 7) without checking the command is that long: a project user's
// docker run of the import script with its stream and entry alone, or one uid, panics the
// door (index out of range) instead of refusing it, so the refusal is neither said as the
// door's nor logged (doorRecord). Fix: a form with mapped places matches only a command that
// holds them (script requiring at least as many arguments as it has patterns, or decideForm
// refusing a mapped place past the command's end).
func TestImportFormShortOfItsMapIsRefused(t *testing.T) {
	t.Parallel()
	entry, tree := "/space/mnt/shop-prod/data", "/home/devopsy-shop/shop/prod"
	for name, tail := range map[string][]string{
		"no uids, no member": {"/in/t", "data"},
		"one uid":            {"/in/t", "data", "10001"},
	} {
		argv := append([]string{"run", "-v", entry + ":/mnt/data", "-v", tree + "/t:/in/t:ro", "util:1", "sh", "-c", "import script", "sh"}, tail...)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("%s: the door panicked rather than refuse: %v", name, r)
				}
			}()
			a, err := Decide(argv, testPolicy, testDaemon, &fakeHost{}, testKnown)
			var r *Refusal
			if !errors.As(err, &r) {
				t.Errorf("%s: not refused: %+v, %v", name, a, err)
			}
		}()
	}
}
