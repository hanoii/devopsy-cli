package main

import (
	"cmp"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// heartbeat writes a zero byte to w every beat (remote.HeartbeatEvery), the
// stdin of a session the server watches, until stop or a failed write
// (the pipe closed).
func heartbeat(w io.Writer) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(remote.HeartbeatEvery())
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if _, err := w.Write([]byte{0}); err != nil {
					return
				}
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// readQuiet reads r, each chunk to fn, until it ends (EOF or an error)
// or, with quiet, nothing came for that long: silent then.
func readQuiet(r io.Reader, quiet time.Duration, fn func([]byte) error) (silent bool, err error) {
	type chunk struct {
		b   []byte
		err error
	}
	ch := make(chan chunk, 1)
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			ch <- chunk{append([]byte(nil), buf[:n]...), err}
			if err != nil {
				return
			}
		}
	}()
	var timer *time.Timer
	var fired <-chan time.Time
	if quiet > 0 {
		timer = time.NewTimer(quiet)
		defer timer.Stop()
		fired = timer.C
	}
	for {
		select {
		case c := <-ch:
			if len(c.b) > 0 {
				if err := fn(c.b); err != nil {
					return false, err
				}
			}
			if c.err == io.EOF {
				return false, nil
			}
			if c.err != nil {
				return false, c.err
			}
			if timer != nil {
				timer.Reset(quiet)
			}
		case <-fired:
			return true, nil
		}
	}
}

// heartbeatQuiet is how long a session's stdin may stay quiet before it
// counts as gone: with devopsy's heartbeat (remote.HeartbeatEnv),
// remote.QuietBeats of it, else forever.
func heartbeatQuiet() time.Duration {
	return remote.Quiet(remote.QuietBeats)
}

// dataQuiet is how long data may pause (remote.DataQuietBeats), 2
// minutes without a heartbeat's beat to count in.
func dataQuiet() time.Duration {
	return cmp.Or(remote.Quiet(remote.DataQuietBeats), 2*time.Minute)
}

// runStdinGuard implements the hidden --stdin-guard: stdin to stdout,
// failing when nothing came for dataQuiet (an upload's tar would wait on
// a session gone silently, the release lock held).
func runStdinGuard() int {
	silent, err := readQuiet(os.Stdin, dataQuiet(), func(b []byte) error {
		_, err := os.Stdout.Write(b)
		return err
	})
	switch {
	case silent:
		report.Error(fmt.Sprintf("nothing came for %s: the session is gone", dataQuiet()))
		return 1
	case err != nil:
		report.Fail(err)
		return 1
	}
	return 0
}
