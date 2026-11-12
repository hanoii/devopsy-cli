// Package testfile writes the files tests then run.
//
// Rules: the Checks in AGENTS.md.
package testfile

import (
	"os"
	"path/filepath"
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

// Flock writes dir/flock where the system has none (macOS): util-linux's
// flock -w <seconds> <fd>, an exclusive lock on a descriptor the caller
// holds open, in perl, so a script a test runs takes its lock as on a
// server. Nothing where the system has one. What dir/flock was goes first,
// never written into: it may be a link to a fake other tests share.
func Flock(dir string) error {
	for _, d := range []string{"/usr/bin", "/bin"} {
		if _, err := os.Stat(filepath.Join(d, "flock")); err == nil {
			return nil
		}
	}
	p := filepath.Join(dir, "flock")
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return Write(p, []byte(flockPerl), 0o755)
}

// flockPerl is Flock's flock. Its lock is the descriptor's open file, which
// outlives perl's exit while the caller holds the descriptor.
const flockPerl = `#!/usr/bin/perl
use Fcntl qw(:flock);
use Time::HiRes qw(sleep time);
my (undef, $wait, $fd) = @ARGV;
open(my $fh, ">&=", $fd) or exit 1;
my $end = time + $wait;
until (flock($fh, LOCK_EX | LOCK_NB)) { exit 1 if time >= $end; sleep 0.02; }
exit 0;
`
