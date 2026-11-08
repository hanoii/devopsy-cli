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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

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

// devopsyPath is what sudo's rules name, wherever this was found as.
func devopsyPath() string {
	if p, err := exec.LookPath("devopsy"); err == nil {
		return p
	}
	return "/usr/local/bin/devopsy"
}

// sudoArgs are sudo's arguments to run devopsy with flag as another user.
func sudoArgs(as, flag string, args ...string) []string {
	return append([]string{"-n", "-u", as, devopsyPath(), flag}, args...)
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
	// No daemon behind these: how devopsy learns compose's words, and a project's login.
	case "__complete", "login", "logout", "--version", "--help":
		return doorExec(real, args)
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

// secretFlag finds a secret docker would read from this environment, `-e NAME`: it
// goes on stdin to the door, since sudo clears the environment and a command line shows.
var secretFlag = regexp.MustCompile(`^RSYNC_(SECRET|PASSWORD)$`)

// doorHandOver asks the door, as the deploy user, and becomes that: sudo relays
// signals and exits with the code of what it runs.
func doorHandOver(args []string) int {
	deploy, err := deployUser()
	if err != nil {
		report.Error("who is the deploy user: " + err.Error())
		return 1
	}
	sudo, err := exec.LookPath("sudo")
	if err != nil {
		report.Error("a project user's docker goes through sudo: " + err.Error())
		return 1
	}
	for i, a := range args {
		if a == "-e" && i+1 < len(args) && secretFlag.MatchString(args[i+1]) {
			if err := stdinIs(os.Getenv(args[i+1])); err != nil {
				report.Error(err.Error())
				return 1
			}
			break
		}
	}
	err = syscall.Exec(sudo, append([]string{"sudo"}, sudoArgs(deploy.Username, doorFlag, args...)...), os.Environ())
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

// stdinIs makes stdin hold text.
func stdinIs(text string) error {
	r, err := filledPipe(text)
	if err != nil {
		return err
	}
	// The lowest free descriptor is what Dup gives: closed first, that is stdin.
	if err := syscall.Close(0); err != nil {
		return err
	}
	fd, err := syscall.Dup(int(r.Fd()))
	if err != nil {
		return err
	}
	if fd != 0 {
		return errors.New("stdin was not free for the door's secret")
	}
	return nil
}

// dockerConfig is where this user's registry logins are.
func dockerConfig() string {
	config := os.Getenv("DOCKER_CONFIG")
	if config == "" {
		config = filepath.Join(os.Getenv("HOME"), ".docker")
	}
	return filepath.Join(config, "config.json")
}

// composeGlobals are compose's own flags that take a value.
var composeGlobals = []string{"-f", "--file", "--project-directory", "-p", "--project-name", "--env-file", "--ansi", "--progress", "--parallel"}

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
			global = append(global, a)
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
	return doorHandOver(append([]string{"compose", dockerConfig(), f, strings.Join(profiles, ","), dir}, rest...))
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
	deploy, err := deployUser()
	if err != nil {
		return "", err
	}
	c := exec.Command("sudo", sudoArgs(deploy.Username, doorFlag, args...)...)
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
