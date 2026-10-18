package main

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"golang.org/x/term"
)

// runWatchSession is the hidden --watch-session <pid> [--answer-fd <fd>]
// [--armed <file>], which a script on a server starts in the
// background with the session's stdin (remote.WatchSessionFlag). It
// passes what comes in on to the fd, the only write end of a fifo whose
// read end is the script's fd 3 (a sync's go or abort), and, when stdin
// ends, the session gone, closes it: no answer. With --armed, it then
// takes the file (renamed to <file>.fired) and sends SIGTERM to the
// script, pid, and everything it runs: its process group when the script
// leads one, as under sshd, else the script alone, which cancels a
// release before its switch (fetch_sig). The script disarms it by taking
// the file first, right before its switch: one rename or the other
// succeeds, never both, so a SIGTERM never lands past a switch the
// script went on with. A file, not a signal: one sent before this
// process could handle it would kill it, and with it a sync's answer.
// Nothing is sent once the script is gone (this process is no longer its
// child).
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
	for i := 1; i < len(args); i++ {
		switch {
		case args[i] == "--armed" && i+1 < len(args):
			armed = args[i+1]
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
	// On a terminal (-t), end of file is a Ctrl-D, not the session's
	// end, which reads as an error (EIO, the terminal hung up).
	tty := term.IsTerminal(int(os.Stdin.Fd()))
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 && out != nil {
			_, _ = out.Write(buf[:n])
		}
		if err == io.EOF && tty {
			continue
		}
		if err != nil {
			break
		}
	}
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
	// Taken: the script cannot disarm any more, and the file is no use.
	os.Remove(armed + ".fired")
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	} else if err == nil {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	return 0
}
