package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// --upgrade without a target upgrades this devopsy, the operator's: run outside any session
// (none of a session's variables) on a machine whose record names another deploy user, it is
// never refused as a login that is not that user, as no other command run there is.
//
// BUG (review round 3): runUpgrade checks the login (checkDeployUser) whether it runs in a
// server's session or as the operator's own --upgrade, so a user other than the deploy user on
// a machine that is also a devopsy server (a CI runner, a box an admin deploys elsewhere from)
// cannot upgrade its own devopsy, told W0009 to address the server as the deploy user.
func TestUpgradeOutsideASessionChecksNoLogin(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("root is never checked")
	}
	host := t.TempDir()
	write(t, filepath.Join(host, setup.DeployUserFile), setup.DeployUserText("someone-else"), 0o644)
	cmd := exec.Command(binary, "--upgrade")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir(), remote.HostRootEnv + "=" + host}
	out, _ := cmd.CombinedOutput()
	if strings.Contains(string(out), report.NotDeployUser.Msg()) {
		t.Errorf("the operator's own --upgrade refused as a login that is not the deploy user:\n%s", out)
	}
}
