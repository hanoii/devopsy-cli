package main

import (
	"bytes"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// runWatchSession is the hidden --watch-session <pid> [--answer-fd <fd>]
// [--armed <file>] [--heartbeat <seconds>], which a script on a server starts in the
// background with the session's stdin (remote.WatchSessionFlag). It
// passes what comes in on to the fd, the only write end of a fifo whose
// read end is the script's fd 3 (a sync's go or abort), and, when stdin
// ends, the session gone, closes it: no answer. With --armed, it then
// takes the file (renamed to <file>.fired) and sends SIGTERM to the
// script, pid, and everything it runs: its process group when the script
// or its login shell leads one, as under sshd, else the script alone, which cancels a
// release before its switch (fetch_sig). The script disarms it by taking
// the file first, right before its switch: one rename or the other
// succeeds, never both, so a SIGTERM never lands past a switch the
// script went on with. A file, not a signal: one sent before this
// process could handle it would kill it, and with it a sync's answer.
// Nothing is sent once the script is gone (this process is no longer its
// child). With --heartbeat, devopsy sends a zero byte every few seconds
// (remote.HeartbeatEnv), never passed on: that many seconds without a
// byte is the session gone too, a connection that died silently.
func runWatchSession(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: devopsy "+remote.WatchSessionFlag+" <pid> [--answer-fd <fd>] [--armed <file>]")
		return 2
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil || pid <= 1 {
		fmt.Fprintln(os.Stderr, "devopsy: "+remote.WatchSessionFlag+": not a pid: "+args[0])
		return 2
	}
	var answer = -1
	var armed string
	var quiet time.Duration
	for i := 1; i < len(args); i++ {
		switch {
		case args[i] == "--armed" && i+1 < len(args):
			armed = args[i+1]
			i++
		case args[i] == "--heartbeat" && i+1 < len(args):
			secs, err := strconv.Atoi(args[i+1])
			if err != nil || secs < 1 {
				fmt.Fprintln(os.Stderr, "devopsy: "+remote.WatchSessionFlag+": not seconds: "+args[i+1])
				return 2
			}
			quiet = time.Duration(secs) * time.Second
			i++
		case args[i] == "--answer-fd" && i+1 < len(args):
			if answer, err = strconv.Atoi(args[i+1]); err != nil || answer < 3 {
				fmt.Fprintln(os.Stderr, "devopsy: "+remote.WatchSessionFlag+": not an fd: "+args[i+1])
				return 2
			}
			i++
		default:
			fmt.Fprintln(os.Stderr, "devopsy: "+remote.WatchSessionFlag+": unknown argument "+args[i])
			return 2
		}
	}
	// The terminal's signals are the script's, and a write to a gone
	// session never matters here.
	signal.Ignore(syscall.SIGINT, syscall.SIGHUP, syscall.SIGPIPE)

	var out *os.File
	if answer >= 0 {
		out = os.NewFile(uintptr(answer), "answer")
	}
	_, _ = readQuiet(os.Stdin, quiet, func(b []byte) error {
		if b = bytes.ReplaceAll(b, []byte{0}, nil); len(b) > 0 && out != nil {
			_, _ = out.Write(b)
		}
		return nil
	})
	if out != nil {
		out.Close()
	}
	if armed == "" {
		return 0
	}
	if os.Getppid() != pid {
		os.Remove(armed)
		return 0
	}
	if os.Rename(armed, armed+".fired") != nil {
		// The script took it: disarmed.
		return 0
	}
	// Taken: the script cannot disarm any more, and finds it to know
	// nobody reads the session (it removes it; prelude a stale one).
	// The script's group: it leads one under sshd when the login shell
	// execs it (bash -c), else its parent, the login shell, does, the
	// session's own (dash -c, which never execs the last command). A
	// group of anything else (a test's) is not the script's: it alone.
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return 0
	}
	if sid, err := unix.Getsid(pid); pgid == pid || (err == nil && sid == pgid && parentPid(pid) == pgid) {
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
	} else {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	return 0
}

// parentPid is pid's parent, from /proc (Linux, where servers run), or 0.
func parentPid(pid int) int {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	_, rest, _ := strings.Cut(string(stat), ") ")
	if f := strings.Fields(rest); len(f) > 1 {
		n, _ := strconv.Atoi(f[1])
		return n
	}
	return 0
}
