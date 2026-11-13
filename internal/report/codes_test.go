package report

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The registry's codes are each W and four digits, none sharing a number or a description, and
// docs/codes.md lists exactly them, each with the description the message prints: an admin
// told a code finds it there.
func TestCodes(t *testing.T) {
	t.Parallel()
	shape := regexp.MustCompile(`^W[0-9]{4}$`)
	ids, descs := map[string]bool{}, map[string]bool{}
	for _, c := range Codes {
		if !shape.MatchString(c.ID) || ids[c.ID] || descs[c.Desc] || c.Desc == "" {
			t.Errorf("%+v: not W and four digits, or its number or description another's", c)
		}
		ids[c.ID], descs[c.Desc] = true, true
	}
	data, err := os.ReadFile("../../docs/codes.md")
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		cells := strings.Split(line, "|")
		if len(cells) > 3 && shape.MatchString(strings.TrimSpace(cells[1])) {
			listed[strings.TrimSpace(cells[1])] = strings.TrimSpace(cells[2])
		}
	}
	for _, c := range Codes {
		if listed[c.ID] != c.Desc {
			t.Errorf("docs/codes.md lists %s as %q, the registry %q", c.ID, listed[c.ID], c.Desc)
		}
		delete(listed, c.ID)
	}
	for id := range listed {
		t.Errorf("docs/codes.md lists %s, which the registry lacks", id)
	}
}

// A coded message reads the same first line for every reader, and a reader who can only pass
// it on gets the code again and no way out.
func TestCodedMessages(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	l := New(&b, false, 0)
	l.SayForAdmin(WarnLevel, FirewallNotLoaded)
	l.SayCode(WarnLevel, FirewallNotLoaded, nil, Fix("", "devopsy @vm1: --server-setup"))
	want := "devopsy: warning: W0001: the server's firewall is not loaded as devopsy loads it\n" +
		"devopsy: | report W0001 to the server's admin\n" +
		"devopsy: warning: W0001: the server's firewall is not loaded as devopsy loads it\n" +
		"devopsy: | to fix it:\n" +
		"devopsy: | $ devopsy @vm1: --server-setup\n"
	if b.String() != want {
		t.Errorf("%q, want %q", b.String(), want)
	}
}
