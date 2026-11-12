package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// A lock taken in Go is waited for as the scripts' lockWait waits: nothing said when it is
// free at once, the wait said with its bound and its end with the time it took, a bound that
// ends it an error saying so, and a cancelled one ending at once.
func TestBoundedWait(t *testing.T) {
	t.Parallel()
	free := func(after int) func() (bool, error) {
		n := 0
		return func() (bool, error) { n++; return n > after, nil }
	}
	for _, c := range []struct {
		name  string
		w     boundedWait
		try   func() (bool, error)
		ctx   func() context.Context
		err   string
		lines string // a pattern the whole output matches
	}{
		{"free at once", boundedWait{what: "another edit of keys"}, free(0), nil, "", `^$`},
		{"free after a wait", boundedWait{what: "another edit of keys", bound: time.Minute}, free(3), nil, "",
			`^devopsy: waiting for another edit of keys, up to a minute\ndevopsy: done waiting for another edit of keys \(<1s\)\n$`},
		{"held by another", boundedWait{what: "the server's images", held: "a removal"}, free(1), nil, "",
			`^devopsy: waiting for the server's images, held by a removal, up to 10 minutes\ndevopsy: done waiting for the server's images \(<1s\)\n$`},
		{"never free", boundedWait{what: "another edit of keys", bound: 300 * time.Millisecond}, free(1 << 30), nil,
			"another edit of keys did not end in 300ms", `^devopsy: waiting for another edit of keys, up to 300ms\n$`},
		{"never free, held", boundedWait{what: "the server's images", held: "a removal", bound: time.Second}, free(1 << 30), nil,
			"the server's images still held by a removal after a second", `^devopsy: waiting for the server's images, held by a removal, up to a second\n$`},
		{"cancelled", boundedWait{what: "x"}, free(1 << 30), func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}, "x did not end in 10 minutes", `^devopsy: waiting for x, up to 10 minutes\n$`},
		{"an error trying", boundedWait{what: "x"}, func() (bool, error) { return false, errors.New("bad lock") }, nil, "bad lock", `^$`},
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
		if bound := c.w.bound; c.err != "" && bound > 0 && (took < bound || took > bound+2*waitEvery+time.Second) {
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

// The server's image lock in Go is taken as the scripts' image_lock takes it: its wait said
// and bounded, a take that never waits busy at once, and an exclusive one let go when Docker
// does not answer, since a removal then reads every image as gone.
func TestImagesLock(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	answers, silent := filepath.Join(root, "docker"), filepath.Join(root, "silent")
	write(t, answers, "#!/bin/sh\n[ \"$1\" = version ]\n", 0o755)
	write(t, silent, "#!/bin/sh\necho 'Cannot connect to the Docker daemon' >&2\nexit 1\n", 0o755)
	var said bytes.Buffer
	log := report.New(&said, false, 0)
	held, err := os.OpenFile(filepath.Join(root, remote.ImagesLock), os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	// Shared beside a shared holder: at once, nothing said.
	unlock, err := imagesLock{root: root, how: syscall.LOCK_SH, docker: silent}.take(context.Background(), log)
	if err != nil || said.Len() != 0 {
		t.Fatalf("shared beside a shared holder: %v\n%s", err, said.String())
	}
	unlock()
	// Exclusive, never waiting: busy.
	if _, err := (imagesLock{root: root, how: syscall.LOCK_EX, now: true, docker: answers}).take(context.Background(), log); !errors.Is(err, errImagesBusy) || said.Len() != 0 {
		t.Errorf("now, held: %v\n%s", err, said.String())
	}
	// Exclusive, waiting: said, and ended by its bound.
	_, err = imagesLock{root: root, how: syscall.LOCK_EX, bound: 300 * time.Millisecond, docker: answers}.take(context.Background(), log)
	if err == nil || err.Error() != "the server's images still held by a release, a claim or a removal after 300ms" ||
		!strings.Contains(said.String(), "devopsy: waiting for the server's images, held by a release, a claim or a removal, up to 300ms\n") {
		t.Errorf("waited for: %v\n%s", err, said.String())
	}
	// Free, exclusive: taken while Docker answers, let go when it does not.
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if _, err := (imagesLock{root: root, how: syscall.LOCK_EX, now: true, docker: silent}).take(context.Background(), log); err == nil || err.Error() != "Docker is not answering" {
		t.Errorf("Docker not answering: %v", err)
	}
	if ok, err := tryFlock(held, syscall.LOCK_EX); !ok || err != nil {
		t.Fatalf("the lock kept with Docker not answering: %v", err)
	}
	_ = syscall.Flock(int(held.Fd()), syscall.LOCK_UN)
	unlock, err = imagesLock{root: root, how: syscall.LOCK_EX, now: true, docker: answers}.take(context.Background(), log)
	if err != nil {
		t.Fatalf("exclusive, free: %v", err)
	}
	if ok, _ := tryFlock(held, syscall.LOCK_SH); ok {
		t.Error("an exclusive take let a shared one in")
	}
	unlock()
}
