// Package report formats what devopsy says about failures: one line
// naming the error, then its details, each on its own line. Errors built
// here carry their details through wrapping (errors.As), so a command
// prints them without knowing where they came from.
package report

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// Error is a failure with details: what to do about it, or why. Error()
// is the message alone, so wrapping keeps the usual "context: message"
// shape; Fail prints the details under it.
type Error struct {
	Msg     string
	Details []string
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

const (
	red    = "\033[0;31m"
	yellow = "\033[0;33m"
	reset  = "\033[0m"
)

// Fail writes err as devopsy reports a failure: "[error] <err>", red
// when color is on, then err's details and more, one per line, plain.
// Lines are never wrapped: a detail is one logical line (a command to
// copy); the terminal wraps it on screen.
func Fail(w io.Writer, color bool, err error, more ...string) {
	write(w, color, red, "[error] "+err.Error(), append(Details(err), more...))
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
