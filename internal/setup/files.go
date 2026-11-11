package setup

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Header is the first line of every file devopsy writes whole, each at a path of its
// own; devopsy writes or removes no other file.
const Header = "# Written by devopsy (--server-setup)."

// SysctlFile raises the socket buffers at every boot.
const SysctlFile = "/etc/sysctl.d/90-devopsy-net.conf"

// BufferSize is the socket buffers' maximum devopsy wants at least, in bytes: what a
// QUIC connection asks for, which runs on the kernel's default with a warning otherwise.
const BufferSize = 7500000

// BufferKeys are the socket buffers' sysctl keys, each under /proc/sys with its dots as
// slashes.
var BufferKeys = []string{"net.core.rmem_max", "net.core.wmem_max"}

// SysctlText is SysctlFile's text.
func SysctlText() string {
	s := Header + "\n"
	for _, k := range BufferKeys {
		s += k + " = " + strconv.Itoa(BufferSize) + "\n"
	}
	return s
}

// BufferPath is key's file under /proc/sys.
func BufferPath(key string) string {
	return "/proc/sys/" + strings.ReplaceAll(key, ".", "/")
}

// BufferAtLeast reports whether a live value (a /proc/sys file's text) is BufferSize or
// more: a lower one is raised, a higher one never lowered.
func BufferAtLeast(live string) (bool, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(live), 10, 64)
	if err != nil {
		return false, err
	}
	return n >= BufferSize, nil
}

// MountsDropIn makes Docker wait at boot for the release root's mounts: containers
// started before them write their bind mounts to the disk under them.
const MountsDropIn = "/etc/systemd/system/docker.service.d/devopsy-mounts.conf"

// MountsDropInText is MountsDropIn's text for the release root, whatever it is on: on /
// the dependency is -.mount, always there. A root systemd cannot read in a unit file (a
// control character, not UTF-8) is an error.
func MountsDropInText(root string) (string, error) {
	p, err := systemdPath(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	return Header + "\n[Unit]\nRequiresMountsFor=" + p + "\n", nil
}

// systemdPath is p as a unit file's path list takes it, read back as p: a separator, a
// quote or a backslash escaped by a backslash, a % doubled, since systemd splits the
// value into words, unquoting them, then expands specifiers.
func systemdPath(p string) (string, error) {
	if !utf8.ValidString(p) {
		return "", fmt.Errorf("%q: not UTF-8, which a unit file must be", p)
	}
	var b strings.Builder
	for _, r := range p {
		switch {
		case r < 0x20 || r == 0x7f:
			return "", fmt.Errorf("%q: a control character, which a unit file cannot hold", p)
		case r == '%':
			b.WriteString("%%")
		case r == ' ' || r == '\\' || r == '"' || r == '\'':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String(), nil
}
