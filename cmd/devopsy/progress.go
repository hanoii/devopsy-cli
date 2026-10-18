package main

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// uploadProgress draws a release's upload on one line of a terminal,
// redrawn in place: the project's bytes read into the tar stream, which
// the upload session takes as it sends them (through a pipe), out of
// total. Never logged: the release log gets done's line.
type uploadProgress struct {
	w      io.Writer // nil: nothing drawn (not a terminal)
	color  string
	total  int64
	start  time.Time
	mu     sync.Mutex
	n      int64
	drawn  time.Time
	active bool
	// first and last are when bytes went in: the summary's time leaves
	// out the server's wait for the release lock before it reads.
	first, last time.Time
}

const progressEvery = 100 * time.Millisecond

func newUploadProgress(w io.Writer, color string, total int64) *uploadProgress {
	return &uploadProgress{w: w, color: color, total: total, start: time.Now()}
}

// add counts n bytes, from Pack's goroutine.
func (p *uploadProgress) add(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n += n
	if p.first.IsZero() {
		p.first = time.Now()
	}
	p.last = time.Now()
	if p.w == nil || time.Since(p.drawn) < progressEvery {
		return
	}
	p.drawn = time.Now()
	p.active = true
	pct := 100
	if p.total > 0 {
		pct = int(p.n * 100 / p.total)
	}
	line := fmt.Sprintf("Uploading %s of %s (%d%%), %s/s", humanBytes(p.n), humanBytes(p.total), pct, humanBytes(rate(p.n, time.Since(p.start))))
	if p.color != "" {
		line = p.color + line + "\033[0m"
	}
	fmt.Fprint(p.w, "\r\033[K"+line)
}

// clear removes the progress line, so what follows starts on a clean one.
func (p *uploadProgress) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active {
		fmt.Fprint(p.w, "\r\033[K")
		p.active = false
	}
	p.w = nil
}

// before is w, but the progress line goes for good before anything is
// written to it.
func (p *uploadProgress) before(w io.Writer) io.Writer {
	return clearingWriter{p, w}
}

type clearingWriter struct {
	p *uploadProgress
	w io.Writer
}

func (c clearingWriter) Write(b []byte) (int, error) {
	c.p.clear()
	return c.w.Write(b)
}

// done is the upload's summary line.
func (p *uploadProgress) done() string {
	p.clear()
	p.mu.Lock()
	d := p.last.Sub(p.first)
	p.mu.Unlock()
	return fmt.Sprintf("Uploaded %s in %s (%s/s).", humanBytes(p.total), d.Round(100*time.Millisecond), humanBytes(rate(p.total, d)))
}

func rate(n int64, d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return int64(float64(n) / d.Seconds())
}

// humanBytes is n in B, kB, MB or GB (powers of 1000, one decimal).
func humanBytes(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d B", n)
	}
	f := float64(n)
	for _, unit := range []string{"kB", "MB", "GB"} {
		f /= 1000
		if f < 1000 || unit == "GB" {
			return fmt.Sprintf("%.1f %s", f, unit)
		}
	}
	return ""
}
