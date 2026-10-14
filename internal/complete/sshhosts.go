package complete

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// sshHosts lists the host names in the user's ~/.ssh/config, following its
// Include lines, in order, once each: what a shell's ssh completion offers,
// read the same way. Patterns (*, ?) and negations are no host to name.
// It only reads files: completion never connects.
func sshHosts() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	dir := filepath.Join(home, ".ssh")
	var hosts []string
	seen := map[string]bool{}
	var read func(file string, depth int)
	read = func(file string, depth int) {
		f, err := os.Open(file)
		if err != nil {
			return
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			key, values := sshConfigLine(sc.Text())
			switch key {
			case "host":
				for _, h := range values {
					if !strings.ContainsAny(h, "*?!") && !seen[h] {
						seen[h] = true
						hosts = append(hosts, h)
					}
				}
			case "include":
				// ssh stops at 16 levels.
				if depth >= 16 {
					continue
				}
				for _, v := range values {
					if strings.HasPrefix(v, "~/") {
						v = filepath.Join(home, v[2:])
					} else if !filepath.IsAbs(v) {
						v = filepath.Join(dir, v)
					}
					matches, _ := filepath.Glob(v)
					for _, m := range matches {
						read(m, depth+1)
					}
				}
			}
		}
	}
	read(filepath.Join(dir, "config"), 0)
	return hosts
}

// sshConfigLine splits an ssh_config line into its keyword, lower case, and
// its values: separated by spaces or one =, double quotes kept together.
func sshConfigLine(line string) (string, []string) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' {
		return "", nil
	}
	i := strings.IndexAny(line, " \t=")
	if i < 0 {
		return strings.ToLower(line), nil
	}
	key, rest := strings.ToLower(line[:i]), strings.TrimLeft(line[i:], " \t")
	rest = strings.TrimLeft(strings.TrimPrefix(rest, "="), " \t")
	var values []string
	for rest != "" {
		var v string
		if rest[0] == '"' {
			end := strings.IndexByte(rest[1:], '"')
			if end < 0 {
				v, rest = rest[1:], ""
			} else {
				v, rest = rest[1:end+1], rest[end+2:]
			}
		} else if j := strings.IndexAny(rest, " \t"); j >= 0 {
			v, rest = rest[:j], rest[j:]
		} else {
			v, rest = rest, ""
		}
		if v != "" {
			values = append(values, v)
		}
		rest = strings.TrimLeft(rest, " \t")
	}
	return key, values
}
