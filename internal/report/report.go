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
	red   = "\033[0;31m"
	reset = "\033[0m"
)

// Fail writes err as devopsy reports a failure: "devopsy: [error] <err>",
// the tag red when color is on, then err's details and more, each
// indented on its own line.
func Fail(w io.Writer, color bool, err error, more ...string) {
	tag := "[error]"
	if color {
		tag = red + tag + reset
	}
	fmt.Fprintf(w, "devopsy: %s %s\n", tag, err.Error())
	for _, d := range append(Details(err), more...) {
		for _, line := range strings.Split(strings.TrimRight(d, "\n"), "\n") {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
}
