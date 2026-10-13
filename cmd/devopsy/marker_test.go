package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// TestMarkerWriter checks what reaches the terminal from the holder's
// stdout: everything but the marker line, partial lines held only while
// they can still become it, and the base handed over once.
func TestMarkerWriter(t *testing.T) {
	marker := remote.CloneMarker
	for name, c := range map[string]struct {
		writes []string
		out    string
		base   string
	}{
		"one write": {
			[]string{"before\n" + marker + "/srv/app/dev\nafter\n"},
			"before\nafter\n", "/srv/app/dev",
		},
		"split across writes": {
			[]string{"devopsy: pausing app\n" + marker[:9], marker[9:] + "/home/devopsy/app/dev\n", "deploy output\n"},
			"devopsy: pausing app\ndeploy output\n", "/home/devopsy/app/dev",
		},
		"byte by byte": {
			strings.Split("x\n"+marker+"/b\ny\n", ""),
			"x\ny\n", "/b",
		},
		"carriage return": {
			[]string{marker + "/b\r\nrest"},
			"rest", "/b",
		},
		"progress line passes through": {
			[]string{"copying 1/3", " 2/3", "\n" + marker + "/b\n"},
			"copying 1/3 2/3\n", "/b",
		},
		"a line that starts like the marker": {
			[]string{"devopsy: ready", " for lunch\n" + marker + "/b\n"},
			"devopsy: ready for lunch\n", "/b",
		},
		"marker only once": {
			[]string{marker + "/b\n" + marker + "/c\n"},
			marker + "/c\n", "/b",
		},
		"no marker": {
			[]string{"just output\n", "tail"},
			"just output\ntail", "",
		},
	} {
		var buf bytes.Buffer
		mw := newMarkerWriter(&buf)
		for _, w := range c.writes {
			if n, err := mw.Write([]byte(w)); err != nil || n != len(w) {
				t.Fatalf("%s: Write(%q) = %d, %v", name, w, n, err)
			}
		}
		mw.flush()
		if buf.String() != c.out {
			t.Errorf("%s: output %q, want %q", name, buf.String(), c.out)
		}
		var base string
		select {
		case base = <-mw.marker:
		default:
		}
		if base != c.base {
			t.Errorf("%s: base %q, want %q", name, base, c.base)
		}
	}
}
