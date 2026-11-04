package report

import (
	"fmt"
	"strconv"
	"time"
)

// Timer times a stretch that is likely to take long, for the line that
// says it ended. Those lines are new ones, never a line the code or --log
// goes by, and only a stretch that went through says one: a failure says
// its error, and no time.
type Timer struct {
	l     *Logger
	start time.Time
}

// Timer starts a clock, saying nothing: for a stretch whose first line
// is said further in.
func (l *Logger) Timer() *Timer { return &Timer{l: l, start: time.Now()} }

// Start says msg and its details and starts a clock.
func (l *Logger) Start(msg string, details ...string) *Timer {
	l.Info(msg, details...)
	return l.Timer()
}

// Stop says msg with the time since the clock started.
func (t *Timer) Stop(msg string, details ...string) {
	t.l.Info(WithTook(msg, time.Since(t.start)), details...)
}

// Restart says msg with the time since the clock started, and starts it
// again.
func (t *Timer) Restart(msg string, details ...string) {
	t.Stop(msg, details...)
	t.start = time.Now()
}

// Start says msg and starts a clock, through Default.
func Start(msg string, details ...string) *Timer { return std.Start(msg, details...) }

// Took is d as a line says it: "<1s", "12s", "1m12s", "1h02m".
func Took(d time.Duration) string {
	s := int64(d / time.Second)
	switch {
	case s < 1:
		return "<1s"
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm", s/3600, s%3600/60)
}

// WithTook is msg followed by d in parentheses.
func WithTook(msg string, d time.Duration) string { return msg + " (" + Took(d) + ")" }

// SinceEpoch is msg with the time since epoch, a clock a script started
// (`date +%s`) in another process. A clock that is no number leaves msg
// as it is: a time is never worth a failure.
func SinceEpoch(msg, epoch string) string {
	s, err := strconv.ParseInt(epoch, 10, 64)
	if err != nil {
		return msg
	}
	return WithTook(msg, max(time.Since(time.Unix(s, 0)), 0))
}
