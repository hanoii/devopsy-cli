package cli

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The data capability's catalog names every command whose sync runs its
// hooks: what --debug capabilities shows a project author.
func TestDataCapabilityCalledBy(t *testing.T) {
	t.Parallel()
	for _, c := range Capabilities {
		if c.Name != DataCapability {
			continue
		}
		for _, flag := range []string{"--sync", "--reset", "--release-sync", "--export", "--import"} {
			if !strings.Contains(c.CalledBy, flag) {
				t.Errorf("the data capability's caller list lacks %s: %s", flag, c.CalledBy)
			}
		}
		return
	}
	t.Fatal("no data capability in the catalog")
}

// The env capability's needs lines say what it needs for a variable it did not compute,
// several lines of one adding up. A line inside a quoted value is the value's, never
// read; one devopsy cannot read (a name alone, a variable among its own needs, a secret's
// value printed by mistake) is dropped by its line number, its text never kept, the
// capability's other lines read as before.
func TestReadNeeds(t *testing.T) {
	t.Parallel()
	compute := "#!/bin/sh\n" + `cat <<'OUT'
DONE='1'
# needs: AUTH_USERS AUTH
# needs: AUTH_USERS SALT
# needs: LONELY
# needs: SELF SELF
# needs: PW hunter2secret
NOTE="first
# needs: INSIDE VALUE
end"
# needs: URL AUTH_USERS
# a comment of the project's own
OUT
`
	root := project(t, "shop", map[string]string{
		"compose.yaml":              minimalCompose,
		"config.yaml":               "project: shop\nsecrets: [KEY]\n",
		"capabilities/env/compute*": compute,
	})
	p, err := loadProject(root, []string{"PATH=/usr/bin:/bin", "KEY=hunter2secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.computeEnv(); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"AUTH_USERS": {"AUTH", "SALT"}, "URL": {"AUTH_USERS"}}
	if fmt.Sprint(p.needs) != fmt.Sprint(want) || !slices.Equal(p.needsMalformed, []int{4, 5, 6}) {
		t.Errorf("needs %v, malformed %v; want %v, [4 5 6]", p.needs, p.needsMalformed, want)
	}
	if v, _ := p.env.Lookup("NOTE"); v != "first\n# needs: INSIDE VALUE\nend" {
		t.Errorf("the quoted value: %q", v)
	}
	if v, _ := p.env.Lookup("DONE"); v != "1" {
		t.Errorf("the capability's own variable: %q", v)
	}
	verbose := strings.Join(p.verbose, "\n")
	if strings.Contains(verbose, "hunter2secret") || !strings.Contains(verbose, "line 6 starts # needs:") || !strings.Contains(verbose, "needs AUTH and SALT for AUTH_USERS") {
		t.Errorf("verbose:\n%s", verbose)
	}
}
