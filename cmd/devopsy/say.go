package main

import (
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// runSay implements the hidden --say <level> <message> [<detail>...]
// (remote.SayFlag): what the scripts on servers say, through report like
// everything devopsy says. A write that fails (the session gone) fails
// it, as it fails echo: a script relies on that before its switch, to
// notice a session that dropped without a signal. Never by SIGPIPE: an
// exit code the script decides on, as with echo under trap '' PIPE.
func runSay(args []string) int {
	lv, ok := remote.SayLevels[strings.Join(args[:min(1, len(args))], "")]
	if len(args) < 2 || !ok {
		report.Error("usage: devopsy " + remote.SayFlag + " trace|debug|info|warning|error <message> [<detail>...]")
		return 2
	}
	signal.Ignore(syscall.SIGPIPE)
	w := &failedWriter{w: os.Stderr}
	report.Default().To(w).Say(lv, args[1], args[2:], nil)
	if w.err != nil {
		return 1
	}
	return 0
}

// failedWriter remembers whether a write failed.
type failedWriter struct {
	w   io.Writer
	err error
}

func (f *failedWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err != nil {
		f.err = err
	}
	return n, err
}
