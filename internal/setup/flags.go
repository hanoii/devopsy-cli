package setup

import (
	"slices"
	"strings"
)

// DaemonCommand is the command docker.service starts dockerd with, from systemctl show -p
// ExecStart --value docker.service (show): the arguments of its last command, argv[0]
// included, nil for none. The unit's own command, not the running daemon's: it is what Docker
// starts with next, beside daemon.json, which a drop-in may have changed since.
func DaemonCommand(show string) []string {
	var argv []string
	for _, entry := range strings.Split(show, "\n") {
		_, rest, ok := strings.Cut(entry, "argv[]=")
		if !ok {
			continue
		}
		args, _, _ := strings.Cut(rest, " ;")
		argv = strings.Fields(args)
	}
	return argv
}

// flagKeys are the daemon.json keys dockerd's flags set, by flag name.
var flagKeys = map[string]string{
	"log-driver":           "log-driver",
	"log-opt":              "log-opts",
	"live-restore":         "live-restore",
	"default-address-pool": "default-address-pools",
}

// DaemonFlags are what dockerd's arguments (argv, DaemonCommand's) set: the daemon.json keys
// of devopsy's parts a flag sets, whatever its value (dockerd refuses a key both in a flag and
// in the file), and the config file it reads ("" for its default).
func DaemonFlags(argv []string) (keys []string, configFile string) {
	for i := 1; i < len(argv); i++ {
		name, value, hasValue := strings.Cut(strings.TrimPrefix(argv[i], "--"), "=")
		if !strings.HasPrefix(argv[i], "--") {
			continue
		}
		if name == "config-file" {
			if !hasValue && i+1 < len(argv) {
				value = argv[i+1]
			}
			configFile = value
			continue
		}
		if key, ok := flagKeys[name]; ok && !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	return keys, configFile
}

// SetByFlags says whether a flag sets any key of p (keys, DaemonFlags'): the part is then the
// flags', none of its keys devopsy's to put.
func SetByFlags(p Part, keys []string) bool {
	return slices.ContainsFunc(p.Keys, func(k string) bool { return slices.Contains(keys, k) })
}
