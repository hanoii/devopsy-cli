package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// The project user's side of the door: what it finds as `docker` (a link to devopsy,
// runDocker) renders a compose file with its own rights and asks through sudo. The
// deploy user's side is door.go, and its rules internals/door.md.

const (
	doorFlag     = "--door"
	doorReadFlag = "--door-read"
	dockerSocket = "/var/run/docker.sock"
)

// reachesDocker says whether this user has Docker of its own: then it has no door.
func reachesDocker() bool {
	return os.Getenv("DOCKER_HOST") != "" || syscall.Access(dockerSocket, 2) == nil
}

// behindDoor says whether this user's docker is the door: the docker it
// finds is devopsy (runDocker), and it has no Docker of its own.
func behindDoor() bool {
	found, err := exec.LookPath("docker")
	if err != nil || reachesDocker() {
		return false
	}
	self, err := os.Executable()
	if err != nil {
		return false
	}
	a, errA := filepath.EvalSymlinks(found)
	b, errB := filepath.EvalSymlinks(self)
	return errA == nil && errB == nil && a == b
}

// devopsyPath is what sudo's rules name and what a project user asks sudo to run: this
// binary's own path, links resolved, which add's devopsy and a project user's are, being one
// file, whatever either's PATH finds first (devopsy upgrades in place, at the same path).
func devopsyPath() string {
	if p := selfPath(); p != "" {
		return p
	}
	return "/usr/local/bin/devopsy"
}

// sudoArgs are sudo's arguments to run devopsy with flag as another user.
func sudoArgs(as, flag string, args ...string) []string {
	return append([]string{"-n", "-u", as, devopsyPath(), flag}, args...)
}

// doorCommand is the door asked as the deploy user through sudo (--door), the one way every
// call of a project user's starts it: a hand-over, a question, a login.
func doorCommand(args ...string) (*exec.Cmd, error) {
	deploy, err := deployUser()
	if err != nil {
		return nil, fmt.Errorf("who is the deploy user: %w", err)
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		return nil, fmt.Errorf("a project user's docker goes through sudo: %w", err)
	}
	return exec.Command(sudo, sudoArgs(deploy.Username, doorFlag, args...)...), nil
}

