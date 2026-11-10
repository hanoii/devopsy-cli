package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Review 4 (Opus), a failing test proving a bug: a variable from the project user's home
// reaches its login shell, which loads it before it runs a line. Ubuntu 24.04's sshd PAM
// stack has `session required pam_env.so user_readenv=1 envfile=/etc/default/locale`
// (/etc/pam.d/sshd, libpam-modules 1.5.3), so pam_env reads ~/.pam_environment, the user's to
// write, at every login and sshd passes it to the login shell. Shown in a throwaway
// ubuntu:24.04 container with this doorShell as the user's shell: a key's login printed
// PROBE=fromhome and ld.so tried to preload /home/u/planted.so. A leaked key that writes
// `LD_PRELOAD=/home/u/x.so` there runs its code in doorShell's /bin/sh, before its `unset`,
// at every later login of a kept key, devopsy's own sessions included: the hook class
// PermitUserRC and doorShell closed (internals/door.md, "Nothing of a project user's home runs
// at a login"). Debian 13's sshd PAM has no user_readenv, so the end-to-end servers do not show it.
func TestReview4DoorShellLoadsNoEnvironmentOfTheHome(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	shell := filepath.Join(dir, "shell")
	write(t, shell, doorShell)
	if err := os.Chmod(shell, 0o755); err != nil {
		t.Fatal(err)
	}
	// What pam_env hands the session from ~/.pam_environment.
	planted := filepath.Join(dir, "planted.so")
	c := exec.Command(shell, "-c", `echo "[$LD_PRELOAD]"`)
	c.Env = []string{"HOME=" + dir, "PATH=/usr/bin:/bin", "LD_PRELOAD=" + planted}
	out, _ := c.CombinedOutput()
	if strings.Contains(string(out), planted) {
		t.Errorf("a login's LD_PRELOAD from the home reaches what the login shell runs:\n%s", out)
	}
	if runtime.GOOS == "linux" && strings.Contains(string(out), "ld.so") {
		t.Errorf("the login shell itself preloads a library from the home:\n%s", out)
	}
}

// Review 4 (Opus): sshd reads ~/.ssh/environment for a project user wherever the admin's
// sshd_config sets PermitUserEnvironment (yes, or a pattern list), and doorDropIn neither sets
// it to no nor does doorSSHDTakes check what sshd resolves, as it does for PermitUserRC: the
// same hook as ~/.pam_environment above (LD_PRELOAD), under a non-default setting.
func TestReview4DoorDropInTakesNoUserEnvironment(t *testing.T) {
	t.Parallel()
	if !strings.Contains(doorDropIn("devopsy"), "\tPermitUserEnvironment no\n") {
		t.Errorf("the drop-in leaves ~/.ssh/environment to the admin's setting:\n%s", doorDropIn("devopsy"))
	}
	if !strings.Contains(doorSSHDTakes("devopsy"), "permituserenvironment no") {
		t.Error("add does not check that sshd resolves PermitUserEnvironment no for the project user")
	}
}
