package main

import (
	"debug/elf"
	"errors"
	"os"
	"os/user"
	"regexp"
	"strings"
	"syscall"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// doorShellPath is a project user's PATH at a login, never one a login hands it.
const doorShellPath = "/usr/local/bin:/usr/bin:/bin"

// doorShellWord is what a kept variable's value may hold: a terminal's or a locale's name.
var doorShellWord = regexp.MustCompile(`^[A-Za-z0-9._@+:-]{1,128}$`)

// doorShellSSH is what sshd's connection words hold: addresses, ports, a terminal's path.
var doorShellSSH = regexp.MustCompile(`^[0-9A-Fa-f.:% /a-z]{1,256}$`)

// runDoorShell is a project user's login shell (remote.DoorShell, a link to devopsy): sshd runs
// it with a command as `-c <command>` and, for a login, interactive (as its argv[0] starts with
// a dash, or `-l`, as --shell-host asks). Static, it is loaded with nothing a login's
// environment names (LD_PRELOAD, LD_LIBRARY_PATH): whatever filled that environment (sshd's PAM
// reading ~/.pam_environment, ~/.ssh/environment under an admin's PermitUserEnvironment, the
// client's), the home is the user's to write, and a leaked key's hook would run in a kept key's
// next login. It execs /bin/sh with an environment of its own (doorShellEnv), never as a login
// shell and with no ENV, so nothing of the home runs either (~/.profile, a file ENV names).
func runDoorShell(args []string) int {
	argv := []string{"sh", "-i"}
	if len(args) >= 2 && args[0] == "-c" {
		argv = []string{"sh", "-c", args[1]}
	}
	u, err := user.Current()
	if err != nil {
		os.Stderr.WriteString("devopsy-shell: " + err.Error() + "\n")
		return 1
	}
	err = syscall.Exec("/bin/sh", argv, doorShellEnv(u, os.Environ()))
	os.Stderr.WriteString("devopsy-shell: " + err.Error() + "\n")
	return 1
}

// doorShellEnv is a login's environment for u: its home, names and shell from the password
// database, a fixed PATH, and of what the login handed (env) only a terminal's and a locale's
// names and sshd's own connection words, each a plain word, and the client's command under a
// forced one (SSH_ORIGINAL_COMMAND, a sync key's request, which its command checks), as it is.
func doorShellEnv(u *user.User, env []string) []string {
	out := []string{"HOME=" + u.HomeDir, "USER=" + u.Username, "LOGNAME=" + u.Username, "SHELL=" + remote.DoorShell, "PATH=" + doorShellPath}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch {
		case k == "TERM" || k == "LANG" || strings.HasPrefix(k, "LC_"):
			if doorShellWord.MatchString(v) {
				out = append(out, kv)
			}
		case k == "SSH_ORIGINAL_COMMAND":
			out = append(out, kv)
		case k == "SSH_CLIENT" || k == "SSH_CONNECTION" || k == "SSH_TTY":
			if doorShellSSH.MatchString(v) {
				out = append(out, kv)
			}
		}
	}
	return out
}

// staticSelf refuses a devopsy that is not static, which a project user's login shell must be
// (runDoorShell): a dynamic one is loaded with what a login's environment names (LD_PRELOAD)
// before it runs a line. A release is static (CGO_ENABLED=0); a build of one's own may not be.
func staticSelf() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	f, err := elf.Open(self)
	if err != nil {
		return errors.New("devopsy is no Linux binary, and a project user's login shell is it")
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return errors.New("devopsy is not static (built with cgo), and a project user's login shell is it: a dynamic one loads what a login's environment names")
		}
	}
	return nil
}
