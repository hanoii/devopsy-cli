package main

import (
	"bytes"
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
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// The deploy user's side of the door, from sudo: it checks a project user's request
// and runs it, and never reads the project's tree itself, what it needs of that coming
// from runDoorRead, run as the project user. So no link or swap there can show it
// anything of the deploy user's. The project user's side is door_client.go.

// doorEnvironment leaves only what this user is, whatever sudo was set to keep: a
// caller's PATH would choose the docker this runs, and its HOME the config it obeys.
func doorEnvironment() (home string, err error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	os.Clearenv()
	os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	os.Setenv("HOME", u.HomeDir)
	return u.HomeDir, nil
}

// doorNotHere is the door's exit when it has nothing to say of this user.
const doorNotHere = 3

// doorLog, once the server's space is known, keeps what was refused and who asked:
// what a leaked key tried is worth having, and a real release asks for nothing refused.
var doorLog string

func doorRefused(err error) int {
	report.Error(err.Error())
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
	if _, err := doorEnvironment(); err != nil {
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
	real   string
	env    []string
	vets   []remote.DoorVet
	// made are this request's work directories, so that a refusal leaves none behind.
	made []string
}

// read runs a read of the project's tree, as its user.
func (s *doorSide) read(op string, args ...string) *exec.Cmd {
	return exec.Command("sudo", sudoArgs(s.policy.User, doorReadFlag, append([]string{op}, args...)...)...)
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

// Real implements door.Paths.
func (s *doorSide) Real(path string) (string, error) {
	out, err := s.read("real", path).Output()
	if err != nil {
		return "", fmt.Errorf("cannot resolve %s", path)
	}
	return strings.TrimSpace(string(out)), nil
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
	sealed := s.policy.Sealed()
	if err := os.MkdirAll(filepath.Dir(sealed), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(sealed, 0o750); err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(sealed, ".seal-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	if err := s.untar(path, work); err != nil {
		return "", err
	}
	copied := filepath.Join(work, filepath.Base(path))
	if err := noLink(copied); err != nil {
		return "", err
	}
	sum, err := contentHash(copied)
	if err != nil {
		return "", err
	}
	final := filepath.Join(sealed, sum)
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

// contentHash hashes what a sealed copy holds: names, links and bytes.
func contentHash(root string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fmt.Fprintf(h, "%s\x00%o\x00", rel, d.Type())
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			h.Write([]byte(target))
		case d.Type().IsRegular():
			fi, err := d.Info()
			if err != nil {
				return err
			}
			fmt.Fprintf(h, "%o\x00", fi.Mode().Perm())
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(h, f); err != nil {
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
		mounts, err := s.docker("inspect", "--format", "{{range .Mounts}}{{.Source}}\n{{end}}", id).Output()
		if err != nil {
			return
		}
		for _, src := range strings.Fields(string(mounts)) {
			if rest, ok := strings.CutPrefix(src, sealed+"/"); ok {
				word, _, _ := strings.Cut(rest, "/")
				held[word] = true
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

// workDir makes a directory in the deploy user's nosuid space, for this user alone.
func (s *doorSide) workDir(perms string) (string, error) {
	root := filepath.Join(s.policy.Space, "work")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	d, err := os.MkdirTemp(root, s.policy.Project+"-")
	if err != nil {
		return "", err
	}
	s.made = append(s.made, d)
	if err := s.acl(d, perms, false); err != nil {
		return "", err
	}
	return d, nil
}

// removeWork removes a work directory, as root where a container left root's files.
func (s *doorSide) removeWork(dir string) {
	if os.RemoveAll(dir) == nil {
		return
	}
	root := filepath.Join(s.policy.Space, "work")
	_ = s.docker("run", "--rm", "--network", "none", "--cap-drop", "MKNOD", "--security-opt", "no-new-privileges", "-v", root+":/w", remote.UtilImage, "rm", "-rf", "/w/"+filepath.Base(dir)).Run()
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

// doorKnown is what devopsy's own util containers run.
func doorKnown() door.Known {
	return door.Known{
		Image:        remote.UtilImage,
		Dockerfile:   strings.Join(remote.UtilDockerfile, "\n") + "\n",
		Owners:       ownEntry,
		Export:       exportTarScript,
		Import:       emptyAndExtract,
		ReflinkProbe: reflinkProbeScript,
		SnapshotTar:  snapshotTarScript,
		Space:        spaceProbeScript,
		Share:        shareScript,
		Daemon:       rsyncDaemonScript,
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
	doorWho = os.Getenv("SUDO_USER")
	home, err := doorEnvironment()
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
	doorLog = filepath.Join(policy.Space, "refused.log")
	side := &doorSide{policy: policy, real: realDocker(), env: os.Environ(), vets: settings.Vets}
	if len(args) == 0 {
		return doorRefused(errors.New("refused: no request"))
	}
	switch args[0] {
	case "entries":
		if renderErr != nil {
			return doorRefused(renderErr)
		}
		return side.entries(rendered)
	case "mnt":
		if len(args) == 2 {
			return side.mnt(args[1])
		}
	case "entry":
		if len(args) == 4 {
			return side.entry(args[1], args[2], args[3] == "file")
		}
	case "clone":
		if len(args) == 2 {
			return side.clone(args[1])
		}
	case "mark":
		if len(args) == 3 {
			return side.mark(args[1], args[2])
		}
	case "compose":
		if len(args) < 5 {
			return doorRefused(errors.New("refused: no compose command"))
		}
		if renderErr != nil {
			return doorRefused(renderErr)
		}
		return side.compose(args[1], rendered, args[3], args[4], args[5:])
	case "request":
		if len(args) >= 3 {
			return side.request(args[1], args[2:])
		}
	}
	return doorRefused(errors.New("refused: not a request: " + strings.Join(args[:min(1, len(args))], " ")))
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
	ids, err := hostIDs(s.Users)
	if err != nil {
		return door.Policy{}, err
	}
	return door.Policy{
		Project: s.Project, User: who, Space: s.Space, Tree: tree, Joinable: s.Joinable, Roles: s.Roles, ReadAll: s.ReadAll,
		ForbiddenIDs: ids, Others: otherProjects(s),
	}, nil
}

// otherProjects are the server's projects but this one: what the door's users hold, and
// what the deploy user's release root does. Names are flat, so a longer one is out of
// this project's reach (door.Policy.OwnName).
func otherProjects(s remote.DoorUser) []string {
	others := slices.Clone(s.Others)
	if r, err := remote.ReleaseSettings(); err == nil {
		if entries, err := os.ReadDir(r.Root); err == nil {
			for _, e := range entries {
				if e.IsDir() && !strings.HasPrefix(e.Name(), ".") && e.Name() != s.Project {
					others = append(others, e.Name())
				}
			}
		}
	}
	return others
}

// hostIDs are ids no container may run as: the deploy user's, the door's users', and the
// Docker socket's group. Other system accounts stay usable: images take low ids of their
// own, and nobody logs in as those.
func hostIDs(users []string) ([]int, error) {
	ids := []int{os.Getuid(), os.Getgid()}
	for _, name := range users {
		u, err := user.Lookup(name)
		if err != nil {
			continue
		}
		for _, v := range []string{u.Uid, u.Gid} {
			if id, err := strconv.Atoi(v); err == nil {
				ids = append(ids, id)
			}
		}
	}
	if fi, err := os.Stat(dockerSocket); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			ids = append(ids, int(st.Gid))
		}
	}
	sort.Ints(ids)
	return slices.Compact(ids), nil
}

// takeFile reads a rendered file by the caller's rights, and removes it.
func takeFile(who, path string) (map[string]any, error) {
	var out bytes.Buffer
	c := exec.Command("sudo", sudoArgs(who, doorReadFlag, "take", path)...)
	c.Stdout = &out
	if err := c.Run(); err != nil {
		return nil, errors.New("refused: not a file this project's user can read")
	}
	d := json.NewDecoder(&out)
	// A large number stays as written: compose reads what this writes back.
	d.UseNumber()
	file := map[string]any{}
	if err := d.Decode(&file); err != nil {
		return nil, errors.New("refused: not a rendered compose file")
	}
	return file, nil
}

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
	if !s.policy.OwnName(filepath.Base(real)) || real != s.policy.Mnt(filepath.Base(real)) || !doorPlainName.MatchString(e) {
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
		for _, word := range strings.Split(rest, "/") {
			if !doorPlainName.MatchString(strings.TrimPrefix(word, ".")) {
				return doorRefused(errors.New("refused: not a clone's name: " + path))
			}
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
	if !strings.HasPrefix(real, s.policy.Clones()+"/") || !doorPlainName.MatchString(id) {
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

var doorPlainName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

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
	if err := door.CheckVerb(argv); err != nil {
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
	real, err := s.Real(dir)
	if err != nil || (real != s.policy.Tree && !s.policy.InTree(real)) {
		return doorRefused(errors.New("refused: not this project's directory: " + dir))
	}
	name, _ := file["name"].(string)
	checked, err := json.Marshal(file)
	if err != nil {
		return doorRefused(err)
	}
	starts := slices.Contains([]string{"up", "create", "run"}, argv[0])
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
		if err := s.checkImageLabels(file, config); err != nil {
			return doorRefused(err)
		}
		// The checked file's sources are real paths already.
		same := func(p string) (string, error) { return p, nil }
		if err := s.bindEntries(file, name, same); err != nil {
			return doorRefused(err)
		}
	}
	if argv[0] == "up" {
		s.pruneSealed()
	}
	fd, err := unlinkedFile(checked)
	if err != nil {
		return doorRefused(err)
	}
	env := s.env
	if slices.Contains([]string{"up", "pull", "create", "run", "build"}, argv[0]) {
		d, err := s.logins(config)
		if err != nil {
			return doorRefused(err)
		}
		env = append(slices.Clone(env), "DOCKER_CONFIG="+d)
	}
	cmd := []string{"docker", "compose", "-p", name, "--project-directory", dir, "--env-file", "/dev/null", "-f", fmt.Sprintf("/dev/fd/%d", fd)}
	for _, p := range strings.Split(profiles, ",") {
		if composeProfile.MatchString(p) {
			cmd = append(cmd, "--profile", p)
		}
	}
	err = syscall.Exec(s.real, append(cmd, argv...), env)
	return doorRefused(err)
}

// checkImageLabels refuses an image that labels its containers with what devopsy or a
// proxy reads: a build's or an image's LABEL lands in a container's labels, past any check
// of a file's. An image not here yet comes first, with this project's logins.
func (s *doorSide) checkImageLabels(file map[string]any, config string) error {
	services, _ := file["services"].(map[string]any)
	for _, svc := range slices.Sorted(maps.Keys(services)) {
		sv, _ := services[svc].(map[string]any)
		image, _ := sv["image"].(string)
		if image == "" {
			continue
		}
		inspect := func() ([]byte, error) {
			return s.docker("image", "inspect", "--format", "{{json .Config.Labels}}", image).Output()
		}
		out, err := inspect()
		if err != nil {
			// Compose would pull it after this check, so it comes first.
			d, lerr := s.logins(config)
			if lerr != nil {
				return lerr
			}
			pull := s.docker("pull", "-q", image)
			pull.Env = append(slices.Clone(s.env), "DOCKER_CONFIG="+d)
			pull.Stderr = os.Stderr
			if pull.Run() != nil {
				// Compose's own error says it better, on what it then fails to find.
				continue
			}
			if out, err = inspect(); err != nil {
				continue
			}
		}
		labels := map[string]string{}
		if json.Unmarshal(out, &labels) != nil {
			continue
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

// logins leaves a project's registry logins alone in a directory of the door's.
// Never its own directory: Docker's CLI loads plugins and helpers from there, which
// would run the project's own program as the deploy user.
func (s *doorSide) logins(config string) (string, error) {
	var out bytes.Buffer
	c := s.read("cat", config)
	c.Stdout = &out
	auths := map[string]map[string]string{}
	if c.Run() == nil {
		var parsed struct {
			Auths map[string]struct {
				Auth string `json:"auth"`
			} `json:"auths"`
		}
		if json.Unmarshal(out.Bytes(), &parsed) == nil {
			for k, v := range parsed.Auths {
				auths[k] = map[string]string{"auth": v.Auth}
			}
		}
	}
	dir := filepath.Join(s.policy.Space, "logins", s.policy.Project)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	data, err := json.Marshal(map[string]any{"auths": auths})
	if err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, ".config.json."+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	return dir, os.Rename(tmp, filepath.Join(dir, "config.json"))
}

var doorSecret = regexp.MustCompile(`^[a-f0-9]{32}$`)

// request answers a Docker command other than compose.
func (s *doorSide) request(config string, argv []string) int {
	s.sweepWork()
	a, err := door.Decide(argv, s.policy, s, s, doorKnown())
	if err != nil {
		for _, d := range s.made {
			s.removeWork(d)
		}
		return doorRefused(err)
	}
	for _, e := range a.Entries {
		if err := s.closedDir(filepath.Dir(e.Path), "rX"); err != nil {
			return doorRefused(err)
		}
		if err := s.makeEntry(e.Path, e.File); err != nil {
			return doorRefused(err)
		}
	}
	for _, path := range a.Remove {
		if s.read("remove", path).Run() != nil {
			report.Warn("could not remove " + path)
		}
	}
	if a.Stream != nil {
		return s.stream(a.Stream)
	}
	if len(a.Docker) == 0 {
		os.Stdout.WriteString(a.Output)
		return a.Exit
	}
	env := s.env
	stdin := io.Reader(os.Stdin)
	if a.Secret != "" {
		value, err := io.ReadAll(io.LimitReader(os.Stdin, 64))
		if err != nil || !doorSecret.Match(value) {
			return doorRefused(errors.New("refused: not a secret of a sync's"))
		}
		env = append(slices.Clone(env), a.Secret+"="+string(value))
		stdin = strings.NewReader("")
	}
	if a.Logins {
		d, err := s.logins(config)
		if err != nil {
			return doorRefused(err)
		}
		env = append(slices.Clone(env), "DOCKER_CONFIG="+d)
	}
	if a.Stdin != "" {
		stdin = strings.NewReader(a.Stdin)
	}
	if a.Stdin == "" && a.Secret == "" && len(a.Collect) == 0 && len(a.Cleanup) == 0 {
		err := syscall.Exec(s.real, append([]string{"docker"}, a.Docker...), env)
		return doorRefused(err)
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
	for _, co := range a.Collect {
		if code == 0 && s.read("collect", co.Dir, co.Dest).Run() != nil {
			code = 1
		}
		s.removeWork(co.Dir)
	}
	for _, d := range a.Cleanup {
		s.removeWork(d)
	}
	return code
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
