package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
	"github.com/hanoii/devopsy-cli/internal/uids"
	"golang.org/x/sys/unix"
)

// The deploy user's side of the door, from sudo: it checks a project user's request
// and runs it, and never reads the project's tree itself, what it needs of that coming
// from runDoorRead, run as the project user. So no link or swap there can show it
// anything of the deploy user's. The project user's side is door_client.go.

// doorEnvironment leaves only what this user is, whatever sudo was set to keep: a
// caller's PATH would choose the docker this runs, and its HOME the config it obeys. What the
// caller's session carries for its hints (the door's sudo keeps it, remote.SessionFrom
// checks it) is read first and returned, so nothing the door starts sees it.
func doorEnvironment() (home string, asked remote.Session, err error) {
	asked = remote.SessionFrom(os.Getenv)
	u, err := user.Current()
	if err != nil {
		return "", remote.Session{}, err
	}
	os.Clearenv()
	os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	os.Setenv("HOME", u.HomeDir)
	return u.HomeDir, asked, nil
}

// doorGroupHeld refuses a door not running under remote.DoorRunGroup, its effective group, by
// which the firewall covers what it starts on the host: sudo's rule gives it that group (-g),
// and a door asked without it would start registry clients nothing holds.
func doorGroupHeld() error {
	g, err := user.LookupGroup(remote.DoorRunGroup)
	if err != nil {
		return fmt.Errorf("refused: no group %s here: --project-users add makes it", remote.DoorRunGroup)
	}
	if strconv.Itoa(os.Getegid()) != g.Gid {
		return fmt.Errorf("refused: the door runs under the group %s (sudo -g %s), which the firewall covers", remote.DoorRunGroup, remote.DoorRunGroup)
	}
	return nil
}

// doorNotHere is the door's exit when it has nothing to say of this user.
const doorNotHere = 3

// doorLog, once the server's space is known, keeps what was refused and who asked:
// what a leaked key tried is worth having, and a real release asks for nothing refused.
var doorLog string

func doorRefused(err error) int {
	report.Fail(err)
	doorRecord(err.Error())
	return 2
}

func doorRecord(what string) {
	if doorLog == "" {
		return
	}
	if f, err := os.OpenFile(doorLog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		// Who is sudo's word. The rest quoted, so a caller's cannot make a line of their own.
		fmt.Fprintf(f, "%s\t%q\t%q\n", doorWho, what, os.Args[1:])
		f.Close()
	}
}

var doorWho string

// runDoorRead is what the door asks of a project's tree, run as that project's
// user: it reads only what that user could already.
func runDoorRead(args []string) int {
	if len(args) < 2 {
		return 2
	}
	if _, _, err := doorEnvironment(); err != nil {
		return 1
	}
	path := args[1]
	switch args[0] {
	case "take", "cat":
		f, err := os.Open(path)
		if err != nil {
			return 1
		}
		defer f.Close()
		if _, err := io.Copy(os.Stdout, f); err != nil {
			return 1
		}
		if args[0] == "take" && os.Remove(path) != nil {
			return 1
		}
		return 0
	case "write":
		// A FIFO or a file of this user's: never made here.
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return 1
		}
		defer f.Close()
		if _, err := io.Copy(f, os.Stdin); err != nil {
			return 1
		}
		return 0
	case "real":
		out, err := exec.Command("realpath", "-m", "--", path).Output()
		if err != nil {
			return 1
		}
		os.Stdout.Write(out)
		return 0
	case "environments":
		// --server-info's, of this user's tree: as the deploy user would list them, but by this
		// user's rights. None when a directory of it fails to read: a list in part would read as
		// the rest gone, its uid claims stale.
		envs, err := projectEnvironments(filepath.Dir(path), path)
		if err != nil || json.NewEncoder(os.Stdout).Encode(envs) != nil {
			return 1
		}
		return 0
	case "isdir":
		if fi, err := os.Stat(path); err == nil && fi.IsDir() {
			return 0
		}
		return 1
	case "tar":
		return doorReadRun(exec.Command("tar", "-C", filepath.Dir(path), "-cf", "-", "--", filepath.Base(path)), true)
	case "collect":
		if len(args) != 3 {
			return 2
		}
		if os.MkdirAll(args[2], 0o755) != nil {
			return 1
		}
		return doorReadRun(exec.Command("cp", "-RP", "--reflink=auto", "--no-preserve=ownership", "--", path+"/.", args[2]+"/"), false)
	case "copy":
		// Into a directory of the door's, open to this user for now: a reflink where the
		// filesystem has them, so an import is not on disk twice.
		if len(args) != 3 {
			return 2
		}
		return doorReadRun(exec.Command("cp", "--reflink=auto", "--no-preserve=mode,ownership", "--", path, args[2]), false)
	case "remove":
		if os.RemoveAll(path) != nil {
			return 1
		}
		return 0
	}
	return 2
}

func doorReadRun(c *exec.Cmd, output bool) int {
	if output {
		c.Stdout = os.Stdout
	}
	c.Stderr = os.Stderr
	if c.Run() != nil {
		return 1
	}
	return 0
}

// doorSide is the deploy user's side of one request.
type doorSide struct {
	policy door.Policy
	// claims are the project's uid claims (projectClaims), read once per request, again under
	// a start's locks: what its containers may run as (uidsOf) and its data's owners may be
	// (Policy.ProjectUIDs). uidRoot is the release root they are under.
	claims  []uids.Claim
	uidRoot string
	// imagesRoot is the deploy user's release root too, where the server's image lock a
	// claim holds is (claimLock).
	imagesRoot string
	real       string
	env        []string
	vets       []remote.DoorVet
	// made are this request's work directories, so that a refusal, or a claim cut by a
	// signal (claimCut), leaves none behind; madeMu guards it for the latter's goroutine.
	made   []string
	madeMu sync.Mutex
	// keysFile is the project user's keys file (remote.DoorKeysFile), and keysAt that file as
	// the request found it: the request acts on the user's behalf only while it is still that
	// file, with a key, and no removal is pending (stillAsked).
	keysFile string
	keysAt   os.FileInfo
	// What a request reads once, for every claim and run it makes: the daemon's
	// "<os> <architecture>" (platform), doorHostReady's answer (hostReady), the directory of
	// its logins (logins) and its builder's environment (builder).
	daemonPlatform memo[string]
	hostChecked    memo[struct{}]
	loginsDir      memo[string]
	builderEnv     memo[[]string]
	// onFetch, when set, is said when a claim goes past its skip to fetch (the pull verb's
	// progress).
	onFetch func()
	// log is what a claim's wait for the server's images says through (claimLock),
	// report.Default when nil.
	log *report.Logger
	// asked is what the caller's session carries (doorEnvironment), for the commands its
	// remedies offer alone: never in a refusal's log, never a decision.
	asked remote.Session
}

// memo is a value a door process reads once, its answer (an error included) kept for the
// request's later uses: one request is one process, and what it reads does not change under it
// in the time it runs.
type memo[T any] struct {
	done bool
	v    T
	err  error
}

// get is the value, read by read the first time.
func (m *memo[T]) get(read func() (T, error)) (T, error) {
	if !m.done {
		m.v, m.err = read()
		m.done = true
	}
	return m.v, m.err
}

// read runs a read of the project's tree, as its user.
func (s *doorSide) read(op string, args ...string) *exec.Cmd {
	return doorRead(s.policy.User, append([]string{op}, args...)...)
}

// docker is a command of the real client's, in the door's environment.
func (s *doorSide) docker(args ...string) *exec.Cmd {
	c := exec.Command(s.real, args...)
	c.Env = s.env
	return c
}

// carriesLabel says whether any service has a label under prefix, in any case:
// labels are read that way by what reads them.
func carriesLabel(file map[string]any, prefix string) bool {
	services, _ := file["services"].(map[string]any)
	for _, sv := range services {
		svc, _ := sv.(map[string]any)
		labels, _ := svc["labels"].(map[string]any)
		for k := range labels {
			if strings.HasPrefix(strings.ToLower(k), strings.ToLower(prefix)) {
				return true
			}
		}
	}
	return false
}

// Real implements door.Paths: the project user's answer of where path ends, which is that
// user's word (its read is its own process, to trace), so taken only absolute and clean: a
// `..` read lexically (filepath.Dir, a prefix) would mean another place to the kernel through
// a link the user made, and every caller (an entry's parent, a bind, a build's context) would
// believe it otherwise.
func (s *doorSide) Real(path string) (string, error) {
	out, err := doorAnswer(s.read("real", path), maxPath, doorReadWait)
	if err != nil {
		return "", fmt.Errorf("cannot resolve %s", path)
	}
	real := strings.TrimSuffix(string(out), "\n")
	if !filepath.IsAbs(real) || filepath.Clean(real) != real {
		return "", fmt.Errorf("cannot resolve %s: its answer is no clean path", path)
	}
	return real, nil
}

// noLink refuses a copy that came out a link: a path of the deploy user's, which
// Docker would follow at the bind. A swap after Real makes one.
func noLink(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s came out a link", filepath.Base(path))
	}
	return nil
}

// Seal implements door.Paths: a copy by content, its bytes by the project user's
// rights, and as an ordinary user's tar leaves them, with no setuid bits. Its directory
// is closed: only the daemon, which is root, has to reach it.
func (s *doorSide) Seal(path string) (string, error) {
	work, copied, sum, err := s.sealRead(path)
	if work != "" {
		defer os.RemoveAll(work)
	}
	if err != nil {
		return "", err
	}
	final := filepath.Join(s.policy.Sealed(), sum)
	if _, err := os.Lstat(final); err == nil {
		// Used again, so not the oldest for pruneSealed.
		now := time.Now()
		_ = os.Chtimes(final, now, now)
		return filepath.Join(final, "x"), nil
	}
	dir := filepath.Join(work, "dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(copied, filepath.Join(dir, "x")); err != nil {
		return "", err
	}
	if err := os.Rename(dir, final); err != nil {
		if _, serr := os.Lstat(final); serr != nil {
			return "", err
		}
	}
	return filepath.Join(final, "x"), nil
}

