package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// BUG: removing one of a project user's keys (a rotation, others left, so the door still
// answers it) drops its sync keys before it ends its processes. What the removed key left
// running asks the door for a sync key (--door sync-key add) in between, and is then ended:
// the sync key stays, for the 48 hours of its life, and whoever holds its private half logs
// in for that sync's requests (an import or a receive into the environment) with the leaked
// key gone. Here the process still running is the fake devopsy, which the script calls
// between the two (the util image's check): it writes the sync keys file as the door would.
func TestReview4bKeyRemovalLeavesNoSyncKeyAddedMeanwhile(t *testing.T) {
	t.Parallel()
	home, bin := t.TempDir(), t.TempDir()
	syncKeys := filepath.Join(home, "door", "sync-keys", "devopsy-shop")
	const syncLine = `restrict,command="devopsy --sync-child 1.0a1b2c /home/devopsy-shop/shop/prod vm2:shop/prod" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g devopsy-sync-1.0a1b2c`
	for name, body := range map[string]string{
		"flock": "exit 0",
		"id":    `case "$1" in -nG) echo devopsy ;; -u) [ "$2" = devopsy-shop ] && echo 10002 || /usr/bin/id -u ;; *) exit 1 ;; esac`,
		// A process of the user's, not ended yet, asking the door for a sync key.
		"devopsy": `mkdir -p ` + filepath.Dir(syncKeys) + ` && printf '%s\n' '` + syncLine + `' >>` + syncKeys,
		"docker":  "exit 0",
	} {
		if err := testfile.Write(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	keys := filepath.Join(home, "door", "keys", "devopsy-shop")
	write(t, keys, "ssh-ed25519 AAAAone leaked\nssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItwo ci\n")
	write(t, syncKeys, syncLine+"\n")
	cmd := exec.Command("sh", "-c", KeysRemoveScript("devopsy-shop"))
	cmd.Env = []string{"HOME=" + home, "DEVOPSY_HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin"}
	cmd.Stdin = strings.NewReader("AAAAone\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the removal: %v\n%s", err, out)
	}
	if strings.Contains(readFile(keys), "AAAAone") {
		t.Fatal("the control: the key is still there")
	}
	if data, err := os.ReadFile(syncKeys); err == nil && len(ParseAuthorizedKeys(string(data))) > 0 {
		t.Errorf("a sync key added before the user's processes ended outlives the removal:\n%s", data)
	}
}
