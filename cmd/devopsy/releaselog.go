package main

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// maxReleaseLog caps the lines a release log keeps, a progress
// display's redrawn lines not counted: a runaway build log stays on the
// screen, not on the server. Past it only devopsy's own lines are kept.
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
	// size is what lines holds, in bytes; full is past maxReleaseLog.
	size int
	full bool
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
	n := len(p)
	now := time.Now()
	for len(p) > 0 {
		if l.cur.Len() == 0 {
			l.start = now
		}
		i := bytes.IndexByte(p, '\n')
		line := p
		if i >= 0 {
			line = p[:i]
		}
		// A line with no end holds no more than the cap.
		if room := maxReleaseLog - l.cur.Len(); len(line) > room {
			line = line[:max(room, 0)]
		}
		l.cur.Write(line)
		if i < 0 {
			break
		}
		l.flush(now)
		p = p[i+1:]
	}
	return n, nil
}

// flush ends the current line: it drops the lines a progress display's
// redraw supersedes, then keeps the line while the log is under the cap,
// and past it only a line of devopsy's own.
func (l *releaseLog) flush(now time.Time) {
	text := l.cur.String()
	l.cur.Reset()
	if !l.full {
		var dropped []logLine
		l.lines, dropped, text = redraw(l.lines, text)
		for _, d := range dropped {
			l.size -= len(d.text)
		}
		if l.size+len(text) <= maxReleaseLog {
			l.size += len(text)
			l.lines = append(l.lines, logLine{l.start, text})
			return
		}
		l.full = true
		l.lines = append(l.lines, logLine{now, "[devopsy: log truncated]"})
	}
	if strings.HasPrefix(terminalCodes.ReplaceAllString(text, ""), report.Prefix) {
		l.lines = append(l.lines, logLine{l.start, text})
	}
}

// Line records one line of devopsy's own.
func (l *releaseLog) Line(s string) {
	_, _ = l.Write([]byte(s + "\n"))
}

// rollingBackTo is the line a rollback's script prints once it has picked
// the release to restore, or the one finishing a cut rollback.
var rollingBackTo = regexp.MustCompile(regexp.QuoteMeta(report.Prefix) + `(?:rolling back|finishing the rollback) to ([0-9]{14})`)

// terminalCodes are colors and cursor movements, which make no sense in a
// file.
var terminalCodes = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[()][0-9A-Za-z]`)

// spinner is a line of a progress display still running (compose's
// "⠋ Container ... Starting"): only its final state (✔, ✘) is kept.
var spinner = regexp.MustCompile(`^\s*[\x{2800}-\x{28FF}]`)

// cursorUp is a progress display (BuildKit's, on a terminal) going back
// up n lines to redraw them: those n lines are superseded.
var cursorUp = regexp.MustCompile(`\x1b\[([0-9]*)A`)

// redraw applies a line's moves up (cursorUp) to the lines before it: it
// returns them without the ones it supersedes, those it dropped, and the
// line without the moves.
func redraw(before []logLine, text string) (kept, dropped []logLine, rest string) {
	for _, m := range cursorUp.FindAllStringSubmatch(text, -1) {
		n := 1
		if m[1] != "" {
			n, _ = strconv.Atoi(m[1])
		}
		n = min(n, len(before))
		dropped = append(dropped, before[len(before)-n:]...)
		before = before[:len(before)-n]
	}
	return before, dropped, cursorUp.ReplaceAllString(text, "")
}

// Text is the log as plain text, each line after its time: without
// terminal codes, with only the last state of lines a progress display
// rewrote, and without the empty lines its redraws leave.
func (l *releaseLog) Text() []byte {
	l.mu.Lock()
	lines := l.lines
	if l.cur.Len() > 0 {
		lines = append(lines[:len(lines):len(lines)], logLine{l.start, l.cur.String()})
	}
	l.mu.Unlock()
	var b strings.Builder
	for _, line := range cleanLines(lines) {
		b.WriteString(line.at.UTC().Format(logTime) + " " + line.text + "\n")
	}
	return []byte(b.String())
}

// cleanLines are lines as a file shows them: without terminal codes, with
// only the last state of lines a progress display rewrote, and without
// the empty lines its redraws leave, nor a spinner's.
func cleanLines(lines []logLine) []logLine {
	// Frames: a line that moves the cursor up n lines redraws them, so
	// those n go, empty ones included (hence they are dropped only below).
	var out []logLine
	for _, line := range lines {
		var text string
		out, _, text = redraw(out, line.text)
		text = strings.TrimRight(terminalCodes.ReplaceAllString(text, ""), "\r")
		if j := strings.LastIndex(text, "\r"); j >= 0 {
			text = text[j+1:]
		}
		out = append(out, logLine{line.at, strings.TrimRight(text, " \t")})
	}
	kept := out[:0]
	for _, line := range out {
		if strings.TrimSpace(line.text) != "" && !spinner.MatchString(line.text) {
			kept = append(kept, line)
		}
	}
	return kept
}

// joinLogs is --log's view of what LogReadScript printed: the session's
// log (remote.LogFile) with each part of the server's (remote.
// ServerLogFile) after the anchor with its tag (remote.LogAnchor, the
// tag shown nowhere), cleaned like the session's and indented. Parts no
// anchor claims (an operator gone before saving its log) follow at the
// end, under their own time. The two never overlap: the session's log
// stops at the switch (remote.SwitchMarker).
func joinLogs(out []byte) []byte {
	session, server, _ := strings.Cut(string(out), remote.LogServerPart+"\n")
	type part struct {
		at, tag string
		lines   []logLine
		shown   bool
	}
	var parts []*part
	byTag := map[string]*part{}
	for _, line := range strings.Split(strings.TrimSuffix(server, "\n"), "\n") {
		if head, ok := strings.CutPrefix(line, remote.LogSection); ok {
			p := &part{at: head}
			if len(head) > len(logTime) {
				p.at, p.tag = head[:len(logTime)], strings.TrimSpace(head[len(logTime):])
				byTag[p.tag] = p
			}
			parts = append(parts, p)
		} else {
			// Lines before any section (a detached step's, written ahead
			// of the activation's) are a part of their own, at the first
			// section's time, never dropped (review: a release snapshot's
			// lines were in no --log).
			if len(parts) == 0 {
				parts = append(parts, &part{})
			}
			p := parts[len(parts)-1]
			p.lines = append(p.lines, logLine{text: line})
		}
	}
	if len(parts) > 1 && parts[0].at == "" {
		parts[0].at = parts[1].at
	}
	indent := strings.Repeat(" ", len(logTime)+1)
	var b strings.Builder
	put := func(p *part) {
		p.shown = true
		for _, line := range cleanLines(p.lines) {
			b.WriteString(indent + line.text + "\n")
		}
	}
	for _, line := range strings.SplitAfter(session, "\n") {
		if line == "" {
			continue
		}
		if len(line) > len(logTime) {
			if tag, ok := strings.CutPrefix(strings.TrimRight(line[len(logTime)+1:], "\n"), remote.LogAnchor); ok {
				b.WriteString(line[:len(logTime)+1] + remote.LogAnchor + "\n")
				if p := byTag[strings.TrimSpace(tag)]; p != nil && !p.shown {
					put(p)
				}
				continue
			}
		}
		b.WriteString(line)
	}
	for _, p := range parts {
		if !p.shown {
			b.WriteString(p.at + " " + remote.LogAnchor + "\n")
			put(p)
		}
	}
	return []byte(b.String())
}
