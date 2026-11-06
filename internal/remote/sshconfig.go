package remote

import (
	"bufio"
	"bytes"
	"cmp"
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// SSHDestination splits a target's host (ParseServer) into ssh's user,
// host and port, the port "" for ssh's own.
func SSHDestination(server string) (user, host, port string) {
	dest, port, _ := ParseServer(server)
	if i := strings.LastIndex(dest, "@"); i >= 0 {
		return dest[:i], dest[i+1:], port
	}
	return "", dest, port
}

// SSHArgs are ssh's arguments for a server: -p and its port when it names
// one, then [user@]host, an IPv6 address without its brackets.
func SSHArgs(server string) []string {
	dest, port, ok := ParseServer(server)
	if !ok {
		return []string{server}
	}
	if port != "" {
		return []string{"-p", port, dest}
	}
	return []string{dest}
}

// ParseServer splits a server into [user@]host and its port ("" for
// none, else as a number, 022 giving 22): [user@]host, [user@]host:port,
// an IPv6 address alone ([user@]2001:db8::1) or in brackets, which a port
// needs ([user@][2001:db8::1]:port). ok is false for anything else, a
// user or host starting with - (ssh would take it for an option) or
// holding a shell's special characters included.
func ParseServer(server string) (dest, port string, ok bool) {
	user, rest := "", server
	if i := strings.LastIndex(server, "@"); i >= 0 {
		user, rest = server[:i+1], server[i+1:]
		if i == 0 {
			return "", "", false
		}
	}
	host, hasPort := rest, false
	switch {
	case strings.HasPrefix(rest, "["):
		var after string
		host, after, ok = strings.Cut(rest[1:], "]")
		if !ok || !isIPv6(host) {
			return "", "", false
		}
		if after != "" {
			if port, hasPort = strings.CutPrefix(after, ":"); !hasPort {
				return "", "", false
			}
		}
	case strings.Count(rest, ":") > 1:
		if !isIPv6(rest) {
			return "", "", false
		}
	default:
		host, port, hasPort = strings.Cut(rest, ":")
	}
	if host == "" || strings.HasPrefix(user, "-") || strings.HasPrefix(host, "-") ||
		strings.ContainsAny(user, "[]/:"+shellSpecial) || strings.ContainsAny(host, "[]/"+shellSpecial) || hasPort && !validPort(port) {
		return "", "", false
	}
	if hasPort {
		n, _ := strconv.Atoi(port)
		port = strconv.Itoa(n)
	}
	return user + host, port, true
}

// shellSpecial are the characters no user or host holds, which a shell
// would read in a hint or a script.
const shellSpecial = " \t'\"`$\\;&|<>(){}*?!#~,="

func isIPv6(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Is6()
}

func validPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n > 0 && n < 65536 && strings.Trim(s, "0123456789") == ""
}

// ServerError refuses a server ParseServer does not take (for example
// ssh://..., vm1:prod), and is nil for any other or none.
func ServerError(server string) error {
	if _, _, ok := ParseServer(server); ok || server == "" {
		return nil
	}
	return &report.Failure{
		Msg:     fmt.Sprintf("server %q: not [user@]host[:port]", server),
		Details: []string{"an IPv6 address goes alone (2001:db8::1), or in brackets with a port ([2001:db8::1]:22)"},
		Fixes:   []report.Remedy{report.Fix("the server as [user@]host[:port], or a Host of ~/.ssh/config")},
	}
}

// PortError refuses a server whose port ssh would not connect to, which
// ssh -G resolves to another: a -p or Port in DEVOPSY_SSH_COMMAND, whose
// options come first on the command line, and ssh keeps the first value.
// Nil for a server without a port, or when ssh -G says none.
func PortError(server string) error {
	_, port, ok := ParseServer(server)
	if !ok || port == "" {
		return nil
	}
	eff, err := SSHEffective(server)
	if err != nil || eff["port"] == "" || eff["port"] == port {
		return nil
	}
	return &report.Failure{
		Msg:     fmt.Sprintf("server %q names port %s, and ssh would connect to %s", server, port, eff["port"]),
		Details: []string{"DEVOPSY_SSH_COMMAND's options come first, and ssh keeps the first value of each: a -p there applies to every server a command reaches"},
		Fixes:   []report.Remedy{report.Fix("take the port out of DEVOPSY_SSH_COMMAND, or out of the server")},
	}
}

// SSHEffective is ssh's resolved configuration for a destination, ssh -G
// (through DEVOPSY_SSH_COMMAND, whose options count): read on this machine,
// it never connects. Keys are lowercase, as ssh prints them. Replaceable in
// tests.
var SSHEffective = func(dest string) (map[string]string, error) {
	// Once per destination and ssh (DEVOPSY_SSH_COMMAND, else PATH's): a
	// sync asks for its two servers' several times (the ports, sameHost,
	// ChildAddress).
	key := os.Getenv("DEVOPSY_SSH_COMMAND") + "\x00" + os.Getenv("PATH") + "\x00" + dest
	if m, ok := sshEffective.Load(key); ok {
		return m.(map[string]string), nil
	}
	out, err := SSHCommand(append([]string{"-G"}, SSHArgs(dest)...)...).Output()
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
	sshEffective.Store(key, m)
	return m, nil
}

var sshEffective sync.Map

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
	port = cmp.Or(effective["port"], port)

	var b strings.Builder
	fmt.Fprintf(&b, "# For @%s (%s). devopsy already passes ClearAllForwardings=yes, and\n# PasswordAuthentication=no unless %s is set.\n", t.Name, t.Host, SSHAllowPasswordEnv)
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

// ChildAddress is where a parent's server reaches a child's host for a
// sync: the user, hostname and port ssh -G gives locally. A host behind a
// ProxyCommand or ProxyJump is out of the parent's reach: a relay through
// this machine is not supported yet.
func ChildAddress(host string) (user, hostname, port string, err error) {
	eff, err := SSHEffective(host)
	if err != nil {
		return "", "", "", fmt.Errorf("ssh -G %s: %w", host, err)
	}
	for _, k := range []string{"proxycommand", "proxyjump"} {
		if v := eff[k]; v != "" && v != "none" {
			return "", "", "", fmt.Errorf("%s is reached through a %s (%s): the parent's server cannot reach it directly; a relay through this machine is not supported yet", host, map[string]string{"proxycommand": "ProxyCommand", "proxyjump": "ProxyJump"}[k], v)
		}
	}
	user, hostname, port = eff["user"], eff["hostname"], eff["port"]
	if hostname == "" {
		return "", "", "", fmt.Errorf("ssh -G %s gives no hostname", host)
	}
	return user, hostname, port, nil
}
