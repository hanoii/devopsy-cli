package cli

import (
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// minSecretLen is the shortest value masked in messages: shorter ones (1,
// prod, true) would garble ordinary arguments. Listings mask a secret's
// value whatever its length.
const minSecretLen = 8

// Masked is what devopsy prints instead of a secret.
const Masked = "***"

// Secrets hides secret values in what devopsy prints: the values of the
// variables the project's secrets: names.
type Secrets struct {
	names  []string
	values []string
}

// NewSecrets collects the values to mask from env: every variable names
// matches (config.yaml's secrets:), wherever it came from.
func NewSecrets(env *Env, names []string) *Secrets {
	s := &Secrets{names: names}
	for _, k := range env.keys {
		if s.Named(k) {
			s.Add(env.values[k])
		}
	}
	return s
}

// Named reports whether variable k is a secret.
func (s *Secrets) Named(k string) bool {
	if s == nil {
		return false
	}
	return matchAny(s.names, k)
}

// matchAny reports whether k matches one of patterns (secrets: entries).
func matchAny(patterns []string, k string) bool {
	for _, n := range patterns {
		if ok, _ := path.Match(n, k); ok {
			return true
		}
	}
	return false
}

// secretLike matches names that look like a secret's. Never a rule for
// masking: only what --release and --vars set ask about.
var secretLike = regexp.MustCompile(`(?i)(PASSWORD|PASSWD|SECRET|TOKEN|SALT|CREDENTIAL|PRIVATE|AUTH|(^|_)KEY$)`)

// UnnamedSecrets lists, sorted and each once, the names that look like
// secrets and that neither secrets nor public lists.
func UnnamedSecrets(names, secrets, public []string) []string {
	var out []string
	for _, k := range names {
		if secretLike.MatchString(k) && !matchAny(secrets, k) && !matchAny(public, k) && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Value is how a listing prints variable k's value v: *** for a secret
// (empty stays empty), else v with any secret in it masked.
func (s *Secrets) Value(k, v string) string {
	if s.Named(k) && v != "" {
		return Masked
	}
	return s.Mask(v)
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
		msg = strings.ReplaceAll(msg, v, Masked)
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