// doorRead is a read as the project user who (--door-read), the one way any caller starts
// it: in a session of its own, with no controlling terminal, since its process is that
// user's to trace, and in the caller's session it would hold the caller's terminal (an
// admin's, under --server-info) to read and type into. It reads no terminal.
func doorRead(who string, args ...string) *exec.Cmd {
	c := exec.Command("sudo", sudoArgs(who, doorReadFlag, args...)...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return c
}

// realDocker is the docker on PATH that is not this binary.
func realDocker() string {
	self, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(self); err == nil {
		self = r
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		candidate := filepath.Join(dir, "docker")
		r, err := filepath.EvalSymlinks(candidate)
		if err != nil || r == self {
			continue
		}
		if fi, err := os.Stat(r); err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return "/usr/bin/docker"
}

// deployUser is who owns this binary: devopsy upgrades itself as its deploy user.
func deployUser() (*user.User, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if r, err := filepath.EvalSymlinks(self); err == nil {
		self = r
	}
	fi, err := os.Stat(self)
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("no owner for " + self)
	}
	return user.LookupId(strconv.Itoa(int(st.Uid)))
}

// runDocker is `docker` for whoever finds devopsy under that name.
func runDocker(args []string) int {
	real := realDocker()
	if reachesDocker() || len(args) == 0 {
		return doorExec(real, args)
	}
	switch args[0] {
	// No daemon behind these: how devopsy learns compose's words, and a project's logout.
	case "__complete", "logout", "--version", "--help":
		return doorExec(real, args)
	case "login":
		return doorLogin(args[1:])
	case "compose":
		return dockerCompose(real, args[1:])
	}
	return doorHandOver(append([]string{"request", dockerConfig()}, args...))
}

func doorExec(path string, args []string) int {
	err := syscall.Exec(path, append([]string{"docker"}, args...), os.Environ())
	report.Error("running " + path + ": " + err.Error())
	return 1
}

// doorHandOver asks the door, as the deploy user, and becomes that: sudo relays
// signals and exits with the code of what it runs.
func doorHandOver(args []string) int {
	c, err := doorCommand(args...)
	if err != nil {
		report.Error(err.Error())
		return 1
	}
	err = syscall.Exec(c.Path, append([]string{"sudo"}, c.Args[1:]...), os.Environ())
	report.Error("running sudo: " + err.Error())
	return 1
}

// filledPipe is the reading end of a pipe already holding text, small enough to sit
// in it unread, with nothing left to write: a reader gone can raise no SIGPIPE, which
// devopsy takes for its session's end (interruptible).
func filledPipe(text string) (*os.File, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	if _, err := w.WriteString(text); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return r, nil
}

// dockerConfig is where this user's registry logins are.
func dockerConfig() string {
	config := os.Getenv("DOCKER_CONFIG")
	if config == "" {
		config = filepath.Join(os.Getenv("HOME"), ".docker")
	}
	return filepath.Join(config, "config.json")
}

// composeGlobals are compose's own flags that take a value, and composeLone those that take
// none and change only what renders. Any other global flag (for example --dry-run) is
// refused: the door gets the verb and what follows it, so it would run the command without.
var (
	composeGlobals = []string{"-f", "--file", "--project-directory", "-p", "--project-name", "--env-file", "--ansi", "--progress", "--parallel"}
	composeLone    = []string{"--compatibility", "--all-resources"}
)

// dockerCompose renders what a compose command is about, with this user's rights,
// and asks the door to run the verb on that.
func dockerCompose(real string, args []string) int {
	var global []string
	profiles := []string{}
	if p := os.Getenv("COMPOSE_PROFILES"); p != "" {
		profiles = append(profiles, p)
	}
	dir, _ := os.Getwd()
	i := 0
loop:
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--profile" && i+1 < len(args):
			profiles = append(profiles, args[i+1])
			i += 2
		case slices.Contains(composeGlobals, a) && i+1 < len(args):
			if a == "--project-directory" {
				dir = args[i+1]
			}
			global = append(global, a, args[i+1])
			i += 2
		case strings.HasPrefix(a, "-"):
			name, value, valued := strings.Cut(a, "=")
			switch {
			case valued && name == "--profile":
				profiles = append(profiles, value)
			case valued && slices.Contains(composeGlobals, name):
				if name == "--project-directory" {
					dir = value
				}
				global = append(global, a)
			case !valued && slices.Contains(composeLone, a):
				global = append(global, a)
			default:
				report.Error("docker compose " + a + ": not taken for a project user, whose door runs the command without it")
				return 2
			}
			i++
		default:
			break loop
		}
	}
	rest := args[i:]
	// No daemon behind these.
	if (len(rest) > 0 && rest[0] == "version") || slices.Contains(rest, "--help") || slices.Contains(rest, "-h") {
		return doorExec(real, append([]string{"compose"}, args...))
	}
	// A whole project by its name (composeDownProject): no file to render.
	if len(rest) > 0 && rest[0] == "down" && !slices.Contains(global, "-f") {
		if i := slices.Index(global, "-p"); i >= 0 {
			return doorHandOver(append([]string{"down", global[i+1]}, rest[1:]...))
		}
	}
	render := func() (string, error) {
		sweepRenders(os.TempDir(), time.Now())
		f, err := os.CreateTemp("", "devopsy-door-*")
		if err != nil {
			return "", err
		}
		defer f.Close()
		c := exec.Command(real, append(append([]string{"compose"}, global...), "--profile", "*", "config", "--format", "json")...)
		c.Stdout, c.Stderr = f, os.Stderr
		if err := c.Run(); err != nil {
			os.Remove(f.Name())
			return "", err
		}
		return f.Name(), nil
	}
	if len(rest) > 0 && rest[0] == "config" {
		// What a release is about to bind: steps write in an entry before anything
		// starts, and only the door may make one. What Docker would make at `up`, sooner.
		if f, err := render(); err == nil {
			_, _ = doorAsk(false, "entries", f)
			os.Remove(f)
		}
		return doorExec(real, append([]string{"compose"}, args...))
	}
	f, err := render()
	if err != nil {
		report.Error("the compose file does not render: " + err.Error())
		return 2
	}
	code := doorHandOver(append([]string{"compose", dockerConfig(), f, strings.Join(profiles, ","), dir}, rest...))
	// Here only when sudo did not run: the door takes the file once it does.
	os.Remove(f)
	return code
}

