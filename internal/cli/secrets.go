package cli

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// minSecretLen is the shortest value masked: shorter ones (1, prod, true)
// would garble ordinary arguments.
const minSecretLen = 8

// secretName matches variables named like secrets.
var secretName = regexp.MustCompile(`(?i)(PASSWORD|PASSWD|SECRET|TOKEN|SALT|CREDENTIAL|PRIVATE|AUTH|(^|_)KEY$)`)

// Secrets hides secret values in what devopsy prints.
type Secrets struct {
	values []string
}

// NewSecrets collects the values to mask from env: every variable loaded
// from dotenvFile (the project's .env, where secrets live by convention) and
// every variable named like a secret, wherever it came from (CI sets them in
// the environment).
func NewSecrets(env *Env, dotenvFile string) *Secrets {
	s := &Secrets{}
	for _, k := range env.keys {
		if env.origin[k] == dotenvFile || secretName.MatchString(k) {
			s.Add(env.values[k])
		}
	}
	return s
}

// Add masks v from now on, unless it is too short.
func (s *Secrets) Add(v string) {
	if len(v) < minSecretLen {
		return
	}
	for _, have := range s.values {
		if have == v {
			return
		}
	}
	s.values = append(s.values, v)
	// Longest first, so a secret containing another is masked whole.
	sort.Slice(s.values, func(i, j int) bool { return len(s.values[i]) > len(s.values[j]) })
}

// Mask replaces every secret in msg with ***.
func (s *Secrets) Mask(msg string) string {
	if s == nil {
		return msg
	}
	for _, v := range s.values {
		msg = strings.ReplaceAll(msg, v, "***")
	}
	return msg
}

// VerboseEnv is the variable that turns on verbose output, like -v (1) and
// -vv (2). devopsy sets it when given the flag, so nested devopsy calls
// (project commands calling devopsy, devopsy on a server) are verbose too.
const VerboseEnv = "DEVOPSY_VERBOSE"

// VerboseLevel is a DEVOPSY_VERBOSE value's level: 0 (off), 1 (1, true, yes
// or on) or 2 (2 or more).
func VerboseLevel(v string) int {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "true", "yes", "on":
		return 1
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0
	}
	return min(n, 2)
}
