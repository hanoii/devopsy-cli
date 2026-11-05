// Package testfile writes the files tests then run.
//
// Rules: the Checks in AGENTS.md.
package testfile

import (
	"os"
	"syscall"
)

// Write is os.WriteFile for a file a test runs (a fake, a script), holding
// off every fork of the test binary until the file is closed. A fork while
// it is open for writing hands that descriptor to the child until the
// child execs, and Linux refuses to run a file open for writing ("Text
// file busy", go.dev/issue/22315), so a parallel test's fork could fail
// this test's first call to its fake.
func Write(path string, data []byte, mode os.FileMode) error {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	return os.WriteFile(path, data, mode)
}
