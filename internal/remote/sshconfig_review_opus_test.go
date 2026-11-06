package remote

import "testing"

// Bug (review, opus): ParseServer takes a server starting with '-'
// (DEVOPSY_SERVER=-oProxyCommand=... in a project's .env), and SSHArgs
// hands it to ssh where a destination goes, which ssh reads as an option:
// the server would choose ssh's options, a ProxyCommand run locally
// included. A host or user never starts with '-'.
func TestServerNeverAnOptionReview(t *testing.T) {
	t.Parallel()
	for _, server := range []string{"-oProxyCommand=x", "-oProxyCommand=x@vm1", "-F@vm1:22"} {
		if ServerError(server) == nil {
			t.Errorf("%s taken, ssh gets %q", server, SSHArgs(server))
		}
	}
}
