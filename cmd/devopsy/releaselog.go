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
// session's output. It is saved on the server at the end (logs/), for
// devopsy @<target> --log. Local steps' own output stays on the screen:
// piping it would take their terminal away (docker's progress, colors).
type releaseLog struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	full bool
}

func newReleaseLog(what string, t *remote.Target) *releaseLog {
	l := &releaseLog{}
	l.Line(fmt.Sprintf("devopsy %s: %s of %s (%s:%s) by %s, %s", version, what, t.Address, t.Host, t.Path,
		remote.Whoami(), time.Now().UTC().Format(time.RFC3339)))
	return l
}

func (l *releaseLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buf.Len()+len(p) > maxReleaseLog {
		if !l.full {
			l.buf.WriteString("\n[devopsy: log truncated]\n")
			l.full = true
		}
		return len(p), nil
	}
	return l.buf.Write(p)
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

// Text is the log as plain text: without terminal codes, and with only
// the last state of lines a progress display rewrote with \r.
func (l *releaseLog) Text() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	lines := strings.Split(terminalCodes.ReplaceAllString(l.buf.String(), ""), "\n")
	for i, line := range lines {
		line = strings.TrimRight(line, "\r")
		if j := strings.LastIndex(line, "\r"); j >= 0 {
			line = line[j+1:]
		}
		lines[i] = line
	}
	return []byte(strings.Join(lines, "\n"))
}
