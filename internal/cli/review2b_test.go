package cli

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Review, round 2. Bug: needs lines are read before the capability's own values are set
// (round 1's fix), so the mask they are checked with holds only the secrets set before
// the capability ran: a secret the capability itself computes, printed by mistake in a
// needs line, is kept as a variable's name, and travels to the operator in the check's
// lines, into a detail and a command to copy.
func TestReview2bNeedsLineWithComputedSecret(t *testing.T) {
	t.Parallel()
	said := "required variable URL is missing a value"
	root := project(t, "shop", map[string]string{
		"compose.yaml":              minimalCompose,
		"config.yaml":               "project: shop\nsecrets: [TOKEN]\n",
		"bin/docker*":               fakeCompose(said, `{"URL":{"Required":true}}`),
		"capabilities/env/compute*": "#!/bin/sh\necho \"TOKEN='hunter2secret'\"\necho '# needs: URL hunter2secret'\n",
	})
	p, err := loadProject(root, []string{"PATH=" + root + "/" + ProjectDirName + "/bin:/usr/bin:/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.computeEnv(); err != nil {
		t.Fatal(err)
	}
	if len(p.needs) != 0 || !slices.Equal(p.needsMalformed, []int{2}) {
		t.Errorf("needs %v, malformed %v: want the line naming the computed secret's value dropped by its number", p.needs, p.needsMalformed)
	}
	m, err := missingOf(p)
	if err != nil || m == nil {
		t.Fatalf("%+v %v", m, err)
	}
	if lines := strings.Join(m.Lines(), "\n"); strings.Contains(lines, "hunter2secret") {
		t.Errorf("the secret's value in the check's lines, which the operator prints:\n%s", lines)
	}
}

// Review, round 2. Bug: an input an import sets empty that the capability also needs
// others for is walked through (inputsOf stops at one a file sets empty, never at one an
// import does), so the fix sets its inputs, which changes nothing: the import's empty
// value wins over whatever the capability then computes, the lowest precedence. It is to
// be said as imported, with no command, as an input without a needs line is.
func TestReview2bImportedInputWithNeedsIsNotWalkedThrough(t *testing.T) {
	t.Parallel()
	m := Missing{Names: []string{"X"}, Needs: map[string][]string{"X": {"A"}, "A": {"B"}}, Imported: map[string]bool{"A": true}}
	e := m.Explain(Edit{File: ".env"})
	if len(e.Steps()) != 0 || !slices.ContainsFunc(e.Details, func(d string) bool { return strings.Contains(d, "A is imported") }) {
		t.Errorf("steps %+v, details %q: want A said as imported and no command", e.Steps(), e.Details)
	}
}

// Review, round 2. Bug: a variable the capability computes that the edited file sets
// empty is unset there when compose misses it, and set there when it is another's
// input: with both missing, one fix says to unset it and then to set it, the second
// undoing the first and bypassing the capability.
func TestReview2bComputedInputIsNotUnsetThenSet(t *testing.T) {
	t.Parallel()
	m := Missing{Names: []string{"A", "X"}, Needs: map[string][]string{"X": {"A"}}, Empty: map[string]string{"A": ".env"}, Computed: map[string]bool{"A": true}}
	e := m.Explain(Edit{File: ".env"})
	if fmt.Sprint(e.Unset, e.Set) != "[A] []" {
		t.Errorf("unset %v, set %v: want A unset only", e.Unset, e.Set)
	}
}

// Review, round 2. Bug: what the capability computes that a file's empty line hides is
// recorded for a missing variable only (Computed), never for an input: an input the
// capability prints a value for, set empty in the edited file, is offered a set, which
// bypasses the capability as setting the missing variable would. It is to unset there.
func TestReview2bComputedInputEmptiedIsUnset(t *testing.T) {
	t.Parallel()
	root := project(t, "shop", map[string]string{
		"compose.yaml":              minimalCompose,
		"config.yaml":               "project: shop\n",
		"bin/docker*":               fakeCompose("required variable URL is missing a value", `{"URL":{"Required":true}}`),
		"capabilities/env/compute*": "#!/bin/sh\necho \"HOST='shop.example.com'\"\necho '# needs: URL HOST'\n",
		".env":                      "HOST=\n",
	})
	p, err := loadProject(root, []string{"PATH=" + root + "/" + ProjectDirName + "/bin:/usr/bin:/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.computeEnv(); err != nil {
		t.Fatal(err)
	}
	m, err := missingOf(p)
	if err != nil || m == nil {
		t.Fatalf("%+v %v", m, err)
	}
	e := ParseMissing(m.Lines()).Explain(Edit{File: ".env"})
	if fmt.Sprint(e.Unset, e.Set) != "[HOST] []" {
		t.Errorf("unset %v, set %v, details %q: want HOST unset, never set by hand", e.Unset, e.Set, e.Details)
	}
}
