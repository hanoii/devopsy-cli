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

// runSay implements the hidden --say <level> [--since <epoch>] <message>
// [<detail>...] (remote.SayFlag): what the scripts on servers say, through
// report like everything devopsy says, --since adding the time since a
// clock the script started, which no process of its own can hold. A write that fails (the session gone) fails
// it, as it fails echo: a script relies on that before its switch, to
// notice a session that dropped without a signal. Never by SIGPIPE: an
// exit code the script decides on, as with echo in a shell ignoring
// SIGPIPE.
func runSay(args []string) int {
	lv, ok := remote.SayLevels[strings.Join(args[:min(1, len(args))], "")]
	since, timed := "", len(args) > 1 && args[1] == remote.SaySinceFlag
	if timed && len(args) > 2 {
		since = args[2]
		args = append(args[:1:1], args[3:]...)
	}
	if len(args) < 2 || !ok {
		report.Error("usage: devopsy " + remote.SayFlag + " info|warning|error [" + remote.SaySinceFlag + " <epoch>] <message> [<detail>...] [" + remote.SayFixFlag + "|" + remote.SayHintFlag + " <text> [<command>...]]...")
		return 2
	}
	if timed {
		args[1] = report.SinceEpoch(args[1], since)
	}
	details, remedies := sayRemedies(args[2:])
	signal.Ignore(syscall.SIGPIPE)
	w := &failedWriter{w: os.Stderr}
	report.Default().To(w).Say(lv, args[1], details, remedies...)
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

// sayRemedies splits --say's words after its message: the details, up
// to the first --fix or --hint, then each remedy, its first word its
// text and the next words its commands (remote.SayFixFlag).
func sayRemedies(words []string) (details []string, remedies []report.Remedy) {
	for i := 0; i < len(words); i++ {
		w := words[i]
		if w != remote.SayFixFlag && w != remote.SayHintFlag {
			if len(remedies) == 0 {
				details = append(details, w)
			} else {
				last := &remedies[len(remedies)-1]
				last.Cmds = append(last.Cmds, w)
			}
			continue
		}
		r := report.Remedy{Hint: w == remote.SayHintFlag}
		if i+1 < len(words) {
			i++
			r.Text = words[i]
		}
		remedies = append(remedies, r)
	}
	return details, remedies
}
