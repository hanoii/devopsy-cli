package remote

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// RestrictedKeyOptions is what a CI key gets in authorized_keys: no shell,
// no pty, no forwarding, nothing but commands. A sync across servers
// needs none of it: the parent reaches the child with a key of the sync's
// own (SyncKeyLine).
const RestrictedKeyOptions = "restrict"

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

// AskedAsProjectUser is a --for script's exit when the session's user is a project user,
// whose keys are its deploy user's to edit: the deploy user's name is on stdout.
const AskedAsProjectUser = 5

// askedAsProjectUser exits AskedAsProjectUser, the deploy user's name on stdout (the owner of
// the devopsy it finds), when the session's user is a project user: what follows is the
// deploy user's to do.
var askedAsProjectUser = `if id -nG | tr ' ' '\n' | grep -qx ` + DoorGroup + `; then
  stat -c %U "$(readlink -f "$(command -v devopsy)")" 2>/dev/null || :
  exit ` + fmt.Sprint(AskedAsProjectUser) + `
fi
`

// keysPrelude is the file whose keys a --ssh-keys script edits: the deploy user's own
// authorized_keys, or with user a project user's keys in the door's directory (DoorKeysFile),
// which the deploy user edits and sshd asks it for (--authorized-keys).
func keysPrelude(user string) string {
	if user == "" {
		return `set -eu
umask 077
f=$HOME/.ssh/authorized_keys
`
	}
	return `set -eu
umask 077
` + askedAsProjectUser + `id -u ` + Quote(user) + ` >/dev/null 2>&1 || { ` + echoLine(report.ErrorLevel, "no project user "+user+" here") + `; exit 3; }
` + doorDirShell + `
f=$door/keys/` + user + `
sf=$door/sync-keys/` + user + `
`
}

// doorDirShell sets $door to DoorDir in a script that runs without the server's devopsy (the
// --ssh-keys scripts): the same rule, kept in step by TestDoorDirShell.
const doorDirShell = `if [ -n "${DEVOPSY_HOME:-}" ]; then door=$DEVOPSY_HOME/door
elif [ -n "${XDG_CONFIG_HOME:-}" ]; then door=$XDG_CONFIG_HOME/devopsy/door
else door=$HOME/.config/devopsy/door
fi`

// keysLock is a lock beside the file: two edits at once would lose one. The --ssh-keys
// scripts say their lines with echoLine, since they need nothing of the server's devopsy.
var keysLock = `d=$(dirname "$f")
mkdir -p "$d"
chmod 700 "$d"
exec 9>"$d/.$(basename "$f").lock"
flock -w 60 9 || { ` + echoLine(report.ErrorLevel, "another edit of $f did not end in a minute") + `; exit 75; }
`

// keysSettle, under keysLock, finishes what a project user's key's removal or the user's own
// removal began and did not see through (DoorEndingFile, "$f.ending"; DoorRemovingFile,
// "$f.removing"), before anything else of that user's keys: a removal's keys go here, under
// this lock, and the end (EndProjectUserFlag) does the rest, what the door kept for a removal
// included. It runs again until it completes.
func keysSettle(user string) string {
	if user == "" {
		return ""
	}
	return `if [ -e "$f.ending" ] || [ -e "$f.removing" ]; then
  if [ -e "$f.removing" ]; then : >"$f.tmp" && mv -f "$f.tmp" "$f"; fi
  devopsy ` + EndProjectUserFlag + ` ` + Quote(user) + ` || { ` + echoLine(report.ErrorLevel, "the end of "+user+"'s processes a removal began did not complete: ask again") + `; exit 1; }
  rm -f "$f.ending"
fi
`
}

// KeysReadScript prints the keys of user ("" the deploy user), if any.
func KeysReadScript(user string) string {
	settle := ""
	if user != "" {
		settle = `if [ -e "$f.ending" ] || [ -e "$f.removing" ]; then
` + keysLock + keysSettle(user) + `fi
`
	}
	return keysPrelude(user) + settle + `[ ! -f "$f" ] || cat "$f"
`
}

