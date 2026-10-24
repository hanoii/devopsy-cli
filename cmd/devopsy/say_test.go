package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// --say formats as everything devopsy says, filters by the verbosity,
// and fails when its write does (a session gone), as echo would, which
// the scripts rely on before a switch; never by SIGPIPE.
func TestSay(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if out, code := runDevopsy(t, dir, nil, "--say", "warning", "careful", "why"); code != 0 || out != "devopsy: warning: careful\ndevopsy: | why\n" {
		t.Errorf("warning (%d): %q", code, out)
	}
	if out, code := runDevopsy(t, dir, nil, "--say", "info", "current is now 1"); code != 0 || out != "devopsy: current is now 1\n" {
		t.Errorf("info (%d): %q", code, out)
	}
	if out, code := runDevopsy(t, dir, nil, "--say", "debug", "hidden"); code != 0 || out != "" {
		t.Errorf("debug without -v (%d): %q", code, out)
	}
	if out, code := runDevopsy(t, dir, []string{"DEVOPSY_VERBOSE=1"}, "--say", "debug", "shown"); code != 0 || out != "devopsy: debug: shown\n" {
		t.Errorf("debug with DEVOPSY_VERBOSE (%d): %q", code, out)
	}
	if out, code := runDevopsy(t, dir, nil, "--say", "loud", "x"); code != 2 || !strings.Contains(out, "devopsy: error: usage: devopsy --say ") {
		t.Errorf("unknown level (%d): %q", code, out)
	}

	// Its stderr gone: exit 1, not a signal.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	cmd := exec.Command(binary, "--say", "info", "nobody reads this")
	cmd.Stderr = w
	err = cmd.Run()
	w.Close()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Errorf("stderr gone: %v", err)
	}
}
