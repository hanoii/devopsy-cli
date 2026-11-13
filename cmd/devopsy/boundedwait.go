package main

import (
	"cmp"
	"context"
	"errors"
	"os"
	"strconv"
	"syscall"
	"time"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// waitBound is how long a wait goes on unless it says otherwise, as long as the scripts'
// lockWait waits.
const waitBound = 10 * time.Minute

// waitEvery is how often a wait tries again.
const waitEvery = 100 * time.Millisecond

// boundedWait is a wait in Go, announced and bounded, for a lock or anything that ends (a
// restart), said as the scripts' lockWait says theirs: "waiting for <what>, up to <bound>"
// once it has to wait, "done waiting for <what>" with the time it took once it ended, "<what>
// did not end in <bound>" when the bound ends it.
// held, for a what that is the thing locked rather than what holds it, names the holder in
// the first line and makes the last "<what> still held by <held> after <bound>". A zero
// bound is waitBound, a zero every waitEvery.
type boundedWait struct {
	what, held   string
	bound, every time.Duration
}

// wait goes through try, which says whether what is waited for is done (a lock taken): at
// once, saying nothing, else
// trying again every so often, saying so through log, until it gets it or the bound or
// ctx ends the wait, as an error. An error of try's ends it as it is.
func (w boundedWait) wait(ctx context.Context, log *report.Logger, try func() (bool, error)) error {
	if ok, err := try(); err != nil || ok {
		return err
	}
	bound := w.bound
	if bound == 0 {
		bound = waitBound
	}
	start := "waiting for " + w.what
	if w.held != "" {
		start += ", held by " + w.held
	}
	timer := log.Start(start + ", up to " + boundWords(bound))
	deadline := time.Now().Add(bound)
	tick := time.NewTicker(cmp.Or(w.every, waitEvery))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return w.ended(bound)
		case <-tick.C:
		}
		ok, err := try()
		if err != nil {
			return err
		}
		if ok {
			timer.Stop("done waiting for " + w.what)
			return nil
		}
		if time.Now().After(deadline) {
			return w.ended(bound)
		}
	}
}

// tryFlock takes f's lock (how, syscall.LOCK_EX or LOCK_SH) if it is free now: false, with no
// error, when another holds it.
func tryFlock(f *os.File, how int) (bool, error) {
	err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

// ended is the error of a wait the bound, or its context, ended: a waitEnded.
func (w boundedWait) ended(bound time.Duration) error {
	if w.held != "" {
		return waitEnded(w.what + " still held by " + w.held + " after " + boundWords(bound))
	}
	return waitEnded(w.what + " did not end in " + boundWords(bound))
}

// waitEnded is a wait that ended without what it waited for (boundedWait.ended), which a
// caller that stops waiting after one (--gc) tells by errors.As.
type waitEnded string

func (e waitEnded) Error() string { return string(e) }

// boundWords is a bound as a line says it: "a minute", "10 minutes", "30 seconds", else as
// Go writes it.
func boundWords(d time.Duration) string {
	for _, u := range []struct {
		d          time.Duration
		one, other string
	}{{time.Minute, "a minute", "minutes"}, {time.Second, "a second", "seconds"}} {
		if d%u.d == 0 && d > 0 {
			if n := d / u.d; n > 1 {
				return strconv.Itoa(int(n)) + " " + u.other
			}
			return u.one
		}
	}
	return d.String()
}
