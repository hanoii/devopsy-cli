package remote

import (
	"testing"
	"time"
)

// A devopsy inside a session with a heartbeat beats as that session does:
// the push's import on another server is counted in the operator's beats.
//
// Not parallel: it sets HeartbeatEveryEnv and HeartbeatEnv (t.Setenv).
func TestHeartbeatEvery(t *testing.T) {
	t.Setenv(HeartbeatEveryEnv, "")
	t.Setenv(HeartbeatEnv, "")
	if got := HeartbeatEvery(); got != 5*time.Second {
		t.Errorf("default: %s", got)
	}
	t.Setenv(HeartbeatEnv, "1")
	if got := HeartbeatEvery(); got != time.Second {
		t.Errorf("in a session beating every second: %s", got)
	}
	t.Setenv(HeartbeatEveryEnv, "2")
	if got := HeartbeatEvery(); got != 2*time.Second {
		t.Errorf("set: %s", got)
	}
}
