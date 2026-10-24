package remote

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// realDevopsy is this checkout's devopsy, built once: release scripts call
// it for their images (--images), whose rules the tests check through
// the scripts too.
var realDevopsy string

// startWait bounds a test's wait for what it then acts on (a script
// waiting for a lock, an image pulled): it only decides how soon a test
// that never gets there fails, so it is long, for a loaded machine (the
// tests run in parallel, and macOS takes seconds then for the first
// exec of each fake script). What a test checks keeps its own bound.
const startWait = time.Minute

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "devopsy-remote-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	realDevopsy = filepath.Join(dir, "devopsy")
	cmd := exec.Command("go", "build", "-o", realDevopsy, "../../cmd/devopsy")
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building devopsy:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// sayCase is a fake devopsy's line passing --say to the real one.
func sayCase() string {
	return "[ \"$1\" != " + SayFlag + " ] || exec " + Quote(realDevopsy) + " \"$@\"\n"
}
