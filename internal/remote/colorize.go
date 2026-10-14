package remote

import (
	"bytes"
	"io"
	"regexp"
	"sync"
)

// Colorize colors a session's lines for a terminal, as they arrive:
// devopsy's own ("devopsy: ..." in cyan, red when they report a failure),
// Compose's progress (" Container x Started" green, the steps on the way
// dim) and anything saying error in red. Everything else, partial lines
// (progress with carriage returns) and lines already colored go through
// untouched. Flush writes what is left of a last line.
func Colorize(w io.Writer) *ColorWriter { return &ColorWriter{w: w} }

// ColorWriter is Colorize's writer.
type ColorWriter struct {
	w    io.Writer
	mu   sync.Mutex
	line []byte
}

var (
	colorDevopsy = regexp.MustCompile(`^devopsy: `)
	colorFailure = regexp.MustCompile(`(?i)\b(fail|failed|error|cannot|could not|refus|did not|interrupted|not found|denied|unhealthy|half copied|aborting)`)
	colorDone    = regexp.MustCompile(`^ (Container|Network|Image|Volume) .* (Started|Healthy|Created|Running|Built|Recreated|Unpaused|Resumed)\s*$`)
	colorStep    = regexp.MustCompile(`^ (Container|Network|Image|Volume) .* (Creating|Starting|Stopping|Stopped|Paused|Pausing|Removing|Removed|Waiting|Building|Recreate|Recreating|Exited|Killing|Killed)\s*$`)
	colorError   = regexp.MustCompile(`(?i)(^|\s)(error|ERROR|fatal|panic):?\s`)
)

const (
	colorReset = "\033[0m"
	colorCyan  = "\033[0;36m"
	colorRed   = "\033[0;31m"
	colorGreen = "\033[0;32m"
	colorDim   = "\033[2m"
)

// colorLine says what to wrap a line in, or nothing.
func colorLine(line []byte) string {
	if bytes.Contains(line, []byte("\x1b[")) {
		return ""
	}
	switch {
	case colorDevopsy.Match(line):
		if colorFailure.Match(line) {
			return colorRed
		}
		return colorCyan
	case colorDone.Match(line):
		return colorGreen
	case colorStep.Match(line):
		return colorDim
	case colorError.Match(line):
		return colorRed
	}
	return ""
}

func (c *ColorWriter) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			c.line = append(c.line, p...)
			break
		}
		line := append(c.line, p[:i]...)
		c.line = nil
		p = p[i+1:]
		if err := c.emit(line, true); err != nil {
			return n, err
		}
	}
	// A partial line that is not the start of something we color goes
	// through now: progress output, prompts.
	if len(c.line) > 0 && !bytes.HasPrefix(c.line, []byte("devopsy: ")) && !bytes.HasPrefix(c.line, []byte(" ")) && len(c.line) > 8 {
		if _, err := c.w.Write(c.line); err != nil {
			return n, err
		}
		c.line = nil
	}
	return n, nil
}

func (c *ColorWriter) emit(line []byte, newline bool) error {
	text := bytes.TrimRight(line, "\r")
	if color := colorLine(text); color != "" {
		out := append([]byte(color), text...)
		out = append(out, colorReset...)
		out = append(out, line[len(text):]...)
		line = out
	}
	if newline {
		line = append(line, '\n')
	}
	_, err := c.w.Write(line)
	return err
}

// Flush writes what is left of a last line without a newline.
func (c *ColorWriter) Flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.line) > 0 {
		_ = c.emit(c.line, false)
		c.line = nil
	}
}
