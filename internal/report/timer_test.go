package report

import (
	"bytes"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// A time reads the same wherever it is said: seconds, then minutes and
// seconds, then hours and minutes, and under a second never "0s".
func TestTook(t *testing.T) {
	t.Parallel()
	for d, want := range map[time.Duration]string{
		0:                      "<1s",
		999 * time.Millisecond: "<1s",
		time.Second:            "1s",
		59 * time.Second:       "59s",
		time.Minute:            "1m00s",
		72 * time.Second:       "1m12s",
		time.Hour - 1:          "59m59s",
		time.Hour:              "1h00m",
		2*time.Hour + 5*time.Minute + 9*time.Second: "2h05m",
	} {
		if got := Took(d); got != want {
			t.Errorf("Took(%s) = %q, want %q", d, got, want)
		}
	}
}

// Start says its line as Info does; Stop and Restart say theirs with the
// time, details and fixes on their own lines, and Restart's clock counts
// from then. Timer starts one and says nothing.
func TestTimer(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	l := New(&b, false, 0)
	if l.Timer(); b.Len() != 0 {
		t.Errorf("Timer said %q", b.String())
	}
	tm := l.Start("pulling", "why")
	if got, want := b.String(), "devopsy: pulling\ndevopsy: | why\n"; got != want {
		t.Errorf("Start said %q, want %q", got, want)
	}
	b.Reset()
	tm.start = time.Now().Add(-72 * time.Second)
	tm.Restart("pulled", "why")
	if got, want := b.String(), "devopsy: pulled (1m12s)\ndevopsy: | why\n"; got != want {
		t.Errorf("Restart said %q, want %q", got, want)
	}
	b.Reset()
	tm.Stop("built")
	if got, want := b.String(), "devopsy: built (<1s)\n"; got != want {
		t.Errorf("Stop after Restart said %q, want %q", got, want)
	}
}

// A clock from a script: its time when it is a number, the message alone
// when it is not (a date that failed), and never a negative time from a
// clock set back.
func TestSinceEpoch(t *testing.T) {
	t.Parallel()
	now := time.Now().Unix()
	if got := SinceEpoch("done", strconv.FormatInt(now-90, 10)); !regexp.MustCompile(`^done \(1m3\ds\)$`).MatchString(got) {
		t.Errorf("90 seconds ago: %q", got)
	}
	if got := SinceEpoch("done", strconv.FormatInt(now+3600, 10)); got != "done (<1s)" {
		t.Errorf("in the future: %q", got)
	}
	for _, bad := range []string{"", "soon"} {
		if got := SinceEpoch("done", bad); got != "done" {
			t.Errorf("clock %q: %q", bad, got)
		}
	}
}
