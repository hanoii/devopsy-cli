package cli

import (
	"slices"
	"strings"
	"testing"
)

// Bug (review 2a): an input the capability needs others for, that the edit removed, is
// walked through by inputsOf and never checked against the edit, so the head ends "and"
// though the edit took one of the missing variable's inputs. The capability says X needs
// Y and Y needs A; Y was set by hand, A never was; --vars unset Y leaves X missing, and
// the edit is why: the ending is "so" (Caused), as reload.md says ("so" when the edit
// removed or emptied what is missing or an input of it).
func TestReview2aRemovedInputWithNeedsCauses(t *testing.T) {
	t.Parallel()
	m := Missing{Names: []string{"X"}, Needs: map[string][]string{"X": {"Y"}, "Y": {"A"}}}
	e := m.Explain(Edit{File: ".env", Known: true, Gone: map[string]bool{"Y": true}})
	if !e.Caused {
		t.Errorf("the edit removed Y, which X needs: caused %v, details %q", e.Caused, e.Details)
	}
	// The same with A set: the capability computed nothing for Y once Y was gone.
	m.Has = map[string]bool{"A": true}
	if e := m.Explain(Edit{File: ".env", Known: true, Gone: map[string]bool{"Y": true}}); !e.Caused {
		t.Errorf("the edit removed Y, A set: caused %v, details %q", e.Caused, e.Details)
	}
}

// Bug (review 2a): an input an import sets empty, that the capability says it needs
// others for, is walked through by inputsOf (whose leaf test checks Empty but not
// Imported), so the fix sets what that input needs. The import's empty value wins over
// anything the capability computes (the capability only fills unset variables), so the
// set changes nothing and X stays missing. Y should be said as imported and empty, never
// replaced by B, and nothing set for it, as for an input an import empties with no needs
// line of its own.
func TestReview2aImportedInputWithNeedsIsNotWalked(t *testing.T) {
	t.Parallel()
	m := Missing{Names: []string{"X"}, Needs: map[string][]string{"X": {"Y"}, "Y": {"B"}}, Imported: map[string]bool{"Y": true}}
	e := m.Explain(Edit{File: ".env"})
	if slices.Contains(e.Set, "B") || slices.Contains(e.SetAgain, "B") {
		t.Errorf("the fix sets B, which an import's empty Y hides: steps %+v", e.Steps())
	}
	if !slices.ContainsFunc(e.Details, func(d string) bool { return strings.Contains(d, "Y is imported") }) {
		t.Errorf("Y, imported empty, is not said: details %q", e.Details)
	}
}
