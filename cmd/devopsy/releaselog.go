package main

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// maxReleaseLog caps a release log: a runaway build log stays on the
// screen, not on the server.
const maxReleaseLog = 8 << 20

// releaseLog records a release or rollback as it runs: devopsy's own
// messages, the local steps' command lines and results, and the server
// session's output, each line with the time it started. It is saved on the
// server at the end (logs/), for devopsy @<target> --log. Local steps' own
// output stays on the screen: piping it would take their terminal away
// (docker's progress, colors).
type releaseLog struct {
	mu    sync.Mutex
	lines []logLine
	// cur is the line being written, started at start.
	cur   bytes.Buffer
	start time.Time
	size  int
	full  bool
}

type logLine struct {
	at   time.Time
	text string
}

// logTime is a log line's time, in UTC (the header says so).
const logTime = "2006-01-02 15:04:05"

func newReleaseLog(what string, t *remote.Target) *releaseLog {
	l := &releaseLog{}
	l.Line(fmt.Sprintf("devopsy %s: %s of %s (%s:%s) by %s, times in UTC", version, what, t.Address, t.Host, t.Path,
		remote.Whoami()))
	return l
}

func (l *releaseLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size+len(p) > maxReleaseLog {
		if !l.full {
			l.flush()
			l.lines = append(l.lines, logLine{time.Now(), "[devopsy: log truncated]"})
			l.full = true
		}
		return len(p), nil
	}
	n := len(p)
	l.size += n
	now := time.Now()
	for len(p) > 0 {
		if l.cur.Len() == 0 {
			l.start = now
		}
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			l.cur.Write(p)
			break
		}
		l.cur.Write(p[:i])
		l.flush()
		p = p[i+1:]
	}
	return n, nil
}

// flush ends the current line.
func (l *releaseLog) flush() {
	l.lines = append(l.lines, logLine{l.start, l.cur.String()})
	l.cur.Reset()
}

// Line records one line of devopsy's own.
func (l *releaseLog) Line(s string) {
	_, _ = l.Write([]byte(s + "\n"))
}

// rollingBackTo is the line a rollback's script prints once it has picked
// the release to restore.
var rollingBackTo = regexp.MustCompile(`devopsy: rolling back to ([0-9]{14})`)

// terminalCodes are colors and cursor movements, which make no sense in a
// file.
var terminalCodes = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[()][0-9A-Za-z]`)

// spinner is a line of a progress display still running (compose's
// "⠋ Container ... Starting"): only its final state (✔, ✘) is kept.
var spinner = regexp.MustCompile(`^\s*[\x{2800}-\x{28FF}]`)

// Text is the log as plain text, each line after its time: without
// terminal codes, with only the last state of lines a progress display
// rewrote, and without the empty lines its redraws leave.
func (l *releaseLog) Text() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	lines := l.lines
	if l.cur.Len() > 0 {
		lines = append(lines[:len(lines):len(lines)], logLine{l.start, l.cur.String()})
	}
	var b strings.Builder
	for _, line := range lines {
		text := strings.TrimRight(terminalCodes.ReplaceAllString(line.text, ""), "\r")
		if j := strings.LastIndex(text, "\r"); j >= 0 {
			text = text[j+1:]
		}
		text = strings.TrimRight(text, " \t")
		if strings.TrimSpace(text) == "" || spinner.MatchString(text) {
			continue
		}
		b.WriteString(line.at.UTC().Format(logTime) + " " + text + "\n")
	}
	return []byte(b.String())
}
