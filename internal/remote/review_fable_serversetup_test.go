package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/setup"
)

// Bug: a daemon.json that is a link (to a file a config tool or an admin keeps elsewhere) is
// replaced by a plain file at the merge: put moves its temporary file over the link, so the
// file the link named keeps the old text and is no longer what Docker reads.
func TestSetupRootScriptKeepsADaemonFileBehindALink(t *testing.T) {
	t.Parallel()
	host, bin := setupHostRoot(t)
	file := filepath.Join(host, setup.DaemonFile)
	kept := filepath.Join(host, "srv", "config", "daemon.json")
	for _, d := range []string{filepath.Dir(file), filepath.Dir(kept)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(kept, []byte(`{"debug": true}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(kept, file); err != nil {
		t.Fatal(err)
	}
	merged := "{\n  \"debug\": true,\n  \"live-restore\": true\n}\n"
	c := exec.Command("sh", "-c", SetupRootScript, "sh", host, "0:0", "", filepath.Join(host, "srv"), merged, "", "", "", "", "inv-1")
	c.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "HOST="+host, "CALLS="+filepath.Join(host, "calls"))
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if fi, err := os.Lstat(file); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("daemon.json was a link and is %v after the merge (%v)", fi.Mode(), err)
	}
	if data, _ := os.ReadFile(kept); string(data) != merged {
		t.Errorf("the file the link names holds %q, not the merge", data)
	}
}