// KeysAddScript appends the authorized_keys line on stdin to user's keys ("" the deploy
// user's), unless its key is there already (exit 3). It prints the file's path.
func KeysAddScript(user string) string {
	return keysPrelude(user) + keysLock + keysSettle(user) + `line=$(cat)
blob=$(printf '%s\n' "$line" | awk '{ for (i = 1; i <= NF; i++) if ($i ~ /^(ssh-|ecdsa-|sk-)/) { print $(i + 1); exit } }')
[ -n "$blob" ] || { ` + echoLine(report.ErrorLevel, "not a public key") + `; exit 1; }
if [ -f "$f" ] && grep -qF -- " $blob" "$f"; then
  ` + echoLine(report.ErrorLevel, "that key is authorized already") + `
  exit 3
fi
{ [ ! -f "$f" ] || awk 1 "$f"; printf '%s\n' "$line"; } > "$f.tmp"
mv -f "$f.tmp" "$f"
printf '%s\n' "$f"
`
}

// KeysRemoveScript drops from user's keys ("" the deploy user's) the lines whose key is the
// blob on stdin: none (exit 3). The deploy user's last key stays (exit 4: it would lock
// everyone out); a project user's goes, which is how its logins stop, and its sync keys
// with it (DoorSyncKeysFile). Any removal of a project user's key ends every process it runs
// and its linger (EndProjectUserFlag): a key removed is one no longer trusted, and what it may
// have left running, or set to start at boot, would outlive its logins, and keep the door by
// its group.
func KeysRemoveScript(user string) string {
	last, end := "", ""
	if user == "" {
		last = `if ! ` + keysLeft(`"$f.tmp"`) + `; then
  rm -f "$f.tmp"
  ` + echoLine(report.ErrorLevel, "that is the last key: removing it would lock everyone out") + `
  exit 4
fi
`
	} else {
		// What it left running, its linger and its sync keys, which a leaked key could have
		// added, go as at --project-users remove, in one helper of devopsy's.
		end = `devopsy ` + EndProjectUserFlag + ` ` + Quote(user) + ` || { ` + echoLine(report.ErrorLevel, "the key is gone, and the end of "+user+"'s processes did not complete: ask again, as any edit or listing of its keys does") + `; exit 1; }
rm -f "$f.ending"
`
	}
	mark := ""
	if user != "" {
		mark = `: >"$f.ending"
`
	}
	return keysPrelude(user) + keysLock + keysSettle(user) + `blob=$(cat)
[ -f "$f" ] || { ` + echoLine(report.ErrorLevel, "no authorized keys on this server") + `; exit 3; }
grep -qF -- " $blob" "$f" || { ` + echoLine(report.ErrorLevel, "that key is not authorized") + `; exit 3; }
grep -vF -- " $blob" "$f" > "$f.tmp" || true
` + last + mark + `mv -f "$f.tmp" "$f"
` + end + `printf '%s\n' "$f"
`
}

// KeysLeft says whether file holds a key anyone logs in with: comments and blank lines aside
// (a commented-out key is none), and sync keys (SyncKeyLine), which only do their sync's
// requests. keysLeft is the same test in a script (TestKeysLeftOneRule).
func KeysLeft(file string) bool {
	data, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	for _, k := range ParseAuthorizedKeys(string(data)) {
		if _, sync := SyncKeyID(k); !sync {
			return true
		}
	}
	return false
}

// keysLeft is KeysLeft in a script, file a shell word: the deploy user's last key's guard,
// under its lock, behind the operator's own check by KeysLeft's parse (sshkeys.go). A key's
// blob is base64 and long; a line of words that only names a type is none.
func keysLeft(file string) string {
	return `grep -Ev '^[[:space:]]*(#|$)' ` + file + ` | grep -Ev ' ` + syncKeyPrefix + strings.Trim(SyncIDPattern.String(), "^$") + `$' | grep -Eq '(^|[[:space:]])(ssh-|ecdsa-|sk-)[^[:space:]]+[[:space:]]+[A-Za-z0-9+/]{16,}={0,3}([[:space:]]|$)'`
}