// sealRead reads path as the project user, in a work directory of its own among the sealed
// copies, and hashes what it read as a sealed copy is named (contentHash): the work directory,
// which the caller removes whatever the error, the copy in it, and its hash. A seal and the
// look at what a restart keeps (warnSealed) read through it, so the two hash alike.
func (s *doorSide) sealRead(path string) (work, copied, sum string, err error) {
	sealed := s.policy.Sealed()
	if err := os.MkdirAll(filepath.Dir(sealed), 0o755); err != nil {
		return "", "", "", err
	}
	if err := os.MkdirAll(sealed, 0o750); err != nil {
		return "", "", "", err
	}
	work, err = os.MkdirTemp(sealed, ".seal-")
	if err != nil {
		return "", "", "", err
	}
	// The copy lands in a directory of its own, so no name the caller's tar holds meets the
	// door's beside it (Seal's dir).
	in := filepath.Join(work, "in")
	if err := os.Mkdir(in, 0o700); err != nil {
		return work, "", "", err
	}
	if err := s.untar(path, in); err != nil {
		return work, "", "", err
	}
	copied = filepath.Join(in, filepath.Base(path))
	if err := noLink(copied); err != nil {
		return work, "", "", err
	}
	sum, err = contentHash(copied)
	if err != nil {
		return work, "", "", err
	}
	return work, copied, sum, nil
}

// warnSealed says, ahead of a restart or a start (door.KeepsSealed), which services' containers
// hold another sealed copy of a read-only bind than the release would seal now: each keeps the
// copy it was made with, and only a container made again (up) takes the release's. The release's
// side is read and hashed as a seal would (sealRead), its copy then removed; a container's is the
// hash its mount's source is named by (<sealed>/<hash>/x). Only containers the door made for
// this user are read (door.Policy.ServiceContainers), and anything that fails loses only the
// warning, never the verb. dir is the real path of compose's project directory, which the paths
// said are relative to.
func (s *doorSide) warnSealed(file map[string]any, argv []string, dir string) {
	binds := s.policy.ReleaseBinds(file, argv)
	if len(binds) == 0 {
		return
	}
	name, _ := file["name"].(string)
	// The door's own filters, of two keys, which narrow each other; whose each one is, its inspect
	// says.
	out, err := s.docker("ps", "-aq", "--no-trunc", "--filter", "label=com.docker.compose.project="+name,
		"--filter", "label="+door.DoorLabel+"="+s.policy.User).Output()
	if err != nil {
		return
	}
	containers := s.policy.ServiceContainers(s, strings.Fields(string(out)), name)
	mounts := map[string]map[string]string{}
	sums := map[string]string{}
	var differ []string
	for _, b := range binds {
		for _, id := range containers[b.Service] {
			if _, ok := mounts[id]; !ok {
				mounts[id], _ = s.containerMounts(id)
			}
			held, ok := s.sealedHash(mounts[id][b.Target])
			if !ok {
				continue
			}
			sum, ok := sums[b.Source]
			if !ok {
				sum = s.releaseHash(b.Source)
				sums[b.Source] = sum
			}
			if sum == "" {
				break
			}
			if held != sum {
				differ = append(differ, b.Service+": "+shownIn(dir, b.Source))
				break
			}
		}
	}
	if len(differ) == 0 {
		return
	}
	report.Say(report.WarnLevel, "what these containers bind read-only differs from the release's: "+argv[0]+" keeps the copy each was made with",
		differ, offer(report.Fix, sealedFix+":", s.asked.Releasing("--release"), sealedFix))
}

// sealedFix is the way out warnSealed offers: a release of the environment by the caller's
// session's address, else in words, never one of compose's commands, which, shown locally in a
// session, would run on the local project.
const sealedFix = "release again, with any change made in the checkout, which makes these containers again"

// releaseHash is the hash a seal of path would name its copy by, read as a seal reads it
// (sealRead) and keeping no copy, or nothing when it cannot be read.
func (s *doorSide) releaseHash(path string) string {
	work, _, sum, err := s.sealRead(path)
	if work != "" {
		_ = os.RemoveAll(work)
	}
	if err != nil {
		return ""
	}
	return sum
}

// containerMounts are the sources of the mounts of the container id (a full one) by their
// targets.
func (s *doorSide) containerMounts(id string) (map[string]string, error) {
	out, err := s.docker("inspect", "--type", "container", "--format", "{{json .Mounts}}", id).Output()
	if err != nil {
		return nil, err
	}
	var mounts []struct{ Source, Destination string }
	if err := json.Unmarshal(out, &mounts); err != nil {
		return nil, err
	}
	by := map[string]string{}
	for _, m := range mounts {
		by[m.Destination] = m.Source
	}
	return by, nil
}

// sealedHash is the hash naming the sealed copy of this project's that source is in, when it is
// in one (<sealed>/<hash>/x).
func (s *doorSide) sealedHash(source string) (string, bool) {
	rest, ok := strings.CutPrefix(source, s.policy.Sealed()+"/")
	sum, _, _ := strings.Cut(rest, "/")
	return sum, ok && sum != ""
}

// shownIn is path as a user reads it from dir: relative when it is inside (./config), else
// as it is.
func shownIn(dir, path string) string {
	if rel, ok := strings.CutPrefix(path, strings.TrimRight(dir, "/")+"/"); ok {
		return "./" + rel
	}
	return path
}

func (s *doorSide) untar(path, into string) error {
	src := s.read("tar", path)
	src.Stderr = nil
	dst := exec.Command("tar", "-xf", "-", "-C", into, "--no-same-owner")
	errs := pipeline(src, dst)
	if errs[0] != nil {
		return fmt.Errorf("%s is not readable", path)
	}
	return errs[1]
}

// pipeline runs commands each into the next, and says how each ended. The door keeps no
// end of a pipe open, so one that dies ends its neighbours, whatever stage they are in.
func pipeline(cmds ...*exec.Cmd) []error {
	errs := make([]error, len(cmds))
	var ours []*os.File
	for i := 0; i+1 < len(cmds); i++ {
		r, w, err := os.Pipe()
		if err != nil {
			errs[i] = err
			return errs
		}
		cmds[i].Stdout, cmds[i+1].Stdin = w, r
		ours = append(ours, r, w)
	}
	started := make([]bool, len(cmds))
	for i, c := range cmds {
		errs[i] = c.Start()
		started[i] = errs[i] == nil
	}
	for _, f := range ours {
		f.Close()
	}
	for i, c := range cmds {
		if started[i] {
			errs[i] = c.Wait()
		}
	}
	return errs
}

// contentHash hashes what a sealed copy holds: names, kinds, modes (a directory's too, its
// special bits included), links and bytes, each file's by its length first, so no two trees
// read as one stream. A copy is named by it and taken again by that name (Seal): what it
// leaves out, a release changing it seals to the copy that does not have it.
func contentHash(root string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fi, err := d.Info()
		if err != nil {
			return err
		}
		mode := fi.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
		fmt.Fprintf(h, "%q %o %o\x00", rel, d.Type(), mode)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%q\x00", target)
		case d.Type().IsRegular():
			fmt.Fprintf(h, "%d\x00", fi.Size())
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.CopyN(h, f, fi.Size()); err != nil {
				return err
			}
		}
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:24], err
}

// pruneSealed removes sealed copies no container of this project holds, an hour
// old at least: a rollback seals its files again.
func (s *doorSide) pruneSealed() {
	sealed := s.policy.Sealed()
	entries, err := os.ReadDir(sealed)
	if err != nil {
		return
	}
	out, err := s.docker("ps", "-a", "--no-trunc", "--format", `{{.ID}} {{.Label "com.docker.compose.project"}}`).Output()
	if err != nil {
		return
	}
	held := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		id, project, _ := strings.Cut(line, " ")
		if id == "" || !s.policy.OwnName(project) {
			continue
		}
		mounts, err := s.containerMounts(id)
		if err != nil {
			return
		}
		for _, src := range mounts {
			if sum, ok := s.sealedHash(src); ok {
				held[sum] = true
			}
		}
	}
	for _, e := range entries {
		fi, err := e.Info()
		// A killed door's half-made copy too, by the same age.
		if err == nil && !held[e.Name()] && time.Since(fi.ModTime()) > time.Hour {
			_ = os.RemoveAll(filepath.Join(sealed, e.Name()))
		}
	}
}

// Within implements door.Paths: rel under a sealed copy, the deploy user's own, walked part by
// part with no link followed (a copy keeps the links it holds), nothing there being no
// refusal.
func (s *doorSide) Within(root, rel string) error {
	parts := strings.Split(filepath.Clean(rel), "/")
	cur := root
	for _, part := range parts {
		if part == ".." || part == "" {
			return errors.New("a path out of it")
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return errors.New("a link on its way")
		}
	}
	return nil
}

// IsDir implements door.Host.
func (s *doorSide) IsDir(path string) bool {
	return s.read("isdir", path).Run() == nil
}

