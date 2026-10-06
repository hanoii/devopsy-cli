package cli

import (
	"regexp"
	"sort"
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

// VerboseEnv is the variable that turns on verbose output, like --verbose.
// devopsy sets it when given the flag, so nested devopsy calls (project
// commands calling devopsy, devopsy on a server) are verbose too.
const VerboseEnv = "DEVOPSY_VERBOSE"

// IsVerbose reports whether a DEVOPSY_VERBOSE value turns verbose output on:
// 1, true, yes or on.
func IsVerbose(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
