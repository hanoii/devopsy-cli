package main

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// lockWaitBound is how long a lock taken in Go is waited for unless its wait says otherwise,
// as long as the scripts' lockWait waits.
const lockWaitBound = 10 * time.Minute

// lockWaitEvery is how often a lock waited for is tried again.
const lockWaitEvery = 100 * time.Millisecond

// lockWaiting is a wait for a lock taken in Go, said as the scripts' lockWait says theirs:
// "waiting for <what>, up to <bound>" once it has to wait, "done waiting for <what>" with the
// time it took once the lock is held, "<what> did not end in <bound>" when the bound ends it.
// held, for a what that is the thing locked rather than what holds it, names the holder in
// the first line and makes the last "<what> still held by <held> after <bound>". A zero
// bound is lockWaitBound.
type lockWaiting struct {
	what, held string
	bound      time.Duration
}

// wait takes a lock through try, which says whether it got it: at once, saying nothing, else
// trying again every lockWaitEvery, saying so through log, until it gets it or the bound or
// ctx ends the wait, as an error. An error of try's ends it as it is.
func (w lockWaiting) wait(ctx context.Context, log *report.Logger, try func() (bool, error)) error {
	if ok, err := try(); err != nil || ok {
		return err
	}
	bound := w.bound
	if bound == 0 {
		bound = lockWaitBound
	}
	start := "waiting for " + w.what
	if w.held != "" {
		start += ", held by " + w.held
	}
	timer := log.Start(start + ", up to " + boundWords(bound))
	deadline := time.Now().Add(bound)
	tick := time.NewTicker(lockWaitEvery)
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

// ended is the error of a wait the bound, or its context, ended.
func (w lockWaiting) ended(bound time.Duration) error {
	if w.held != "" {
		return errors.New(w.what + " still held by " + w.held + " after " + boundWords(bound))
	}
	return errors.New(w.what + " did not end in " + boundWords(bound))
}

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