// CopyIn implements door.Host. A file is copied by the project user, by its rights, in a
// directory it may write until then; a directory by a tar of its, extracted by the deploy
// user, in one it may never write. After, it can change no part of a path to the copy.
func (s *doorSide) CopyIn(path string, dir bool) (string, error) {
	perms := "rwX"
	if dir {
		perms = "rX"
	}
	work, err := s.workDir(perms)
	if err != nil {
		return "", err
	}
	copied := filepath.Join(work, filepath.Base(path))
	if dir {
		err = s.untar(path, work)
	} else {
		err = s.read("copy", path, copied).Run()
	}
	if err != nil {
		return "", err
	}
	if err := exec.Command("setfacl", "-b", work).Run(); err != nil {
		return "", err
	}
	if err := noLink(copied); err != nil {
		return "", err
	}
	return copied, nil
}

// WorkDir implements door.Host.
func (s *doorSide) WorkDir() (string, error) {
	return s.workDir("rX")
}

// workDir makes a directory in the deploy user's nosuid space, for this user alone with
// perms (none with "": the deploy user's own, a claim's copy), swept a day on (sweepWork) when
// a killed door leaves it.
func (s *doorSide) workDir(perms string) (string, error) {
	root := filepath.Join(s.policy.Space, "work")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	d, err := os.MkdirTemp(root, s.policy.Project+"-")
	if err != nil {
		return "", err
	}
	s.madeMu.Lock()
	s.made = append(s.made, d)
	s.madeMu.Unlock()
	if perms == "" {
		return d, nil
	}
	if err := s.acl(d, perms, false); err != nil {
		return "", err
	}
	return d, nil
}

// removeWork removes a work directory, as root where a container left root's files.
func (s *doorSide) removeWork(dir string) { _ = removeSpaceDir(s.docker, dir) }

