package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// A server's port reaches ssh -G as -p too (SSHEffective through
// SSHArgs), so a server with a port and nothing overriding it is not
// refused: ssh -G resolves to the port named, and the session goes there.
// No test kept this: TestTargetHostFromDotenv's ssh stands in with echo
// (ssh -G prints no port) or always says port 22 (the refusal), so a -G
// without the -p would pass both. Belongs in TestTargetHostFromDotenv.
func TestServerPortReachesSSHGReview(t *testing.T) {
	t.Parallel()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(tmp, "app")
	write(t, filepath.Join(project, ".devopsy", "compose.yaml"), "services: {}\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", "config.yaml"), "project: app\nenvironments:\n  prod: {}\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", ".env"), "DEVOPSY_SERVER=devopsy@vm1:5369\n", 0o644)
	// As ssh: -G resolves the port to -p's, else 22; a session prints
	// its arguments.
	ssh := filepath.Join(tmp, "ssh")
	write(t, ssh, "#!/bin/sh\ncase \" $* \" in *\" -G \"*) port=22; prev=; for a; do [ \"$prev\" = -p ] && port=$a; prev=$a; done; echo 'hostname vm1'; echo \"port $port\"; exit 0 ;; esac\necho \"$@\"\n", 0o755)
	out, code := runDevopsy(t, project, []string{"DEVOPSY_SSH_COMMAND=" + ssh}, "@prod", "ps")
	if code != 0 || !strings.Contains(out, " -T -p 5369 devopsy@vm1 sh -c") || strings.Contains(out, "names port") {
		t.Errorf("a server with a port, nothing overriding it (%d):\n%s", code, out)
	}
}
