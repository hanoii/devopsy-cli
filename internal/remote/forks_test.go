package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Bug: a fake a test writes (write, fakeImageTools) and runs at once
// through sh fails now and then with "Text file busy" at its first call,
// so the test fails in a hundredth of a second: a fork another parallel
// test makes while the fake is open for writing inherits that fd until
// the child execs (go.dev/issue/22315), and on Linux no file open for
// writing may be executed. Linux only, and hundreds of thousands of runs,
// so only when DEVOPSY_TEST_FORKS is set.
func TestWrittenFakeRunsBesideForks(t *testing.T) {
	t.Parallel()
	if os.Getenv("DEVOPSY_TEST_FORKS") == "" {
		t.Skip("DEVOPSY_TEST_FORKS is not set")
	}
	if runtime.GOOS != "linux" {
		t.Skip("ETXTBSY is Linux's")
	}
	dir := t.TempDir()
	done := make(chan struct{})
	var forkers, writers sync.WaitGroup
	// What the parallel tests do all along: fork and exec.
	for i := 0; i < 8; i++ {
		forkers.Add(1)
		go func() {
			defer forkers.Done()
			for {
				select {
				case <-done:
					return
				default:
					_ = exec.Command("true").Run()
				}
			}
		}()
	}
	var runs atomic.Int64
	var mu sync.Mutex
	var failure string
	deadline := time.Now().Add(10 * time.Second)
	for i := 0; i < 8; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for time.Now().Before(deadline) {
				mu.Lock()
				failed := failure != ""
				mu.Unlock()
				if failed {
					return
				}
				bin, err := os.MkdirTemp(dir, "bin")
				if err != nil {
					t.Error(err)
					return
				}
				write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\nif [ \"$1\" = --release-settings ]; then echo root=/x; exit; fi\nexit 0\n")
				if err := os.Chmod(filepath.Join(bin, "devopsy"), 0o755); err != nil {
					t.Error(err)
					return
				}
				cmd := exec.Command("sh", "-c", `settings=$(devopsy --release-settings) || exit 1`)
				cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
				out, err := cmd.CombinedOutput()
				runs.Add(1)
				if err != nil {
					mu.Lock()
					if failure == "" {
						failure = err.Error() + ":\n" + string(out)
					}
					mu.Unlock()
				}
				os.RemoveAll(bin)
			}
		}()
	}
	writers.Wait()
	close(done)
	forkers.Wait()
	if failure != "" {
		t.Fatalf("a fake written then run failed after %d runs:\n%s", runs.Load(), failure)
	}
}