// removeSpaceDir removes dir, a directory of the door's space (a work directory, a project's
// sealed copies or clones), as the deploy user, else as root in the util image, docker the
// deploy user's own, where a container left other uids' files: its parent is the deploy
// user's, and rm -rf follows no link inside.
func removeSpaceDir(docker func(args ...string) *exec.Cmd, dir string) error {
	if os.RemoveAll(dir) == nil {
		return nil
	}
	parent := filepath.Dir(dir)
	if out, err := docker(slices.Concat([]string{"run", "--rm", "--network", "none"}, door.RootUtil(), []string{"-v", parent + ":/w", remote.UtilImage, "rm", "-rf", "/w/" + filepath.Base(dir)})...).CombinedOutput(); err != nil {
		return fmt.Errorf("removing %s: %w: %s", dir, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ownWork says whether a work directory is this project's, by its name (workDir): not one
// of a project whose name extends this one's, whose prefix it also has.
func (s *doorSide) ownWork(name string) bool {
	return strings.HasPrefix(name, s.policy.Project+"-") && s.policy.OwnName(name)
}

// sweepWork removes what a door killed mid-way left of this project's, a day old: nothing
// runs that long from one, and a signal through sudo ends it before it tidies up.
func (s *doorSide) sweepWork() {
	root := filepath.Join(s.policy.Space, "work")
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		fi, err := e.Info()
		if err == nil && s.ownWork(e.Name()) && time.Since(fi.ModTime()) > 24*time.Hour {
			s.removeWork(filepath.Join(root, e.Name()))
		}
	}
}

// Inspect implements door.Daemon.
func (s *doorSide) Inspect(target string) (string, map[string]string, error) {
	out, err := s.docker("inspect", "--type", "container", "--format", "{{.Id}} {{json .Config.Labels}}", target).Output()
	if err != nil {
		return "", nil, err
	}
	id, raw, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	labels := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &labels); err != nil {
		return "", nil, err
	}
	return id, labels, nil
}

// Containers implements door.Daemon.
func (s *doorSide) Containers() ([]string, error) {
	out, err := s.docker("ps", "-aq", "--no-trunc").Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}

// Image is an image's id and names (door.Daemon), as the deploy user's docker has them.
func (s *doorSide) Image(ref string) (string, []string, error) {
	out, err := s.docker("image", "inspect", "--format", "{{.Id}}{{range .RepoTags}} {{.}}{{end}}", ref).Output()
	f := strings.Fields(string(out))
	if err != nil || len(f) == 0 {
		return "", nil, fmt.Errorf("no image %s", ref)
	}
	return f[0], f[1:], nil
}

// errNoRead is a registry that gives a project's logins nothing for a ref.
var errNoRead = errors.New("its registry logins do not read it")

// platformManifest is the digest of the one image manifest ref comes to on this server, read
// with the logins in the DOCKER_CONFIG dir logins: ref's own when it is a manifest, else the
// entry of its index for the daemon's platform, itself a manifest, its bytes checked against
// its digest. A claim copies and pins that digest, never an index's: imagetools' copy and
// Docker's load could each choose from an index by rules of their own, and one index could
// send each to another image.
func (s *doorSide) platformManifest(g *claimCut, ref, logins string) (string, error) {
	raw, err := s.registryRead(g, logins)
	if err != nil {
		return "", err
	}
	type platform struct{ OS, Architecture, Variant string }
	type manifest struct {
		Manifests []struct {
			Digest   string
			Platform *platform
		}
		Config *struct{ Digest string }
	}
	data, err := raw(ref)
	if err != nil {
		return "", err
	}
	// A ref by digest, an index's as a manifest's, is those bytes or nothing: an index not
	// checked would name any entry its registry chose.
	if _, digest, ok := strings.Cut(ref, "@"); ok && digestOf(data) != digest {
		return "", fmt.Errorf("its registry served other bytes than %s", digest)
	}
	var m manifest
	if json.Unmarshal(data, &m) != nil {
		return "", errNoRead
	}
	if len(m.Manifests) == 0 {
		if m.Config == nil {
			return "", errors.New("its registry serves no image manifest for it")
		}
		return digestOf(data), nil
	}
	goos, arch, variant, err := s.platform()
	if err != nil {
		return "", err
	}
	entry := ""
	for _, e := range m.Manifests {
		if p := e.Platform; p != nil && p.OS == goos && p.Architecture == arch && (p.Variant == "" || p.Variant == variant) {
			entry = e.Digest
			break
		}
	}
	if entry == "" {
		return "", registryFailure{fmt.Errorf("no image for %s/%s in its index", goos, arch)}
	}
	data, err = raw(images.ByDigest(ref, entry))
	if err != nil && err != errNoRead {
		return "", err
	}
	m = manifest{}
	if err != nil || digestOf(data) != entry || json.Unmarshal(data, &m) != nil || len(m.Manifests) > 0 || m.Config == nil {
		return "", fmt.Errorf("its index's entry for %s/%s, %s, is no image manifest", goos, arch, entry)
	}
	return entry, nil
}

// registryRead gives what reads a ref's manifest, with the logins in the DOCKER_CONFIG dir
// logins: buildx's client on the host, its builder's settings (a mirror, as the admin gave
// them to the builders; a CA from the host's store), under the door's group, which the
// firewall keeps off the host's loopback and the metadata services (remote.DoorRunGroup), as a
// registry the project user names answers as it likes (a redirect, a token's realm). A
// registry's refusal is errNoRead, what it said on stderr, as a refused pull's. Each read is
// the claim's child (g), bounded in size and time: a registry that answers and then says
// nothing would hold the door.
func (s *doorSide) registryRead(g *claimCut, logins string) (func(ref string) ([]byte, error), error) {
	b, err := s.builder()
	if err != nil {
		return nil, errors.New(strings.TrimPrefix(err.Error(), "refused: "))
	}
	return func(ref string) ([]byte, error) {
		var said bytes.Buffer
		c := s.docker("buildx", "imagetools", "inspect", "--raw", ref)
		c.Env = append(append(slices.Clone(s.env), b...), "DOCKER_CONFIG="+logins)
		out := capBuffer{max: maxManifest}
		c.Stdout, c.Stderr = &out, &said
		c.WaitDelay = time.Second
		if err := g.runWithin(c, doorReadWait); err != nil {
			os.Stderr.Write(said.Bytes())
			return nil, errNoRead
		}
		return out.Bytes(), nil
	}, nil
}

// digestOf is data's sha256 digest, as a registry names it.
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// platform is the daemon's os, architecture and the variant an index's entry for it may name
// (arm64's v8, arm's v7), read once.
func (s *doorSide) platform() (goos, arch, variant string, err error) {
	read, err := s.daemonPlatform.get(func() (string, error) {
		out, err := s.docker("version", "--format", "{{.Server.Os}} {{.Server.Arch}}").Output()
		if err != nil {
			return "", fmt.Errorf("the daemon's platform: %v", err)
		}
		return strings.TrimSpace(string(out)), nil
	})
	if err != nil {
		return "", "", "", err
	}
	goos, arch, _ = strings.Cut(read, " ")
	variant = map[string]string{"arm64": "v8", "arm": "v7"}[arch]
	return goos, arch, variant, nil
}

// inspectImage is ref's image as Docker has it now, read once.
func (s *doorSide) inspectImage(ref string) (images.Image, error) {
	out, err := s.docker("image", "inspect", "--format", images.InspectFormat, ref).Output()
	if err != nil {
		return images.Image{}, fmt.Errorf("no image %s", ref)
	}
	return images.ParseImage(out)
}

// claimedImages reads a project user's record of claimed images (door.Policy.ImagesFile).
func claimedImages(file string) []string {
	data, _ := os.ReadFile(file)
	return strings.Fields(string(data))
}

// doorKnown is what devopsy's own util containers run.
func doorKnown() door.Known {
	return door.Known{
		Image:        remote.UtilImage,
		Dockerfile:   strings.Join(remote.UtilDockerfile, "\n") + "\n",
		Owners:       ownEntry,
		VolumeOwner:  ownVolumeRoot,
		VolumeRoots:  volumeRoots,
		Export:       exportTarScript,
		Import:       emptyAndExtract,
		ReflinkProbe: reflinkProbeScript,
		SnapshotTar:  snapshotTarScript,
		Space:        spaceProbeScript,
		Share:        shareScript,
		Daemon:       rsyncDaemonScript,
		Receive:      receiveScript,
		Restore:      restoreStreamScript,
		RsyncCopy:    remote.RsyncCopy,
		Clone:        door.CloneScriptForm,
	}
}

// runDoor is the deploy user's side, from sudo: who asks comes from sudo and what
// they may from this server's config, and every word of theirs is hostile.
func runDoor(args []string) int {
	// A caller's umask comes through sudo, and the door's modes are its own.
	syscall.Umask(0o022)
	holdStops()
	if err := doorLimits(); err != nil {
		report.Error(err.Error())
		return 2
	}
	doorWho = os.Getenv("SUDO_USER")
	home, asked, err := doorEnvironment()
	if err != nil {
		return doorRefused(err)
	}
	// The caller's directory is its own, and none of the deploy user's business.
	_ = os.Chdir(home)
	// A rendered file holds the project's environment, so it goes first, whatever comes
	// of the request: nothing of it stays behind a refusal.
	var rendered map[string]any
	var renderErr error
	switch {
	case len(args) >= 3 && args[0] == "compose":
		rendered, renderErr = takeFile(doorWho, args[2])
	case len(args) == 2 && args[0] == "entries":
		rendered, renderErr = takeFile(doorWho, args[1])
	}
	settings, err := remote.DoorSettingsOf(doorWho)
	if err != nil {
		// Not the door saying no: there is none for this user, which a question whose failure
		// has a plain way on takes as such (doorRefusedIt).
		report.Error(fmt.Sprintf("refused: %s is not a project user here: %v", doorWho, err))
		return doorNotHere
	}
	policy, err := doorPolicy(doorWho, settings)
	if err != nil {
		return doorRefused(err)
	}
	if err := doorGroupHeld(); err != nil {
		return doorRefused(err)
	}
	// From here every refusal is recorded, the space's own included.
	doorLog = filepath.Join(policy.Space, "refused.log")
	if err := refuseUnguarded(report.Default(), policy.Space); err != nil {
		doorRecord(err.Error())
		return 2
	}
	// A project user with no key left logs in no more, and what it left running gets nothing
	// from the door either (KeysRemoveScript ends it).
	keysAt, _ := os.Stat(remote.DoorKeysFile(doorWho))
	releases, err := remote.ReleaseSettings()
	if err != nil {
		return doorRefused(err)
	}
	claims, err := projectClaims(claimsStore(releases.Root), policy.Project)
	if err != nil {
		return doorRefused(fmt.Errorf("refused: the project's uid claims: %w", err))
	}
	policy.ProjectUIDs = claimedUIDs(claims)
	side := &doorSide{policy: policy, claims: claims, uidRoot: releases.Root, imagesRoot: releases.Root, real: realDocker(), env: os.Environ(), vets: settings.Vets, keysAt: keysAt, keysFile: remote.DoorKeysFile(doorWho), asked: asked}
	// With no key left, or an end pending: what a removed key left running gets nothing.
	if err := side.stillAsked(); err != nil {
		return doorRefused(fmt.Errorf("refused: %w", err))
	}
	return side.answer(args, rendered, renderErr)
}

// answer is the door's answer to a request, args its words (none of them trusted), rendered
// the file a compose request handed over: each request by its shape, anything else refused.
func (s *doorSide) answer(args []string, rendered map[string]any, renderErr error) int {
	if len(args) == 0 {
		return doorRefused(errors.New("refused: no request"))
	}
	switch args[0] {
	case "entries":
		if renderErr != nil {
			return doorRefused(renderErr)
		}
		return s.entries(rendered)
	case "mnt":
		if len(args) == 2 {
			return s.mnt(args[1])
		}
	case "entry":
		if len(args) == 4 {
			return s.entry(args[1], args[2], args[3] == "file")
		}
	case "clone":
		if len(args) == 2 {
			return s.clone(args[1])
		}
	case "mark":
		if len(args) == 3 {
			return s.mark(args[1], args[2])
		}
	case "compose":
		if len(args) < 5 {
			return doorRefused(errors.New("refused: no compose command"))
		}
		if renderErr != nil {
			return doorRefused(renderErr)
		}
		return s.compose(args[1], rendered, args[3], args[4], args[5:])
	case "down":
		if len(args) >= 2 {
			return s.downProject(args[1], args[2:])
		}
	case "request":
		if len(args) >= 3 {
			return s.request(args[1], args[2:])
		}
	case "sync-key":
		return s.syncKey(args[1:])
	case "firewall":
		if len(args) == 1 {
			return s.firewall()
		}
	case "uids":
		if len(args) == 2 {
			return s.uids(args[1])
		}
	case "free":
		if len(args) == 2 {
			return s.free(args[1])
		}
	}
	return doorRefused(errors.New("refused: not a request: " + strings.Join(args[:min(1, len(args))], " ")))
}

// firewall answers whether the server's firewall is loaded as devopsy loads it, for a project
// user's release (warnDoorFirewall): the deploy user's own read as root (firewallAnswer, the
// one a deploy user's release makes), a fixed script, no argument of the caller's and none of
// its environment, through the real client. It answers right, wrong or unknown, nothing more:
// what is wrong is the admin's to read (--server-info).
func (s *doorSide) firewall() int {
	allow, door := firewallAllowed()
	fmt.Println(firewallAnswer(setupHost{}, s.real, allow, door))
	return 0
}

// syncKey is --sync-key for the caller: its sync keys (remote.DoorSyncKeysFile), never its
// keys, each line the door's own (remote.SyncKeyLine: the id's and the key's form checked,
// its options and expiry the door's), for an environment in the caller's tree. A sync's gate
// there is no boundary, the tree being the caller's: what a sync key may do runs as the
// caller, so it does nothing the caller cannot already.
func (s *doorSide) syncKey(args []string) int {
	if len(args) == 6 && args[0] == "add" && !s.policy.InTree(args[2]) {
		return doorRefused(fmt.Errorf("refused: a sync key for %s, outside %s's tree", args[2], s.policy.User))
	}
	// A user's end drops its sync keys and makes its keys file anew under this lock
	// (endProjectUser): a request begun before it, past its sudo, which no end reaches, finds
	// that file changed and adds none.
	if err := editSyncKeys(remote.DoorSyncKeysFile(s.policy.User), selfPath(), args, time.Now(), s.stillAsked); err != nil {
		return doorRefused(fmt.Errorf("refused: %w", err))
	}
	return 0
}

// stillAsked says whether the request's user is still as the request found it: its keys file
// the same (keysAt), a key left in it, no end of its processes pending (remote.DoorEndingFile).
// A user's end renews its keys file and kills its processes, never the door's, which are the
// deploy user's: a request begun before, past its sudo (one parked on a registry's copy for
// days), finds the file changed, and acts no further. Every request asks before it acts on the
// user's behalf: before Docker or compose runs, before what the door keeps for the user is
// written (whileAUser), before a sync key goes in. A key added meanwhile renews the file too,
// so such a request is refused and asked again.
func (s *doorSide) stillAsked() error {
	if s.keysFile == "" {
		return nil
	}
	now, err := os.Stat(s.keysFile)
	if err != nil || !remote.KeysLeft(s.keysFile) {
		return errors.New(s.policy.User + " has no key left")
	}
	if s.keysAt == nil || !os.SameFile(now, s.keysAt) || !now.ModTime().Equal(s.keysAt.ModTime()) || now.Size() != s.keysAt.Size() {
		return errors.New(s.policy.User + "'s keys changed during this request: ask again")
	}
	if _, err := os.Lstat(remote.DoorDestroyingFile(s.policy.User)); err == nil {
		return errors.New(s.policy.User + " is being removed with all it holds")
	}
	for _, mark := range []string{remote.DoorEndingFile(s.policy.User), remote.DoorRemovingFile(s.policy.User)} {
		if _, err := os.Lstat(mark); err == nil {
			return errors.New("a removal of " + s.policy.User + "'s did not complete: the next listing or edit of its keys completes it")
		}
	}
	return nil
}

// refuseUnguarded refuses, through l, a request on a door whose space is not guarded
// (spaceGuarded), giving what the refusals' log records, nil when it is: the project user
// asking can only pass it on, so it is told the code for the server's admin and no way out
// (report.SpaceUnguarded), which the admin finds with its fix in --server-info.
func refuseUnguarded(l *report.Logger, space string) error {
	err := spaceGuarded(space)
	if err != nil {
		l.SayForAdmin(report.ErrorLevel, report.SpaceUnguarded)
	}
	return err
}

// spaceGuarded refuses a door whose space is not on a mount with nosuid and nodev: there a
// container's setuid file crosses projects and a crafted tar reaches root, so the door does
// nothing rather than half its promise.
func spaceGuarded(space string) error {
	nosuid, nodev, err := mountGuards(space)
	if err != nil {
		return fmt.Errorf("the project users' directory %s: %v", space, err)
	}
	if !nosuid || !nodev {
		return fmt.Errorf("the project users' directory %s is not on a mount with nosuid and nodev", space)
	}
	return nil
}

// doorPolicy reads what the host says of a project user.
func doorPolicy(who string, s remote.DoorUser) (door.Policy, error) {
	u, err := user.Lookup(who)
	if err != nil {
		return door.Policy{}, err
	}
	if s.Space == "" {
		return door.Policy{}, errors.New("refused: the server's door has no space")
	}
	tree := s.Tree
	if tree == "" {
		tree = filepath.Join(u.HomeDir, s.Project)
	}
	deploy, err := user.Current()
	if err != nil {
		return door.Policy{}, err
	}
	ids := hostIDs(deploy, s.Users)
	// The projects this one's names stop at: a request is refused rather than judged without
	// them, which would take a deploy project's names for this one's.
	r, err := remote.ReleaseSettings()
	if err != nil {
		return door.Policy{}, err
	}
	others, err := otherProjects(s, r.Root)
	if err != nil {
		return door.Policy{}, fmt.Errorf("refused: %w", err)
	}
	p := door.Policy{
		Project: s.Project, User: who, UID: u.Uid, Space: s.Space, Tree: tree, Joinable: s.Joinable, Roles: s.Roles,
		ForbiddenIDs: ids, Others: others,
	}
	p.Images = claimedImages(p.ImagesFile())
	return p, nil
}

// otherProjects are the server's projects but this one: what the door's users hold, and
// what the deploy user's release root does (serverProjects, an error when it does not
// read). Names are flat, so a longer one is out of this project's reach
// (door.Policy.OwnName).
func otherProjects(s remote.DoorUser, root string) ([]string, error) {
	ps, err := serverProjects(s.Others, root)
	if err != nil {
		return nil, err
	}
	var others []string
	for _, p := range ps {
		if p != s.Project {
			others = append(others, p)
		}
	}
	return others, nil
}

// hostIDs are ids no container may run as: the deploy user's (deploy, from its passwd entry,
// never this process's ids: the door's group is sudo's -g), the door's users', devopsy's two
// groups, and the Docker socket's group. Other system accounts stay usable: images take low
// ids of their own, and nobody logs in as those.
func hostIDs(deploy *user.User, users []string) []int {
	var ids []int
	add := func(vs ...string) {
		for _, v := range vs {
			if id, err := strconv.Atoi(v); err == nil {
				ids = append(ids, id)
			}
		}
	}
	add(deploy.Uid, deploy.Gid)
	for _, name := range users {
		if u, err := user.Lookup(name); err == nil {
			add(u.Uid, u.Gid)
		}
	}
	for _, name := range []string{remote.DoorGroup, remote.DoorRunGroup} {
		if g, err := user.LookupGroup(name); err == nil {
			add(g.Gid)
		}
	}
	if fi, err := os.Stat(dockerSocket); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			ids = append(ids, int(st.Gid))
		}
	}
	sort.Ints(ids)
	return slices.Compact(ids)
}

