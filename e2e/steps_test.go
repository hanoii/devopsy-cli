//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// Remote steps' arguments with quotes and $ reach the command as
// written, on the server's pseudo-terminal (with_tty, the operator at a
// terminal: before and prepare before the fetch, after under setsid)
// and without one; on a terminal they get one.
func TestStepArguments(t *testing.T) {
	step := `a'b $HOME x"y`
	dir := project(t, "stepargs", map[string]string{
		".devopsy/config.yaml": "project: stepargs\nenvironments:\n  prod:\n    mode: image\n    release:\n      before:\n        - remote: args before " + step + "\n      prepare:\n        - args prepare " + step + "\n      run: deploy\n      after:\n        - remote: args after " + step + "\n    rollback: deploy\n",
		".devopsy/compose.yaml":    "services:\n  app:\n    image: busybox:1.37\n    command: [sleep, infinity]\n",
		".devopsy/commands/deploy": "#!/bin/sh\n## Description: starts the project\nexec devopsy up -d --wait\n",
		".devopsy/commands/args":   "#!/bin/sh\n## Description: records its arguments in ~/args-<first>, and whether it has a terminal\nphase=$1; shift\n{ printf '[%s]' \"$@\"; echo; [ ! -t 2 ] || echo tty; } >\"$HOME/args-$phase\"\n",
	})
	if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
		t.Fatalf("first release (%d):\n%s", code, out)
	}
	want := `[a'b][$HOME][x"y]`
	for _, terminal := range []bool{false, true} {
		srv(t, "rm -f args-*")
		op(t, dir, "sleep 1.1")
		if terminal {
			inTerminal(t, dir, "devopsy @server:prod --release", "/tmp/stepargs")
			if code := terminalCode(t, "/tmp/stepargs"); code != "exit=0" {
				out, _ := op(t, "/", "cat /tmp/stepargs")
				t.Fatalf("on a terminal: %s\n%s", code, out)
			}
		} else if out, code := op(t, dir, "devopsy @server:prod --release"); code != 0 {
			t.Fatalf("release (%d):\n%s", code, out)
		}
		for _, phase := range []string{"before", "prepare", "after"} {
			got, _ := srv(t, "cat args-"+phase)
			args, tty, _ := strings.Cut(got, "\n")
			if args != want || (strings.TrimSpace(tty) == "tty") != terminal {
				t.Errorf("%s, terminal %v: %q, want %s", phase, terminal, got, want)
			}
		}
	}
}
