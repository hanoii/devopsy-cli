package remote

import "github.com/hanoii/devopsy-cli/internal/report"

// SayFlag is the hidden `devopsy --say <level> <message> [<detail>...]`:
// how scripts say things, formatted, colored and filtered as devopsy's
// own lines (report). Scripts call it after basePrelude, which makes sure
// the server's devopsy has it (Protocol 5).
const SayFlag = "--say"

// SayLevels are --say's levels, by name.
var SayLevels = map[string]report.Level{
	"trace":   report.TraceLevel,
	"debug":   report.DebugLevel,
	"info":    report.InfoLevel,
	"warning": report.WarnLevel,
	"error":   report.ErrorLevel,
}

// sayQuoted is a script's line saying msg, built in Go, at level lv
// through --say: quoted, so nothing in it (a step's $VAR, a quote,
// $(...)) is ever expanded or run on the server.
func sayQuoted(lv report.Level, msg string) string {
	name := lv.Tag()
	if name == "" {
		name = "info"
	}
	return "devopsy " + SayFlag + " " + name + " " + Quote(msg) + " >&2\n"
}

// echoLine is a script's command saying msg at level lv without the
// server's devopsy, for what runs before basePrelude knows it works, or
// without it (the --ssh-keys scripts): msg is shell text in double
// quotes, so its $f expands.
func echoLine(lv report.Level, msg string) string {
	return `echo "` + report.Head(lv) + msg + `" >&2`
}
