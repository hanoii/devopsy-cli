package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/testfile"
)

func TestAuthorizedKeys(t *testing.T) {
	t.Parallel()
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

// A commented-out key is no key: removing the last real one is refused.
func TestKeysRemoveLast(t *testing.T) {
	t.Parallel()
	home, bin := t.TempDir(), t.TempDir()
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(filepath.Join(bin, "flock"), 0o755); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(home, ".ssh", "authorized_keys")
	remove := func(blob string) int {
		cmd := exec.Command("sh", "-c", KeysRemoveScript(""))
		cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin"}
		cmd.Stdin = strings.NewReader(blob + "\n")
		_ = cmd.Run()
		return cmd.ProcessState.ExitCode()
	}
	write(t, keys, "ssh-ed25519 AAAAone laptop\n# ssh-ed25519 AAAAold retired\n")
	if code := remove("AAAAone"); code != 4 || !strings.Contains(readFile(keys), "AAAAone") {
		t.Errorf("last key beside a comment: exit %d, file %q", code, readFile(keys))
	}
	write(t, keys, "ssh-ed25519 AAAAone laptop\nssh-ed25519 AAAAtwo ci\n")
	if code := remove("AAAAone"); code != 0 || strings.Contains(readFile(keys), "AAAAone") {
		t.Errorf("one of two keys: exit %d, file %q", code, readFile(keys))
	}
}

// A project user's keys are found by two: the --for scripts, from the deploy user's
// environment, and --authorized-keys, from sshd's (DoorKeysFile). Both are one file, wherever
// the deploy user's config is (DEVOPSY_HOME, XDG_CONFIG_HOME, else ~/.config).
//
// Not parallel: it sets DEVOPSY_HOME, XDG_CONFIG_HOME and HOME (t.Setenv).
func TestDoorKeysFileOneRule(t *testing.T) {
	bin := t.TempDir()
	if err := testfile.Write(filepath.Join(bin, "id"), []byte("#!/bin/sh\n[ \"$1\" = -u ] && echo 10002 || echo devopsy\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, env := range [][2]string{{"DEVOPSY_HOME", t.TempDir()}, {"XDG_CONFIG_HOME", t.TempDir()}, {"HOME", t.TempDir()}} {
		t.Setenv("DEVOPSY_HOME", "")
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv(env[0], env[1])
		cmd := exec.Command("sh", "-c", keysPrelude("devopsy-shop")+`printf '%s' "$f"`)
		cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("%s: %v", env[0], err)
		}
		if want := DoorKeysFile("devopsy-shop"); string(out) != want {
			t.Errorf("%s: the script's %s, --authorized-keys's %s", env[0], out, want)
		}
	}
}
