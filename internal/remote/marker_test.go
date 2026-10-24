package remote

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLineMarker checks what goes through from a session's streams:
// everything but the marker line, partial lines held only while they can
// still become it, the rest handed over once whichever stream it came
// on, and nothing after it with Cut.
func TestLineMarker(t *testing.T) {
	t.Parallel()
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

// A line one stream held while the marker came on the other goes before
// that stream's next bytes.
func TestLineMarkerHeld(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	m := &LineMarker{Prefix: SyncMarker}
	o, e := m.Writer(&out), m.Writer(&errOut)
	_, _ = o.Write([]byte("devopsy: "))
	_, _ = e.Write([]byte(SyncMarker + "/b\n"))
	_, _ = o.Write([]byte("restoring\n"))
	if out.String() != "devopsy: restoring\n" {
		t.Errorf("held line: %q", out.String())
	}
}

// Only a logged session filters SwitchMarker (and logs LogAnchor where it
// was): any other is a plain passthrough, a shell's typing and an
// export's data untouched.
//
// Not parallel: it sets DEVOPSY_SSH_COMMAND (t.Setenv).
func TestSSHSwitchMarker(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, "fakessh"), "#!/bin/sh\nfor a; do s=$a; done\nexec sh -c \"$s\"\n")
	if err := os.Chmod(filepath.Join(bin, "fakessh"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVOPSY_SSH_COMMAND", filepath.Join(bin, "fakessh"))
	tg := &Target{Host: "h"}
	// One stream, as activations print: on two, the order is a race.
	script := "exec 1>&2; echo before; echo " + SwitchMarker + " t1; echo after"
	var out, log bytes.Buffer
	if _, err := SSHStreams(context.Background(), tg, nil, script, strings.NewReader(""), &out, &out, false, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), SwitchMarker) {
		t.Errorf("unlogged session filtered: %q", out.String())
	}
	out.Reset()
	if _, err := SSHStreams(context.Background(), tg, nil, script, strings.NewReader(""), &out, &out, false, &log); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), SwitchMarker) || !strings.Contains(out.String(), "after") {
		t.Errorf("logged session's output: %q", out.String())
	}
	if l := log.String(); !strings.Contains(l, "before\n"+LogAnchor+" t1\n") || strings.Contains(l, "after") {
		t.Errorf("logged session's log: %q", l)
	}
}

// An ssh killed by a signal devopsy did not send reads as ssh's own lost
// connection (255), through DEVOPSY_SSH_COMMAND too: what explains a lost
// connection explains it.
//
// Not parallel: it sets DEVOPSY_SSH_COMMAND (t.Setenv).
func TestSSHKilled(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, "fakessh"), "#!/bin/sh\nkill -9 $$\n")
	if err := os.Chmod(filepath.Join(bin, "fakessh"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEVOPSY_SSH_COMMAND", filepath.Join(bin, "fakessh")+" -o X=1")
	code, err := SSHStreams(context.Background(), &Target{Host: "h"}, nil, "true", strings.NewReader(""), io.Discard, io.Discard, false, nil)
	if err != nil || code != 255 {
		t.Errorf("ssh killed: %d %v", code, err)
	}
}
