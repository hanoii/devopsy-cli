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
	// A sync's key is no one's way in: it only does that sync's requests.
	write(t, keys, "ssh-ed25519 AAAAone laptop\nrestrict,command=\"devopsy --sync-child 1.0a1b2c /srv/x vm2:x\" ssh-ed25519 AAAAsync devopsy-sync-1.0a1b2c\n")
	if code := remove("AAAAone"); code != 4 || !strings.Contains(readFile(keys), "AAAAone") {
		t.Errorf("last key beside a sync's: exit %d, file %q", code, readFile(keys))
	}
	// A key's blob is long: a line naming a type with a word after it is none (a key cut short).
	write(t, keys, "ssh-ed25519 AAAAone laptop\nssh-ed25519 AAAAtwo ci\n")
	if code := remove("AAAAone"); code != 4 {
		t.Errorf("last key beside a key cut short: exit %d, file %q", code, readFile(keys))
	}
	write(t, keys, "ssh-ed25519 AAAAone laptop\nssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItwo ci\n")
	if code := remove("AAAAone"); code != 0 || strings.Contains(readFile(keys), "AAAAone") {
		t.Errorf("one of two keys: exit %d, file %q", code, readFile(keys))
	}
}

// Removing any of a project user's keys ends the user once the key is gone, as --project-users
// remove does (devopsy's EndProjectUserFlag: its processes, its linger, then its sync keys): a
// key removed is one no longer trusted, and what it may have left running (nohup, setsid)
// would outlive it and keep calling the door by its group. The deploy user's own keys never
// end anything. An end that fails fails the removal: the key is gone, and the operator must
// know what runs.
func TestKeysRemoveEndsTheUsersProcesses(t *testing.T) {
	t.Parallel()
	home, bin := t.TempDir(), t.TempDir()
	calls := filepath.Join(bin, "calls")
	for name, body := range map[string]string{
		"flock":   "exit 0",
		"id":      `case "$1" in -nG) echo devopsy ;; -u) [ "$2" = devopsy-shop ] && echo 10002 || /usr/bin/id -u ;; *) exit 1 ;; esac`,
		"devopsy":`echo "devopsy $*" >>` + calls + `; [ ! -e ` + filepath.Join(bin, "fails") + ` ]`,
	} {
		if err := testfile.Write(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	keys := filepath.Join(home, "door", "keys", "devopsy-shop")
	remove := func(user, blob string) (int, string) {
		t.Helper()
		os.Remove(calls)
		cmd := exec.Command("sh", "-c", KeysRemoveScript(user))
		cmd.Env = []string{"HOME=" + home, "DEVOPSY_HOME=" + filepath.Join(home), "PATH=" + bin + ":/usr/bin:/bin"}
		cmd.Stdin = strings.NewReader(blob + "\n")
		out, _ := cmd.CombinedOutput()
		got, _ := os.ReadFile(calls)
		return cmd.ProcessState.ExitCode(), string(out) + string(got)
	}
	ended := func(out string) bool { return strings.Contains(out, "devopsy "+EndProjectUserFlag+" devopsy-shop\n") }
	write(t, keys, "ssh-ed25519 AAAAone laptop\nssh-ed25519 AAAAtwo ci\n")
	if code, out := remove("devopsy-shop", "AAAAone"); code != 0 || !ended(out) {
		t.Errorf("one of two keys (%d) leaves the user:\n%s", code, out)
	}
	if code, out := remove("devopsy-shop", "AAAAtwo"); code != 0 || !ended(out) {
		t.Errorf("its last key (%d) leaves the user:\n%s", code, out)
	}
	write(t, filepath.Join(bin, "fails"), "")
	write(t, keys, "ssh-ed25519 AAAAtwo ci\n")
	if code, out := remove("devopsy-shop", "AAAAtwo"); code == 0 || !ended(out) || readFile(keys) != "" {
		t.Errorf("an end that failed (%d) passes, or keeps the key:\n%s", code, out)
	}
	os.Remove(filepath.Join(bin, "fails"))
	write(t, filepath.Join(home, ".ssh", "authorized_keys"), "ssh-ed25519 AAAAone laptop\nssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItwo ci\n")
	if code, out := remove("", "AAAAone"); code != 0 || strings.Contains(out, EndProjectUserFlag) {
		t.Errorf("the deploy user's key (%d) ends a user:\n%s", code, out)
	}
}

// A file has a key left when it holds one anyone logs in with: a commented-out key, a sync's
// key (which only does its sync's requests) and a line that only names a type are none. The
// door's refusal of a keyless user (KeysLeft) and the deploy user's last key's guard
// (keysLeft, in its script) read it one way.
func TestKeysLeftOneRule(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g"
	for content, want := range map[string]bool{
		"":                          false,
		"# " + key + " retired\n\n": false,
		`restrict,command="devopsy --sync-child 1.0a1b2c /srv/x vm2:x" ` + key + " devopsy-sync-1.0a1b2c\n": false,
		key + " laptop\n":                              true,
		RestrictedKeyOptions + " " + key + " ci\n":     true,
		"ssh-ed25519 not*base64 laptop\n":              false,
		"ssh-ed25519\n":                                false,
		"the old ssh-ed25519 key went on 2026-01-01\n": false,
	} {
		f := filepath.Join(dir, "keys")
		write(t, f, content)
		shell := exec.Command("sh", "-c", keysLeft(`"$1"`), "sh", f).Run() == nil
		if got := KeysLeft(f); got != want || shell != want {
			t.Errorf("%q: KeysLeft %v, the script's %v, want %v", content, got, shell, want)
		}
	}
	if KeysLeft(filepath.Join(dir, "missing")) {
		t.Error("a missing file has keys")
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
