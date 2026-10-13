package remote

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

// RestrictedKeyOptions is what a CI key gets in authorized_keys: no shell,
// no X11, nothing but commands, plus the two forwardings a clone across
// servers needs (the operator's agent to the parent, the parent's reverse
// forward to the child's rsync daemon).
const RestrictedKeyOptions = "restrict,agent-forwarding,port-forwarding"

// AuthorizedKey is one line of the deploy user's authorized_keys.
type AuthorizedKey struct {
	Options string // before the key, empty for none
	Type    string // ssh-ed25519, ecdsa-sha2-nistp256, sk-ssh-ed25519@openssh.com...
	Blob    string // the base64 key
	Comment string
}

// Line is the key as authorized_keys has it.
func (k AuthorizedKey) Line() string {
	s := k.Type + " " + k.Blob
	if k.Comment != "" {
		s += " " + k.Comment
	}
	if k.Options != "" {
		s = k.Options + " " + s
	}
	return s
}

// Fingerprint is the key's SHA256 fingerprint, as ssh-keygen -l prints it.
func (k AuthorizedKey) Fingerprint() (string, error) {
	raw, err := base64.StdEncoding.DecodeString(k.Blob)
	if err != nil || len(raw) == 0 {
		return "", errors.New("not a public key")
	}
	sum := sha256.Sum256(raw)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), nil
}

// isKeyType reports whether a token starts a public key.
func isKeyType(s string) bool {
	return strings.HasPrefix(s, "ssh-") || strings.HasPrefix(s, "ecdsa-") || strings.HasPrefix(s, "sk-")
}

// ParseAuthorizedKey reads one line: options (which may quote spaces),
// type, blob and comment. A blank line or a comment is nil.
func ParseAuthorizedKey(line string) (*AuthorizedKey, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return nil, nil
	}
	// Options end where the key type starts; a quoted option may hold
	// spaces, so walk the fields minding quotes.
	var fields []string
	var cur strings.Builder
	quoted := false
	for _, r := range line {
		switch {
		case r == '"':
			quoted = !quoted
			cur.WriteRune(r)
		case (r == ' ' || r == '\t') && !quoted:
			if cur.Len() > 0 {
				fields = append(fields, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		fields = append(fields, cur.String())
	}
	i := 0
	for i < len(fields) && !isKeyType(fields[i]) {
		i++
	}
	if i+1 >= len(fields) {
		return nil, errors.New("not a public key: " + line)
	}
	k := &AuthorizedKey{Options: strings.Join(fields[:i], " "), Type: fields[i], Blob: fields[i+1], Comment: strings.Join(fields[i+2:], " ")}
	if _, err := k.Fingerprint(); err != nil {
		return nil, errors.New("not a public key: " + line)
	}
	return k, nil
}

// ParseAuthorizedKeys reads a whole file, skipping what is not a key.
func ParseAuthorizedKeys(content string) []AuthorizedKey {
	var keys []AuthorizedKey
	for _, line := range strings.Split(content, "\n") {
		if k, err := ParseAuthorizedKey(line); err == nil && k != nil {
			keys = append(keys, *k)
		}
	}
	return keys
}

// keysPrelude is the deploy user's authorized_keys, and a lock next to it:
// two edits at once would lose one.
const keysPrelude = `set -eu
umask 077
f=$HOME/.ssh/authorized_keys
`

const keysLock = `mkdir -p "$HOME/.ssh"
chmod 700 "$HOME/.ssh"
exec 9>"$HOME/.ssh/.authorized_keys.lock"
flock -w 60 9 || { echo "devopsy: another edit of $f did not end in a minute" >&2; exit 75; }
`

// KeysReadScript prints the deploy user's authorized_keys, if any.
func KeysReadScript() string {
	return keysPrelude + `[ ! -f "$f" ] || cat "$f"
`
}

// KeysAddScript appends the authorized_keys line on stdin, unless its key
// is there already (exit 3). It prints the file's path.
func KeysAddScript() string {
	return keysPrelude + keysLock + `line=$(cat)
blob=$(printf '%s\n' "$line" | awk '{ for (i = 1; i <= NF; i++) if ($i ~ /^(ssh-|ecdsa-|sk-)/) { print $(i + 1); exit } }')
[ -n "$blob" ] || { echo "devopsy: not a public key" >&2; exit 1; }
if [ -f "$f" ] && grep -qF -- " $blob" "$f"; then
  echo "devopsy: that key is authorized already" >&2
  exit 3
fi
{ [ ! -f "$f" ] || awk 1 "$f"; printf '%s\n' "$line"; } > "$f.tmp"
mv -f "$f.tmp" "$f"
printf '%s\n' "$f"
`
}

// KeysRemoveScript drops the lines whose key is the blob on stdin: none
// (exit 3), or the last key left (exit 4: it would lock everyone out).
func KeysRemoveScript() string {
	return keysPrelude + keysLock + `blob=$(cat)
[ -f "$f" ] || { echo "devopsy: no authorized keys here" >&2; exit 3; }
grep -qF -- " $blob" "$f" || { echo "devopsy: that key is not authorized" >&2; exit 3; }
grep -vF -- " $blob" "$f" > "$f.tmp" || true
if ! grep -Eq '(^|[[:space:]])(ssh-|ecdsa-|sk-)' "$f.tmp"; then
  rm -f "$f.tmp"
  echo "devopsy: that is the last key: removing it would lock everyone out" >&2
  exit 4
fi
mv -f "$f.tmp" "$f"
printf '%s\n' "$f"
`
}
