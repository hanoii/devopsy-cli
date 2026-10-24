package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// With a heartbeat, the watcher takes a silence for the session gone: an
// armed one signals the script; beats keep it quiet, and never reach the
// answer.
func TestWatchHeartbeat(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	armed := filepath.Join(dir, "armed")
	answer := filepath.Join(dir, "answer")
	// The script: the watcher's parent, waiting for it, as the server's
	// shell does.
	watch := func(args string) (*exec.Cmd, *os.File) {
		t.Helper()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		// As watchSession does: a background job's stdin is /dev/null
		// (dash), so the session's goes through fd 4.
		cmd := exec.Command("sh", "-c", `exec 5>"$ANSWER" 4<&0; "$BIN" --watch-session $$ `+args+` <&4 & exec 4<&- 5>&-; wait`)
		cmd.Env = append(os.Environ(), "BIN="+binary, "ANSWER="+answer)
		cmd.Stdin = r
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		r.Close()
		return cmd, w
	}
	ended := func(cmd *exec.Cmd, within time.Duration) bool {
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
			return true
		case <-time.After(within):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
			return false
		}
	}

	// Silence: the armed watcher signals the script.
	write(t, armed, "", 0o644)
	cmd, w := watch(`--heartbeat 1 --armed "` + armed + `"`)
	start := time.Now()
	if !ended(cmd, 10*time.Second) || time.Since(start) < 900*time.Millisecond || fileExists(armed) || !fileExists(armed+".fired") {
		t.Errorf("silence: ended after %s, armed %v", time.Since(start), fileExists(armed))
	}
	w.Close()

	// Beats keep it going; the answer gets none of them.
	write(t, armed, "", 0o644)
	cmd, w = watch(`--heartbeat 1 --answer-fd 5 --armed "` + armed + `"`)
	for i := 0; i < 8; i++ {
		_, _ = w.Write([]byte{0})
		time.Sleep(300 * time.Millisecond)
	}
	if syscall.Kill(cmd.Process.Pid, 0) != nil || !fileExists(armed) {
		t.Fatalf("cancelled despite the beats")
	}
	_, _ = w.Write([]byte("\x00go\n\x00"))
	time.Sleep(200 * time.Millisecond)
	_ = os.Rename(armed, armed+".off") // disarmed, as before a switch
	w.Close()
	if !ended(cmd, 10*time.Second) {
		t.Errorf("the script did not end once its stdin ended")
	}
	if got := readString(answer); got != "go\n" {
		t.Errorf("answer %q", got)
	}

	// A holder past its lock wait asks to be disarmed at the next byte
	// (.next): a beat disarms it, a silence still signals the script.
	os.Remove(armed + ".off")
	os.Remove(armed + ".fired")
	write(t, armed+".next", "", 0o644)
	cmd, w = watch(`--heartbeat 1 --armed "` + armed + `"`)
	_, _ = w.Write([]byte{0})
	time.Sleep(300 * time.Millisecond)
	if fileExists(armed+".next") || !fileExists(armed+".off") {
		t.Errorf("a beat did not disarm it: next %v, off %v", fileExists(armed+".next"), fileExists(armed+".off"))
	}
	w.Close()
	if !ended(cmd, 10*time.Second) || fileExists(armed+".fired") {
		t.Errorf("signalled once disarmed")
	}
	os.Remove(armed + ".off")
	write(t, armed+".next", "", 0o644)
	cmd, w = watch(`--heartbeat 1 --armed "` + armed + `"`)
	if !ended(cmd, 10*time.Second) || fileExists(armed+".next") || !fileExists(armed+".fired") {
		t.Errorf("silence after the lock wait: next %v, fired %v", fileExists(armed+".next"), fileExists(armed+".fired"))
	}
	w.Close()

	// devopsy interrupted before the sync's marker writes abort and closes
	// the session's stdin (runSyncSession): the session is going, so a
	// script waiting for the next byte (a holder past its lock, a release
	// that syncs right before its switch) is signalled, never disarmed by
	// the abort's bytes. Only a zero byte before any answer (devopsy's
	// answer to BeatMarker, a beat) says the session is there: otherwise
	// a release that syncs, interrupted in that round trip, switched (then
	// the abort failed its sync and brought the previous release back),
	// and a holder went on to stop the child's services.
	os.Remove(armed + ".fired")
	write(t, armed+".next", "", 0o644)
	cmd, w = watch(`--heartbeat 5 --answer-fd 5 --armed "` + armed + `"`)
	// A beat written after the abort, before the close, says nothing more.
	_, _ = w.Write([]byte("abort\n"))
	_, _ = w.Write([]byte{0})
	w.Close()
	if !ended(cmd, 10*time.Second) || fileExists(armed+".off") || !fileExists(armed+".fired") {
		t.Errorf("abort then EOF while it waited for the next byte: off %v, fired %v", fileExists(armed+".off"), fileExists(armed+".fired"))
	}
	os.Remove(armed + ".off")
	os.Remove(armed + ".fired")
}

// The upload's guard passes data through and fails on a silence.
func TestStdinGuard(t *testing.T) {
	t.Parallel()
	cmd := exec.Command(binary, "--stdin-guard")
	cmd.Stdin = strings.NewReader("data\n")
	if out, err := cmd.Output(); err != nil || string(out) != "data\n" {
		t.Errorf("guard (%v): %q", err, out)
	}
}

// Under a login shell that does not exec the script (dash -c), the
// session's group is the login shell's: the watcher signals all of it,
// what the script runs included.
func TestWatchLoginShellGroup(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs /proc (Linux, as servers)")
	}
	dir := t.TempDir()
	armed, pidFile := filepath.Join(dir, "armed"), filepath.Join(dir, "child")
	write(t, armed, "", 0o644)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	inner := `exec 4<&0; sleep 30 & echo $! >"$CHILD"; "$BIN" --watch-session $$ --heartbeat 1 --armed "$ARMED" <&4 & exec 4<&-; wait`
	cmd := exec.Command("sh", "-c", "sh -c "+remote.Quote(inner)+"; echo after")
	cmd.Env = append(os.Environ(), "BIN="+binary, "ARMED="+armed, "CHILD="+pidFile)
	cmd.Stdin = r
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r.Close()
	defer w.Close()
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		t.Fatal("the session's group was not signalled")
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(readString(pidFile)))
	time.Sleep(200 * time.Millisecond)
	if pid > 0 && alive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Error("what the script ran lives on")
	}
}

// alive reports whether pid runs: an orphan nobody reaps (PID 1 in a
// container being go test) is a zombie, which kill -0 finds.
func alive(pid int) bool {
	if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		_, rest, _ := strings.Cut(string(stat), ") ")
		return !strings.HasPrefix(rest, "Z")
	}
	return syscall.Kill(pid, 0) == nil
}

// A command name holding ") " never shifts the fields after it.
func TestStatParent(t *testing.T) {
	t.Parallel()
	for stat, want := range map[string]int{
		"4242 (sh) S 77 4242 4242 0 -1":       77,
		"4242 (a) b) S 77 4242 4242 0 -1":     77,
		"4242 (x) (y) ) R 1 4242 4242 0 -1\n": 1,
		"garbage":                             0,
	} {
		if got := statParent(stat); got != want {
			t.Errorf("%q: %d, want %d", stat, got, want)
		}
	}
}
