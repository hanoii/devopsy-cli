package remote

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// BUG: the two spellings of "a key is left" differ on a line that is no key. KeysLeft, which
// the door and --authorized-keys go by, takes only lines that parse (a type and a blob that
// decodes); keysLeft, the removal's script, takes any line with a word starting like a key
// type. With such a line in a project user's keys (an edit by hand, a key cut short), the
// removal of its last real key finds "a key left" and ends no process, while sshd is given
// no key and the door refuses the user: what it left running stays.
//
// A file with no line sshd is given holds no key, for the script as for the door.
func TestKeysLeftOneRuleOnALineThatIsNoKey(t *testing.T) {
	t.Parallel()
	for _, content := range []string{
		"ssh-ed25519 not*base64 laptop\n",
		"ssh-ed25519\n",
		"the old ssh-ed25519 key went on 2026-01-01\n",
	} {
		f := filepath.Join(t.TempDir(), "keys")
		write(t, f, content)
		shell := exec.Command("sh", "-c", keysLeft(`"$1"`), "sh", f).Run() == nil
		if got := KeysLeft(f); got || shell {
			t.Errorf("%q: KeysLeft %v, the script's %v, and --authorized-keys prints %d keys", content, got, shell, len(ParseAuthorizedKeys(content)))
		}
	}
}
