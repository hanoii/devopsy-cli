package remote

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// A sync across servers reaches the child with a key made for it alone:
// the operator's devopsy makes the pair (NewSyncKey), the holder on the
// child authorizes the public half (SyncKeyLine, through --sync-key) and
// the push on the parent gets the private half on its stdin. The key's
// forced command (--sync-child) does that sync's requests on the child
// and nothing else, while that sync is open there. The operator's agent
// is never forwarded.

// SyncKeyFlag is the hidden flag the holder authorizes and removes its
// sync's key with (cmd/devopsy's runSyncKey), SyncChildFlag the key's
// forced command (runSyncChild).
const (
	SyncKeyFlag   = "--sync-key"
	SyncChildFlag = "--sync-child"
)

// SyncIDPattern is a sync's id (cmd/devopsy's newSyncID: the time, a dot,
// six hex digits), checked wherever an id is put in a key's line or read
// from one: a " or a , would end the line's quoted option.
var SyncIDPattern = regexp.MustCompile(`^[0-9]{1,20}\.[0-9a-f]{6}$`)

// syncKeyPrefix starts a sync key's comment, the sync's id after it.
const syncKeyPrefix = "devopsy-sync-"

// SyncKeyLife is how long a sync key lasts on the child at most: it
// only bounds a key nothing removed (the holder removes its own, and what
// settles a cut sync), and a sync's later sessions (its streams) log in
// long after its first.
const SyncKeyLife = 48 * time.Hour

// syncAddress is what a key's line may hold of the child's address (for
// the child's hints): a target's characters, never a quote, a comma or a
// space.
var syncAddress = regexp.MustCompile(`^[A-Za-z0-9@._:/\[\]-]+$`)

// syncPathChars is what a key's line may hold of a path.
var syncPathChars = regexp.MustCompile(`^/[A-Za-z0-9._/+-]+$`)

// NewSyncKey makes an ed25519 pair: the private half in OpenSSH's own
// format, which ssh reads, and the public half as authorized_keys has it,
// "ssh-ed25519 <blob>".
func NewSyncKey() (private []byte, public string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	wire := sshString(nil, []byte("ssh-ed25519"))
	wire = sshString(wire, pub)
	var check [4]byte
	if _, err := rand.Read(check[:]); err != nil {
		return nil, "", err
	}
	// The private section: two equal check words, the key, a comment, then
	// padding 1, 2, 3... to a block of 8 (cipher none).
	sec := append(append([]byte{}, check[:]...), check[:]...)
	sec = sshString(sec, []byte("ssh-ed25519"))
	sec = sshString(sec, pub)
	sec = sshString(sec, priv)
	sec = sshString(sec, []byte("devopsy-sync"))
	for i := byte(1); len(sec)%8 != 0; i++ {
		sec = append(sec, i)
	}
	blob := []byte("openssh-key-v1\x00")
	blob = sshString(blob, []byte("none"))
	blob = sshString(blob, []byte("none"))
	blob = sshString(blob, nil)
	blob = binary.BigEndian.AppendUint32(blob, 1)
	blob = sshString(blob, wire)
	blob = sshString(blob, sec)
	private = pem.EncodeToMemory(&pem.Block{Type: "OPENSSH PRIVATE KEY", Bytes: blob})
	return private, "ssh-ed25519 " + base64.StdEncoding.EncodeToString(wire), nil
}

// sshString appends b to buf as SSH's wire format has a string: its
// length, then its bytes.
func sshString(buf, b []byte) []byte {
	return append(binary.BigEndian.AppendUint32(buf, uint32(len(b))), b...)
}

// SyncKeyLine is the authorized_keys line of sync id's key blob (an
// ssh-ed25519's) on the child: no shell, no pty, no agent, no -L (permitopen
// to an address nothing listens on), a -R only on the loopback (the
// receive's, to the parent's rsync daemon; named 127.0.0.1, since sshd
// reads a forward with no address as localhost's), until expiry, and only
// devopsy (its absolute path: sshd runs a forced command with its own
// PATH) as --sync-child for that sync in the child's directory base, to
// its address and whether it is a reset. Every part is checked, since a
// project user's door writes it from what that user asks.
func SyncKeyLine(devopsy, id, base, to string, reset bool, blob string, expiry time.Time) (string, error) {
	switch {
	case !SyncIDPattern.MatchString(id):
		return "", fmt.Errorf("not a sync's id: %q", id)
	case !syncPathChars.MatchString(base) || path.Clean(base) != base || strings.Contains(base, "/../"):
		return "", fmt.Errorf("not an environment's directory: %q", base)
	case !syncPathChars.MatchString(devopsy) || path.Clean(devopsy) != devopsy:
		return "", fmt.Errorf("not devopsy's absolute path: %q", devopsy)
	case !syncAddress.MatchString(to):
		return "", fmt.Errorf("not an address: %q", to)
	}
	if !ed25519Blob(blob) {
		return "", fmt.Errorf("not an ed25519 public key: %q", blob)
	}
	command := devopsy + " " + SyncChildFlag + " " + id + " " + base + " " + to
	if reset {
		command += " reset"
	}
	return `restrict,port-forwarding,permitopen="127.0.0.1:1",permitlisten="127.0.0.1:*",expiry-time="` + expiry.UTC().Format("20060102150405") + `Z",command="` + command + `" ssh-ed25519 ` + blob + " " + syncKeyPrefix + id, nil
}

// ed25519Blob says whether blob is an ssh-ed25519 public key's base64,
// as NewSyncKey writes it: its type, then its 32 bytes.
func ed25519Blob(blob string) bool {
	b, err := base64.StdEncoding.DecodeString(blob)
	want := sshString(nil, []byte("ssh-ed25519"))
	return err == nil && len(b) == len(want)+4+ed25519.PublicKeySize && string(b[:len(want)]) == string(want) &&
		binary.BigEndian.Uint32(b[len(want):]) == ed25519.PublicKeySize
}

// SyncKeyID is the sync whose key k is, from its comment.
func SyncKeyID(k AuthorizedKey) (string, bool) {
	id, ok := strings.CutPrefix(k.Comment, syncKeyPrefix)
	return id, ok && SyncIDPattern.MatchString(id)
}

var syncKeyExpiry = regexp.MustCompile(`(^|,)expiry-time="([0-9]{14})Z"`)

// SyncKeyExpiry is when sync key k stops being accepted.
func SyncKeyExpiry(k AuthorizedKey) (time.Time, bool) {
	m := syncKeyExpiry.FindStringSubmatch(k.Options)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102150405", m[2])
	return t, err == nil
}

// SyncHostKeyAlias is the name the push's ssh checks the child's host
// key under (HostKeyAlias), whatever port or address it connects to.
func SyncHostKeyAlias(id string) string { return syncKeyPrefix + id }

// SyncKnownHosts is known_hosts for sync id: each public key line of
// keys (the child's /etc/ssh/ssh_host_*_key.pub), under its alias.
func SyncKnownHosts(id, keys string) string {
	out := ""
	for _, line := range strings.Split(keys, "\n") {
		k, err := ParseAuthorizedKey(line)
		if err != nil || k == nil || k.Options != "" {
			continue
		}
		out += SyncHostKeyAlias(id) + " " + k.Type + " " + k.Blob + "\n"
	}
	return out
}