// takeFile reads a rendered file by the caller's rights, and removes it.
func takeFile(who, path string) (map[string]any, error) {
	out, err := doorAnswer(doorRead(who, "take", path), maxRendered, doorReadWait)
	if err != nil {
		return nil, errors.New("refused: not a file this project's user can read")
	}
	d := json.NewDecoder(bytes.NewReader(out))
	// A large number stays as written: compose reads what this writes back.
	d.UseNumber()
	file := map[string]any{}
	if err := d.Decode(&file); err != nil {
		return nil, errors.New("refused: not a rendered compose file")
	}
	return file, nil
}

// capBuffer holds at most max bytes of what a read of a caller's file writes, and fails the
// write past them, which fails the read: a file a project user names, however large, takes no
// more of the host's memory than the door would ever use of it. Its buffer is a field, never
// embedded: io.Copy would take the buffer's ReadFrom, past this Write.
type capBuffer struct {
	buf bytes.Buffer
	max int
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.max {
		return 0, errors.New("past what the door reads of a file")
	}
	return b.buf.Write(p)
}

func (b *capBuffer) Bytes() []byte { return b.buf.Bytes() }

// doorReadWait is how long a door request waits for a read as its project user (a path's
// resolution, a rendered file, its docker config): one held open (a FIFO the user names)
// would hold a process of the deploy user's for good, and many, the deploy user's process
// limit, which sshd's look at every project user's keys runs under.
const doorReadWait = time.Minute

// maxRendered, maxLogins, maxPath and maxEnvironments are what the deploy user reads at most
// of a project user's answer: a rendered compose file, a project's docker config, a path's
// resolution, and its environments for --server-info; maxManifest of a registry's manifest or
// index, which the door holds whole (a registry's own limit is 4 MiB).
const (
	maxRendered     = 8 << 20
	maxLogins       = 1 << 20
	maxPath         = 64 << 10
	maxEnvironments = 4 << 20
	maxManifest     = 4 << 20
)

// closedDir makes a directory only the deploy user and this project user can reach.
func (s *doorSide) closedDir(path, perms string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o750); err != nil {
		return err
	}
	return s.acl(path, perms, false)
}

// acl gives this project user perms on path, and on what is made under it if inherit.
func (s *doorSide) acl(path, perms string, inherit bool) error {
	spec := "u:" + s.policy.User + ":" + perms
	args := []string{"-m", spec, path}
	if inherit {
		args = []string{"-m", spec, "-d", "-m", spec, path}
	}
	return exec.Command("setfacl", args...).Run()
}

// mnt makes an environment's mnt in the deploy user's space and says where. The
// compose name comes from where the environment is in this user's tree, never from
// the caller: <tree>/<environment> or <tree>/<instance>/<environment>. Closed to
// other users: an entry's files keep whatever modes their containers give them.
func (s *doorSide) mnt(base string) int {
	real, err := s.Real(base)
	if err != nil {
		return doorRefused(err)
	}
	name, err := s.policy.NameOf(real)
	if err != nil {
		return doorRefused(err)
	}
	dir := s.policy.Mnt(name)
	if err := s.closedDir(dir, "rX"); err != nil {
		return doorRefused(err)
	}
	fmt.Println(dir)
	return 0
}

// entry makes one entry of a mnt of this project's.
func (s *doorSide) entry(mnt, e string, file bool) int {
	real, err := s.Real(mnt)
	if err != nil {
		return doorRefused(err)
	}
	if !s.policy.OwnName(filepath.Base(real)) || real != s.policy.Mnt(filepath.Base(real)) || !door.Word.MatchString(e) {
		return doorRefused(fmt.Errorf("refused: not an entry of this project's: %s/%s", mnt, e))
	}
	if err := s.makeEntry(filepath.Join(real, e), file); err != nil {
		return doorRefused(err)
	}
	return 0
}

// clone makes a clone's directory in the door's space: the project user reads it,
// and cannot change any part of a path to it. For a path in its tree the answer is
// where to link it; a path already inside is made, and the answer is empty.
func (s *doorSide) clone(path string) int {
	real, err := s.Real(path)
	if err != nil {
		return doorRefused(err)
	}
	root := s.policy.Clones()
	if err := s.closedDir(root, "rX"); err != nil {
		return doorRefused(err)
	}
	if err := s.acl(root, "rX", true); err != nil {
		return doorRefused(err)
	}
	if rest, ok := strings.CutPrefix(real, root+"/"); ok {
		if !door.CloneDir(rest) {
			return doorRefused(errors.New("refused: not a clone's name: " + path))
		}
		if err := os.MkdirAll(real, 0o750); err != nil {
			return doorRefused(err)
		}
		return 0
	}
	if !s.policy.InTree(real) {
		return doorRefused(errors.New("refused: not this project's: " + path))
	}
	sum := sha256.Sum256([]byte(real))
	dir := filepath.Join(root, hex.EncodeToString(sum[:])[:24])
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return doorRefused(err)
	}
	fmt.Println(dir)
	return 0
}

// mark says a clone of this project's is complete.
func (s *doorSide) mark(dir, id string) int {
	real, err := s.Real(dir)
	if err != nil {
		return doorRefused(err)
	}
	if rest, ok := strings.CutPrefix(real, s.policy.Clones()+"/"); !ok || !door.CloneDir(rest) || !door.Word.MatchString(id) {
		return doorRefused(errors.New("refused: not a clone of this project's: " + dir))
	}
	// Under a directory a root container filled, whose owner may be anyone's: a file of
	// the deploy user's, for a project user to read through the door's default ACL.
	marker := filepath.Join(real, remote.SnapshotComplete)
	if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return doorRefused(err)
	}
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return doorRefused(err)
	}
	defer f.Close()
	if _, err := f.WriteString(id + "\n"); err != nil {
		return doorRefused(err)
	}
	return 0
}

// makeEntry makes an entry the project user writes inside, and cannot replace.
func (s *doorSide) makeEntry(path string, file bool) error {
	if file {
		// `entries` made it a directory, as Docker would, not knowing: an empty one goes.
		if fi, err := os.Lstat(path); err == nil && fi.IsDir() {
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("mnt/%s is a directory, and not empty", filepath.Base(path))
			}
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o664)
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		if err != nil {
			return err
		}
		f.Close()
		return s.acl(path, "rw", false)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	return s.acl(path, "rwX", true)
}

// entries makes what a file not yet checked binds of its compose name's mnt, ahead of
// `up`: a step writes in an entry first. A path goes by where the project user says it
// ends, which only ever makes a directory inside that project's own mnt.
func (s *doorSide) entries(file map[string]any) int {
	name, _ := file["name"].(string)
	if !s.policy.OwnName(name) {
		return 0
	}
	if err := s.bindEntries(file, name, s.Real); err != nil {
		return doorRefused(err)
	}
	return 0
}

