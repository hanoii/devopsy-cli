package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestHumanBytes(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 999: "999 B", 1000: "1.0 kB", 1536000: "1.5 MB", 2_500_000_000: "2.5 GB", 3_000_000_000_000: "3000.0 GB"} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestUploadProgress(t *testing.T) {
	var out bytes.Buffer
	p := newUploadProgress(&out, "", 2000)
	p.add(1000)
	if got := out.String(); !strings.Contains(got, "\r\033[KUploading 1.0 kB of 2.0 kB (50%)") {
		t.Fatalf("progress line: %q", got)
	}
	p.add(1000) // within progressEvery: not drawn
	if strings.Count(out.String(), "Uploading") != 1 {
		t.Fatalf("drawn too often: %q", out.String())
	}
	if done := p.done(); !strings.HasPrefix(done, "Uploaded 2.0 kB in ") {
		t.Fatalf("summary: %q", done)
	}
	if !strings.HasSuffix(out.String(), "\r\033[K") {
		t.Fatalf("line not cleared: %q", out.String())
	}
	p.add(10) // after done: nothing drawn
	if strings.Count(out.String(), "Uploading") != 1 {
		t.Fatalf("drawn after done: %q", out.String())
	}

	// Not a terminal: nothing drawn, the summary only.
	q := newUploadProgress(nil, "", 10)
	q.add(10)
	if done := q.done(); !strings.HasPrefix(done, "Uploaded 10 B in ") {
		t.Fatalf("summary: %q", done)
	}
}
