package remote

import "testing"

func TestAuthorizedKeys(t *testing.T) {
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g sample@test"
	const fp = "SHA256:n/tqoqzvcpkAa4l/m4VLZA4qrzyW77jYa7MCEQpfF/Q"
	k, err := ParseAuthorizedKey(key)
	if err != nil || k.Options != "" || k.Type != "ssh-ed25519" || k.Comment != "sample@test" || k.Line() != key {
		t.Fatalf("plain: %+v %v", k, err)
	}
	if got, _ := k.Fingerprint(); got != fp {
		t.Errorf("fingerprint %s, want %s (ssh-keygen -lf)", got, fp)
	}
	// Options, one quoting a space; the comment may have spaces too.
	k, err = ParseAuthorizedKey(`restrict,command="echo hi there",agent-forwarding ` + key + " more words")
	if err != nil || k.Options != `restrict,command="echo hi there",agent-forwarding` || k.Comment != "sample@test more words" || k.Blob != "AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g" {
		t.Errorf("options: %+v %v", k, err)
	}
	for _, bad := range []string{"not a key", "ssh-ed25519", "ssh-ed25519 ***", "-----BEGIN OPENSSH PRIVATE KEY-----"} {
		if k, err := ParseAuthorizedKey(bad); err == nil {
			t.Errorf("%q parsed: %+v", bad, k)
		}
	}
	if k, err := ParseAuthorizedKey("# a comment"); err != nil || k != nil {
		t.Errorf("comment line: %+v %v", k, err)
	}
	if keys := ParseAuthorizedKeys("# head\n\n" + key + "\nbroken line\n" + RestrictedKeyOptions + " " + key + "\n"); len(keys) != 2 || keys[1].Options != RestrictedKeyOptions {
		t.Errorf("file: %+v", keys)
	}
}
