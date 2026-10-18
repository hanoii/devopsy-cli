package remote

import "time"

// A session whose stdin is a pipe devopsy keeps open gets a heartbeat:
// one zero byte every HeartbeatEvery, so the server tells a connection
// that died silently (a laptop asleep, a network gone: no FIN, no RST)
// from a quiet one, in seconds rather than when TCP gives up (16 minutes
// with writes pending, two hours without). Without a byte for its
// quiet time, a reader treats the session as gone, as at EOF.
const (
	// HeartbeatEnv says devopsy sends the heartbeat.
	HeartbeatEnv   = "DEVOPSY_INTERNAL_HEARTBEAT"
	HeartbeatEvery = 5 * time.Second
	// HeartbeatQuiet is a push's, an import's, an export's and a release
	// before its switch.
	HeartbeatQuiet = 30 * time.Second
	// HeartbeatQuietHolder is a sync's holder's: longer than the push's
	// and than the operator's ssh needs to see the connection gone
	// (KeepAlive), so a push still copying is cancelled before the
	// holder restores what it copies into.
	HeartbeatQuietHolder = 90 * time.Second
	// DataQuiet is how long an upload's or an import's data may pause:
	// it carries no heartbeat, and its writer may be slow to start (an
	// export stopping services first).
	DataQuiet = 2 * time.Minute
)

// StdinGuardFlag is the hidden devopsy flag that copies stdin to stdout
// and fails when nothing comes for DataQuiet: in front of an upload's
// tar, which would wait on a dead session holding the release lock.
const StdinGuardFlag = "--stdin-guard"
