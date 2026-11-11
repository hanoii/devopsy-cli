package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/setup"
)

// The merge keeps daemon.json as its admin has it but for devopsy's parts, its mode included:
// a file closed to others (a proxy's credentials in its proxies) stays closed.
// Bug: the root script puts the merged file with mode 644 whatever it was, so a daemon.json
// only root and a group read becomes every user's to read, a project user's included.
func TestSetupRootScriptKeepsDaemonMode(t *testing.T) {
	t.Parallel()
	host, bin := setupHostRoot(t)
	file := filepath.Join(host, setup.DaemonFile)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(`{"proxies": {"http-proxy": "http://user:secret@proxy.example.com:3128"}}`), 0o640); err != nil {
		t.Fatal(err)
	}
	merged := "{\n  \"live-restore\": true,\n  \"proxies\": {\n    \"http-proxy\": \"http://user:secret@proxy.example.com:3128\"\n  }\n}\n"
	c := exec.Command("sh", "-c", SetupRootScript, "sh", host, "0:0", "", filepath.Join(host, "srv"), merged, "", "", "", "", "inv-1")
	c.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "HOST="+host, "CALLS="+filepath.Join(host, "calls"))
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o640 {
		t.Errorf("daemon.json was 640 and is %o after the merge", got)
	}
}
