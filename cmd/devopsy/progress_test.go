package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestHumanBytes(t *testing.T) {
	t.Parallel()
	for n, want := range map[int64]string{0: "0 B", 999: "999 B", 1000: "1.0 kB", 1536000: "1.5 MB", 2_500_000_000: "2.5 GB", 3_000_000_000_000: "3000.0 GB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestUploadProgress(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	p := newUploadProgress(&out, false, 2000)
	p.add(1000)
	if got := out.String(); !strings.Contains(got, "\r\033[Kdevopsy: uploading 1.0 kB of 2.0 kB (50%)") {
		t.Fatalf("progress line: %q", got)
	}
	p.add(1000) // within progressEvery: not drawn
	if strings.Count(out.String(), "uploading") != 1 {
		t.Fatalf("drawn too often: %q", out.String())
	}
	if done := p.done("20261019214911"); !strings.HasPrefix(done, "uploaded 20261019214911: 2.0 kB in ") {
		t.Fatalf("summary: %q", done)
	}
	if !strings.HasSuffix(out.String(), "\r\033[K") {
		t.Fatalf("line not cleared: %q", out.String())
	}
	p.add(10) // after done: nothing drawn
	if strings.Count(out.String(), "uploading") != 1 {
		t.Fatalf("drawn after done: %q", out.String())
	}

	// Not a terminal: nothing drawn, the summary only.
	q := newUploadProgress(nil, false, 10)
	q.add(10)
	if done := q.done("1"); !strings.HasPrefix(done, "uploaded 1: 10 B in ") {
		t.Fatalf("summary: %q", done)
	}
}

// What the server says during the upload starts on its own line, and
// the progress line is not drawn again after it.
func TestUploadProgressBefore(t *testing.T) {
	t.Parallel()
	var term, said bytes.Buffer
	p := newUploadProgress(&term, false, 100)
	p.add(10)
	_, _ = p.before(&said).Write([]byte("devopsy: upgrade it\n"))
	p.drawn = time.Time{}
	p.add(10)
	if got := term.String(); !strings.HasSuffix(got, "\r\033[K") || strings.Count(got, "uploading") != 1 || said.String() != "devopsy: upgrade it\n" {
		t.Errorf("terminal %q, said %q", got, said.String())
	}
}
