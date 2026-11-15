//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// recordDeployUser writes the deploy user's record on the server container name as
// --server-setup's root run writes it (the e2e servers have no systemd for the setup itself),
// removed after the test.
func recordDeployUser(t *testing.T, name, user string) {
	t.Helper()
	t.Cleanup(func() { asRoot(t, name, "rm -f "+setup.DeployUserFile) })
	if out, code := asRoot(t, name, "mkdir -p /etc/devopsy && printf '%s' "+quote(setup.DeployUserText(user))+" >"+setup.DeployUserFile); code != 0 {
		t.Fatalf("the record (%d): %s", code, out)
	}
}

// A second user in the docker group, logged in by sshd with the operator's key, is refused by
// its session's first call once the server records its deploy user, W0009 offering the command
// it ran as the deploy user on the host as typed, and nothing read; the deploy user goes on.
func TestDeployUserRefusesAnotherLogin(t *testing.T) {
	t.Cleanup(func() { asRoot(t, server, "userdel -r admin2 2>/dev/null; true") })
	if out, code := asRoot(t, server, "useradd -m -s /bin/sh -G docker admin2 && install -d -m 700 -o admin2 -g admin2 /home/admin2/.ssh && "+
		"install -m 600 -o admin2 -g admin2 /home/devopsy/.ssh/authorized_keys /home/admin2/.ssh/authorized_keys"); code != 0 {
		t.Fatalf("a second docker user (%d): %s", code, out)
	}
	// With no record, it passes for the deploy user, as before.
	if out, code := op(t, "/", "devopsy @admin2@server: --server-info 2>&1"); strings.Contains(out, report.NotDeployUser.ID) || code != 0 && code != 3 {
		t.Fatalf("no record (%d): %s", code, out)
	}
	// The deploy user set up by devopsy and not recorded yet: the second user's setup refuses to
	// record itself, in its plan, naming the deploy user and the way to run it as that user.
	t.Cleanup(func() {
		srv(t, "[ ! -e ~/.config/devopsy/server.yaml.e2e-made ] || rm -f ~/.config/devopsy/server.yaml ~/.config/devopsy/server.yaml.e2e-made")
	})
	if out, code := srv(t, "mkdir -p ~/.config/devopsy && if [ ! -e ~/.config/devopsy/server.yaml ]; then echo '{}' >~/.config/devopsy/server.yaml && touch ~/.config/devopsy/server.yaml.e2e-made; fi"); code != 0 {
		t.Fatalf("the deploy user's setup record (%d): %s", code, out)
	}
	if out, code := op(t, "/", "devopsy @admin2@server: --server-setup --yes 2>&1"); code != 1 || !saysWords(out, "devopsy is set up as this server's deploy user already") && !saysWords(out, "devopsy is set up on this server too") || !strings.Contains(out, "$ devopsy @devopsy@server: --server-setup") {
		t.Errorf("the second user's setup (%d): %s", code, out)
	}
	if _, code := asRoot(t, server, "test -e "+setup.DeployUserFile); code == 0 {
		t.Errorf("the second user's setup recorded it")
	}
	recordDeployUser(t, server, "devopsy")
	out, code := op(t, "/", "devopsy @admin2@server: --gc --dry-run 2>&1")
	if code != 1 || !saysWords(out, "error: "+report.NotDeployUser.Msg()) || !saysWords(out, "logged in as admin2, but this server's deploy user is devopsy") || !strings.Contains(out, "$ devopsy @devopsy@server: --gc --dry-run") || strings.Contains(out, "nothing to remove") {
		t.Errorf("another login (%d): %s", code, out)
	}
	if out, code := op(t, "/", "devopsy @devopsy@server: --server-info 2>&1"); strings.Contains(out, report.NotDeployUser.ID) || code != 0 && code != 3 {
		t.Errorf("the deploy user (%d): %s", code, out)
	}
}
