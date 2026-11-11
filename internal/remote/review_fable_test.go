package remote

import (
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// An address a session carries holds no control character: the door's caller sets it, and
// it is one word of a command offered to copy, on one line.
//
// Bug (review): IsAddress takes a server with a newline, a carriage return or an escape in
// it (ParseServer's shellSpecial lists none), so a forged hint address is offered as a
// command cut over two lines, the second any text after the bar; the other control
// characters only show escaped (report.Copyable), which is no address to copy either.
func TestSessionAddressHoldsNoControlCharacter(t *testing.T) {
	t.Parallel()
	for _, a := range []string{"vm1\nsecond line:prod", "vm1\nx:prod", "vm1\rx:prod", "vm1\x1b[2Jx:prod", "devopsy@vm1\x00:prod"} {
		if IsAddress(a) {
			t.Errorf("%q is taken as an address", a)
		}
		s := SessionFrom(getenv(map[string]string{AddressEnv: "prod", HintAddressEnv: a}))
		if cmd := s.Releasing("--release"); cmd != "" {
			lines := report.Lines(report.WarnLevel, "a warning", nil, []report.Remedy{report.Fix("release again:", cmd)})
			t.Errorf("%q is offered as a command:\n%s", a, strings.Join(lines, "\n"))
		}
	}
}
