package report

import (
	"io"
	"os"
	"strings"
)

// Logger writes what devopsy says: Lines, filtered by the verbosity,
// masked, painted when color is on, in one write per message so two
// writers never interleave its lines. Its methods never fail: what a
// message cannot reach is lost, never fatal.
type Logger struct {
	w       io.Writer
	color   bool
	verbose int
	mask    func(string) string
	tee     func(string)
}

// New is a Logger writing to w: painted when color, debug lines shown
// from verbose 1 (-v), trace lines from 2 (-vv).
func New(w io.Writer, color bool, verbose int) *Logger {
	return &Logger{w: w, color: color, verbose: verbose}
}

var std = New(os.Stderr, false, 0)

// Default is the Logger the package functions use, set once by main.
func Default() *Logger { return std }

// SetDefault makes l the Logger the package functions use.
func SetDefault(l *Logger) { std = l }

// WithMask is a copy of l that passes every line through mask first (a
// project's secrets).
func (l *Logger) WithMask(mask func(string) string) *Logger {
	c := *l
	c.mask = mask
	return &c
}

// Tee is a copy of l that also gives every line it writes, unpainted, to
// f (a release's log).
func (l *Logger) Tee(f func(line string)) *Logger {
	c := *l
	c.tee = f
	return &c
}

// To is a copy of l writing to w (the terminal itself, a session's
// colored writer).
func (l *Logger) To(w io.Writer) *Logger {
	c := *l
	c.w = w
	return &c
}

// Color says whether l paints.
func (l *Logger) Color() bool { return l.color }

// Verbose is l's verbosity: 0, 1 (-v) or 2 (-vv).
func (l *Logger) Verbose() int { return l.verbose }

// Shows says whether l writes lines of level lv.
func (l *Logger) Shows(lv Level) bool {
	switch lv {
	case TraceLevel:
		return l.verbose > 1
	case DebugLevel:
		return l.verbose > 0
	}
	return true
}

// Say writes msg, its details and its fixes at level lv (Lines).
func (l *Logger) Say(lv Level, msg string, details []string, fixes []Remedy) {
	if !l.Shows(lv) {
		return
	}
	lines := Lines(lv, msg, details, fixes)
	var b strings.Builder
	for _, line := range lines {
		if l.mask != nil {
			line = l.mask(line)
		}
		if l.tee != nil {
			l.tee(line)
		}
		if l.color {
			line = Paint(line)
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	_, _ = io.WriteString(l.w, b.String())
}

// Info says what devopsy does, or did.
func (l *Logger) Info(msg string, details ...string) { l.Say(InfoLevel, msg, details, nil) }

// Warn says something needs attention while the command goes on.
func (l *Logger) Warn(msg string, details ...string) { l.Say(WarnLevel, msg, details, nil) }

// Error says why the command fails.
func (l *Logger) Error(msg string, details ...string) { l.Say(ErrorLevel, msg, details, nil) }

// Fail says why the command fails, from err: its message, then the
// details and fixes a Failure in its chain carries, then more fixes.
func (l *Logger) Fail(err error, more ...Remedy) {
	fixes := Fixes(err)
	l.Say(ErrorLevel, err.Error(), Details(err), append(fixes[:len(fixes):len(fixes)], more...))
}

// Debug says what devopsy runs, shown with -v.
func (l *Logger) Debug(msg string, details ...string) { l.Say(DebugLevel, msg, details, nil) }

// Trace says the detail of what devopsy runs (scripts), shown with -vv.
func (l *Logger) Trace(msg string, details ...string) { l.Say(TraceLevel, msg, details, nil) }

// LineReader reads an answer: a bufio.Reader, shared by every prompt on
// one input so a buffered line is never lost.
type LineReader interface {
	ReadString(delim byte) (string, error)
}

// Prompt writes question as a prompt, bold cyan when painting, with no
// newline: the answer follows on the same line. For answers read some
// other way (a hidden one); Ask reads a line.
func (l *Logger) Prompt(question string) {
	line := Prefix + question + " "
	if l.mask != nil {
		line = l.mask(line)
	}
	if l.tee != nil {
		l.tee(strings.TrimRight(line, " "))
	}
	if l.color {
		line = boldCyan + line + reset
	}
	_, _ = io.WriteString(l.w, line)
}

// EndPrompt ends a prompt's line when the answer was read without echo
// (a hidden one), so what follows starts on a line of its own.
func (l *Logger) EndPrompt() { _, _ = io.WriteString(l.w, "\n") }

// Ask prompts question and reads one line from in, trimmed.
func (l *Logger) Ask(in LineReader, question string) string {
	l.Prompt(question)
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line)
}

// Confirm asks a yes or no question: true only for y or yes.
func (l *Logger) Confirm(in LineReader, question string) bool {
	a := strings.ToLower(l.Ask(in, question+" [y/N]"))
	return a == "y" || a == "yes"
}

// Input asks for a value: def when the answer is empty.
func (l *Logger) Input(in LineReader, question, def string) string {
	q := question + ":"
	if def != "" {
		q = question + " [" + def + "]:"
	}
	if a := l.Ask(in, q); a != "" {
		return a
	}
	return def
}

// ConfirmName asks to type name to go on: true only when it was.
func (l *Logger) ConfirmName(in LineReader, name string) bool {
	return l.Ask(in, "type "+name+" to go on:") == name
}

// Info says what devopsy does, or did, through Default.
func Info(msg string, details ...string) { std.Info(msg, details...) }

// Warn says something needs attention, through Default.
func Warn(msg string, details ...string) { std.Warn(msg, details...) }

// Error says why the command fails, through Default.
func Error(msg string, details ...string) { std.Error(msg, details...) }

// Fail says why the command fails, from err, through Default.
func Fail(err error, more ...Remedy) { std.Fail(err, more...) }

// Debug says what devopsy runs (-v), through Default.
func Debug(msg string, details ...string) { std.Debug(msg, details...) }

// Trace says the scripts devopsy runs (-vv), through Default.
func Trace(msg string, details ...string) { std.Trace(msg, details...) }
