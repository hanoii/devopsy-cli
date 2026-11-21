package cli

import (
	"fmt"
	"slices"
	"testing"
)

// Bug (review 1a): readNeeds parses the capability's output again with p.env.Lookup after
// computeEnv has already set the output's own variables into p.env, and compose-go's
// dotenv asks the lookup before the variables parsed so far. An output whose value
// refers to a variable it prints later (B=${A:-x}, then A) parses differently the second
// time, so every needs line reads as part of a value and is dropped, unsaid even at -v.
func TestReview1aNeedsLineBesideForwardReference(t *testing.T) {
	t.Parallel()
	compute := "#!/bin/sh\n" + `cat <<'OUT'
B=${A:-x}
A='1'
# needs: URL AUTH
OUT
`
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
	if fmt.Sprint(p.needs) != fmt.Sprint(map[string][]string{"URL": {"AUTH"}}) {
		t.Errorf("needs %v, malformed %v; want map[URL:[AUTH]]", p.needs, p.needsMalformed)
	}
}

// Bug (review 1a): an edit that removed X itself, X having a needs line (a value set by
// hand over the capability, now unset) or being one the capability computes, ends "and"
// though the edit removed what is missing: the plan's "so" is for an edit that removed or
// emptied X or one of its inputs. Explain's needs cases (inputs not set, inputs all set,
// and an empty line in a file below) set Caused from the inputs only, never gone(x).
func TestReview1aRemovingXOverAnEmptyLineBelowIsTheCause(t *testing.T) {
	t.Parallel()
	took := Edit{File: ".env", Known: true, Gone: map[string]bool{"X": true}}
	for _, m := range []Missing{
		{Names: []string{"X"}, Needs: map[string][]string{"X": {"A"}}},
		{Names: []string{"X"}, Needs: map[string][]string{"X": {"A"}}, Has: map[string]bool{"A": true}},
		{Names: []string{"X"}, Needs: map[string][]string{"X": {"A"}}, Empty: map[string]string{"X": "project.env"}},
		{Names: []string{"X"}, Computed: map[string]bool{"X": true}, Empty: map[string]string{"X": "project.env"}},
	} {
		if e := m.Explain(took); !e.Caused {
			t.Errorf("%+v: the edit removed X and Caused is false (%q)", m, e.Details)
		}
	}
}

// Bug (review 1a): an input that has its own needs line and that a file sets empty is
// replaced by its own inputs (inputsOf), and its empty line is never said nor fixed: the
// fix sets Z only, though the edited file's Y='' would still hide whatever the
// capability computes for Y once Z is set. The plan treats a key that a file sets empty
// by one helper for every key, X or an input, and for X with a needs line empty in the
// edited file the fix unsets it and sets its inputs.
func TestReview1aIntermediateInputSetEmpty(t *testing.T) {
	t.Parallel()
	m := Missing{Names: []string{"X"}, Needs: map[string][]string{"X": {"Y"}, "Y": {"Z"}}, Empty: map[string]string{"Y": ".env"}}
	e := m.Explain(Edit{File: ".env"})
	unsets := slices.ContainsFunc(e.Steps(), func(s FixStep) bool { return s.Verb == "unset" && slices.Contains(s.Keys, "Y") })
	if !unsets {
		t.Errorf("Y is empty in the edited file and needs Z: steps %+v, details %q", e.Steps(), e.Details)
	}
}
