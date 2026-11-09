package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A read as a project user (--door-read) runs in a session of its own: in the caller's, its
// process, which that project user can trace, would have the caller's terminal as its
// controlling one, an admin's under --server-info, to read and to type into.
func TestDoorReadRunsInASessionOfItsOwn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	fake := filepath.Join(dir, "sudo")
	write(t, fake, "#!/bin/sh\necho $$ >"+pidFile+".tmp && mv "+pidFile+".tmp "+pidFile+"\nexec sleep 30\n", 0o755)
	c := doorRead("devopsy-shop", "environments", "/srv/shop")
	c.Path, c.Args[0] = fake, fake
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Process.Kill(); _ = c.Wait() }()
	var pid int
	for deadline := time.Now().Add(startWait); pid == 0 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		data, err := os.ReadFile(pidFile)
		if err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
		}
	}
	if pid == 0 {
		t.Fatal("the read never started")
	}
	mine, err := unix.Getsid(0)
	if err != nil {
		t.Fatal(err)
	}
	if sid, err := unix.Getsid(pid); err != nil || sid == mine {
		t.Errorf("a read as a project user runs in the caller's session %d (%d, %v)", mine, sid, err)
	}
}