// bindEntries makes what a file binds of this compose name's mnt and is not there yet:
// a directory, as Docker would make one, but this user's to write in.
func (s *doorSide) bindEntries(file map[string]any, name string, real func(string) (string, error)) error {
	mnt := s.policy.Mnt(name)
	services, _ := file["services"].(map[string]any)
	for _, sv := range services {
		svc, _ := sv.(map[string]any)
		volumes, _ := svc["volumes"].([]any)
		for _, v := range volumes {
			m, _ := v.(map[string]any)
			src, _ := m["source"].(string)
			if t, _ := m["type"].(string); t != "bind" || src == "" {
				continue
			}
			end, err := real(src)
			if err != nil || filepath.Dir(end) != mnt {
				continue
			}
			if _, err := os.Lstat(end); err == nil {
				continue
			}
			if err := s.closedDir(mnt, "rX"); err != nil {
				return err
			}
			if err := s.makeEntry(end, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// compose checks a rendered file and a verb, and runs compose on the checked file:
// an unlinked file, so nothing of a project's environment stays on disk, with no .env,
// in the release it says it is from, since devopsy reads compose's working_dir.
func (s *doorSide) compose(config string, file map[string]any, profiles, dir string, argv []string) int {
	// A claim's copy is a work directory too, which a door killed mid-start leaves.
	s.sweepWork()
	real, err := s.Real(dir)
	if err != nil || (real != s.policy.Tree && !s.policy.InTree(real)) {
		return doorRefused(errors.New("refused: not this project's directory: " + dir))
	}
	// What its containers may run as: the claims of the environment the request's directory
	// is of, never one the file's name picks, read again under its start locks when it starts
	// containers, which compose then holds.
	name, _ := file["name"].(string)
	owners := dirOwners(s.policy.Project, s.policy.Tree, real, name, s.claims)
	var locks []*uids.Lock
	if door.StartsContainers(argv[0]) {
		if owners, locks, err = s.startLocks(real, name); err != nil {
			return doorRefused(fmt.Errorf("refused: the start locks of %s: %w", real, err))
		}
	}
	s.policy.UIDs = uidsOfOwners(s.claims, owners)
	if err := s.policy.CheckVerb(argv); err != nil {
		return doorRefused(err)
	}
	if err := s.foreign(); err != nil {
		return doorRefused(err)
	}
	refused := door.CheckFile(file, s.policy, argv[0], s)
	if len(refused) > 0 {
		var words []string
		for _, r := range refused {
			words = append(words, strings.TrimPrefix(r.Error(), "refused: "))
		}
		return doorRefused(errors.New("refused: " + strings.Join(words, ", ")))
	}
	if argv[0] == "pull" {
		return s.pull(config, file, profiles, dir, argv)
	}
	s.policy.Label(file)
	starts := door.StartsContainers(argv[0])
	// The image check pins what it checked into the file (startImage), so the file compose
	// gets, and the vets judge, is marshalled after it.
	if starts {
		if err := s.checkImageLabels(file, config); err != nil {
			return doorRefused(err)
		}
		// What starts is the image claimed: one gone in the moment since fails the start, never
		// pulled by Docker's daemon, which contacts no registry a project names.
		services, _ := file["services"].(map[string]any)
		for _, svc := range services {
			if m, ok := svc.(map[string]any); ok {
				m["pull_policy"] = "never"
			}
		}
	}
	checked, err := json.Marshal(file)
	if err != nil {
		return doorRefused(err)
	}
	if starts {
		for _, v := range s.vets {
			if !carriesLabel(file, v.Prefix) {
				continue
			}
			var said bytes.Buffer
			c := exec.Command(v.Command)
			c.Stdin, c.Stderr = bytes.NewReader(checked), io.MultiWriter(os.Stderr, &said)
			c.Env = append(slices.Clone(s.env), "DEVOPSY_DOOR_PROJECT="+s.policy.Project, "DEVOPSY_DOOR_JOINABLE="+strings.Join(s.policy.Joinable, ","))
			if err := c.Run(); err != nil {
				doorRecord("refused by the vet: " + strings.TrimSpace(said.String()))
				return 2
			}
		}
	}
	if starts {
		// The checked file's sources are real paths already.
		same := func(p string) (string, error) { return p, nil }
		if err := s.bindEntries(file, name, same); err != nil {
			return doorRefused(err)
		}
	}
	if argv[0] == "up" {
		s.pruneSealed()
	}
	// Ahead of the file's descriptor, which every process the compare starts would inherit.
	if door.KeepsSealed(argv[0]) {
		s.warnSealed(file, argv, real)
	}
	fd, err := unlinkedFile(checked)
	if err != nil {
		return doorRefused(err)
	}
	env, err := s.composeEnv(config, argv[0] == "build")
	if err != nil {
		return doorRefused(err)
	}
	if argv[0] == "build" {
		b, err := s.builder()
		if err != nil {
			return doorRefused(err)
		}
		env = append(slices.Clone(env), b...)
		// Read under the build's own environment: Docker finds compose in
		// $DOCKER_CONFIG/cli-plugins first, so another DOCKER_CONFIG may be another compose.
		if door.HasContexts(file) {
			if err := s.composeKnowsServiceContexts(env); err != nil {
				return doorRefused(err)
			}
		}
	}
	cmd := slices.Concat([]string{"docker", "compose", "-p", name, "--project-directory", dir, "--env-file", "/dev/null", "-f", fmt.Sprintf("/dev/fd/%d", fd)}, profileArgs(profiles))
	if err := s.stillAsked(); err != nil {
		return doorRefused(fmt.Errorf("refused: %w", err))
	}
	if err := keepAcrossExec(locks); err != nil {
		return doorRefused(err)
	}
	err = execDecided(s.real, append(cmd, argv...), env)
	// The locks' descriptors stay open up to the exec (main's KeepAlive says why).
	runtime.KeepAlive(locks)
	return doorRefused(err)
}

// doorStops are the terminal's stop signals, which the door takes until it execs.
var doorStops = []os.Signal{syscall.SIGTSTP, syscall.SIGTTIN, syscall.SIGTTOU}

// holdStops keeps its caller's terminal from stopping the door between a check and its use,
// which would stretch every window between them: the signals are caught, never ignored, so
// what the door execs has them at their default again (an exec resets a caught signal, and
// keeps an ignored one ignored), compose's Ctrl-Z being compose's (devopsy replaces itself).
func holdStops() {
	signal.Notify(make(chan os.Signal, 1), doorStops...)
}

// execDecided execs what the door decided.
func execDecided(path string, argv, env []string) error {
	return syscall.Exec(path, argv, env)
}

// doorLimits lifts the file size limit the door's caller hands it through sudo, or refuses
// to run under one it cannot lift: under it the door's append to its refusals' log would fail
// without a word (SIGXFSZ ignored or not), and its caller keep what it was refused out of the
// admin's sight.
func doorLimits() error {
	var l unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_FSIZE, &l); err != nil {
		return err
	}
	if l.Cur != l.Max {
		l.Cur = l.Max
		if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &l); err != nil {
			return err
		}
	}
	if l.Max != unix.RLIM_INFINITY {
		return errors.New("refused: the door runs with no file size limit, and its caller's cannot be lifted")
	}
	return nil
}

// composeKnowsServiceContexts refuses a build with contexts (all service:<name>, CheckFile)
// on a compose below its floor (dockerFloors), which first reads service:<name> as another
// service's build, or one whose version cannot be read: the compose the build's environment
// env runs, one before reading it as a path in the project's directory, the project user's to
// make, which its client then reads with the deploy user's rights.
func (s *doorSide) composeKnowsServiceContexts(env []string) error {
	if err := floorError(dockerFloor(s.real, env, floorCompose)); err != nil {
		return fmt.Errorf("refused: a build with contexts: %w", err)
	}
	return nil
}

// downProject takes this project's compose name down by that name alone, no file to
// check: compose finds what it holds by its own label (composeDownProject).
func (s *doorSide) downProject(name string, flags []string) int {
	argv, env, err := s.checkedDown(name, flags)
	if err != nil {
		return doorRefused(err)
	}
	err = execDecided(s.real, argv, env)
	return doorRefused(err)
}

// checkedDown is a down of this project's compose name by that name alone, checked in
// checkedProjectDown's order, its flags a down's, the caller still asking; its command line
// (docker first) and environment, for a run from /, which holds no compose file of anyone's:
// compose looks for one in its directory and every parent, and a down by name runs with none.
// One check for the request and for a --destroy's free.
func (s *doorSide) checkedDown(name string, flags []string) (argv, env []string, err error) {
	down := append([]string{"down"}, flags...)
	err = checkedProjectDown(s.policy, s.docker, name, func() error { return s.policy.CheckVerb(down) }, func() error {
		if env, err = s.composeEnv("", false); err != nil {
			return err
		}
		if err := os.Chdir("/"); err != nil {
			return err
		}
		if err := s.stillAsked(); err != nil {
			return fmt.Errorf("refused: %w", err)
		}
		argv = append([]string{"docker", "compose", "-p", name}, down...)
		return nil
	})
	if err != nil {
		return nil, nil, refusal(err)
	}
	return argv, env, nil
}

// checkedProjectDown is a down of a project user's compose name by that name alone, the door's
// (checkedDown) and a destroy's (userDestroy.down), its checks in their one order: the name the
// project's (Policy.OwnName), what its caller checks or clears next (the door's flags, the
// destroy's removeLeft of devopsy's own containers of the name, which a cut sync or import
// leaves writing into its mnt), no container under the project's names the door did not make
// (foreignUnder, read through docker), then down.
func checkedProjectDown(p door.Policy, docker func(args ...string) *exec.Cmd, name string, next, down func() error) error {
	if !p.OwnName(name) {
		return errors.New("refused: not this project's name: " + name)
	}
	if err := next(); err != nil {
		return err
	}
	if err := foreignUnder(p, docker); err != nil {
		return err
	}
	return down()
}

// checkImageLabels refuses an image that labels its containers with what devopsy or a
// proxy reads: a build's or an image's LABEL lands in a container's labels, past any check
// of a file's. It also decides the image compose runs, written into the file (startImage):
// one compose must not resolve again after this check.
func (s *doorSide) checkImageLabels(file map[string]any, config string) error {
	services, _ := file["services"].(map[string]any)
	for _, svc := range slices.Sorted(maps.Keys(services)) {
		sv, _ := services[svc].(map[string]any)
		// The render named every build (images.DoorBuild) and dropped its build: section.
		image, _ := sv["image"].(string)
		if image == "" {
			continue
		}
		run, labels, err := s.startImage(svc, image, config)
		if err != nil {
			return err
		}
		if run != image {
			sv["image"] = run
		}
		for k := range labels {
			low := strings.ToLower(k)
			// Compose stamps its own on what it builds, and overrides them on a container.
			reserved := strings.HasPrefix(low, "devopsy.")
			for _, v := range s.vets {
				reserved = reserved || strings.HasPrefix(low, strings.ToLower(v.Prefix))
			}
			if reserved {
				return fmt.Errorf("refused: %s: its image %s labels its containers %s", svc, image, k)
			}
		}
	}
	return nil
}

// startImage is the image svc's start runs, as compose is to name it, and that image's
// labels. Every name moves between this check and compose's create: a name only this
// project writes (a build's, a hold of its own, OwnRef) or devopsy's util image by the
// project's own build or tag in another environment, so it goes by the id read here; any
// other is anyone's on the server (another project's pull of a tag its registry moved, the
// deploy user's build of a registry's name), so it goes pinned, <image>@<digest>
// (images.Pin): an image of this project's (claimed by its id, or named by a hold of its
// own) by its digest in the image's repository, as the devopsy.digest label chooses it,
// read in one inspect; else claimed first, by the digest its claim loaded, a reference by an
// index's digest included. A reference by digest the project has, or an id, is a pin already. An image not here yet is claimed too, with this
// project's logins, and one still missing refuses the start: compose would pull or build it
// after this check.
func (s *doorSide) startImage(svc, image, config string) (string, map[string]string, error) {
	missing := fmt.Errorf("refused: %s: its image %s is not here: build or pull it first", svc, image)
	if image == doorKnown().Image || s.policy.OwnRef(image) {
		img, err := s.inspectImage(image)
		if err != nil || img.ID == "" {
			return "", nil, missing
		}
		return img.ID, img.Labels, nil
	}
	img, err := s.inspectImage(image)
	owned := err == nil && s.policy.OwnsImageOf(img.ID, img.Tags)
	pinned := images.Pinned(image)
	digest := ""
	if owned && !pinned {
		digest = images.DigestIn(img, image)
	}
	if !owned || !pinned && digest == "" {
		if digest, img, err = s.claim(image, config); err != nil {
			return "", nil, fmt.Errorf("%s: %w", svc, err)
		}
		// A pin by an index's digest goes by the entry the claim checked and loaded for the
		// daemon's platform: no image here goes by the index's.
		image, _, _ = strings.Cut(image, "@")
	}
	return images.Pin(image, digest), img.Labels, nil
}

// regrant gives this project user back rwX in entries a root container wrote, where
// the copy took it: an import's tar and a sync's rsync set each item's mode, which on a
// file with an ACL sets its mask, and the mask narrows the project user's entry. As root,
// since an entry's files are its container user's once its owners are given, and only an
// owner or root changes an ACL. Only what lost its group class's write is touched.
func (s *doorSide) regrant(entries []string) error {
	if len(entries) == 0 {
		return nil
	}
	if s.policy.UID == "" {
		return errors.New("no project user to give " + strings.Join(entries, ", ") + " back to")
	}
	if err := ensureUtilImageWith(context.Background(), s.real); err != nil {
		return err
	}
	args := slices.Concat([]string{"run", "--rm", "--network", "none"}, door.RootUtil())
	var targets []string
	for i, e := range entries {
		t := fmt.Sprintf("/e/%d", i)
		args, targets = append(args, "-v", e+":"+t), append(targets, t)
	}
	args = append(append(append(args, remote.UtilImage, "find"), targets...),
		"-xdev", "(", "-type", "d", "-o", "-type", "f", ")", "!", "-perm", "-g=w", "-exec", "setfacl", "-m", "u:"+s.policy.UID+":rwX", "{}", "+")
	c := s.docker(args...)
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("what %s may write in its entries could not be given back: %v", s.policy.User, err)
	}
	return nil
}

// writtenEntries are the entries of this project's that containers write, by their
// mounts: a detached copy's, given back once it is removed.
func (s *doorSide) writtenEntries(containers []string) []string {
	var entries []string
	for _, c := range containers {
		out, err := s.docker("inspect", "--type", "container", "-f", `{{range .Mounts}}{{if .RW}}{{println .Source}}{{end}}{{end}}`, c).Output()
		if err != nil {
			continue
		}
		for _, src := range strings.Fields(string(out)) {
			if _, entry, ok := s.policy.MntPath(src); ok && entry != "" {
				entries = append(entries, src)
			}
		}
	}
	return entries
}

// builder is what a build's environment needs to run on this project's own BuildKit,
// made at its first build: on one builder, cache mounts are shared across projects by
// their id, so one project's build would fill a cache another's trusts. Its settings are
// the door's (BUILDX_CONFIG), apart from the directory of logins a build gets. Read once
// per request.
func (s *doorSide) builder() ([]string, error) {
	return s.builderEnv.get(s.ensureBuilder)
}

// ensureBuilder makes the project's builder when it is not there as it should be (builder).
func (s *doorSide) ensureBuilder() ([]string, error) {
	name := door.BuilderOf(s.policy.Project)
	// Its network is its own: on Docker's default bridge, which every container shares, a
	// build step is a member of it, and reaches another project's builder and what its build
	// listens on.
	network := door.BuilderNetworkOf(s.policy.Project)
	// No git of the deploy user's in a context a project user wrote: buildx's client asks git
	// about the context it builds, and a sealed copy is the deploy user's own, so git trusts
	// the repository config the project user put in it, which can name a program (a promisor
	// remote's uploadpack) git then runs as the deploy user, Docker's socket in hand.
	b := []string{"BUILDX_CONFIG=" + filepath.Join(s.policy.Space, "buildx"), "BUILDX_BUILDER=" + name, "BUILDX_GIT_INFO=false", "BUILDX_GIT_LABELS=false"}
	env := append(slices.Clone(s.env), b[0])
	run := func(args ...string) ([]byte, error) {
		c := s.docker(args...)
		c.Env, c.Stderr = env, os.Stderr
		return c.Output()
	}
	// A look whose failure is an answer (no builder yet, no network yet) says nothing: its
	// words read as a failure of the build.
	probe := func(args ...string) ([]byte, error) {
		c := s.docker(args...)
		c.Env = env
		return c.Output()
	}
	out, err := probe("buildx", "inspect", name)
	if err == nil && strings.Contains(string(out), `network="`+network+`"`) {
		return b, nil
	}
	if err == nil {
		// Made otherwise, on the default bridge: made again.
		if _, err := run("buildx", "rm", name); err != nil {
			return nil, fmt.Errorf("refused: %s's builder is on Docker's default bridge, and could not be removed: %v", s.policy.Project, err)
		}
	}
	if _, err := probe("network", "inspect", network); err != nil {
		if _, err := run("network", "create", network); err != nil {
			return nil, fmt.Errorf("refused: %s's builder's network could not be made: %v", s.policy.Project, err)
		}
	}
	if _, err := run("buildx", "create", "--name", name, "--driver", "docker-container", "--driver-opt", "network="+network); err != nil {
		// Another environment's build may have made it meanwhile.
		if out, ierr := probe("buildx", "inspect", name); ierr == nil && strings.Contains(string(out), `network="`+network+`"`) {
			return b, nil
		}
		return nil, fmt.Errorf("refused: %s's builder could not be made: %v", s.policy.Project, err)
	}
	return b, nil
}

var composeProfile = regexp.MustCompile(`^(\*|[a-zA-Z0-9][a-zA-Z0-9_.-]*)$`)

// unlinkedFile is a descriptor that survives exec on a file with no name.
func unlinkedFile(data []byte) (int, error) {
	f, err := os.CreateTemp("", "devopsy-door-*")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if err := os.Remove(f.Name()); err != nil {
		return 0, err
	}
	if _, err := f.Write(data); err != nil {
		return 0, err
	}
	fd, err := syscall.Dup(int(f.Fd()))
	if err != nil {
		return 0, err
	}
	return fd, nil
}

// logins leaves a project's registry logins alone in a directory of the door's, read once
// per request. Never its own directory: Docker's CLI loads plugins and helpers from there,
// which would run the project's own program as the deploy user.
func (s *doorSide) logins(config string) (string, error) {
	return s.loginsDir.get(func() (string, error) { return s.writeLogins(config) })
}

// writeLogins writes the logins' directory (logins).
func (s *doorSide) writeLogins(config string) (string, error) {
	out, err := doorAnswer(s.read("cat", config), maxLogins, doorReadWait)
	auths := map[string]map[string]string{}
	if err == nil {
		var parsed struct {
			Auths map[string]struct {
				Auth string `json:"auth"`
			} `json:"auths"`
		}
		if json.Unmarshal(out, &parsed) == nil {
			for k, v := range parsed.Auths {
				auths[k] = map[string]string{"auth": v.Auth}
			}
		}
	}
	dir := door.LoginsDirOf(s.policy.Space, s.policy.Project)
	return dir, s.whileAUser(func() error { return writeAuths(dir, auths) })
}

// writeAuths writes a project's logins' directory, dir, holding auths alone.
func writeAuths(dir string, auths map[string]map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(map[string]any{"auths": auths})
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".config.json."+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "config.json"))
}

