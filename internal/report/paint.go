package report

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"sync"
)

// The palette, one place for devopsy's own lines and the session lines
// Colorize sees.
const (
	reset    = "\033[0m"
	red      = "\033[0;31m"
	yellow   = "\033[0;33m"
	cyan     = "\033[0;36m"
	green    = "\033[0;32m"
	dim      = "\033[2m"
	bold     = "\033[1m"
	boldCyan = "\033[1;36m"
)

var (
	composeDone  = regexp.MustCompile(`^ (Container|Network|Image|Volume) .* (Started|Healthy|Created|Running|Built|Recreated|Unpaused|Resumed)\s*$`)
	composeStep  = regexp.MustCompile(`^ (Container|Network|Image|Volume) .* (Creating|Starting|Stopping|Stopped|Paused|Pausing|Removing|Removed|Waiting|Building|Recreate|Recreating|Exited|Killing|Killed)\s*$`)
	foreignError = regexp.MustCompile(`(?i)(^|\s)(error|ERROR|fatal|panic):?\s`)
)

// Paint colors one line for a terminal, by what it starts with, never by
// its words: devopsy's error red, warning yellow, info cyan, debug and
// trace dim, a Bar dim with its text plain and a command bold; Compose's
// progress green when done and dim on the way; another tool's error red.
// A line that carries colors already, and anything else, stays as it is.
func Paint(line string) string {
	if line == "" || strings.Contains(line, "\x1b[") {
		return line
	}
	switch {
	case strings.HasPrefix(line, Head(ErrorLevel)):
		return red + line + reset
	case strings.HasPrefix(line, Head(WarnLevel)):
		return yellow + line + reset
	case strings.HasPrefix(line, Head(DebugLevel)), strings.HasPrefix(line, Head(TraceLevel)):
		return dim + line + reset
	case line == strings.TrimRight(Bar, " ") || strings.HasPrefix(line, Bar):
		bar, text := line[:min(len(line), len(Bar))], line[min(len(line), len(Bar)):]
		if cmd := strings.TrimLeft(text, " 0123456789."); strings.HasPrefix(cmd, CmdMark) {
			lead := text[:len(text)-len(cmd)]
			return dim + bar + reset + lead + bold + cmd + reset
		}
		return dim + bar + reset + text
	case strings.HasPrefix(line, Prefix):
		return cyan + line + reset
	case composeDone.MatchString(line):
		return green + line + reset
	case composeStep.MatchString(line):
		return dim + line + reset
	case foreignError.MatchString(line):
		return red + line + reset
	}
	return line
}

// Colorize paints a session's lines for a terminal as they arrive
// (Paint): the lines devopsy on a server says, and Compose's around
// them. Partial lines (progress with carriage returns, a prompt) go
// through untouched; Flush writes what is left of a last line.
func Colorize(w io.Writer) *ColorWriter { return &ColorWriter{w: w} }

// ColorWriter is Colorize's writer.
type ColorWriter struct {
	w    io.Writer
	mu   sync.Mutex
	line []byte
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
	// A partial line that is not the start of something painted goes
	// through now: progress output, prompts.
	if len(c.line) > 0 && !bytes.HasPrefix(c.line, []byte(Prefix)) && !bytes.HasPrefix(c.line, []byte(" ")) && len(c.line) > len(Prefix) {
		if _, err := c.w.Write(c.line); err != nil {
			return n, err
		}
		c.line = nil
	}
	return n, nil
}

func (c *ColorWriter) emit(line []byte, newline bool) error {
	text := bytes.TrimRight(line, "\r")
	out := append([]byte(Paint(string(text))), line[len(text):]...)
	if newline {
		out = append(out, '\n')
	}
	_, err := c.w.Write(out)
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
