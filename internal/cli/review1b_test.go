package cli

import (
	"fmt"
	"testing"
)

// Bug (review 1b): readNeeds parses the capability's output again with p.env.Lookup after
// computeEnv has set the computed variables, so a value that reads a variable the output
// defines on a later line expands to nothing in the first parse and to that variable in
// the second: no candidate line "parses to the same variables without it", and every
// needs line is dropped, neither read nor counted malformed. The needs lines of an
// output are read whatever its other lines expand to.
func TestReview1bNeedsReadBesideAValueOfALaterVariable(t *testing.T) {
	t.Parallel()
	compute := "#!/bin/sh\ncat <<'OUT'\nSITE_URL=\"https://${SITE_HOST}\"\nSITE_HOST='shop.example.com'\n# needs: AUTH_USERS AUTH\nOUT\n"
	root := project(t, "shop", map[string]string{
		"compose.yaml":              minimalCompose,
		"config.yaml":               "project: shop\n",
		"capabilities/env/compute*": compute,
	})
	p, err := loadProject(root, []string{"PATH=/usr/bin:/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.computeEnv(); err != nil {
		t.Fatal(err)
	}
	if want := map[string][]string{"AUTH_USERS": {"AUTH"}}; fmt.Sprint(p.needs) != fmt.Sprint(want) || len(p.needsMalformed) != 0 {
		t.Errorf("needs %v, malformed %v; want %v and none", p.needs, p.needsMalformed, want)
	}
}

// Bug (review 1b): Explain says the edit caused what is missing only when it took an
// input, never the missing variable itself when the capability has a needs line for it:
// a variable set by hand and then removed (the capability computing nothing for it) ends
// the head ", and ... will fail", as if devopsy did not know, though the edit's own
// outcome says it removed what compose now misses. The plan's ending is "so" when the
// edit removed or emptied X or one of its inputs.
func TestReview1bRemovingAVariableWithNeedsIsCaused(t *testing.T) {
	t.Parallel()
	removed := Edit{File: ".env", Known: true, Gone: map[string]bool{"X": true}}
	for name, m := range map[string]Missing{
		"its input not set": {Names: []string{"X"}, Needs: map[string][]string{"X": {"A"}}},
		"its input set":     {Names: []string{"X"}, Needs: map[string][]string{"X": {"A"}}, Has: map[string]bool{"A": true}},
	} {
		if e := m.Explain(removed); !e.Caused {
			t.Errorf("%s: X removed by the edit and missing after it, Caused=%v; details %q", name, e.Caused, e.Details)
		}
	}
}
