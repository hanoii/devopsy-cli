package remote

import (
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// Bug (review, kept failing): IsAddress takes a server holding a newline or another control
// character (shellSpecial has none of them), so a session's forged address passes
// SessionFrom, and report.Lines splits the offered command at the newline before Copyable
// escapes anything: the door's warning shows a line of the caller's choosing after the
// command, never escaped. An address holds no control character, so none should pass.
func TestReviewSessionAddressHoldsNoControl(t *testing.T) {
	t.Parallel()
	forged := "vm1\nhost:prod"
	s := SessionFrom(getenv(map[string]string{AddressEnv: forged, HintAddressEnv: forged}))
	if s.Hint != "" {
		lines := report.Lines(report.WarnLevel, "a warning", nil, []report.Remedy{report.Fix("release again:", s.Releasing("--release"))})
		t.Errorf("a forged address with a newline taken, its command shown as:\n%s", strings.Join(lines, "\n"))
	}
	for _, a := range []string{"vm1\x1b:prod", "vm1\r:prod", "devopsy@vm1\x7f:prod"} {
		if IsAddress(a) {
			t.Errorf("%q taken as an address", a)
		}
	}
}
