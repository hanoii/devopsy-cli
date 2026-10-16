package remote

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// realDevopsy is this checkout's devopsy, built once: release scripts call
// it for their images (--images), whose rules the tests check through
// the scripts too.
var realDevopsy string

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
