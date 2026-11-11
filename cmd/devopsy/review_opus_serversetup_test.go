package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// Bug: apply --restart restarts Docker whatever the states read under the apply lock say.
// The operator asks for the restart from the facts session's states; when the part it was
// to replace changed between the sessions, the apply leaves it (the answer is stale) yet
// still restarts Docker, with nothing in daemon.json for it to take and nothing pending:
// every docker command under way is cut, and every container stopped without live-restore,
// for no change. The restart should be judged again under the lock, as every put is.
func TestReviewSetupRestartJudgedUnderTheLock(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	other := `{"default-address-pools":[{"base":"10.200.0.0/16","size":24}]}`
	write(t, filepath.Join(host, setup.DaemonFile), `{"log-driver":"local","log-opts":{"max-file":"5","max-size":"20m"},"live-restore":true,"default-address-pools":[{"base":"10.200.0.0/16","size":24}]}`, 0o644)
	dockerTook(t, host, `[{"Base":"10.200.0.0/16","Size":24}]`)
	// Asked about a value the part held at the facts session, replaced since by other.
	stale := `{"default-address-pools":[{"base":"10.100.0.0/16","size":24}]}`
	out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--replace", "default-address-pools="+setup.Digest(stale), "--restart")
	if code != 0 || !strings.Contains(out, "changed since it was asked about") {
		t.Fatalf("(%d):\n%s", code, out)
	}
	d, _ := setup.ParseDaemon([]byte(readString(filepath.Join(host, setup.DaemonFile))))
	if p, _ := setup.PartNamed("default-address-pools"); d.Observed(p) != other {
		t.Fatalf("the part left is %s", d.Observed(p))
	}
	if l := readString(filepath.Join(host, "systemctl.log")); strings.Contains(l, "restart --no-block") {
		t.Errorf("Docker restarted with nothing to take:\n%s\nsystemctl:\n%s", out, l)
	}
}

