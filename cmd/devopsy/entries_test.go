package main

import (
	"bytes"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// A remedy's command in the status table copies as shown with an
// address in brackets quoted (report.Copyable), the whole command bold:
// the quote belongs inside the bold, so the command reads as one, never
// a closing quote after the reset escape.
func TestEntryTableQuotesBeforePainting(t *testing.T) {
	t.Parallel()
	es := []remote.StatusEntry{{Level: remote.StatusInfo, Key: "sync", Lines: []string{"none"},
		Remedies: []report.Remedy{report.Hint("from its parent:", "devopsy @dev --sync --from @[2001:db8::1]:prod")}}}
	var b bytes.Buffer
	newEntryTable(&b, style{on: true}, es).print(es)
	want := "sync  none\n" +
		"      from its parent:\n" +
		"      \033[1m$ devopsy @dev --sync --from @'[2001:db8::1]:prod'\033[0m\n"
	if b.String() != want {
		t.Errorf("a command ending in a bracketed address:\n%q\nwant:\n%q", b.String(), want)
	}
}
