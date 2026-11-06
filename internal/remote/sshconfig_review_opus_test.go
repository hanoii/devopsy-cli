package remote

import (
	"strconv"
	"strings"
	"testing"
)

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

// Bug (review, opus): PortError compares the port as written with ssh -G's,
// which ssh prints as a number: vm1:022 names port 22, ssh connects to 22,
// and the server is refused for naming 022 while ssh would connect to 22.
//
// Not parallel: it sets SSHEffective.
func TestPortErrorSamePortWrittenOtherwiseReview(t *testing.T) {
	old := SSHEffective
	defer func() { SSHEffective = old }()
	// As ssh -G prints the port of -p: a number.
	SSHEffective = func(dest string) (map[string]string, error) {
		_, port, _ := ParseServer(dest)
		n, _ := strconv.Atoi(port)
		return map[string]string{"port": strconv.Itoa(n)}, nil
	}
	for _, server := range []string{"vm1:022", "devopsy@[::1]:0022"} {
		if err := PortError(server); err != nil && strings.Contains(err.Error(), "names port") {
			t.Errorf("%s: %v", server, err)
		}
	}
}
