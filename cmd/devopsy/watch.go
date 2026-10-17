package main

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// runWatchSession is the hidden --watch-session <pid> [--answer-fd <fd>]
// [--cancel-until <file>], which a script on a server starts in the
// background with the session's stdin (remote.WatchSessionFlag). It
// passes what comes in on to the fd, the only write end of a fifo whose
// read end is the script's fd 3 (a sync's go or abort), and, when stdin
// ends, the session gone, closes it: no answer. With --cancel-until, unless the file exists by then (the
// script writes it right before its switch), it sends SIGTERM to the
// script, pid, and everything it runs: its process group when the script
// leads one, as under sshd, else the script alone, which cancels a
// release before its switch (fetch_sig). A file, not a signal: one sent
// before this process could handle it would kill it, and with it a
// sync's answer. Nothing is sent once the script is gone (this process
// is no longer its child).
func runWatchSession(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: devopsy "+remote.WatchSessionFlag+" <pid> [--answer-fd <fd>] [--cancel-until <file>]")
		return 2
	}
	pid, err := strconv.Atoi(args[0])
	if err != nil || pid <= 1 {
		fmt.Fprintln(os.Stderr, "devopsy: "+remote.WatchSessionFlag+": not a pid: "+args[0])
		return 2
	}
	var answer = -1
	var until string
	for i := 1; i < len(args); i++ {
		switch {
		case args[i] == "--cancel-until" && i+1 < len(args):
			until = args[i+1]
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
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 && out != nil {
			_, _ = out.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	if out != nil {
		out.Close()
	}
	if until == "" {
		return 0
	}
	if _, err := os.Stat(until); err == nil || os.Getppid() != pid {
		os.Remove(until)
		return 0
	}
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	} else if err == nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	return 0
}
