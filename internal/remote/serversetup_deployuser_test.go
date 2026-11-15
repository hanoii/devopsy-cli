package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/setup"
	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// A root run the server's devopsy refuses the deploy user's record to leaves nothing beside
// where the record goes: no file of its own making stays in /etc/devopsy.
//
// BUG (review round 3): the run writes the record's text to a temporary file beside it
// (deploy-user.XXXXXX) before it asks, and its refusal exits before the file's removal, so each
// refused --server-setup or --upgrade leaves one more file of root's in /etc/devopsy, naming
// the login it refused.
func TestSetupRootScriptRefusedLeavesNoFile(t *testing.T) {
	t.Parallel()
	host, bin := setupHostRoot(t)
	fake := filepath.Join(t.TempDir(), "devopsy")
	said := SetupOther + "\tops\tits setup record /home/ops/.config/devopsy/server.yaml"
	if err := testfile.Write(fake, []byte("#!/bin/sh\nprintf '%s' \""+said+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c := exec.Command("sh", "-c", SetupRootScript, "sh", host, "1000:1000", "", filepath.Join(host, "srv"), "", "", "", "", "", "inv-1", "", "", setup.DeployUserText("admin"), fake)
	c.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "HOST="+host, "CALLS="+filepath.Join(host, "calls"))
	out, _ := c.CombinedOutput()
	if code := c.ProcessState.ExitCode(); code != 4 {
		t.Fatalf("the run was not refused (%d):\n%s", code, out)
	}
	left, _ := filepath.Glob(filepath.Join(host, filepath.Dir(setup.DeployUserFile), "*"))
	if len(left) != 0 {
		t.Errorf("a refused run left %v", left)
	}
}
