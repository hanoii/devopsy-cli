package cli

import (
	"strings"
	"testing"
)

// BUG (review, fable): CheckMissing's Unknown carries the env capability's stderr
// (computeEnv's error, its first line the file and exit status, the rest what it wrote),
// and Lines prints it after one "unknown " word: every line after the first is read by
// ParseMissing as a line of its own. The capability's reason is dropped, and one of its
// lines starting with a word of the protocol is taken for it: "set A ..." makes the
// variables before the edit known, so the operator says the variable was not set before
// the change either and that the release refuses as before, which nobody found.
func TestReviewFableUnknownOfSeveralLines(t *testing.T) {
	t.Parallel()
	m := &Missing{Names: []string{"X"}, Unknown: "devopsy could not tell whether the env capability computed them: compute: exit status 1\nset A (user:password) first"}
	// As the check prints them and the edit's script reads them back, line by line.
	got := ParseMissing(strings.Split(strings.Join(m.Lines(), "\n"), "\n"))
	if got == nil {
		t.Fatal("nothing parsed")
	}
	if got.Before != nil {
		t.Errorf("the variables before the edit taken for known: %+v", got.Before)
	}
	if !strings.Contains(got.Unknown, "set A (user:password) first") {
		t.Errorf("the capability's words lost: %q", got.Unknown)
	}
	if e := got.Explain(nil); e.AsBefore {
		t.Errorf("said to refuse as before, which nothing found: %v", e.Details)
	}
}

// BUG (review, fable): a variable set before the edit and missing after it, with no key
// of the file differing (an edit that sets a key to the value it had, the capability
// answering differently on its second run), gets no key to set or unset: varsFix then
// says a fix of no words and no command ("to fix it:" and ":").
func TestReviewFableExplainAlwaysHasAFix(t *testing.T) {
	t.Parallel()
	m := &Missing{Names: []string{"X"}, Before: &MissingBefore{Computed: map[string]bool{"X": true}, Set: map[string]bool{"X": true}, Changed: map[string]string{}}}
	if e := m.Explain(nil); len(e.SetAgain)+len(e.UnsetAgain)+len(e.SetThem) == 0 {
		t.Errorf("no key in the fix: %+v", e)
	}
}
