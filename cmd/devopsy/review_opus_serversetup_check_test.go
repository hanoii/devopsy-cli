package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The checks again after an apply keep it honest: a live buffer the root run could not raise
// (sysctl refusing, as in a container that may not write it) fails the apply, naming the key,
// its value and the reboot that loads the file, while the file itself is put.
func TestReviewOpusApplyFailsWhatDidNotHold(t *testing.T) {
	t.Parallel()
	s, _ := setupServer(t)
	write(t, filepath.Join(filepath.Dir(s.root), "bin", "sysctl"), "#!/bin/sh\nexit 1\n", 0o755)
	out, code := s.run(nil, "@devopsy@one:", "--server-setup", "--yes")
	if code != 1 || !strings.Contains(out, "error: what was applied did not hold") ||
		!strings.Contains(out, "net.core.rmem_max is 212992, below 7500000, until a reboot loads /etc/sysctl.d/90-devopsy-net.conf") ||
		!strings.Contains(out, "put /etc/sysctl.d/90-devopsy-net.conf") {
		t.Errorf("(%d):\n%s", code, out)
	}
}
