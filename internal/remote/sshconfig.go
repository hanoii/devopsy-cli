package remote

import (
	"bufio"
	"bytes"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
)

// SSHDestination splits a target's host, an SSH destination
// ([user@]host or ssh://[user@]host[:port]), into its parts.
func SSHDestination(dest string) (user, host, port string) {
	if strings.HasPrefix(dest, "ssh://") {
		if u, err := url.Parse(dest); err == nil {
			return u.User.Username(), u.Hostname(), u.Port()
		}
	}
	if i := strings.LastIndex(dest, "@"); i >= 0 {
		return dest[:i], dest[i+1:], ""
	}
	return "", dest, ""
}

// SSHEffective is ssh's resolved configuration for a destination, ssh -G:
// read on this machine, it never connects. Keys are lowercase, as ssh prints
// them. Replaceable in tests.
var SSHEffective = func(dest string) (map[string]string, error) {
	out, err := exec.Command("ssh", "-G", dest).Output()
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		// Repeated keys (identityfile, localforward): the first is enough here.
		if _, seen := m[k]; !seen {
			m[k] = v
		}
	}
	return m, nil
}

// SSHConfig is the ~/.ssh/config block devopsy suggests for a target's
// host: where it connects (from the target and ssh -G, when effective is
// not nil), then optional settings, commented out.
func SSHConfig(t *Target, effective map[string]string) string {
	user, host, port := SSHDestination(t.Host)
	hostname := host
	if v := effective["hostname"]; v != "" {
		hostname = v
	}
	if user == "" {
		user = effective["user"]
	}
	if port == "" {
		port = effective["port"]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# For @%s (%s). devopsy already passes ClearAllForwardings=yes.\n", t.Name, t.Host)
	fmt.Fprintf(&b, "Host %s\n  Hostname %s\n", host, hostname)
	if user != "" {
		fmt.Fprintf(&b, "  User %s\n", user)
	}
	if port != "" && port != "22" {
		fmt.Fprintf(&b, "  Port %s\n", port)
	}
	fmt.Fprintf(&b, `
  # Faster releases and commands: sessions reuse one open connection instead
  # of a new handshake each (seconds through a jump host or VPN; a release
  # opens several). It stays open 10 minutes after the last one; close it
  # with: ssh -O exit %s
  #ControlMaster auto
  #ControlPath ~/.ssh/cm-%%C
  #ControlPersist 10m

  # No hanging on a dropped connection: ssh gives up after about 45 seconds
  # without an answer from the server, instead of waiting on TCP.
  #ServerAliveInterval 15
  #ServerAliveCountMax 3
`, host)
	return b.String()
}
