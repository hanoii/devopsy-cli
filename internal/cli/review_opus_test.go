package cli

import (
	"path/filepath"
	"slices"
	"testing"
)

// BUG (review, opus): Explain takes a missing variable the edit removed or changed for
// the edit's own ("set <key> again") before asking whether it was set before: one that
// was empty before as well (MissingVars' test) is the edit's then, so the head never
// says "as before", and at a level its fix sets the key in the edited file, where a
// nearer file's empty line still wins and the fix fixes nothing. Here a project's
// REQUIRED='1' is unset while the environment's .env has REQUIRED= (empty): compose
// missed it before the edit too.
func TestReviewOpusRemovedButEmptyBeforeIsAsBefore(t *testing.T) {
	t.Parallel()
	said := "error while interpolating services.app.environment.REQUIRED: required variable REQUIRED is missing a value"
	root := project(t, "shop", map[string]string{
		"compose.yaml": minimalCompose,
		"config.yaml":  "project: shop\n",
		"bin/docker*":  fakeCompose(said, `{"REQUIRED":{"Required":true}}`),
		".env":         "REQUIRED=\n",
		"project.env":  "",
	})
	m, err := CheckMissing(root, []string{"PATH=" + filepath.Join(root, ProjectDirName, "bin") + ":/usr/bin:/bin"}, &Before{File: BeforeFiles["project"], Content: []byte("REQUIRED='1'\n")})
	if err != nil || m == nil || m.Before == nil {
		t.Fatalf("%+v %v", m, err)
	}
	e := m.Explain([]string{"REQUIRED"})
	if !e.AsBefore {
		t.Errorf("not as before, though REQUIRED was empty before the edit too: %q", e.Details)
	}
	if slices.Contains(e.SetAgain, "REQUIRED") {
		t.Errorf("fix sets REQUIRED again in the project's .env, where the environment's empty line wins: %+v", e)
	}
}

// BUG (review, opus): Lines says the variables before the edit are known only through a
// "set" or "changed" line, and ParseMissing takes them for unknown without one: an edit
// that changed no key of its file (an unset of a key the file lacks, a set to the value
// it had) with a variable missing before as well comes back with Before nil, so the
// operator says "set <key> again" for an unset key compose missed before, and never "as
// before". The same Missing explained before its round trip says it right.
func TestReviewOpusBeforeKnownSurvivesLines(t *testing.T) {
	t.Parallel()
	m := &Missing{Names: []string{"REQUIRED"}, Before: &MissingBefore{Computed: map[string]bool{}, Set: map[string]bool{}, Changed: map[string]string{}}}
	if e := m.Explain([]string{"REQUIRED"}); !e.AsBefore || len(e.SetAgain) != 0 {
		t.Fatalf("before the round trip: %+v", e)
	}
	got := ParseMissing(m.Lines())
	if got == nil || got.Before == nil {
		t.Fatalf("the variables before the edit lost: %q -> %+v", m.Lines(), got)
	}
	if e := got.Explain([]string{"REQUIRED"}); !e.AsBefore || len(e.SetAgain) != 0 {
		t.Errorf("after the round trip: %+v", e)
	}
}
