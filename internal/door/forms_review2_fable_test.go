package door

import "testing"

// Bug (review, fable, round 2): a form's mapped places (form.mapped, the import's 6 and 7)
// are read from the command without a look at its length, and script() takes a command
// shorter than its patterns: an import asked with no map (or one uid of it, or no argument
// at all) passes matches and decideForm indexes past its end, so the door panics behind
// sudo where it refuses every other malformed request with words.
func TestImportShortOfItsMapIsRefusedReview2Fable(t *testing.T) {
	t.Parallel()
	const entry, tree = "/space/mnt/shop-prod/data", "/home/devopsy-shop/shop/prod"
	for name, args := range map[string][]string{
		"no argument":    nil,
		"no map":         {"sh", "/in/t", "data"},
		"one uid of two": {"sh", "/in/t", "data", "10001"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("the door panicked: %v", r)
				}
			}()
			argv := append([]string{"run", "-v", entry + ":/mnt/data", "-v", tree + "/t:/in/t:ro", "util:1", "sh", "-c", "import script"}, args...)
			if _, err := decide(t, argv...); err == nil {
				t.Error("taken")
			}
		})
	}
}