// EndUserArgs are docker's arguments to end what the user of uid, named user, runs or would
// run with no session: devopsy itself (the binary at devopsy, static, mounted read-only) as
// root in UtilImage under the host's pids, which clears its linger and then kills every
// process of the user's at once (EndUIDFlag). No container runs as a project user's uid
// (hostIDs), so it ends none. It runs outside Docker's AppArmor profile: docker-default lets
// a container signal only its own peers, never a host process, so on a host with AppArmor
// on, its kills would reach none of the user's.
func EndUserArgs(uid, user, devopsy string) []string {
	return []string{"run", "--rm", "--pid", "host", "--network", "none", "--read-only", "--cap-drop", "MKNOD", "--security-opt", "no-new-privileges", "--security-opt", "apparmor=unconfined",
		"-v", "/var/lib/systemd/linger:/linger", "-v", devopsy + ":/devopsy:ro", UtilImage, "/devopsy", EndUIDFlag, uid, user}
}

// EndUIDFlag is the hidden flag EndUserArgs runs in its container, root under the host's
// pids: `devopsy --end-uid <uid> <user>`.
const EndUIDFlag = "--end-uid"

// EndProjectUserFlag is the hidden flag that ends a project user once a key of its is gone
// (KeysRemoveScript), as --project-users remove does: its processes and linger
// (EndUserArgs), then its sync keys, under their lock. `devopsy --end-project-user <user>`,
// as the deploy user.
const EndProjectUserFlag = "--end-project-user"

// DoorGroup is the group every project user is in: sshd's Match, the door's sudo rules and
// the firewall rule go by it.
const DoorGroup = "devopsy-projects"

// DoorRunGroup is the group the door runs under, its primary one through sudo's -g: the
// firewall covers every process the door starts on the host by it (firewallJumps), so a registry
// client of compose's or buildx's, which a project's registry answers as it likes, reaches no
// loopback or metadata address. No user is in it.
const DoorRunGroup = "devopsy-door"

// DoorKeysFile is a project user's keys, in the door's directory (DoorDir): what
// --authorized-keys prints for sshd, whose environment for it is its own (HOME, USER, PATH),
// never the login's. keysPrelude finds the same file in a script.
func DoorKeysFile(user string) string {
	return filepath.Join(DoorDir(), "keys", user)
}

// DoorEndingFile marks a project user whose key's removal began the end of its processes
// (EndProjectUserFlag) and did not see it through (a failure, a cut session): written under the
// keys' lock before the key goes, removed once the end completes, and settled by the next
// edit or listing of that user's keys (keysSettle). The door answers no request of the user's
// while it is there: what the removed key left running would otherwise keep the door.
func DoorEndingFile(user string) string {
	return DoorKeysFile(user) + ".ending"
}

// DoorRemovingFile marks a project user whose removal (--project-users remove) did not see
// itself through: written, with the project's name in it, under the keys' lock together with
// the entry's and the keys' removal, and removed last. A key's removal clears only its own
// mark (DoorEndingFile), so this one stays until what it says is done: the keys empty, the
// end, what the door kept for the project (its logins, its record) gone. Whatever settles a
// user (a removal asked again, keysSettle) finishes it, and the door answers the user nothing
// while it is there.
func DoorRemovingFile(user string) string {
	return DoorKeysFile(user) + ".removing"
}

// DoorSyncKeysFile is a project user's sync keys (SyncKeyLine), apart from its keys: the
// door writes them for that user's syncs (--sync-key), --authorized-keys prints them after
// its keys, and removing any of its keys drops them all, since a leaked key could have
// added one.
func DoorSyncKeysFile(user string) string {
	return filepath.Join(DoorDir(), "sync-keys", user)
}
