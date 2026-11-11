package main

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// A lock taken in Go is waited for as the scripts' lockWait waits: nothing said when it is
// free at once, the wait said with its bound and its end with the time it took, a bound that
// ends it an error saying so, and a cancelled one ending at once.
func TestLockWaiting(t *testing.T) {
	t.Parallel()
	free := func(after int) func() (bool, error) {
		n := 0
		return func() (bool, error) { n++; return n > after, nil }
	}
	for _, c := range []struct {
		name  string
		w     lockWaiting
		try   func() (bool, error)
		ctx   func() context.Context
		err   string
		lines string // a pattern the whole output matches
	}{
		{"free at once", lockWaiting{what: "another edit of keys"}, free(0), nil, "", `^$`},
		{"free after a wait", lockWaiting{what: "another edit of keys", bound: time.Minute}, free(3), nil, "",
			`^devopsy: waiting for another edit of keys, up to a minute\ndevopsy: done waiting for another edit of keys \(<1s\)\n$`},
		{"held by another", lockWaiting{what: "the server's images", held: "a removal"}, free(1), nil, "",
			`^devopsy: waiting for the server's images, held by a removal, up to 10 minutes\ndevopsy: done waiting for the server's images \(<1s\)\n$`},
		{"never free", lockWaiting{what: "another edit of keys", bound: 300 * time.Millisecond}, free(1 << 30), nil,
			"another edit of keys did not end in 300ms", `^devopsy: waiting for another edit of keys, up to 300ms\n$`},
		{"never free, held", lockWaiting{what: "the server's images", held: "a removal", bound: time.Second}, free(1 << 30), nil,
			"the server's images still held by a removal after a second", `^devopsy: waiting for the server's images, held by a removal, up to a second\n$`},
		{"cancelled", lockWaiting{what: "x"}, free(1 << 30), func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}, "x did not end in 10 minutes", `^devopsy: waiting for x, up to 10 minutes\n$`},
		{"an error trying", lockWaiting{what: "x"}, func() (bool, error) { return false, errors.New("bad lock") }, nil, "bad lock", `^$`},
	} {
		var out bytes.Buffer
		ctx := context.Background()
		if c.ctx != nil {
			ctx = c.ctx()
		}
		start := time.Now()
		err := c.w.wait(ctx, report.New(&out, false, 0), c.try)
		took := time.Since(start)
		if (err == nil) != (c.err == "") || err != nil && err.Error() != c.err {
			t.Errorf("%s: %v, want %q", c.name, err, c.err)
		}
		if !regexp.MustCompile(c.lines).MatchString(out.String()) {
			t.Errorf("%s: said\n%s", c.name, out.String())
		}
		// Bounded by its own bound, plus a try at most.
		if bound := c.w.bound; c.err != "" && bound > 0 && (took < bound || took > bound+2*lockWaitEvery+time.Second) {
			t.Errorf("%s: ended after %s, its bound %s", c.name, took, bound)
		}
		if c.ctx != nil && took > time.Second {
			t.Errorf("%s: ended after %s", c.name, took)
		}
	}
	for d, want := range map[time.Duration]string{time.Minute: "a minute", 10 * time.Minute: "10 minutes", time.Second: "a second", 30 * time.Second: "30 seconds", 1500 * time.Millisecond: "1.5s"} {
		if got := boundWords(d); got != want {
			t.Errorf("boundWords(%s) = %q, want %q", d, got, want)
		}
	}
}
