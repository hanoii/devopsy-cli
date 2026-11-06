package remote

import (
	"strings"
	"testing"
)

// A server goes to ssh as [user@]host, an IPv6 address without its
// brackets, after -p and its port when it names one.
func TestSSHArgs(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"vm1":                       "vm1",
		"devopsy@vm1.example.org":   "devopsy@vm1.example.org",
		"devopsy@vm1:5369":          "-p 5369 devopsy@vm1",
		"vm1:022":                   "-p 22 vm1",
		"2001:db8::1":               "2001:db8::1",
		"devopsy@::1":               "devopsy@::1",
		"devopsy@[2001:db8::1]":     "devopsy@2001:db8::1",
		"[2001:db8::1]:5369":        "-p 5369 2001:db8::1",
		"devopsy@[fe80::1%eth0]:22": "-p 22 devopsy@fe80::1%eth0",
	} {
		if got := strings.Join(SSHArgs(in), " "); got != want {
			t.Errorf("%s: %s", in, got)
		}
		if ServerError(in) != nil {
			t.Errorf("%s refused", in)
		}
	}
	// A port as ssh -G prints it, a number, which PortError compares.
	if user, host, port := SSHDestination("devopsy@[::1]:0022"); user != "devopsy" || host != "::1" || port != "22" {
		t.Errorf("SSHDestination: %q %q %q", user, host, port)
	}
}

func TestSSHConfig(t *testing.T) {
	t.Parallel()
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

	// Without ssh -G: the destination alone, its port included.
	got = SSHConfig(&Target{Name: "prod", Host: "devopsy@vm1:2200"}, nil)
	if !strings.Contains(got, "Host vm1\n  Hostname vm1\n  User devopsy\n  Port 2200\n") {
		t.Errorf("without ssh -G:\n%s", got)
	}
}

// A child's server is reached directly from the parent's: no proxies.
//
// Not parallel: it sets SSHEffective.
func TestChildAddress(t *testing.T) {
	old := SSHEffective
	defer func() { SSHEffective = old }()
	eff := map[string]string{"user": "devopsy", "hostname": "203.0.113.7", "port": "2222", "proxycommand": "none"}
	SSHEffective = func(string) (map[string]string, error) { return eff, nil }
	if u, h, p, err := ChildAddress("vm2"); err != nil || u != "devopsy" || h != "203.0.113.7" || p != "2222" {
		t.Errorf("direct: %s %s %s %v", u, h, p, err)
	}
	eff["proxyjump"] = "bastion"
	if _, _, _, err := ChildAddress("vm2"); err == nil || !strings.Contains(err.Error(), "ProxyJump") {
		t.Errorf("proxyjump: %v", err)
	}
	delete(eff, "proxyjump")
	eff["proxycommand"] = "orb ssh %h"
	if _, _, _, err := ChildAddress("vm2"); err == nil || !strings.Contains(err.Error(), "ProxyCommand") {
		t.Errorf("proxycommand: %v", err)
	}
}
