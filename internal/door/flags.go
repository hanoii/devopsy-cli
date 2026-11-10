package door

import (
	"strings"
)

// Verbs are what compose may be asked to do through the door, each with its
// flags and how many values each takes: a closed list, since a flag may read a
// file or change what runs.
var Verbs = map[string]map[string]int{
	"build":   {"--pull": 0, "-q": 0, "--quiet": 0, "--no-cache": 0},
	"create":  {"--no-build": 0, "--pull": 1, "--force-recreate": 0},
	"down":    {"-v": 0, "--volumes": 0, "-t": 1, "--timeout": 1, "--remove-orphans": 0, "--rmi": 1},
	"exec":    {"-T": 0, "-d": 0, "-e": 1, "--env": 1, "-w": 1, "--workdir": 1, "-u": 1, "--user": 1},
	"images":  {"-q": 0, "--format": 1},
	"kill":    {"-s": 1},
	"logs":    {"--tail": 1, "-f": 0, "--follow": 0, "--no-log-prefix": 0, "--no-color": 0, "--since": 1, "-t": 0},
	"port":    {},
	"ps":      {"-a": 0, "--all": 0, "-q": 0, "--quiet": 0, "--services": 0, "--format": 1, "--status": 1, "--filter": 1},
	"pull":    {"-q": 0, "--quiet": 0, "--ignore-buildable": 0, "--ignore-pull-failures": 0, "--include-deps": 0},
	"restart": {"-t": 1, "--timeout": 1, "--no-deps": 0},
	"rm":      {"-f": 0, "-s": 0, "-v": 0, "-fs": 0, "-fsv": 0},
	"run":     {"--rm": 0, "-T": 0, "--no-deps": 0, "-e": 1, "--env": 1, "--quiet-pull": 0, "-w": 1, "--workdir": 1},
	"start":   {},
	"stop":    {"-t": 1, "--timeout": 1},
	"top":     {},
	"up": {"-d": 0, "--detach": 0, "--no-build": 0, "--pull": 1, "--force-recreate": 0, "--wait": 0, "--remove-orphans": 0,
		"--no-start": 0, "--no-deps": 0, "--quiet-pull": 0, "--no-recreate": 0, "--wait-timeout": 1},
}

// CheckVerb checks a compose command: its verb, and every flag up to where its
// arguments stop being compose's. Compose takes a flag after a service too, so
// only what follows exec's and run's service is not compose's.
func (p Policy) CheckVerb(argv []string) error {
	if len(argv) == 0 {
		return refuse("no compose command")
	}
	verb, rest := argv[0], argv[1:]
	known, ok := Verbs[verb]
	if !ok {
		return refuse("not a request: %s", verb)
	}
	for i := 0; i < len(rest); {
		arg := rest[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			if verb == "exec" || verb == "run" {
				return nil
			}
			i++
			continue
		}
		name, value, hasValue := strings.Cut(arg, "=")
		takes, ok := known[name]
		if !ok {
			return refuse("%s: flag %s", verb, arg)
		}
		if takes == 0 && hasValue {
			return refuse("%s: flag %s", verb, arg)
		}
		next := i + 1
		if takes == 1 && !hasValue {
			if next >= len(rest) {
				return refuse("%s: flag %s takes a value", verb, arg)
			}
			value = rest[next]
			next++
		}
		// A start runs the image the door claimed, never one compose pulls then: missing
		// would override the file's pull_policy never.
		if name == "--pull" && takes == 1 && value != "never" {
			return refuse("%s: --pull takes only never: pull first, then start", verb)
		}
		// `--rmi all` would take images other projects run.
		if name == "--rmi" && value != "local" {
			return refuse("%s: --rmi takes only local", verb)
		}
		// A shell's user (devopsy.shell.user) is held as a service's `user:` is.
		if name == "-u" || name == "--user" {
			if why := p.userRefused(value); why != "" {
				return refuse("%s: user %s", verb, why)
			}
		}
		i = next
	}
	return nil
}
