package main

import (
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A project user's login shell is devopsy (a link named devopsy-shell): it runs a command as
// sh -c does and a login (argv[0] with a dash, or -l) as an interactive shell, with an
// environment of its own: its home, names and PATH from the password database and itself,
// never what a login handed it (LD_PRELOAD or a PATH from a home's ~/.pam_environment, ENV),
// so nothing of the home runs, and static, nothing a login names is loaded into it either.
func TestDoorShellTakesNothingOfALogin(t *testing.T) {
	t.Parallel()
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	shell := filepath.Join(dir, "devopsy-shell")
	if err := os.Symlink(binary, shell); err != nil {
		t.Fatal(err)
	}
	ran := filepath.Join(dir, "ran")
	hook := filepath.Join(dir, "env")
	write(t, hook, "echo hooked >>"+ran+"\n", 0o644)
	planted := filepath.Join(dir, "planted.so")
	login := []string{"HOME=" + dir, "PATH=" + dir + ":/usr/bin:/bin", "LD_PRELOAD=" + planted, "ENV=" + hook, "BASH_ENV=" + hook, "TERM=xterm", "LANG=C.UTF-8", "LC_ALL=$(evil)"}
	c := exec.Command(shell, "-c", "env")
	c.Env = login
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("a command (%v):\n%s", err, out)
	}
	got := string(out)
	for _, want := range []string{"HOME=" + u.HomeDir + "\n", "USER=" + u.Username + "\n", "PATH=" + doorShellPath + "\n", "TERM=xterm\n", "LANG=C.UTF-8\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("a command's environment has no %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"LD_PRELOAD", "ENV=", "LC_ALL", dir + ":"} {
		if strings.Contains(got, not) {
			t.Errorf("a command's environment keeps %q of the login's:\n%s", not, got)
		}
	}
	if runtime.GOOS == "linux" && strings.Contains(got, "ld.so") {
		t.Errorf("the login shell itself loaded what the login named:\n%s", got)
	}
	// A login, as sshd starts one, and --shell-host's -l: interactive, and the hook never runs.
	for _, argv := range [][]string{{"-devopsy-shell"}, {"devopsy-shell", "-l"}} {
		c := &exec.Cmd{Path: shell, Args: argv, Env: login, Stdin: strings.NewReader("echo interactive\nexit\n")}
		if out, err := c.CombinedOutput(); err != nil || !strings.Contains(string(out), "interactive") {
			t.Errorf("%q (%v):\n%s", argv, err, out)
		}
	}
	if _, err := os.Stat(ran); err == nil {
		t.Errorf("a login ran the hook ENV named: %s", readString(ran))
	}
}
