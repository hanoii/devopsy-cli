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
	"github.com/hanoii/devopsy-cli/internal/images"
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
	case "environments":
		// --server-info's, of this user's tree: as the deploy user would list them, but by this
		// user's rights.
		if json.NewEncoder(os.Stdout).Encode(projectEnvironments(filepath.Dir(path), path)) != nil {
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
	real   string
	env    []string
	vets   []remote.DoorVet
	// made are this request's work directories, so that a refusal leaves none behind.
	made []string
	// daemonPlatform is the daemon's "<os> <architecture>", once read (platform).
	daemonPlatform string
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
	_ = s.docker(slices.Concat([]string{"run", "--rm", "--network", "none"}, door.RootUtil(), []string{"-v", root + ":/w", remote.UtilImage, "rm", "-rf", "/w/" + filepath.Base(dir)})...).Run()
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

// claim pulls ref with the project's registry logins (config, the caller's docker config)
// and records the image the pull got as the project's (door.Policy.Images, ImagesFile):
// an image another project pulled with its own logins is this one's only once its own
// pull of it goes through, a public one at the cost of one pull. It gives the digest the
// pull got and that image, read by that digest: the name is anyone's on the server to move
// right after the pull (a project user's build of a registry's name), so nothing reads it
// again. What the pull says goes to stderr on a failure, stdout being the request's answer.
func (s *doorSide) claim(ref, config string) (string, images.Image, error) {
	d, err := s.logins(config)
	if err != nil {
		return "", images.Image{}, err
	}
	refused := fmt.Errorf("refused: %s is not this project's, and its registry logins do not pull it", ref)
	m, err := s.platformManifest(ref, d)
	if err != nil {
		if err == errNoRead {
			return "", images.Image{}, refused
		}
		return "", images.Image{}, fmt.Errorf("refused: %s: %w", ref, err)
	}
	pinned := images.ByDigest(ref, m)
	var said bytes.Buffer
	pull := s.docker("pull", pinned)
	pull.Env = append(slices.Clone(s.env), "DOCKER_CONFIG="+d)
	pull.Stdout, pull.Stderr = &said, &said
	if pull.Run() != nil {
		os.Stderr.Write(said.Bytes())
		return "", images.Image{}, refused
	}
	img, err := s.inspectImage(pinned)
	if err != nil {
		return "", images.Image{}, fmt.Errorf("refused: %s: %v", ref, err)
	}
	if !slices.Contains(s.policy.Images, img.ID) {
		if err := s.prove(pinned, d); err != nil {
			// The reference the pull made goes, and the image with it unless another name
			// holds it: a later claim landing there would pull nothing.
			_ = s.docker("image", "rm", pinned).Run()
			return "", images.Image{}, fmt.Errorf("refused: %s: %w", ref, err)
		}
		if err := s.record(img.ID); err != nil {
			return "", images.Image{}, err
		}
	}
	// The name asked, the registry's content proven under it, so a start finds it here and
	// asks the registry nothing.
	if !images.Pinned(ref) {
		_ = s.docker("tag", pinned, ref).Run()
	}
	return m, img, nil
}

// record adds id to this project's claimed images (Policy.ImagesFile).
func (s *doorSide) record(id string) error {
	file := s.policy.ImagesFile()
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(id + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	s.policy.Images = append(s.policy.Images, id)
	return err
}

// errNoRead is a registry that gives a project's logins nothing for a ref.
var errNoRead = errors.New("its registry logins do not read it")

// platformManifest is the digest of the one image manifest ref comes to on this server, read
// with the logins in the DOCKER_CONFIG dir logins: ref's own when it is a manifest, else the
// entry of its index for the daemon's platform, itself a manifest, its bytes checked against
// its digest. A claim pulls, proves and pins that digest, never an index's: Docker's pull and
// BuildKit's fetch choose from an index by rules of their own, and one index could send each
// to another image.
func (s *doorSide) platformManifest(ref, logins string) (string, error) {
	// Read as the proof fetches: the project's builder names the registries' settings (a
	// mirror, a CA), as the admin gave them to the builders.
	b, err := s.builder()
	if err != nil {
		return "", errors.New(strings.TrimPrefix(err.Error(), "refused: "))
	}
	raw := func(r string) ([]byte, error) {
		c := s.docker("buildx", "imagetools", "inspect", "--raw", r)
		c.Env = append(append(slices.Clone(s.env), b...), "DOCKER_CONFIG="+logins)
		return c.Output()
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
	var m manifest
	if err != nil || json.Unmarshal(data, &m) != nil {
		return "", errNoRead
	}
	if len(m.Manifests) == 0 {
		if m.Config == nil {
			return "", errors.New("its registry serves no image manifest for it")
		}
		if _, digest, ok := strings.Cut(ref, "@"); ok {
			return digest, nil
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
		return "", fmt.Errorf("no image for %s/%s in its index", goos, arch)
	}
	data, err = raw(images.ByDigest(ref, entry))
	m = manifest{}
	if err != nil || digestOf(data) != entry || json.Unmarshal(data, &m) != nil || len(m.Manifests) > 0 || m.Config == nil {
		return "", fmt.Errorf("its index's entry for %s/%s, %s, is no image manifest", goos, arch, entry)
	}
	return entry, nil
}

// digestOf is data's sha256 digest, as a registry names it.
func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// platform is the daemon's os, architecture and the variant an index's entry for it may name
// (arm64's v8, arm's v7), read once.
func (s *doorSide) platform() (goos, arch, variant string, err error) {
	if s.daemonPlatform == "" {
		out, err := s.docker("version", "--format", "{{.Server.Os}} {{.Server.Arch}}").Output()
		if err != nil {
			return "", "", "", fmt.Errorf("the daemon's platform: %v", err)
		}
		s.daemonPlatform = strings.TrimSpace(string(out))
	}
	goos, arch, _ = strings.Cut(s.daemonPlatform, " ")
	variant = map[string]string{"arm64": "v8", "arm": "v7"}[arch]
	return goos, arch, variant, nil
}

// prove fetches the image pinned names (<repo>@<digest>, what a claim's pull got) on the
// project's own builder, with its logins (the DOCKER_CONFIG dir logins): the builder's store
// holds only what the project fetched or built, so every blob it lacks comes from the
// claimant's registry, checked by its digest, and the exporter needs every one. A pull alone
// proves only that a registry answered: Docker fetches no layer it holds, so a registry that
// names another project's layers, serving none, lands on them. Every first claim proves.
func (s *doorSide) prove(pinned, logins string) error {
	b, err := s.builder()
	if err != nil {
		return errors.New(strings.TrimPrefix(err.Error(), "refused: "))
	}
	dir, err := os.MkdirTemp("", "devopsy-door-proof-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	var said bytes.Buffer
	// A tar of its files: each layer unpacked, so checked against the image's own config,
	// with no shell of the image's needed (RUN would refuse a distroless one).
	c := s.docker("buildx", "build", "--output", "type=tar,dest=/dev/null", "-f", "-", dir)
	c.Env = append(append(slices.Clone(s.env), b...), "DOCKER_CONFIG="+logins)
	c.Stdin = strings.NewReader("FROM " + pinned + "\n")
	c.Stdout, c.Stderr = &said, &said
	if c.Run() != nil {
		os.Stderr.Write(said.Bytes())
		return errors.New("its registry did not serve the image to this project's logins")
	}
	return nil
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
	if err := spaceGuarded(policy.Space); err != nil {
		report.Fail(err, spaceFix(policy.Space))
		doorRecord(err.Error())
		return 2
	}
	doorLog = filepath.Join(policy.Space, "refused.log")
	// A project user with no key left logs in no more, and what it left running gets nothing
	// from the door either (KeysRemoveScript ends it).
	if !remote.KeysLeft(remote.DoorKeysFile(doorWho)) {
		return doorRefused(errors.New("refused: " + doorWho + " has no key left"))
	}
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
	case "down":
		if len(args) >= 2 {
			return side.downProject(args[1], args[2:])
		}
	case "request":
		if len(args) >= 3 {
			return side.request(args[1], args[2:])
		}
	case "sync-key":
		return side.syncKey(args[1:])
	case "login":
		if len(args) == 3 {
			return side.login(args[1], args[2])
		}
	}
	return doorRefused(errors.New("refused: not a request: " + strings.Join(args[:min(1, len(args))], " ")))
}

// login has Docker check a project user's registry login, its password on stdin (doorLogin):
// the deploy user's docker login asks the daemon, which reaches the registry as its pulls
// do, where the project users' firewall keeps the project user off. Its config is a
// directory of its own, gone after, so nothing of the login stays here.
func (s *doorSide) login(registry, user string) int {
	if !door.LoginRegistry(registry) || !door.LoginUser(user) {
		return doorRefused(fmt.Errorf("refused: a login to %q as %q", registry, user))
	}
	password, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<16))
	if err != nil {
		return doorRefused(err)
	}
	root := filepath.Join(s.policy.Space, "work")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return doorRefused(err)
	}
	config, err := os.MkdirTemp(root, s.policy.Project+"-login-")
	if err != nil {
		return doorRefused(err)
	}
	defer os.RemoveAll(config)
	// In a pipe filled before it starts, never written after: a docker login that ended
	// first would raise SIGPIPE.
	in, err := filledPipe(string(bytes.TrimSuffix(password, []byte("\n"))))
	if err != nil {
		return doorRefused(err)
	}
	defer in.Close()
	var out bytes.Buffer
	c := s.docker("--config", config, "login", "--username", user, "--password-stdin", registry)
	c.Stdin, c.Stdout, c.Stderr = in, &out, &out
	if err := c.Run(); err != nil {
		report.Error("docker login "+registry+" failed", strings.Split(strings.TrimSpace(out.String()), "\n")...)
		return 1
	}
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
	if err := editSyncKeys(remote.DoorSyncKeysFile(s.policy.User), selfPath(), args, time.Now()); err != nil {
		return doorRefused(fmt.Errorf("refused: %w", err))
	}
	return 0
}

// spaceGuarded refuses a door whose space is not on a mount with nosuid and nodev: there a
// container's setuid file crosses projects and a crafted tar reaches root, so the door does
// nothing rather than half its promise.
func spaceGuarded(space string) error {
	nosuid, nodev, err := mountGuards(space)
	if err != nil {
		return fmt.Errorf("refused: the door's space %s: %v", space, err)
	}
	if !nosuid || !nodev {
		return fmt.Errorf("refused: the door's space %s is not on a mount with nosuid and nodev", space)
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
	ids, err := hostIDs(s.Users)
	if err != nil {
		return door.Policy{}, err
	}
	p := door.Policy{
		Project: s.Project, User: who, UID: u.Uid, Space: s.Space, Tree: tree, Joinable: s.Joinable, Roles: s.Roles,
		ForbiddenIDs: ids, Others: otherProjects(s),
	}
	p.Images = claimedImages(p.ImagesFile())
	return p, nil
}

// otherProjects are the server's projects but this one: what the door's users hold, and
// what the deploy user's release root does. Names are flat, so a longer one is out of
// this project's reach (door.Policy.OwnName).
func otherProjects(s remote.DoorUser) []string {
	root := ""
	if r, err := remote.ReleaseSettings(); err == nil {
		root = r.Root
	}
	var others []string
	for _, p := range serverProjects(s.Others, root) {
		if p != s.Project {
			others = append(others, p)
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
	c := doorRead(who, "take", path)
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
	real, err := s.Real(dir)
	if err != nil || (real != s.policy.Tree && !s.policy.InTree(real)) {
		return doorRefused(errors.New("refused: not this project's directory: " + dir))
	}
	name, _ := file["name"].(string)
	s.policy.Label(file)
	starts := slices.Contains([]string{"up", "create", "run"}, argv[0])
	// The image check pins what it checked into the file (startImage), so the file compose
	// gets, and the vets judge, is marshalled after it.
	if starts {
		if err := s.checkImageLabels(file, config); err != nil {
			return doorRefused(err)
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
	if argv[0] == "build" {
		b, err := s.builder()
		if err != nil {
			return doorRefused(err)
		}
		env = append(slices.Clone(env), b...)
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

// downProject takes this project's compose name down by that name alone, no file to
// check: compose finds what it holds by its own label (composeDownProject).
func (s *doorSide) downProject(name string, flags []string) int {
	if !s.policy.OwnName(name) {
		return doorRefused(errors.New("refused: not this project's name: " + name))
	}
	argv := append([]string{"down"}, flags...)
	if err := s.policy.CheckVerb(argv); err != nil {
		return doorRefused(err)
	}
	if err := s.foreign(); err != nil {
		return doorRefused(err)
	}
	err := syscall.Exec(s.real, append([]string{"docker", "compose", "-p", name}, argv...), s.env)
	return doorRefused(err)
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
// read in one inspect; else claimed first, by the digest its pull got. A reference by
// digest, or an id, is a pin already. An image not here yet is claimed too, with this
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
		if err := s.policy.Claimable(image); err != nil {
			return "", nil, err
		}
		if digest, img, err = s.claim(image, config); err != nil {
			return "", nil, fmt.Errorf("%s: %w", svc, err)
		}
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
	mnt := filepath.Join(s.policy.Space, "mnt") + "/"
	var entries []string
	for _, c := range containers {
		out, err := s.docker("inspect", "--type", "container", "-f", `{{range .Mounts}}{{if .RW}}{{println .Source}}{{end}}{{end}}`, c).Output()
		if err != nil {
			continue
		}
		for _, src := range strings.Fields(string(out)) {
			if strings.HasPrefix(src, mnt) && filepath.Dir(src)+"/" != mnt {
				entries = append(entries, src)
			}
		}
	}
	return entries
}

// builder is what a build's environment needs to run on this project's own BuildKit,
// made at its first build: on one builder, cache mounts are shared across projects by
// their id, so one project's build would fill a cache another's trusts. Its settings are
// the door's (BUILDX_CONFIG), apart from the directory of logins a build gets.
func (s *doorSide) builder() ([]string, error) {
	name := "devopsy-" + s.policy.Project
	// Its network is its own: on Docker's default bridge, which every container shares, a
	// build step is a member of it, and reaches another project's builder and what its build
	// listens on.
	network := name + "-build"
	b := []string{"BUILDX_CONFIG=" + filepath.Join(s.policy.Space, "buildx"), "BUILDX_BUILDER=" + name}
	env := append(slices.Clone(s.env), b[0])
	run := func(args ...string) ([]byte, error) {
		c := s.docker(args...)
		c.Env, c.Stderr = env, os.Stderr
		return c.Output()
	}
	out, err := run("buildx", "inspect", name)
	if err == nil && strings.Contains(string(out), `network="`+network+`"`) {
		return b, nil
	}
	if err == nil {
		// Made otherwise, on the default bridge: made again.
		if _, err := run("buildx", "rm", name); err != nil {
			return nil, fmt.Errorf("refused: %s's builder is on Docker's default bridge, and could not be removed: %v", s.policy.Project, err)
		}
	}
	if _, err := run("network", "inspect", network); err != nil {
		if _, err := run("network", "create", network); err != nil {
			return nil, fmt.Errorf("refused: %s's builder's network could not be made: %v", s.policy.Project, err)
		}
	}
	if _, err := run("buildx", "create", "--name", name, "--driver", "docker-container", "--driver-opt", "network="+network); err != nil {
		// Another environment's build may have made it meanwhile.
		if out, ierr := run("buildx", "inspect", name); ierr == nil && strings.Contains(string(out), `network="`+network+`"`) {
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
	for _, ref := range a.Claims {
		digest, _, err := s.claim(ref, config)
		if err != nil {
			return doorRefused(err)
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
	if a.Stream != nil {
		return s.stream(a.Stream)
	}
	if len(a.Docker) == 0 {
		os.Stdout.WriteString(a.Output)
		return a.Exit
	}
	env := s.env
	stdin := io.Reader(os.Stdin)
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
	regrant := slices.Concat(a.Regrant, s.writtenEntries(a.Removes))
	if a.Stdin == "" && len(a.Collect) == 0 && len(a.Cleanup) == 0 && len(regrant) == 0 {
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

// containerRows is what says whose each container is, one JSON array per line: a label's
// value is anyone's to write, spaces included.
var containerRows = []string{"ps", "-a", "--no-trunc", "--format",
	`[{{json .ID}},{{json (.Label "com.docker.compose.project")}},{{json (.Label "devopsy.env")}},{{json (.Label "devopsy.door")}}]`}

// containerLabels are the server's containers by id, with the labels that say whose they are.
func containerLabels(c *exec.Cmd) (map[string]map[string]string, error) {
	out, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("cannot read what runs: %v", err)
	}
	all := map[string]map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var row []string
		if err := json.Unmarshal([]byte(line), &row); err != nil || len(row) != 4 {
			return nil, fmt.Errorf("cannot read what runs: %q", line)
		}
		all[row[0]] = map[string]string{"com.docker.compose.project": row[1], "devopsy.env": row[2], door.DoorLabel: row[3]}
	}
	return all, nil
}

// foreign refuses while a container the door did not make for this user is under the
// project's names (door.Policy.Foreign).
func (s *doorSide) foreign() error {
	all, err := containerLabels(s.docker(containerRows...))
	if err != nil {
		return fmt.Errorf("refused: %v", err)
	}
	if ids := s.policy.Foreign(all); len(ids) > 0 {
		return foreignError(s.policy.Project, ids, all)
	}
	return nil
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
