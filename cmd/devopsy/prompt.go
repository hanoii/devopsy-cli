package main

import (
	"bufio"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"golang.org/x/term"
)

// stdinReader is shared by every prompt on stdin, so a buffered line is
// never lost between two.
var stdinReader = bufio.NewReader(os.Stdin)

// atTerminal says whether devopsy can ask on stdin.
func atTerminal() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// typed is the word a prompt asks to type and what it is, said with it ("this server's
// hostname"): a word alone reads as a token, not as the name it proves.
type typed struct{ of, word string }

// envWord is what a prompt that replaces or removes an environment's data asks to type: its
// path, <project>/[<instance>/]<environment>, the same as its directory under the release root.
func envWord(t *remote.Target) typed { return typed{"this environment's path", t.Path} }

// projectWord is a project's word to type, its name.
func projectWord(name string) typed { return typed{"the project's name", name} }

// typeToGoOn says what (what follows, the risk) with its details and asks
// to type the word to go on: nothing (what was not done, "nothing removed")
// as the error when something else is typed. Without a terminal it fails,
// with --yes as the fix. tty asks on the terminal itself, when stdin carries
// data.
func typeToGoOn(what string, details []string, word typed, nothing string, tty bool, hints ...report.Remedy) error {
	noTerminal := &report.Failure{Msg: nothing + ": no terminal to confirm it on", Details: append([]string{what}, details...), Remedies: append(hints, report.Fix("add --yes to go on without asking"))}
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
	log.Say(report.InfoLevel, what, details, hints...)
	if !log.ConfirmName(in, word.of, word.word) {
		return errors.New(nothing)
	}
	return nil
}

// readSecret asks for a secret on the terminal, never echoed: an interrupt while it reads puts
// the terminal back as it was before it ends devopsy, which a dead process would leave silent.
func readSecret(question string) (string, error) {
	fd := int(os.Stdin.Fd())
	state, err := term.GetState(fd)
	if err != nil {
		return "", err
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-sigs:
			_ = term.Restore(fd, state)
			report.Default().EndPrompt()
			os.Exit(130)
		case <-done:
		}
	}()
	log := report.Default()
	log.Prompt(question + ":")
	b, err := term.ReadPassword(fd)
	log.EndPrompt()
	return string(b), err
}
