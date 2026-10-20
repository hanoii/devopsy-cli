package main

import (
	"bufio"
	"errors"
	"os"

	"github.com/hanoii/devopsy-cli/internal/report"
	"golang.org/x/term"
)

// stdinReader is shared by every prompt on stdin, so a buffered line is
// never lost between two.
var stdinReader = bufio.NewReader(os.Stdin)

// atTerminal says whether devopsy can ask on stdin.
func atTerminal() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// typeToGoOn says what (what follows, the risk) and asks to type name to
// go on: nothing (what was not done, "nothing removed") as the error when
// something else is typed. Without a terminal it fails, with --yes as the
// fix. tty asks on the terminal itself, when stdin carries data.
func typeToGoOn(what, name, nothing string, tty bool) error {
	noTerminal := &report.Failure{Msg: nothing + ": no terminal to confirm it on", Details: []string{what}, Fixes: []report.Remedy{report.Fix("add --yes to go on without asking")}}
	in, log := report.LineReader(stdinReader), report.Default()
	if tty {
		f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return noTerminal
		}
		defer f.Close()
		in, log = bufio.NewReader(f), log.To(f)
	} else if !atTerminal() {
		return noTerminal
	}
	log.Info(what)
	if !log.ConfirmName(in, name) {
		return errors.New(nothing)
	}
	return nil
}
