package setup

import (
	"os"
	"path/filepath"
	"strings"
)

// Candidate is a mounted volume the release root could be on: a device of its own, on
// another disk than /'s. devopsy never picks one, since it cannot tell what a volume is
// for; it only names them.
type Candidate struct {
	MountPoint string `json:"mount_point"`
	Source     string `json:"source"`
	FSType     string `json:"fs_type"`
	// Size and Free are the caller's, from the filesystem, in bytes.
	Size int64 `json:"size,omitempty"`
	Free int64 `json:"free,omitempty"`
	// Refusal is why the volume is not offered for the root, with its fix, the caller's, nil
	// for one offered.
	Refusal *Refusal `json:"refusal,omitempty"`
}

// skipped are the mount points no candidate is on or under, besides Docker's root.
var skipped = []string{"/boot", "/proc", "/sys", "/dev", "/run", "/snap", "/var/lib/containerd"}

// Candidates are the volumes in mountinfo (/proc/self/mountinfo's text) that could hold
// the release root: a device under /dev/ (no loop, zram or ram), the whole filesystem
// (root field /), mounted rw, at a mount point other than / and off the system's and
// Docker's own (dockerRoot, Docker's default when ""), on a disk other than /'s by sysfs
// (the sysfs root, /sys). A source mounted twice is listed once, at its first mount.
func Candidates(mountinfo, sysfs, dockerRoot string) []Candidate {
	if dockerRoot == "" {
		dockerRoot = "/var/lib/docker"
	}
	mounts := parseMountinfo(mountinfo)
	rootDisk := ""
	for _, m := range mounts {
		if m.point == "/" {
			rootDisk = diskOf(sysfs, m.dev) // the last mount at / is the one seen
		}
	}
	var out []Candidate
	seen := map[string]bool{}
	for _, m := range mounts {
		name, ok := strings.CutPrefix(m.source, "/dev/")
		if !ok || name == "" || seen[m.source] {
			continue
		}
		base := filepath.Base(name)
		if strings.HasPrefix(base, "loop") || strings.HasPrefix(base, "zram") || strings.HasPrefix(base, "ram") {
			continue
		}
		if m.root != "/" || !hasOption(m.options, "rw") || m.point == "/" {
			continue
		}
		if under(m.point, dockerRoot) || anyUnder(m.point, skipped) {
			continue
		}
		if rootDisk != "" && diskOf(sysfs, m.dev) == rootDisk {
			continue
		}
		seen[m.source] = true
		out = append(out, Candidate{MountPoint: m.point, Source: m.source, FSType: m.fstype})
	}
	return out
}

// OnCandidate reports whether path (links resolved) is on c.
func OnCandidate(path string, c Candidate) bool { return under(path, c.MountPoint) }

// MountOf is the mount in mountinfo (/proc/self/mountinfo's text) that path is on, its mount
// point and filesystem: the longest mount point holding it, "" for none.
func MountOf(mountinfo, path string) (point, fstype string) {
	for _, m := range parseMountinfo(mountinfo) {
		if under(path, m.point) && len(m.point) >= len(point) {
			point, fstype = m.point, m.fstype
		}
	}
	return point, fstype
}

// mount is one line of mountinfo.
type mount struct {
	dev, root, point, options, fstype, source string
}

// parseMountinfo reads mountinfo's lines (proc(5)): id, parent, major:minor, root, mount
// point, options, optional fields up to "-", then the filesystem, the source and the
// superblock's options. A line it cannot read is skipped.
func parseMountinfo(text string) []mount {
	var out []mount
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		sep := -1
		for i := 6; i < len(f); i++ {
			if f[i] == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || len(f) < sep+3 {
			continue
		}
		out = append(out, mount{
			dev: f[2], root: unescapeMount(f[3]), point: unescapeMount(f[4]), options: f[5],
			fstype: f[sep+1], source: unescapeMount(f[sep+2]),
		})
	}
	return out
}

// unescapeMount undoes the kernel's octal escapes in a mountinfo field (\040 a space,
// \011 a tab, \012 a newline, \134 a backslash).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			b.WriteByte((s[i+1]-'0')<<6 | (s[i+2]-'0')<<3 | (s[i+3] - '0'))
			i += 3
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }

func hasOption(options, want string) bool {
	for _, o := range strings.Split(options, ",") {
		if o == want {
			return true
		}
	}
	return false
}

// under reports whether path is dir or inside it.
func under(path, dir string) bool {
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	return path == dir || dir == "/" || strings.HasPrefix(path, dir+"/")
}

func anyUnder(path string, dirs []string) bool {
	for _, d := range dirs {
		if under(path, d) {
			return true
		}
	}
	return false
}

// diskOf is the disk of the device dev (major:minor) by sysfs: the device itself, or the
// parent of a partition, each as its sysfs path; dev itself when sysfs has no entry
// (an anonymous device, a filesystem of several).
func diskOf(sysfs, dev string) string {
	target, err := filepath.EvalSymlinks(filepath.Join(sysfs, "dev", "block", dev))
	if err != nil {
		return "dev:" + dev
	}
	if _, err := os.Stat(filepath.Join(target, "partition")); err == nil {
		return filepath.Dir(target)
	}
	return target
}
