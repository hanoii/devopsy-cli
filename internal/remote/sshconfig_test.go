package remote

import (
	"strings"
	"testing"
)

func TestSSHDestination(t *testing.T) {
	for _, c := range []struct{ in, user, host, port string }{
		{"vm1", "", "vm1", ""},
		{"devopsy@vm1", "devopsy", "vm1", ""},
		{"devopsy@vm1.example.org", "devopsy", "vm1.example.org", ""},
		{"ssh://devopsy@vm1:2222", "devopsy", "vm1", "2222"},
		{"ssh://vm1", "", "vm1", ""},
	} {
		user, host, port := SSHDestination(c.in)
		if user != c.user || host != c.host || port != c.port {
			t.Errorf("%s: %q %q %q", c.in, user, host, port)
		}
	}
}

func TestSSHConfig(t *testing.T) {
	// The target's user wins over ssh -G's; port 22 is left out.
	got := SSHConfig(&Target{Name: "prod", Host: "devopsy@vm1"}, map[string]string{"hostname": "203.0.113.5", "port": "22", "user": "me"})
	want := "Host vm1\n  Hostname 203.0.113.5\n  User devopsy\n\n"
	if !strings.Contains(got, want) {
		t.Errorf("missing %q in:\n%s", want, got)
	}
	for _, want := range []string{
		"  #ControlMaster auto\n  #ControlPath ~/.ssh/cm-%C\n  #ControlPersist 10m\n",
		"ssh -O exit vm1",
		"  #ServerAliveInterval 15\n  #ServerAliveCountMax 3\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}

	// From ssh -G only: user and a port other than 22.
	got = SSHConfig(&Target{Name: "prod", Host: "vm1"}, map[string]string{"hostname": "localhost", "port": "2222", "user": "devopsy"})
	if !strings.Contains(got, "Host vm1\n  Hostname localhost\n  User devopsy\n  Port 2222\n") {
		t.Errorf("from ssh -G:\n%s", got)
	}

	// Without ssh -G: the destination alone.
	got = SSHConfig(&Target{Name: "prod", Host: "ssh://vm1:2200"}, nil)
	if !strings.Contains(got, "Host vm1\n  Hostname vm1\n  Port 2200\n") || strings.Contains(got, "  User ") {
		t.Errorf("without ssh -G:\n%s", got)
	}
}
