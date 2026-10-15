// Package report formats what devopsy says about failures: one line
// naming the error, then its details (why), each on its own line, then
// the ways to fix it. Errors built here carry their details and fixes
// through wrapping (errors.As), so a command prints them without knowing
// where they came from, and adds the fixes only it knows.
package report

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// Error is a failure with details (why it happens) and fixes (what to
// do about it). Error() is the message alone, so wrapping keeps the usual
// "context: message" shape; Fail prints the rest under it.
type Error struct {
	Msg     string
	Details []string
	Fixes   []string
}

func (e *Error) Error() string { return e.Msg }

// Errorf is an Error with a formatted message and details.
func Errorf(details []string, format string, args ...any) *Error {
	return &Error{Msg: fmt.Sprintf(format, args...), Details: details}
}

// Details are err's, when an Error is in its chain.
func Details(err error) []string {
	var e *Error
	if errors.As(err, &e) {
		return e.Details
	}
	return nil
}

// Fixes are err's, when an Error is in its chain.
func Fixes(err error) []string {
	var e *Error
	if errors.As(err, &e) {
		return e.Fixes
	}
	return nil
}

const (
	red    = "\033[0;31m"
	yellow = "\033[0;33m"
	reset  = "\033[0m"
)

// Fail writes err as devopsy reports a failure: "[error] <err>", red
// when color is on, then err's details, one per line, plain, then its
// fixes and more: "To fix it:" and the one fix, or "To fix it, do one
// of these:" and each numbered. A fix's own lines (commands to copy)
// follow it as they are. Lines are never wrapped or indented: a detail
// is one logical line; the terminal wraps it on screen.
func Fail(w io.Writer, color bool, err error, more ...string) {
	lines := Details(err)
	switch fixes := append(Fixes(err), more...); len(fixes) {
	case 0:
	case 1:
		lines = append(lines, "To fix it:", fixes[0])
	default:
		lines = append(lines, "To fix it, do one of these:")
		for i, f := range fixes {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, f))
		}
	}
	write(w, color, red, "[error] "+err.Error(), lines)
}

// Warn writes a warning the same way: "[warning] <msg>", yellow.
func Warn(w io.Writer, color bool, msg string, details ...string) {
	write(w, color, yellow, "[warning] "+msg, details)
}

func write(w io.Writer, color bool, code, line string, details []string) {
	if color {
		line = code + line + reset
	}
	fmt.Fprintln(w, line)
	for _, d := range details {
		fmt.Fprintln(w, strings.TrimRight(d, "\n"))
	}
}
