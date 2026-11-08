package remote

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// A sync's key pair is OpenSSH's own: ssh-keygen reads the private half
// and derives the public half devopsy authorizes from it.
func TestNewSyncKey(t *testing.T) {
	t.Parallel()
	priv, pub, err := NewSyncKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(priv), "-----BEGIN OPENSSH PRIVATE KEY-----\n") || !strings.HasPrefix(pub, "ssh-ed25519 ") {
		t.Fatalf("private:\n%s\npublic: %s", priv, pub)
	}
	if other, _, _ := NewSyncKey(); string(other) == string(priv) {
		t.Fatal("two syncs got one key")
	}
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen")
	}
	f := filepath.Join(t.TempDir(), "key")
	if err := testfile.Write(f, priv, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(keygen, "-y", "-f", f).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh-keygen -y: %v\n%s", err, out)
	}
	if got := strings.Fields(string(out)); len(got) < 2 || got[0]+" "+got[1] != pub {
		t.Fatalf("ssh-keygen derives %q, devopsy authorizes %q", out, pub)
	}
}

// A sync key's line is devopsy's own: its options never come from who
// asks, and nothing it is given breaks out of them (a " or a , in the id
// would end the quoted command and add options of the asker's).
func TestSyncKeyLine(t *testing.T) {
	t.Parallel()
	_, pub, err := NewSyncKey()
	if err != nil {
		t.Fatal(err)
	}
	blob := strings.Fields(pub)[1]
	at := time.Date(2026, 11, 10, 9, 8, 7, 0, time.UTC)
	line, err := SyncKeyLine("/usr/local/bin/devopsy", "1762590000.0a1b2c", "/srv/shop/stage", "vm2:stage", true, blob, at)
	if err != nil {
		t.Fatal(err)
	}
	want := `restrict,port-forwarding,permitopen="127.0.0.1:1",permitlisten="127.0.0.1:*",expiry-time="20261110090807Z",command="/usr/local/bin/devopsy --sync-child 1762590000.0a1b2c /srv/shop/stage vm2:stage reset" ssh-ed25519 ` + blob + ` devopsy-sync-1762590000.0a1b2c`
	if line != want {
		t.Fatalf("line:\n%s\nwant:\n%s", line, want)
	}
	k, err := ParseAuthorizedKey(line)
	if err != nil || k == nil || k.Comment != "devopsy-sync-1762590000.0a1b2c" || !strings.Contains(k.Options, `command="/usr/local/bin/devopsy --sync-child 1762590000.0a1b2c /srv/shop/stage vm2:stage reset"`) {
		t.Fatalf("parsed back: %+v %v", k, err)
	}
	if id, ok := SyncKeyID(*k); !ok || id != "1762590000.0a1b2c" {
		t.Fatalf("SyncKeyID: %q %v", id, ok)
	}
	if exp, ok := SyncKeyExpiry(*k); !ok || !exp.Equal(at) {
		t.Fatalf("SyncKeyExpiry: %v %v", exp, ok)
	}
	if line, err := SyncKeyLine("/usr/local/bin/devopsy", "1762590000.0a1b2c", "/srv/shop/stage", "vm2:stage", false, blob, at); err != nil || !strings.Contains(line, `vm2:stage" ssh-ed25519`) {
		t.Fatalf("no reset: %s %v", line, err)
	}
	for _, c := range []struct{ devopsy, id, base, to, blob string }{
		{"/usr/local/bin/devopsy", `1.0a1b2c" ,agent-forwarding`, "/srv/shop/stage", "vm2:stage", blob},
		{"/usr/local/bin/devopsy", "1.0a1b2c,x", "/srv/shop/stage", "vm2:stage", blob},
		{"/usr/local/bin/devopsy", "1.0A1B2C", "/srv/shop/stage", "vm2:stage", blob},
		{"/usr/local/bin/devopsy", "1.0a1b2c", `/srv/shop" /x`, "vm2:stage", blob},
		{"/usr/local/bin/devopsy", "1.0a1b2c", "/srv/shop/../x", "vm2:stage", blob},
		{"/usr/local/bin/devopsy", "1.0a1b2c", "srv/shop", "vm2:stage", blob},
		{"/usr/local/bin/devopsy", "1.0a1b2c", "/srv/shop/stage", `vm2" x`, blob},
		{"/usr/local/bin/devopsy", "1.0a1b2c", "/srv/shop/stage", "vm2:stage,x", blob},
		{"/usr/local/bin/devopsy", "1.0a1b2c", "/srv/shop/stage", "vm2:stage", blob + `" x`},
		{"/usr/local/bin/devopsy", "1.0a1b2c", "/srv/shop/stage", "vm2:stage", "AAAA"},
		{`/usr/local/bin/devopsy" x`, "1.0a1b2c", "/srv/shop/stage", "vm2:stage", blob},
		{"devopsy", "1.0a1b2c", "/srv/shop/stage", "vm2:stage", blob},
	} {
		if line, err := SyncKeyLine(c.devopsy, c.id, c.base, c.to, false, c.blob, at); err == nil {
			t.Errorf("%+v: took it:\n%s", c, line)
		}
	}
}

// The child's host keys go to the push as known_hosts lines under the
// sync's alias, whatever port or address the push connects to; anything
// but a public key line is left out.
func TestSyncKnownHosts(t *testing.T) {
	t.Parallel()
	_, pub, err := NewSyncKey()
	if err != nil {
		t.Fatal(err)
	}
	got := SyncKnownHosts("1762590000.0a1b2c", pub+" root@vm2\nnot a key\n\necdsa-sha2-nistp256 AAAAE2VjZHNh\n")
	want := "devopsy-sync-1762590000.0a1b2c " + pub + "\ndevopsy-sync-1762590000.0a1b2c ecdsa-sha2-nistp256 AAAAE2VjZHNh\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if SyncHostKeyAlias("1762590000.0a1b2c") != "devopsy-sync-1762590000.0a1b2c" {
		t.Fatal(SyncHostKeyAlias("1762590000.0a1b2c"))
	}
}