// sweepRenders removes this user's renders in dir an hour old: one a sudo that ran and
// refused left (the door takes the file once it answers, and nothing of the client's runs
// after sudo). Each holds the project's variables, its owner's alone to read.
func sweepRenders(dir string, now time.Time) {
	old, _ := filepath.Glob(filepath.Join(dir, "devopsy-door-*"))
	for _, f := range old {
		fi, err := os.Lstat(f)
		if err != nil || !fi.Mode().IsRegular() || now.Sub(fi.ModTime()) < time.Hour {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == os.Getuid() {
			os.Remove(f)
		}
	}
}

// runMntReady gets an environment's shared/mnt ready, whichever user this is: a
// plain directory, as ever, for who reaches Docker, or where the door gives no answer,
// whose binds it then refuses. For a project user a link into the deploy user's space,
// where it writes inside an entry and never replaces one.
func runMntReady(args []string) int {
	if len(args) != 1 {
		report.Error("usage: devopsy " + remote.MntReadyFlag + " <environment directory>")
		return 2
	}
	mnt := filepath.Join(args[0], "shared", "mnt")
	if _, err := os.Lstat(mnt); err == nil {
		return 0
	}
	if !reachesDocker() {
		out, err := doorAsk(false, "mnt", args[0])
		if err == nil && out != "" {
			if err := os.Symlink(out, mnt); err != nil {
				report.Error(err.Error())
				return 1
			}
			return 0
		}
		// The door itself said no, past sudo: not a server with no door.
		if doorRefusedIt(err) {
			report.Error("the door would not ready " + mnt + ": " + err.Error())
			return 1
		}
	}
	if err := os.MkdirAll(mnt, 0o755); err != nil {
		report.Error(err.Error())
		return 1
	}
	return 0
}

// cloneDirReady gets a clone's directory ready, whichever user this is: a plain one
// for who reaches Docker, or where the door gives no answer, which it then keeps
// reflinks off. For a project user the door's, on the data's filesystem, where a root
// container's clone is out of reach: this user would otherwise own what holds it.
func cloneDirReady(into string) error {
	if !reachesDocker() {
		if out, err := doorAsk(false, "clone", into); err == nil {
			if out == "" {
				return nil
			}
			if err := os.MkdirAll(filepath.Dir(into), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(out, into); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			return nil
		}
	}
	return os.MkdirAll(into, 0o700)
}

// doorEntry asks the door to make an entry of mnt: a file, or a directory.
func doorEntry(mnt, e string, file bool) error {
	kind := "dir"
	if file {
		kind = "file"
	}
	_, err := doorAsk(true, "entry", mnt, e, kind)
	return err
}

// doorRefusedIt says whether an ask got as far as the door, whose refusal is 2. sudo with
// no rule for this user fails 1, which is every server with no door.
func doorRefusedIt(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == 2
}

// doorAsk asks the door something it answers on stdout. A question whose failure
// has a plain way on says nothing of it: it is every server with no door.
func doorAsk(say bool, args ...string) (string, error) {
	c, err := doorCommand(args...)
	if err != nil {
		return "", err
	}
	var said bytes.Buffer
	c.Stderr = &said
	if say {
		c.Stderr = errOut
	}
	out, err := c.Output()
	// What the door said, for the caller to say: it is a line of report's already.
	if doorRefusedIt(err) && !say {
		line := strings.TrimSpace(said.String())
		err = fmt.Errorf("%s: %w", strings.TrimPrefix(line, report.Prefix+"error: "), err)
	}
	return strings.TrimSpace(string(out)), err
}