// profileArgs are compose's --profile flags for profiles, a comma-separated list: a profile
// that is not one of compose's names is left out.
func profileArgs(profiles string) []string {
	var args []string
	for _, p := range strings.Split(profiles, ",") {
		if composeProfile.MatchString(p) {
			args = append(args, "--profile", p)
		}
	}
	return args
}

// composeEnv is the environment every compose the door runs gets: compose reads its logins,
// helpers and plugins from DOCKER_CONFIG, the deploy user's own ~/.docker when unset, so a
// build gets the project's logins (from its docker config, config) and anything else an empty
// one.
func (s *doorSide) composeEnv(config string, build bool) ([]string, error) {
	dir, err := s.emptyConfig()
	if build {
		dir, err = s.logins(config)
	}
	if err != nil {
		return nil, err
	}
	return append(slices.Clone(s.env), "DOCKER_CONFIG="+dir), nil
}

// emptyConfig is a Docker config directory with nothing in it, the deploy user's and closed to
// writes (<space>/docker-empty), for a compose that needs no logins.
func (s *doorSide) emptyConfig() (string, error) {
	dir := filepath.Join(s.policy.Space, "docker-empty")
	if err := os.MkdirAll(s.policy.Space, 0o755); err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0o555); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	return dir, os.Chmod(dir, 0o555)
}

