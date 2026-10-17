package remote

import (
	"bytes"
	"io"
	"strings"
	"sync"
)

// LineMarker passes a session's output through to its writers, but takes
// out the first line starting with Prefix (a marker a script prints for
// devopsy, never for people) and calls Found with the rest of it. Its
// writers (Writer) share that state, so a marker that may come on either
// stream (stdout, or stderr once a script follows its output from a file)
// is found once. With Cut, nothing after the marker goes through:
// SwitchMarker's for the release log, which the server has from there on.
type LineMarker struct {
	Prefix string
	Cut    bool
	Found  func(rest string)

	mu    sync.Mutex
	found bool
}

// Writer is one stream through m into w.
func (m *LineMarker) Writer(w io.Writer) *MarkerWriter {
	return &MarkerWriter{m: m, w: w}
}

// MarkerWriter is one stream of a LineMarker.
type MarkerWriter struct {
	m    *LineMarker
	w    io.Writer
	line []byte
}

func (mw *MarkerWriter) Write(p []byte) (int, error) {
	m := mw.m
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(p)
	if m.found {
		if m.Cut {
			return n, nil
		}
		_, err := mw.w.Write(p)
		return n, err
	}
	for len(p) > 0 && !m.found {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			mw.line = append(mw.line, p...)
			p = nil
			break
		}
		line := append(mw.line, p[:i]...)
		mw.line = nil
		p = p[i+1:]
		if rest, ok := strings.CutPrefix(strings.TrimRight(string(line), "\r"), m.Prefix); ok {
			m.found = true
			if m.Found != nil {
				m.Found(rest)
			}
			continue
		}
		if _, err := mw.w.Write(append(line, '\n')); err != nil {
			return n, err
		}
	}
	// A partial line that cannot become the marker goes through now
	// (progress output); the rest after the marker as it comes.
	if len(mw.line) > 0 && !strings.HasPrefix(m.Prefix, string(mw.line)) && !bytes.HasPrefix(mw.line, []byte(m.Prefix)) {
		if _, err := mw.w.Write(mw.line); err != nil {
			return n, err
		}
		mw.line = nil
	}
	if m.found && len(p) > 0 && !m.Cut {
		if _, err := mw.w.Write(p); err != nil {
			return n, err
		}
	}
	return n, nil
}

// Flush writes what is left of a last line without a newline.
func (mw *MarkerWriter) Flush() {
	mw.m.mu.Lock()
	defer mw.m.mu.Unlock()
	if len(mw.line) > 0 && !(mw.m.found && mw.m.Cut) {
		_, _ = mw.w.Write(mw.line)
	}
	mw.line = nil
}
