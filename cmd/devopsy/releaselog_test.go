package main

import (
	"regexp"
	"strings"
	"testing"
)

// The log keeps final states only: no terminal codes, spinner frames or
// the empty lines compose's redraws leave, each line after its time.
func TestReleaseLogText(t *testing.T) {
	l := &releaseLog{}
	l.Line("devopsy: running 'devopsy deploy' (run)")
	_, _ = l.Write([]byte("[+] up 1/2\n \x1b[32m✔\x1b[0m Network n Created      \n \u280b Container c Creating   0.0s\n\n\n"))
	_, _ = l.Write([]byte("\x1b[2A \u2819 Container c Starting\r\n\n ✔ Container c Healthy    0.9s\nwait"))
	_, _ = l.Write([]byte("ing\rdone\n"))
	got := regexp.MustCompile(`(?m)^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d `).ReplaceAllString(string(l.Text()), "")
	want := "devopsy: running 'devopsy deploy' (run)\n[+] up 1/2\n ✔ Network n Created\n ✔ Container c Healthy    0.9s\ndone\n"
	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
	if !strings.HasPrefix(string(l.Text()), "20") {
		t.Errorf("no time: %q", l.Text())
	}
}