// request answers a Docker command other than compose.
func (s *doorSide) request(config string, argv []string) int {
	s.sweepWork()
	a, err := door.Decide(argv, s.policy, s, s, doorKnown())
	// Whatever refuses leaves none of the work directories it made (copies, logins).
	refused := func(err error) int {
		for _, d := range s.made {
			s.removeWork(d)
		}
		return doorRefused(err)
	}
	if err != nil {
		return refused(err)
	}
	for _, e := range a.Entries {
		if err := s.closedDir(filepath.Dir(e.Path), "rX"); err != nil {
			return refused(err)
		}
		if err := s.makeEntry(e.Path, e.File); err != nil {
			return refused(err)
		}
	}
	// A removal that fails fails the request: what asked for it (a release's removal, a
	// clone's) resumes it, and goes on only from one that went.
	var kept []string
	for _, path := range a.Remove {
		if s.read("remove", path).Run() != nil {
			kept = append(kept, path)
		}
	}
	if len(kept) > 0 {
		report.Error("could not remove " + strings.Join(kept, ", "))
		return 1
	}
	for _, ref := range a.Claims {
		digest, _, err := s.claim(ref, config)
		if err != nil {
			return refused(err)
		}
		// What Docker then runs names the image the claim got, by its digest: the name is
		// anyone's to move right after the pull (a tag's source would take the mover's). A
		// format's value is no image.
		for i := 1; i < len(a.Docker); i++ {
			if a.Docker[i] == ref && a.Docker[i-1] != "--format" && a.Docker[i-1] != "-f" {
				a.Docker[i] = images.ByDigest(ref, digest)
			}
		}
	}
	if a.Stream != nil || len(a.Docker) > 0 {
		if err := s.stillAsked(); err != nil {
			return refused(fmt.Errorf("refused: %w", err))
		}
	}
	if a.Stream != nil {
		return s.stream(a.Stream)
	}
	if a.DropClaimHolds {
		s.dropClaimHolds()
	}
	if len(a.Docker) == 0 {
		os.Stdout.WriteString(a.Output)
		return a.Exit
	}
	env := s.env
	stdin := io.Reader(os.Stdin)
	if a.Stdin != "" {
		stdin = strings.NewReader(a.Stdin)
	}
	regrant := slices.Concat(a.Regrant, s.writtenEntries(a.Removes))
	if a.Stdin == "" && len(a.Collect) == 0 && len(a.Cleanup) == 0 && len(regrant) == 0 && a.Unclaim == "" {
		err := execDecided(s.real, append([]string{"docker"}, a.Docker...), env)
		return refused(err)
	}
	c := exec.Command(s.real, a.Docker...)
	c.Env = env
	c.Stdin, c.Stdout, c.Stderr = stdin, os.Stdout, os.Stderr
	code := 0
	if err := c.Run(); err != nil {
		code = 1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		}
	}
	// A copy cut halfway narrowed what it wrote too.
	if err := s.regrant(regrant); err != nil {
		report.Warn(err.Error())
		if code == 0 {
			code = 1
		}
	}
	for _, co := range a.Collect {
		if code == 0 && s.read("collect", co.Dir, co.Dest).Run() != nil {
			code = 1
		}
		s.removeWork(co.Dir)
	}
	for _, d := range a.Cleanup {
		s.removeWork(d)
	}
	if code == 0 && a.Unclaim != "" {
		s.unclaim(a.Unclaim)
	}
	return code
}

// unclaim removes this project's claim holds (images.ClaimHold) from the image ref names, a
// hold of its own (a release's bookmark) that keeps it from now: a claim's hold only bridges
// the claim to that bookmark. A hold left by a failure here goes with the project's next
// pruning (dropClaimHolds).
func (s *doorSide) unclaim(ref string) {
	img, err := s.inspectImage(ref)
	if err != nil {
		return
	}
	for _, t := range img.Tags {
		if p, ok := images.ClaimHoldOf(t); ok && p == s.policy.Project {
			_ = s.docker("image", "rm", t).Run()
		}
	}
}

// dropClaimHolds removes this project's claim holds from every image, the project's pruning
// (its image prune): one a cut claim or a failed unclaim left, or a claim's no bookmark
// followed (an inspect's, a start's). Its releases' bookmarks hold what they run. Nothing here
// fails the pruning, which never fails a release.
func (s *doorSide) dropClaimHolds() {
	out, err := s.docker("image", "ls", "--format", "{{.Repository}}:{{.Tag}}").Output()
	if err != nil {
		return
	}
	for _, t := range strings.Fields(string(out)) {
		if p, ok := images.ClaimHoldOf(t); ok && p == s.policy.Project {
			_ = s.docker("image", "rm", t).Run()
		}
	}
}

// stream runs a restore's pipe: the file by the project user's rights, through a
// container with no mounts, into its FIFO by the same rights.
func (s *doorSide) stream(st *door.Stream) int {
	c := s.docker(st.Docker...)
	c.Stderr = os.Stderr
	for _, err := range pipeline(s.read("cat", st.From), c, s.read("write", st.To)) {
		if err != nil {
			return 1
		}
	}
	return 0
}

// containerRows is what says whose each container is, one JSON array per line: a label's
// value is anyone's to write, spaces included.
var containerRows = []string{"ps", "-a", "--no-trunc", "--format",
	`[{{json .ID}},{{json (.Label "com.docker.compose.project")}},{{json (.Label "devopsy.env")}},{{json (.Label "devopsy.door")}},{{json .Names}}]`}

// containerLabels are the server's containers by id, with the labels that say whose they are,
// and each one's name, for what devopsy says of it.
func containerLabels(c *exec.Cmd) (labels map[string]map[string]string, names map[string]string, err error) {
	out, err := c.Output()
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read what runs: %v", err)
	}
	labels, names = map[string]map[string]string{}, map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var row []string
		if err := json.Unmarshal([]byte(line), &row); err != nil || len(row) != 5 {
			return nil, nil, fmt.Errorf("cannot read what runs: %q", line)
		}
		labels[row[0]] = map[string]string{"com.docker.compose.project": row[1], "devopsy.env": row[2], door.DoorLabel: row[3]}
		names[row[0]] = row[4]
	}
	return labels, names, nil
}

// foreign refuses while a container the door did not make for this user is under the
// project's names (foreignUnder).
func (s *doorSide) foreign() error { return refusal(foreignUnder(s.policy, s.docker)) }

// foreignUnder refuses while containers the door did not make are under p's project's names
// (door.Policy.Foreign), every container read through docker, naming them (foreignError): what
// the deploy user runs there stays out of the door's reach, for the door, add and a destroy.
// A read that fails is an error.
func foreignUnder(p door.Policy, docker func(args ...string) *exec.Cmd) error {
	all, _, err := containerLabels(docker(containerRows...))
	if err != nil {
		return err
	}
	if ids := p.Foreign(all); len(ids) > 0 {
		return foreignError(p.Project, ids, all)
	}
	return nil
}

// dockerCommand is a docker command as the deploy user runs it, for a check that reads Docker
// (foreignUnder) outside the door.
func dockerCommand(args ...string) *exec.Cmd { return exec.Command("docker", args...) }

// refusal is err as the door says it, "refused: " first, a refusal's own words kept.
func refusal(err error) error {
	if err == nil || strings.HasPrefix(err.Error(), "refused: ") {
		return err
	}
	return fmt.Errorf("refused: %w", err)
}

// foreignError names what holds a project's names outside the door, and how it goes.
func foreignError(project string, ids []string, all map[string]map[string]string) error {
	var names []string
	for _, id := range ids {
		name := all[id]["com.docker.compose.project"]
		if name == "" {
			name = all[id]["devopsy.env"]
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return &report.Failure{Msg: fmt.Sprintf("refused: containers the door did not make are under %s's names: %s", project, strings.Join(names, ", ")),
		Remedies: []report.Remedy{report.Fix("the deploy user takes them down, which keeps their volumes:", "docker compose -p "+names[0]+" down")}}
}
