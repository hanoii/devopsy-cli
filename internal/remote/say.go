package remote

import "github.com/hanoii/devopsy-cli/internal/report"

// SayFlag is the hidden `devopsy --say <level> <message> [<detail>...]
// [--fix|--hint <text> [<command>...]]...`: how scripts say things,
// formatted and colored as devopsy's own lines (report). Scripts call it
// after basePrelude, which makes sure the server's devopsy has it (the
// say feature).
const SayFlag = "--say"

// SaySinceFlag adds to --say's message the time since a script's clock
// (timerStart).
const SaySinceFlag = "--since"

// SayFixFlag and SayHintFlag start a remedy in --say's words, a fix
// (report.Fix) or a hint (report.Hint): the next word its text (empty
// for none), the words up to the next one its commands. The first ends
// the details (sayRemedies).
const (
	SayFixFlag  = "--fix"
	SayHintFlag = "--hint"
)

// SayLevels are --say's levels, by name.
var SayLevels = map[string]report.Level{
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

// timerStart is a script's line starting a clock in the variable name.
// A date that fails leaves it empty, and the line that says the time goes
// without one.
func timerStart(name string) string {
	return name + "=$(date +%s) || " + name + "=\n"
}

// sayTook is a script's line saying msg, a shell word, with the time since
// timerStart's clock in name. It never fails the script, before or past
// the switch: a time decides nothing, and later lines notice a session
// gone.
func sayTook(name, msg string) string {
	return "devopsy " + SayFlag + " info " + SaySinceFlag + ` "${` + name + `:-}" ` + msg + " || true\n"
}

// echoLine is a script's command saying msg at level lv, with its
// remedies, without the server's devopsy, for what runs before
// basePrelude knows it works, or without it (the --ssh-keys scripts):
// each line report lays out is shell text in double quotes, so its $f
// expands.
func echoLine(lv report.Level, msg string, remedies ...report.Remedy) string {
	s := "printf '%s\\n'"
	for _, l := range report.Lines(lv, msg, nil, remedies) {
		s += ` "` + l + `"`
	}
	return s + " >&2"
}
