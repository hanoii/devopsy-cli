package remote

import (
	"os"
	"strconv"
	"time"
)

// A session whose stdin is a pipe devopsy keeps open gets a heartbeat:
// one zero byte every beat (HeartbeatEvery), so the server tells a
// connection that died silently (a laptop asleep, a network gone: no
// FIN, no RST) from a quiet one, in seconds rather than when TCP gives
// up (16 minutes with writes pending, two hours without). Without a byte
// for its quiet time, a reader treats the session as gone, as at EOF.
// The quiet times are counted in beats, so the end-to-end tests shorten
// them all at once (HeartbeatEveryEnv).
const (
	// HeartbeatEnv carries the beat's seconds into a session: devopsy
	// sends the heartbeat.
	HeartbeatEnv = "DEVOPSY_INTERNAL_HEARTBEAT"
	// HeartbeatEveryEnv sets the beat on the operator's side, seconds:
	// for tests.
	HeartbeatEveryEnv = "DEVOPSY_INTERNAL_HEARTBEAT_EVERY"
	// QuietBeats is a push's, an import's, an export's and a release's
	// before its switch: 30 seconds.
	QuietBeats = 6
	// HolderQuietBeats is a sync's holder's, 90 seconds: longer than the
	// push's and than the operator's ssh needs to see the connection
	// gone (KeepAlive), so a push still copying is usually cancelled
	// before the holder decides. That order only shapes the messages:
	// what a late write does is settled by the record the holder takes
	// and the writers it ends (endWriters), whatever the timing.
	HolderQuietBeats = 18
	// DataQuietBeats is how long an upload's or an import's data may
	// pause, 2 minutes: it carries no heartbeat, and its writer may be
	// slow to start (an export stopping services first).
	DataQuietBeats = 24
)

// HeartbeatEvery is the operator's beat: 5 seconds, or HeartbeatEveryEnv.
func HeartbeatEvery() time.Duration {
	if n, err := strconv.Atoi(os.Getenv(HeartbeatEveryEnv)); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return 5 * time.Second
}

// HeartbeatLine is the line a session's script starts with to say devopsy
// sends the heartbeat, exported for what the script runs.
func HeartbeatLine() string {
	return "export " + HeartbeatEnv + "=" + strconv.Itoa(int(HeartbeatEvery().Seconds())) + "\n"
}

// Quiet is beats of the session's heartbeat (HeartbeatEnv) on the server,
// or 0 without one.
func Quiet(beats int) time.Duration {
	n, err := strconv.Atoi(os.Getenv(HeartbeatEnv))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n*beats) * time.Second
}

// StdinGuardFlag is the hidden devopsy flag that copies stdin to stdout
// and fails when nothing comes for DataQuietBeats: in front of an
// upload's tar, which would wait on a dead session holding the release
// lock.
const StdinGuardFlag = "--stdin-guard"
