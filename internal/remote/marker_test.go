package remote

import (
	"bytes"
	"strings"
	"testing"
)

// TestLineMarker checks what goes through from a session's streams:
// everything but the marker line, partial lines held only while they can
// still become it, the rest handed over once whichever stream it came
// on, and nothing after it with Cut.
func TestLineMarker(t *testing.T) {
	marker := SyncMarker
	for name, c := range map[string]struct {
		writes []string
		cut    bool
		out    string
		base   string
	}{
		"one write": {
			writes: []string{"before\n" + marker + "/srv/app/dev\nafter\n"},
			out:    "before\nafter\n", base: "/srv/app/dev",
		},
		"split across writes": {
			writes: []string{"devopsy: pausing app\n" + marker[:9], marker[9:] + "/home/devopsy/app/dev\n", "deploy output\n"},
			out:    "devopsy: pausing app\ndeploy output\n", base: "/home/devopsy/app/dev",
		},
		"byte by byte": {
			writes: strings.Split("x\n"+marker+"/b\ny\n", ""),
			out:    "x\ny\n", base: "/b",
		},
		"carriage return": {
			writes: []string{marker + "/b\r\nrest"},
			out:    "rest", base: "/b",
		},
		"progress line passes through": {
			writes: []string{"copying 1/3", " 2/3", "\n" + marker + "/b\n"},
			out:    "copying 1/3 2/3\n", base: "/b",
		},
		"a line that starts like the marker": {
			writes: []string{"devopsy: ready", " for lunch\n" + marker + "/b\n"},
			out:    "devopsy: ready for lunch\n", base: "/b",
		},
		"marker only once": {
			writes: []string{marker + "/b\n" + marker + "/c\n"},
			out:    marker + "/c\n", base: "/b",
		},
		"no marker": {
			writes: []string{"just output\n", "tail"},
			out:    "just output\ntail",
		},
		"cut": {
			writes: []string{"before\npart", "ial\n" + marker + "/b\nafter\n", "more"},
			cut:    true, out: "before\npartial\n", base: "/b",
		},
	} {
		var buf bytes.Buffer
		var base string
		m := &LineMarker{Prefix: marker, Cut: c.cut, Found: func(rest string) { base += rest }}
		mw := m.Writer(&buf)
		for _, w := range c.writes {
			if n, err := mw.Write([]byte(w)); err != nil || n != len(w) {
				t.Fatalf("%s: Write(%q) = %d, %v", name, w, n, err)
			}
		}
		mw.Flush()
		if buf.String() != c.out {
			t.Errorf("%s: output %q, want %q", name, buf.String(), c.out)
		}
		if base != c.base {
			t.Errorf("%s: base %q, want %q", name, base, c.base)
		}
	}

	// Two streams: found on stderr, stdout's later marker-like line
	// passes.
	var out, errOut bytes.Buffer
	found := 0
	m := &LineMarker{Prefix: marker, Found: func(string) { found++ }}
	o, e := m.Writer(&out), m.Writer(&errOut)
	_, _ = e.Write([]byte("x\n" + marker + "/b\n"))
	_, _ = o.Write([]byte(marker + "/c\n"))
	if found != 1 || errOut.String() != "x\n" || out.String() != marker+"/c\n" {
		t.Errorf("two streams: found %d, stderr %q, stdout %q", found, errOut.String(), out.String())
	}
}
