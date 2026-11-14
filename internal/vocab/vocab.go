// Package vocab is the words of devopsy's own terms, each written here once: a message, a help
// text or a howto topic splices a term's form where it is written (for example "run it " +
// vocab.Operator.From + ", by this server's address:"), so a term reworded is an edit here and
// report still gets finished words. A term owns its forms and a sentence names the form it
// needs, as Mozilla Fluent's terms with variants do. Each form is one literal, holding no ", $,
// backtick or backslash: it goes into a script's double-quoted text as it is.
//
// Rules: the Output conventions in AGENTS.md.
package vocab

import "strings"

// Place is a term for where something runs, in the forms a sentence needs.
type Place struct {
	// Noun names it: "checked from <Noun>".
	Noun string
	// On is where something is or runs: "runs only <On>".
	On string
	// From is where something is done from: "run it <From>".
	From string
	// Poss is what is its own, beside another's: "the server's 1.2.0, <Poss> 1.3.0".
	Poss string
}

// Operator is the machine devopsy is run from against a server, a laptop or a CI runner.
var Operator = Place{
	Noun: "the operator",
	On:   "on the operator's machine",
	From: "from the operator's machine",
	Poss: "the operator's",
}

// NoTarget is how a command runs when it names no target.
const NoTarget = "without a target"

// Cap is form with its first letter in upper case, for a sentence's start.
func Cap(form string) string {
	if form == "" {
		return ""
	}
	return strings.ToUpper(form[:1]) + form[1:]
}
