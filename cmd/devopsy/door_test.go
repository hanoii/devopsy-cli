package main

import (
	"cmp"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/testfile"
	"github.com/hanoii/devopsy-cli/internal/uids"
	"golang.org/x/sys/unix"
)

// utilScripts are the util containers' scripts by name, as the code calls them: what
// a project user's door must take, or its release fails at runtime.
var utilScripts = map[string]string{
	"exportTarScript": exportTarScript, "reflinkProbeScript": reflinkProbeScript, "snapshotTarScript": snapshotTarScript,
	"spaceProbeScript": spaceProbeScript, "ownEntry": ownEntry, "ownVolumeRoot": ownVolumeRoot, "volumeRoots": volumeRoots, "emptyAndExtract": emptyAndExtract, "shareScript": shareScript,
	"rsyncDaemonScript": rsyncDaemonScript, "restoreStreamScript": restoreStreamScript, "receiveScript": receiveScript,
}

// TestDoorKnowsDevopsysUtilScripts fails when devopsy runs a util container whose
// script the door does not take. A script is one of doorKnown's, by value.
func TestDoorKnowsDevopsysUtilScripts(t *testing.T) {
	t.Parallel()
	known := doorKnown()
	scripts := []string{known.Owners, known.VolumeOwner, known.VolumeRoots, known.Export, known.Import, known.ReflinkProbe, known.SnapshotTar, known.Space, known.Share, known.Daemon, known.Restore, known.Receive}
	for name, script := range utilScripts {
		if !slices.Contains(scripts, script) {
			t.Errorf("%s is not in doorKnown", name)
		}
	}
	found := 0
	walkSources(t, ".", func(fset *token.FileSet, args []ast.Expr) {
		for i := 0; i+3 < len(args); i++ {
			image, ok := args[i].(*ast.SelectorExpr)
			a, aok := args[i+1].(*ast.BasicLit)
			b, bok := args[i+2].(*ast.BasicLit)
			if !ok || image.Sel.Name != "UtilImage" || !aok || !bok || a.Value != `"sh"` || b.Value != `"-c"` {
				continue
			}
			found++
			switch s := args[i+3].(type) {
			case *ast.Ident:
				if _, ok := utilScripts[s.Name]; !ok {
					t.Errorf("%s: a util container's script %s is not in utilScripts", fset.Position(s.Pos()), s.Name)
				}
			case *ast.CallExpr:
				// Computed from a project's labels: its form is checked (TestDoorTakesComputedForms).
				if id, ok := s.Fun.(*ast.Ident); !ok || id.Name != "cloneScript" {
					t.Errorf("%s: a util container's script is computed: name it", fset.Position(s.Pos()))
				}
			default:
				t.Errorf("%s: a util container's script is written in place: name it", fset.Position(s.Pos()))
			}
		}
	})
	if found < 5 {
		t.Errorf("found %d scripts: the test no longer sees them", found)
	}
}

// doorVerbs are Docker's commands the door answers, or says why it has no need to.
var doorVerbs = map[string]string{
	"build": "", "compose": "", "exec": "", "image": "", "inspect": "", "login": "", "network": "", "ps": "", "pull": "", "rm": "", "run": "",
	"tag": "", "version": "", "volume": "", "__complete": "",
	"buildx": "--context-hash, local; --server-setup, the deploy user's", "info": "--server-info and --server-setup, the admin's", "system": "--server-info, the admin's",
	"builder": "a hint to the admin", "not": "a message",
}

// TestDoorKnowsDevopsysDockerCommands fails when devopsy calls a Docker command the
// door has no answer for: a project user's release would then fail there. It reads
// devopsy's calls of docker and its scripts' lines; a new command goes in doorVerbs.
func TestDoorKnowsDevopsysDockerCommands(t *testing.T) {
	t.Parallel()
	inScript := regexp.MustCompile(`(?m)(?:^\s*|[;&|(]\s*)docker ([a-z_-]+)`)
	see := func(fset *token.FileSet, args []ast.Expr) {
		for i, arg := range args {
			lit, ok := arg.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil {
				continue
			}
			verbs := []string{}
			// exec.Command("docker", "<verb>", ...), and a helper's first word.
			if text == "docker" && i+1 < len(args) {
				if next, ok := args[i+1].(*ast.BasicLit); ok && next.Kind == token.STRING {
					if v, err := strconv.Unquote(next.Value); err == nil {
						verbs = append(verbs, v)
					}
				}
			}
			for _, m := range inScript.FindAllStringSubmatch(text, -1) {
				if !strings.Contains(text, " %") {
					verbs = append(verbs, m[1])
				}
			}
			for _, v := range verbs {
				if _, ok := doorVerbs[v]; !ok && !strings.Contains(text, "devopsy: ") {
					t.Errorf("%s: docker %s: the door has no answer: add it, or say why in doorVerbs", fset.Position(lit.Pos()), v)
				}
			}
		}
	}
	for _, dir := range []string{".", "../../internal/remote", "../../internal/cli", "../../internal/images"} {
		walkSources(t, dir, see)
	}
}

// walkSources calls see with the arguments of every call and list in dir's code.
func walkSources(t *testing.T, dir string, see func(*token.FileSet, []ast.Expr)) {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.CallExpr:
				see(fset, v.Args)
			case *ast.CompositeLit:
				see(fset, v.Elts)
			}
			return true
		})
	}
}

// The forms the door checks of what devopsy computes, against what it computes: a change
// to either side alone would otherwise fail only in a project user's release.
func TestDoorTakesComputedForms(t *testing.T) {
	t.Parallel()
	d := dataSet{entries: []string{"storage"}, volumes: []cli.Volume{{Key: "db", Name: "shop-prod_db"}}}
	if script := cloneScript(d); !door.CloneScriptForm(script) {
		t.Errorf("the door does not take devopsy's own clone:\n%s", script)
	}
	p := door.Policy{Project: "shop", User: "devopsy-shop", Space: "/space", Tree: "/home/devopsy-shop/shop", ProjectUIDs: []uint32{2000000123, 1900000123}}
	daemon := []string{"run", "--rm", "-d", "--network", "none", "-e", "RSYNCD_CONF=" + rsyncdConf([]string{"storage", "app.ini"}, []string{"app.ini"}, []string{"db"}),
		"-v", "/space/mnt/shop-prod:/mnt:ro", "-v", "shop-prod_db:/vol/db:ro", remote.UtilImage, "sh", "-c", rsyncDaemonScript}
	if _, err := door.Decide(daemon, p, nil, doorTestHost{}, doorKnown()); err != nil {
		t.Errorf("the door does not take devopsy's own rsync daemon: %v", err)
	}
	child := []childService{{Dirs: []string{"storage", "app.ini"}, Volumes: []string{"db"}, Owners: map[string]string{"storage": "2000000123:2000000123"}}}
	c := newContainer("sync", "shop-dev", "1.abcdef-receive", remote.SyncTask("1.abcdef"))
	for _, r := range []syncReceive{{Entry: "storage"}, {Entry: "app.ini", File: true}, {Volume: "db"}} {
		plan, err := planReceive(&r, child, uids.Map{Env: 2000000123, Project: 1900000123}, "/space/mnt/shop-dev", "shop-dev", false)
		if err != nil {
			t.Fatalf("%+v: %v", r, err)
		}
		if _, err := door.Decide(receiveRun(c, plan), p, nil, doorTestHost{}, doorKnown()); err != nil {
			t.Errorf("the door does not take devopsy's own receive of %+v: %v", r, err)
		}
	}
}

// doorTestHost takes every path as it says.
type doorTestHost struct{}

func (doorTestHost) Real(p string) (string, error)           { return filepath.Clean(p), nil }
func (doorTestHost) Seal(p string) (string, error)           { return p, nil }
func (doorTestHost) Within(root, rel string) error           { return nil }
func (doorTestHost) IsDir(string) bool                       { return false }
func (doorTestHost) CopyIn(p string, _ bool) (string, error) { return p, nil }
func (doorTestHost) WorkDir() (string, error)                { return "/space/work/x", nil }

// TestRunDoorRead is the project user's side, on real files, as sudo runs it: the binary,
// since its doorEnvironment clears the whole process's environment, which in this test
// binary would leave every parallel test without TMPDIR (macOS's /tmp then gives new files
// the group wheel, not the user's).
func TestRunDoorRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := func(args ...string) int {
		cmd := exec.Command(binary, append([]string{doorReadFlag}, args...)...)
		cmd.Env = []string{"PATH=/usr/bin:/bin"}
		_ = cmd.Run()
		return cmd.ProcessState.ExitCode()
	}
	if code := read("nothing-known", f); code != 2 {
		t.Errorf("an unknown read: %d", code)
	}
	if code := read("isdir", dir); code != 0 {
		t.Errorf("isdir of a directory: %d", code)
	}
	if code := read("isdir", f); code != 1 {
		t.Errorf("isdir of a file: %d", code)
	}
	if code := read("remove", f); code != 0 {
		t.Errorf("remove: %d", code)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Error(strconv.Quote(f) + " is still there")
	}
}

// A copy holds no link: Real resolves a path and the tar comes later, by the project user's
// rights. Swapped for a link between the two, the copy would be that link, in a path of the
// deploy user's, and Docker follows it at the bind: a sealed copy would reach the host's
// files, a CopyIn would do so from a root util container.
//
// Not parallel: it sets PATH, for a sudo that answers as a swapped tree would.
func TestDoorCopiesHoldNoLink(t *testing.T) {
	bin, tree := t.TempDir(), t.TempDir()
	for name, script := range map[string]string{
		"sudo":    "#!/bin/sh\nexec tar -C " + tree + " -cf - x\n",
		"setfacl": "#!/bin/sh\nexit 0\n",
	} {
		if err := testfile.Write(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.Symlink("/etc", filepath.Join(tree, "x")); err != nil {
		t.Fatal(err)
	}
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: t.TempDir()}}
	check := func(what, got string, err error) {
		t.Helper()
		if err != nil {
			return
		}
		if fi, lerr := os.Lstat(got); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			t.Errorf("%s is a link: %s", what, got)
		}
	}
	sealed, err := side.Seal(filepath.Join(tree, "x"))
	check("a sealed copy", sealed, err)
	copied, err := side.CopyIn(filepath.Join(tree, "x"), true)
	check("a copy for a root container", copied, err)
}

// A sealed copy holds whatever the path is named, dir included: the copy lands in a directory
// of the door's own, apart from the names the door then uses beside it.
//
// Not parallel: it sets PATH, for a sudo that answers with the tree's tar.
func TestDoorSealsAPathOfAnyName(t *testing.T) {
	bin, tree := t.TempDir(), t.TempDir()
	if err := testfile.Write(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\nexec tar -C "+tree+" -cf - dir\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	write(t, filepath.Join(tree, "dir", "f"), "content\n", 0o644)
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: t.TempDir()}}
	sealed, err := side.Seal(filepath.Join(tree, "dir"))
	if err != nil {
		t.Fatalf("a path named dir: %v", err)
	}
	if got := readString(filepath.Join(sealed, "f")); got != "content\n" {
		t.Errorf("the sealed copy holds %q", got)
	}
}

// The door keeps `--profile *`, which devopsy asks for a release's pull
// (remote.ProfilePull), its failure report and --destroy: it drops what composeProfile does
// not match, without a word, and a service under a profile would then not be pulled, listed
// or taken down.
func TestDoorKeepsTheStarProfile(t *testing.T) {
	t.Parallel()
	if !composeProfile.MatchString("*") {
		t.Error("--profile * is dropped on the door's side")
	}
}

// A down by name, with no file to check, takes only this project's name and the flags a
// down may have: with another's it would take that project's containers and data.
func TestDoorDownsOnlyItsOwnName(t *testing.T) {
	t.Parallel()
	side := &doorSide{policy: door.Policy{Project: "shop", Others: []string{"shop-two"}}, real: "/nonexistent"}
	for _, c := range []struct {
		name  string
		flags []string
	}{
		{"other-prod", nil},
		{"shop-two-prod", nil},
		{"../shop-prod", nil},
		{"shop-prod", []string{"--rmi", "all"}},
		{"shop-prod", []string{"--project-directory", "/"}},
	} {
		if code := side.downProject(c.name, c.flags); code == 0 {
			t.Errorf("%s %v: not refused", c.name, c.flags)
		}
	}
}

// With nothing behind the door, shared/mnt is a plain directory, as ever, and one
// already there stays what it is.
func TestRunMntReady(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	if err := os.Mkdir(filepath.Join(base, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	mnt := filepath.Join(base, "shared", "mnt")
	if code := runMntReady([]string{base}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	fi, err := os.Lstat(mnt)
	if err != nil || !fi.IsDir() {
		t.Fatalf("not a directory: %v, %v", fi, err)
	}
	other := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(mnt); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, mnt); err != nil {
		t.Fatal(err)
	}
	if code := runMntReady([]string{base}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if got, _ := os.Readlink(mnt); got != other {
		t.Errorf("a link there became %q", got)
	}
	if code := runMntReady(nil); code != 2 {
		t.Errorf("no argument: %d", code)
	}
}

// doorFakes puts on PATH a sudo that answers reads as a project user's tree would, a
// setfacl that logs to the returned file, and a docker that logs its arguments, one per
// line, to the other. Whoever uses it is serial: it sets PATH.
func doorFakes(t *testing.T) (acl, docker string) {
	t.Helper()
	bin := t.TempDir()
	acl, docker = filepath.Join(bin, "acl.log"), filepath.Join(bin, "docker.log")
	for name, script := range map[string]string{
		"sudo": `#!/bin/sh
shift 5
op=$1; shift
case $op in
  real) printf '%s\n' "$1" ;;
  isdir) [ -d "$1" ] ;;
  tar) exec tar -C "$(dirname "$1")" -cf - -- "$(basename "$1")" ;;
  copy) exec cp -- "$1" "$2" ;;
  cat) exec dd if=/dev/zero bs=65536 count=64 2>/dev/null ;;
  write) exec cat >/dev/null ;;
esac
`,
		"setfacl": "#!/bin/sh\necho \"$*\" >>" + acl + "\n",
		"docker":  "#!/bin/sh\nprintf '%s\\n' \"$@\" >" + docker + "\nprintf '1\\t/m\\n'\n",
	} {
		if err := testfile.Write(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return acl, docker
}

// `entries` makes nothing outside its mnt, though it takes a file not yet checked: with a
// name holding `..` the mnt would be anywhere, and as the deploy user it would give this
// user rX on a directory of its choice and make one inside, which that user writes.
// ~/.docker/cli-plugins is what the deploy user's next `docker compose` loads a plugin
// from, so this would be the socket.
//
// Not parallel: it sets PATH (doorFakes).
func TestDoorEntriesStayInItsMnt(t *testing.T) {
	doorFakes(t)
	root := t.TempDir()
	victim := filepath.Join(root, "victim")
	made := filepath.Join(victim, "cli-plugins")
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(root, "space")}}
	side.entries(map[string]any{
		"name": "shop-x/../../../victim",
		"services": map[string]any{"web": map[string]any{"volumes": []any{
			map[string]any{"type": "bind", "source": made, "target": "/x"},
		}}},
	})
	if _, err := os.Stat(made); err == nil {
		t.Errorf("the door made %s, outside its space", made)
	}
}

// mark follows no link: it writes as the deploy user into a directory a root container
// filled, where a rsync or a tar can leave one, and through it the deploy user's file of
// the caller's choice would be overwritten with the id.
//
// Not parallel: it sets PATH (doorFakes).
func TestDoorMarkFollowsNoLink(t *testing.T) {
	doorFakes(t)
	root := t.TempDir()
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(root, "space")}}
	dir := filepath.Join(side.policy.Clones(), "abc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "deploy-file")
	if err := os.WriteFile(victim, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, remote.SnapshotComplete)); err != nil {
		t.Fatal(err)
	}
	side.mark(dir, "id1")
	if got, _ := os.ReadFile(victim); string(got) != "mine\n" {
		t.Errorf("the deploy user's file now holds %q", got)
	}
}

// A CopyIn of a directory gives the project user no write on it: the copy is the deploy
// user's tar, not that user's cp, and with write it could swap a directory tar has just
// made for a link to the deploy user's own, where tar would write that user's file. The
// write is needed only for a file's `copy`.
//
// Not parallel: it sets PATH (doorFakes).
func TestDoorCopyInOfADirectoryGivesNoWrite(t *testing.T) {
	acl, _ := doorFakes(t)
	tree := t.TempDir()
	if err := os.Mkdir(filepath.Join(tree, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: t.TempDir()}}
	if _, err := side.CopyIn(filepath.Join(tree, "x"), true); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(acl); strings.Contains(string(got), ":rw") {
		t.Errorf("write given while the deploy user's tar runs in it: %s", got)
	}
}

// A refusal leaves nothing in <space>/work: place copies each mount in turn, and a later
// mount's refusal takes the earlier copies with it, or a project user would fill the disk
// with refused requests.
//
// Not parallel: it sets PATH (doorFakes).
func TestDoorRefusalLeavesNoWork(t *testing.T) {
	doorFakes(t)
	tree := t.TempDir()
	if err := os.Mkdir(filepath.Join(tree, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	space := t.TempDir()
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: space, Tree: tree}}
	argv := append(append([]string{"run", "--rm", "-v", filepath.Join(tree, "x") + ":/src:ro", "-v", "/etc:/dst", remote.UtilImage}, remote.RsyncCopy...), "/src/a/", "/dst/a/")
	if code := side.request("", argv); code == 0 {
		t.Fatal("a bind of /etc taken")
	}
	entries, _ := os.ReadDir(filepath.Join(space, "work"))
	if len(entries) != 0 {
		t.Errorf("left in <space>/work: %v", entries)
	}
}

// A stream ends with its container, as untar does when tar fails (pipeline): with a pipe's
// reader held open in the door until Wait, a container that fails early would never end the
// reader's writer, which blocks past a full pipe, and Wait would wait on it. A restore of a
// corrupt snapshot would hang here.
//
// Not parallel: it sets PATH (doorFakes).
func TestDoorStreamEndsWithItsContainer(t *testing.T) {
	doorFakes(t)
	fail := filepath.Join(t.TempDir(), "docker")
	if err := testfile.Write(fail, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop"}, real: fail}
	done := make(chan int, 1)
	go func() { done <- side.stream(&door.Stream{From: "/f", To: "/o/tar", Docker: []string{"run"}}) }()
	select {
	case code := <-done:
		if code == 0 {
			t.Error("a failed container reads as a success")
		}
	case <-time.After(10 * time.Second):
		t.Error("still waiting 10 s after its container failed")
	}
}

// A caller's arguments to a util script are never code: a util container runs only a
// script in doorKnown, but the caller gives the arguments, and exportTarScript passes them
// to find as "$@", where `-exec` would run anything, as root, with no `--network none`
// asked. The other scripts take one word per place.
func TestDoorScriptArgumentsAreNotCode(t *testing.T) {
	t.Parallel()
	p := door.Policy{Project: "shop", User: "devopsy-shop", Space: "/space", Tree: "/home/devopsy-shop/shop"}
	argv := []string{"run", "--rm", "-v", "shop-prod_db:/x/db", remote.UtilImage, "sh", "-c", exportTarScript, "sh", ".", "-exec", "sh", "-c", "id", ";"}
	if a, err := door.Decide(argv, p, nil, doorTestHost{}, doorKnown()); err == nil {
		t.Errorf("a command of the caller's, as root: %q", a.Docker)
	}
}

// The door takes a command only with the mounts its form pairs it with: devopsy's rsync
// mounts a whole mnt as /src, and its extract one entry as /mnt/<entry>. Paired otherwise,
// a root container could leave a link at the top of the mnt, the deploy user's path a
// bind's source goes by, which is what pinning sources is for: the daemon follows it at
// the next restart (restart: always needs no request), and a race with a util container's
// Real gets a root /dst on the host. rsync with RsyncCopy replaces an empty directory with
// a link to /. A receive from the project user's own daemon on the loopback, which no form
// tells apart, is held to the entry it writes instead (narrow, TestDecideRunNarrowsAnRsync).
func TestDoorPairsCommandsWithTheirMounts(t *testing.T) {
	t.Parallel()
	p := door.Policy{Project: "shop", User: "devopsy-shop", Space: "/space", Tree: "/home/devopsy-shop/shop"}
	mnt := "/space/mnt/shop-prod"
	rsync := append([]string{remote.UtilImage}, remote.RsyncCopy...)
	for what, argv := range map[string][]string{
		"rsync of a link in the release onto an entry": append(append([]string{"run", "-v", p.Tree + "/prod/x:/src:ro", "-v", mnt + ":/dst"}, rsync...), "/src/link", "/dst/"),
		"rsync of a link in an entry onto an entry":    append(append([]string{"run", "-v", mnt + "/data:/src:ro", "-v", mnt + ":/dst"}, rsync...), "/src/link", "/dst/"),
		"a tar's link, the whole mnt mounted":          {"run", "-v", mnt + ":/mnt", "-v", p.Tree + "/prod/t.tar:/in/t.tar:ro", remote.UtilImage, "sh", "-c", emptyAndExtract, "sh", "/in/t.tar", "data", "data"},
	} {
		if a, err := door.Decide(argv, p, nil, doorTestHost{}, doorKnown()); err == nil {
			t.Errorf("%s: %q", what, a.Docker)
		}
	}
}

// The door takes a du of a named volume, which duData and restoreSpace mount at /v
// (mountTarget, duPath): otherwise --snapshot fails for a project user with one ("measuring
// the data (du)"), and a restore's space check says nothing. This test runs duData with a
// named volume against a fake docker and asks the door.
//
// Not parallel: it sets PATH (doorFakes).
func TestDoorTakesDuOfANamedVolume(t *testing.T) {
	_, logged := doorFakes(t)
	d := dataSet{mnt: "/space/mnt/shop-prod", entries: []string{"storage"}, volumes: []cli.Volume{{Key: "db", Name: "shop-prod_db"}}}
	_, _ = duData(context.Background(), newContainer("export", "shop-prod", "1", remote.TaskExport), d)
	out, err := os.ReadFile(logged)
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	p := door.Policy{Project: "shop", User: "devopsy-shop", Space: "/space", Tree: "/home/devopsy-shop/shop"}
	if _, err := door.Decide(argv, p, nil, doorTestHost{}, doorKnown()); err != nil {
		t.Errorf("%q: %v", argv, err)
	}
}

// TestDoorTakesWhatDevopsyRuns runs devopsy's own util containers against a fake docker and
// asks the door about each: a form that drifts from its command fails here, not in a
// project user's release. Each run the door decides holds --pull never.
//
// Not parallel: it sets PATH (doorFakes).
func TestDoorTakesWhatDevopsyRuns(t *testing.T) {
	_, logged := doorFakes(t)
	// The probe and the clone make their directory before docker: one this test may write as
	// any user, as CI's is no root.
	space := t.TempDir()
	mnt, dev := space+"/mnt/shop-prod", space+"/mnt/shop-dev"
	clone := space + "/clones/shop/abc/1"
	tree := "/home/devopsy-shop/shop/prod"
	// The caller is this user, as devopsy runs as the project user: its tar gives it its own ids.
	p := door.Policy{Project: "shop", User: "devopsy-shop", UID: strconv.Itoa(os.Getuid()), Space: space, Tree: "/home/devopsy-shop/shop", ProjectUIDs: []uint32{10001, 2000000123, 1900000123}}
	m := uids.Map{Env: 2000000123, Project: 1900000123}
	c := newContainer("export", "shop-prod", "1", remote.TaskExport)
	ctx := context.Background()
	d := dataSet{mnt: mnt, entries: []string{"storage"}, volumes: []cli.Volume{{Key: "db", Name: "shop-prod_db"}}}
	inClone := d.in(clone)
	runs := map[string]func(){
		"du":                 func() { _, _ = duData(ctx, c, d) },
		"du of a clone":      func() { _, _ = duData(ctx, c, inClone) },
		"probe":              func() { reflinks(ctx, c, clone, d) },
		"clone":              func() { _ = cloneData(ctx, c, clone, d) },
		"rsync an entry":     func() { _ = rsyncEntry(ctx, c, mnt, dev, "storage", false) },
		"rsync with owners":  func() { _ = rsyncEntry(ctx, c, mnt, dev, "storage", false, "--chown=10001:10001") },
		"rsync a file":       func() { _ = rsyncEntry(ctx, c, mnt, dev, "app.ini", true) },
		"rsync from a clone": func() { _ = rsyncEntry(ctx, c, inClone.mnt, dev, "storage", false) },
		"rsync a volume":     func() { _ = rsyncVolume(ctx, c, "shop-prod_db:/src/db:ro", "shop-dev_db:/dst/db", "db") },
		"rsync mapped":       func() { _ = rsyncEntry(ctx, c, mnt, dev, "storage", false, m.RsyncArgs()...) },
		"rsync a volume mapped": func() {
			_ = rsyncVolume(ctx, c, "shop-prod_db:/src/db:ro", "shop-dev_db:/dst/db", "db", m.RsyncArgs()...)
		},
		"an extraction into a volume": func() {
			_ = extractEntry(ctx, tree, mnt, tree+"/t.tar", tarEntry{name: "db", forms: []string{"db"}, volume: &cli.Volume{Key: "db", Name: "shop-prod_db"}}, c, "shop-prod", nil, m)
		},
		"a tar": func() {
			_ = tarInto(ctx, c, tree+"/snapshots/1.tmp/data/mnt.storage.tar.zst", []string{"-v", clone + ":/m:ro"}, "mnt", "storage")
		},
		"a share": func() { shareInto(ctx, c, mnt+"/storage", dev+"/storage", "mnt/storage", m) },
		"a volume's root": func() {
			_ = ownVolume(ctx, c, cli.OwnedVolume{Volume: cli.Volume{Key: "db", Name: "shop-prod_db"}, Owner: "10001:10001"})
		},
		"volumes' roots read": func() {
			_, _ = readVolumeRoots(ctx, map[string]cli.OwnedVolume{"db": {Volume: cli.Volume{Key: "db", Name: "shop-prod_db"}, Owner: "10001:10001"}}, c)
		},
	}
	// An entry's extraction maps its owners on Linux, where the door runs: locally on macOS
	// it maps none (extractEntry).
	if runtime.GOOS != "darwin" {
		runs["an extraction"] = func() {
			_ = extractEntry(ctx, tree, mnt, tree+"/t.tar", tarEntry{name: "storage", forms: []string{"storage"}}, c, "shop-prod", nil, m)
		}
	}
	for name, run := range runs {
		if err := os.Remove(logged); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		run()
		out, err := os.ReadFile(logged)
		if err != nil {
			t.Errorf("%s: docker was not called: %v", name, err)
			continue
		}
		argv := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
		// One script's lines come out of the fake as lines: put them back, by its shape.
		argv = doorJoinScript(argv)
		a, err := door.Decide(argv, p, nil, doorTestHost{}, doorKnown())
		if err != nil {
			t.Errorf("%s: %v\n%q", name, err, argv)
			continue
		}
		// What the door runs for it never has Docker's daemon pull the util image.
		if run := strings.Join(a.Docker, " "); strings.HasPrefix(run, "run ") && !strings.Contains(run, " --pull never ") {
			t.Errorf("%s: the door's run may pull: %s", name, run)
		}
	}
}

// doorJoinScript undoes the fake's one word per line for what follows `sh -c`: the word
// after -c runs to the next word the scripts' arguments start with, which is `sh`.
func doorJoinScript(argv []string) []string {
	for i, a := range argv {
		if a != "-c" || i == 0 || argv[i-1] != "sh" {
			continue
		}
		end := len(argv)
		for j := i + 2; j < len(argv); j++ {
			if argv[j] == "sh" {
				end = j
				break
			}
		}
		text := strings.Join(argv[i+1:end], "\n")
		// A clone's ends with a newline the fake's lines lost.
		if strings.HasPrefix(text, "set -e\nmkdir -p /c/mnt") {
			text += "\n"
		}
		return append(append(slices.Clone(argv[:i+1]), text), argv[end:]...)
	}
	return argv
}

// A start runs the image the door read: what a service builds is checked by the door's
// name for it (images.DoorBuild, as the render gives it, whatever image: says), and an
// image still missing refuses, since compose would pull or build one past the check. A label
// under a vet's prefix refuses its image too, in any case, as what reads such labels takes
// them: it would reach its containers past the vet, which judges the file's labels alone.
func TestDoorChecksWhatAServiceBuilds(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	docker := filepath.Join(bin, "docker")
	if err := testfile.Write(docker, []byte(`#!/bin/sh
case "$*" in
  *" devopsy.internal/shop-prod/labelled") echo '{"Id":"sha256:l","Config":{"Labels":{"devopsy.role":"proxy"}}}' ;;
  *" devopsy.internal/shop-prod/plain") echo '{"Id":"sha256:p","Config":{"Labels":{"a":"b"}}}' ;;
  *" devopsy.internal/shop-prod/routed") echo '{"Id":"sha256:r","Config":{"Labels":{"Vetted.Enable":"true"}}}' ;;
  *) exit 1 ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}
	side := &doorSide{policy: door.Policy{Project: "shop", UIDs: []uint32{10001}}, claims: testShopClaims, real: docker}
	file := func(svc string) map[string]any {
		f := map[string]any{"name": "shop-prod", "services": map[string]any{svc: map[string]any{
			"build": map[string]any{"context": "."}, "image": "mongo-express:1", "user": "10001:10001", "security_opt": []any{"no-new-privileges"}}}}
		if errs := door.CheckFile(f, side.policy, "up", side); len(errs) != 0 {
			t.Fatal(errs)
		}
		return f
	}
	plain := file("plain")
	if err := side.checkImageLabels(plain, ""); err != nil {
		t.Errorf("the control: a plain build is refused: %v", err)
	}
	// Compose gets it by the id checked.
	if got := plain["services"].(map[string]any)["plain"].(map[string]any)["image"]; got != "sha256:p" {
		t.Errorf("a build goes to compose as %v", got)
	}
	if err := side.checkImageLabels(file("labelled"), ""); err == nil || !strings.Contains(err.Error(), "devopsy.role") {
		t.Errorf("a build labelled devopsy.role: %v", err)
	}
	if err := side.checkImageLabels(file("missing"), ""); err == nil || !strings.Contains(err.Error(), "not here") {
		t.Errorf("a build not here yet, which compose would build at the start: %v", err)
	}
	if err := side.checkImageLabels(file("routed"), ""); err != nil {
		t.Errorf("the control: with no vet, a label of no one's is refused: %v", err)
	}
	side.vets = []remote.DoorVet{{Prefix: "vetted.", Command: "/bin/false"}}
	if err := side.checkImageLabels(file("routed"), ""); err == nil || !strings.Contains(err.Error(), "Vetted.Enable") {
		t.Errorf("a build labelled under a vet's prefix, in another case: %v", err)
	}
	if err := side.checkImageLabels(file("plain"), ""); err != nil {
		t.Errorf("the control: with a vet, a plain build is refused: %v", err)
	}
}

// A project's build runs on a BuildKit of its own, made at its first build, on a network of
// its own: on one builder, a cache mount is shared across projects by its id, and on Docker's
// default bridge, which every container shares, one project's build step reaches another's
// builder and what its build listens on. A builder made otherwise (on that bridge) is made
// again. Its settings stay the door's.
func TestDoorBuildsOnTheProjectsBuilder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log := filepath.Join(dir, "docker.log")
	made := filepath.Join(dir, "made")
	docker := filepath.Join(dir, "docker")
	if err := testfile.Write(docker, []byte(`#!/bin/sh
echo "$BUILDX_CONFIG $*" >>`+log+`
case "$*" in
  "buildx inspect "*) [ -e `+made+` ] && cat `+made+` ;;
  "buildx create "*) echo "Driver Options: network=\"${8#network=}\"" >`+made+` ;;
  "buildx rm "*) rm -f `+made+` ;;
  "network inspect "*) [ -e `+dir+`/net ] ;;
  "network create "*) touch `+dir+`/net ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}
	space := filepath.Join(dir, "space", "buildx")
	// build is a request's build, its own door process.
	build := func() string {
		t.Helper()
		os.Remove(log)
		side := &doorSide{policy: door.Policy{Project: "shop", Space: filepath.Join(dir, "space")}, real: docker}
		env, err := side.builder()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(env, "BUILDX_BUILDER=devopsy-shop") || !slices.Contains(env, "BUILDX_CONFIG="+space) {
			t.Errorf("a build's environment: %q", env)
		}
		// One request reads it once, for every claim and build it makes.
		got, _ := os.ReadFile(log)
		if again, err := side.builder(); err != nil || !slices.Equal(again, env) || readString(log) != string(got) {
			t.Errorf("the builder read again in one request (%v):\n%s", err, readString(log))
		}
		return string(got)
	}
	create := space + " buildx create --name devopsy-shop --driver docker-container --driver-opt network=devopsy-shop-build\n"
	if got := build(); got != space+" buildx inspect devopsy-shop\n"+space+" network inspect devopsy-shop-build\n"+space+" network create devopsy-shop-build\n"+create {
		t.Errorf("the first build asked:\n%s", got)
	}
	if got := build(); got != space+" buildx inspect devopsy-shop\n" {
		t.Errorf("a build on the builder made asked:\n%s", got)
	}
	// One made on the default bridge, before builders had networks of their own.
	write(t, made, "Driver Options: \n", 0o644)
	if got := build(); got != space+" buildx inspect devopsy-shop\n"+space+" buildx rm devopsy-shop\n"+space+" network inspect devopsy-shop-build\n"+create {
		t.Errorf("a builder on the default bridge, asked:\n%s", got)
	}
}

// A root copy's modes narrow what the project user may write in an entry it wrote (an ACL's
// mask), so the door gives it back there, as root, where it was lost; with no entry, nothing.
func TestDoorRegrantsWhatACopyWrote(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log := filepath.Join(dir, "docker.log")
	docker := filepath.Join(dir, "docker")
	// The util image is there, built today.
	fake := "#!/bin/sh\ncase \"$*\" in \"image inspect \"*) echo \"sha256:util $(date -u +%Y-%m-%dT%H:%M:%SZ)\"; exit ;; esac\necho \"$*\" >>" + log + "\n"
	if err := testfile.Write(docker, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", UID: "10002", Space: filepath.Join(dir, "space")}, real: docker}
	if err := side.regrant(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("a grant with no entry")
	}
	data, ini := filepath.Join(side.policy.Mnt("shop-prod"), "data"), filepath.Join(side.policy.Mnt("shop-prod"), "app.ini")
	if err := side.regrant([]string{data, ini}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(log)
	want := "run --rm --network none --pull never --cap-drop MKNOD --security-opt no-new-privileges --read-only --tmpfs /tmp:rw,noexec,nosuid,nodev,size=1m -v " + data + ":/e/0 -v " + ini + ":/e/1 " + remote.UtilImage +
		" find /e/0 /e/1 -xdev ( -type d -o -type f ) ! -perm -g=w -exec setfacl -m u:10002:rwX {} +\n"
	if string(got) != want {
		t.Errorf("docker was asked:\n%s\nwant:\n%s", got, want)
	}
	// A detached copy's entries are read from its mounts when it is removed (Removes): what
	// it wrote in an entry, never a whole mnt, the deploy user's, nor a path elsewhere.
	mounts := filepath.Join(dir, "mounts")
	if err := testfile.Write(mounts, []byte("#!/bin/sh\n[ \"$1 $6\" = 'inspect daemon' ] || exit 1\nprintf '%s\\n' "+data+" "+filepath.Dir(data)+" /etc\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	side.real = mounts
	if got := side.writtenEntries([]string{"daemon", "gone"}); !slices.Equal(got, []string{data}) {
		t.Errorf("a removed container's written entries: %q, want %q", got, data)
	}
}

// The door's space on a mount that takes setuid files and devices lets a container's setuid
// file cross projects and a crafted tar reach root, so the door does nothing there. A test's
// temporary directory is on no such mount, mostly.
func TestDoorRefusesASpaceWithSetuid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if nosuid, nodev, _ := mountGuards(dir); nosuid && nodev {
		t.Skip("this machine's temporary directories are on such a mount")
	}
	err := spaceGuarded(dir)
	if err == nil || !strings.Contains(err.Error(), "nosuid and nodev") {
		t.Errorf("a space on a plain mount: %v", err)
	}
	if err := spaceGuarded(filepath.Join(t.TempDir(), "none")); err == nil {
		t.Error("a space that is not there")
	}
}

// sshd lets a project user in by what --authorized-keys prints: only lines that are keys, none
// for a user with no keys, and nothing at all for a name that is no project user's.
func TestAuthorizedKeysPrintsOnlyKeys(t *testing.T) {
	t.Parallel()
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g op@laptop"
	file := filepath.Join(t.TempDir(), "devopsy-shop")
	write(t, file, "# a comment\nnot a key at all\n"+key+"\n", 0o600)
	var out strings.Builder
	if code := printAuthorizedKeys(&out, file); code != 0 || out.String() != key+"\n" {
		t.Errorf("printed (%d) %q", code, out.String())
	}
	out.Reset()
	if code := printAuthorizedKeys(&out, file+"-none"); code != 0 || out.Len() != 0 {
		t.Errorf("no keys (%d) %q", code, out.String())
	}
	for _, args := range [][]string{nil, {"root"}, {"devopsy-"}, {"devopsy-Shop"}, {"devopsy-shop", "x"}, {"devopsy-../x"}} {
		if code := runAuthorizedKeys(args); code == 0 {
			t.Errorf("%q: answered", args)
		}
	}
}

// Compose names are flat, so a project whose name extends another's, or that another's
// extends, would share names with it: whichever came first holds the other's networks,
// volumes and containers. The same name is the project itself, as a release root's project
// moving to its own user is.
func TestProjectNameClash(t *testing.T) {
	t.Parallel()
	others := []string{"shop", "blog-admin", "traefik"}
	// devopsy's own names are under devopsy-: the util image's old names (devopsy-util, root with host
	// mounts, which a project named devopsy could build and remove), the project users, the
	// group, the builders. Nor is a project whose user would bear a group of devopsy's name
	// (devopsy-projects, devopsy-door): useradd gives a user a group of its own name.
	for project, clash := range map[string]bool{
		"shop": false, "shopadmin": false, "admin-shop": false, "blog": true, "shop-admin": true, "traefik-x": true, "blog-admin": false, "blog-admin-eu": true, "devopsy": true, "devopsy-util": true,
		"projects": true, "door": true,
	} {
		if why := projectNameClash(project, others); (why != "") != clash {
			t.Errorf("%s: %q", project, why)
		}
	}
}

// No compose command runs while a container the door did not make for this user is under the
// project's names, the deploy user's or another project user's: compose acts on whatever its
// project's label finds, and such a container runs as root with any mount.
func TestDoorRefusesWhatItDidNotMake(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rows := filepath.Join(dir, "rows")
	docker := filepath.Join(dir, "docker")
	if err := testfile.Write(docker, []byte("#!/bin/sh\n[ \"$1 $2\" = 'ps -a' ] || exit 1\ncat "+rows+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Others: []string{"shop-two"}}, real: docker}
	mine := `["aaa","shop-prod","","devopsy-shop"]` + "\n" + `["bbb","","shop-dev","devopsy-shop"]` + "\n" +
		`["ccc","other-prod","",""]` + "\n" + `["ddd","shop-two-prod","",""]` + "\n"
	for _, c := range []struct {
		name, rows, want string
	}{
		{"the door's own, and others' under other names", mine, ""},
		{"the deploy user's", mine + `["eee","shop-prod","",""]` + "\n", "shop-prod"},
		{"another project user's", mine + `["fff","","shop-stage","devopsy-other"]` + "\n", "shop-stage"},
		{"a label with spaces", mine + `["ggg","shop-prod","","devopsy-shop x"]` + "\n", "shop-prod"},
	} {
		if err := testfile.Write(rows, []byte(c.rows), 0o644); err != nil {
			t.Fatal(err)
		}
		err := side.foreign()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: %v, want it named", c.name, err)
		}
	}
	if err := (&doorSide{policy: side.policy, real: "/nonexistent"}).foreign(); err == nil {
		t.Error("with no answer from Docker: not refused")
	}
}

// doorImageFake is a docker for the door's image checks: its store is dir/store, one
// image a line, `<ref>|<id>|<digests>|<tags>|<labels json>` (digests and tags separated
// by spaces, a ref with no line not here, so a name and the digest it holds are separate
// lines, which a test moves apart); its registry dir/registry, `<ref>|<digest>` a line,
// what a pull of the ref gets, printed as docker pull prints it (`Digest: ...`), and what
// buildx imagetools inspect answers; a pull of a ref listed in dir/fails fails. A claim's copy
// (imagetools create) is the layout fakeManifest wrote for the ref (spoilCopy spoils one), and
// its load puts the image in the store (see the cases). It logs
// every call in dir/calls, and answers compose with its arguments
// and the file it was given, for a door that execs it (doorComposeHelper). Image
// inspects answer the two formats the door uses today and `{{json .}}`, as
// images.CLI.Inspect reads it, and a listing of images says dir/names. Its containers are dir/containers, one a line,
// `<id>|<labels json>|<mounts json>`, which a listing by label filters gives whatever its
// filters (whose each one is, its inspect says) and a container's inspect answers, unless
// dir/inspectfails is there; a listing of every container gives none.
func doorImageFake(t *testing.T, dir string) string {
	t.Helper()
	in := func(name string) string { return filepath.Join(dir, name) }
	docker := in("docker")
	write(t, docker, `#!/bin/sh
all="$*"
echo "$all" >>`+in("calls")+`
for a; do ref=$a; done
# Docker resolves repo:tag@digest by the digest alone, as repo@digest.
entry() {
  r=$1; case $r in *@*) n=${r%@*}; case ${n##*/} in *:*) r=${n%:*}@${r#*@} ;; esac ;; esac
  awk -F'|' -v r="$1" -v s="$r" '$1==r || $1==s' `+in("store")+` 2>/dev/null | head -1
}
field() { printf '%s\n' "$1" | cut -d'|' -f"$2"; }
tojson() { printf '['; first=1; for x in $1; do [ $first = 1 ] || printf ','; printf '"%s"' "$x"; first=0; done; printf ']'; }
registry() { awk -F'|' -v r="$1" '$1==r{print $2}' `+in("registry")+` 2>/dev/null; }
case "$all" in
  'pull -q '*|'pull '*)
    grep -qxF -- "$ref" `+in("fails")+` 2>/dev/null && exit 1
    case "$all" in
      'pull -q '*) echo "$ref" ;;
      *) echo "$ref: Pulling from a registry"; d=$(registry "$ref"); case $ref in *@*) d=${ref#*@} ;; esac; [ -n "$d" ] && echo "Digest: $d"; echo "Status: Downloaded newer image for $ref" ;;
    esac ;;
  'buildx imagetools inspect --raw '*)
    # What a registry serves for a ref (fakeManifest), nothing for one it does not know.
    f=`+in("raw")+`/$(printf '%s' "$ref" | tr '/:@' '___'); [ -f "$f" ] || exit 1; cat "$f" ;;
  'version --format '*) echo "linux amd64" ;;
  'info --format '*) echo '[[driver-type io.containerd.snapshotter.v1]]' ;;
  'buildx version') echo "github.com/docker/buildx v0.38.2 e25e2b7a4b6a" ;;
  'buildx imagetools create '*)
    # A copy of what the registry serves for the ref (fakeManifest's layout), none for one it
    # does not, or one listed in dir/uncopied; with dir/slowcopy, it says dir/copying once
    # the copy is there and then hangs on a child of its own (as the docker CLI runs its
    # buildx plugin), its pid in dir/child, for a signal to cut it.
    dest=; while [ $# -gt 0 ]; do [ "$1" = --tag ] && dest=${2#oci-layout://} && dest=${dest%:claim}; shift; done
    grep -qxF -- "$ref" `+in("uncopied")+` 2>/dev/null && exit 1
    l=`+in("layouts")+`/$(printf '%s' "$ref" | tr '/:@' '___'); [ -d "$l" ] || exit 1
    mkdir -p "$dest" && cp -R "$l/." "$dest" || exit 1
    [ -f `+in("slowcopy")+` ] && { sleep 30 & echo $! >`+in("child")+`; touch `+in("copying")+`; wait; }
    true ;;
  'load')
    # Docker's load: the name its index gives, said as Docker says it (familiar), and the image
    # put first in the store under it, its id the manifest's digest, its labels those of an
    # image of that digest the store has (the same config, by content); one listed in
    # dir/unpacked says Docker's unpacking error after, and exits 0, as Docker does; with
    # dir/loadsays, it says that file instead, and with dir/loadid the image goes by that id.
    d=$(mktemp -d) && tar -xf - -C "$d"
    name=$(sed -n 's/.*"io.containerd.image.name":"\([^"]*\)".*/\1/p' "$d/index.json")
    fam=${name#docker.io/library/}; fam=${fam#docker.io/}
    echo "load $name" >>`+in("calls")+`
    if [ -f `+in("loadsays")+` ]; then cat `+in("loadsays")+`; else echo "Loaded image: $fam"; fi
    if grep -qxF -- "$fam" `+in("unpacked")+` 2>/dev/null; then echo "Error unpacking image $fam: content digest not found"; exit 0; fi
    ls=$(field "$(entry "$fam")" 5); [ -n "$ls" ] || ls='{}'
    id=${fam#*@}; [ -f `+in("loadid")+` ] && id=$(cat `+in("loadid")+`)
    { echo "$fam|$id|$fam||$ls"; cat `+in("store")+` 2>/dev/null || true; } >"$d/store" && cat "$d/store" >`+in("store")+` ;;
  'compose version --short') echo "${FAKE_COMPOSE_VERSION:-2.40.0}" | grep -v '^$' || echo 2.40.0 ;;
  'tag '*|'image rm '*) ;;
  'image ls '*) cat `+in("names")+` 2>/dev/null ;;
  'buildx inspect '*) ;;
  'buildx build '*) cat >/dev/null ;;
  'ps -a '*) ;;
  'ps -aq --no-trunc --filter '*) cut -d'|' -f1 `+in("containers")+` 2>/dev/null ;;
  'inspect --type container '*)
    [ -f `+in("inspectfails")+` ] && exit 1
    e=$(awk -F'|' -v r="$ref" '$1==r' `+in("containers")+` 2>/dev/null | head -1); [ -n "$e" ] || exit 1
    case "$all" in
      *'{{json .Mounts}}'*) field "$e" 3 ;;
      *) printf '%s %s\n' "$(field "$e" 1)" "$(field "$e" 2)" ;;
    esac ;;
  'compose '*' config --format json')
    # Compose's reading of profiles, of the file on stdin (kept in dir/configread): a service
    # with profiles is active only under one of those asked.
    cat >`+in("configread")+`
    perl -MJSON::PP -e 'my @p; for (my $i = 0; $i < @ARGV; $i++) { push @p, $ARGV[$i+1] if $ARGV[$i] eq "--profile" }
      local $/; my $f = decode_json(<STDIN>); my %asked = map { $_ => 1 } @p;
      for my $s (keys %{$f->{services}}) { my $pr = $f->{services}{$s}{profiles} or next; delete $f->{services}{$s} if @$pr && !grep { $asked{$_} } @$pr }
      print encode_json($f)' -- "$@" <`+in("configread")+` ;;
  'compose '*)
    f=; while [ $# -gt 0 ]; do [ "$1" = -f ] && f=$2; shift; done
    echo "ARGS: $all"
    echo "CONFIG: $DOCKER_CONFIG"
    echo "DIR: $(pwd)"
    # The file's descriptor was written and then handed over at its end: from its start.
    perl -e 'open(F, "<&=", $ARGV[0]) or die; seek(F, 0, 0); print while <F>;' "${f#/dev/fd/}" ;;
  'image inspect '*)
    e=$(entry "$ref"); [ -n "$e" ] || { echo "Error: No such image: $ref" >&2; exit 1; }
    id=$(field "$e" 2); ds=$(field "$e" 3); ts=$(field "$e" 4); ls=$(field "$e" 5); [ -n "$ls" ] || ls='{}'
    case "$all" in
      *'{{.Id}}{{range .RepoTags}} {{.}}{{end}}'*) printf '%s %s\n' "$id" "$ts" ;;
      *'{{json .Config.Labels}}'*) echo "$ls" ;;
      *) printf '{"Id":"%s","RepoTags":%s,"RepoDigests":%s,"Config":{"Labels":%s}}\n' "$id" "$(tojson "$ts")" "$(tojson "$ds")" "$ls" ;;
    esac ;;
esac
`, 0o755)
	return docker
}

// fakeManifest has the fake registry of doorImageFake serve body for each of refs and for the
// first one's repository at body's digest, which it returns: a manifest, or an index. For a
// manifest of plainManifest's, it also writes the OCI layout imagetools would copy of it, by
// that digest: the manifest and its config, each under its hash.
func fakeManifest(t *testing.T, dir, body string, refs ...string) string {
	t.Helper()
	d := digestOf([]byte(body))
	key := strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace
	for _, r := range append(refs, images.RepoOf(refs[0])+"@"+d) {
		write(t, filepath.Join(dir, "raw", key(r)), body, 0o644)
	}
	var m struct {
		Annotations map[string]string `json:"annotations"`
	}
	if json.Unmarshal([]byte(body), &m) == nil && m.Annotations["x-config"] != "" {
		layout := filepath.Join(dir, "layouts", key(images.RepoOf(refs[0])+"@"+d))
		blob := func(data string) {
			write(t, filepath.Join(layout, "blobs", "sha256", strings.TrimPrefix(digestOf([]byte(data)), "sha256:")), data, 0o644)
		}
		blob(body)
		blob(m.Annotations["x-config"])
		write(t, filepath.Join(layout, "index.json"), `{"schemaVersion":2,"manifests":[{"digest":"`+d+`","annotations":{"org.opencontainers.image.ref.name":"claim"}}]}`, 0o644)
		write(t, filepath.Join(layout, "oci-layout"), `{"imageLayoutVersion":"1.0.0"}`, 0o644)
	}
	return d
}

// spoilCopy spoils the copy fakeManifest wrote of ref (<repo>@<digest>): its config, config,
// replaced by other bytes under the same name, as a registry serving other bytes than it names.
// It gives what puts it back.
func spoilCopy(t *testing.T, dir, ref, config string) func() {
	t.Helper()
	path := filepath.Join(dir, "layouts", strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(ref), "blobs", "sha256", strings.TrimPrefix(digestOf([]byte(config)), "sha256:"))
	write(t, path, strings.Repeat("x", len(config)), 0o644)
	return func() { write(t, path, config, 0o644) }
}

// plainManifest is an image manifest's body, its config named for name: a config blob of its
// own, which its annotation carries for fakeManifest's layout.
func plainManifest(name string) string {
	config := `{"name":"` + name + `"}`
	return `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"` + digestOf([]byte(config)) + `","size":` + strconv.Itoa(len(config)) + `},"layers":[],"annotations":{"x-config":` + strconv.Quote(config) + `}}`
}

// A start runs an image of this project's by name (a build's, a hold of its own, devopsy's
// util image: names only this project or the deploy user writes, under this project's
// lock), or one it claims first: copied by the door's client with its own registry logins and
// loaded, the id then recorded in its space, so the next start asks the registry nothing.
// One already on disk that another project claimed (a private one with that project's
// login), or a name without a host the deploy user built (blog-prod-web, a registry's name to
// the door), or an id alone, is refused when this project's claim of it fails, and another's
// build name or hold is never claimed at all.
//
// A claimed image goes to compose pinned, `<image as given>@<digest>`, the digest the
// image has in the service's repository (as the devopsy.digest label chooses it, the tag
// kept so docker ps shows it, a reference by digest kept as given): the name is anyone's
// on the server to move between this check and compose's create (another project's pull of
// a tag its registry moved, the deploy user's build of a Docker Hub name), and Docker
// resolves repo:tag@digest by the digest. The digest, the labels read and the id recorded
// are the image the project's claim loaded, read by digest or id, never by the name again
// after the load; an image with no digest in its repository never goes by its plain name.
func TestDoorStartsItsOwnOrClaimedImages(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := doorImageFake(t, dir)
	store := func(lines ...string) { write(t, filepath.Join(dir, "store"), strings.Join(lines, "\n")+"\n", 0o644) }
	private := "registry.example.com/blog/private"
	labelled := "registry.example.com/blog/labelled"
	// What the registries serve: a manifest for each, by its tag and its digest.
	p1 := fakeManifest(t, dir, plainManifest("private"), private+":1")
	l1 := fakeManifest(t, dir, plainManifest("labelled"), labelled+":1")
	bb := fakeManifest(t, dir, plainManifest("busybox"), "busybox:1.37")
	store(
		// Pulled under two repositories, a mirror's digest first: the pin is this one's.
		private+":1|sha256:p1|other.example.com/mirror@sha256:zz "+private+"@"+p1+"||{}",
		private+"@"+p1+"|sha256:p1|"+private+"@"+p1+"||{}",
		labelled+":1|sha256:l1|"+labelled+"@"+l1+"||{\"devopsy.role\":\"proxy\"}",
		labelled+"@"+l1+"|sha256:l1|"+labelled+"@"+l1+"||{\"devopsy.role\":\"proxy\"}",
		"blog-prod-web|sha256:b1||blog-prod-web blog-prod-web:latest|{}",
		"blog-prod-web:latest|sha256:b1||blog-prod-web blog-prod-web:latest|{}",
		"devopsy.internal/blog-prod/web|sha256:b2||devopsy.internal/blog-prod/web|{}",
		"registry.example.com/app:2|sha256:a2|registry.example.com/app@sha256:a2||{}",
		"registry.example.com/app:devopsy-blog-prod-20260101000000|sha256:a2|registry.example.com/app@sha256:a2||{}",
		"sha256:0123|sha256:0123|||{}",
		"devopsy.internal/shop-prod/web|sha256:s1||devopsy.internal/shop-prod/web|{}",
		"busybox:devopsy-shop-prod-20260101000000|sha256:bb|busybox@"+bb+"|busybox:devopsy-shop-prod-20260101000000|{}",
		"busybox@"+bb+"|sha256:bb|busybox@"+bb+"||{}",
		remote.UtilImage+"|sha256:u4|||{}",
		"loaded:1|sha256:n1||loaded:1|{}",
	)
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: dir, Others: []string{"blog"}}, real: fake, imagesRoot: dir}
	calls := filepath.Join(dir, "calls")
	// start checks a file naming image, and says what compose then gets as the image.
	start := func(image string) (string, error) {
		t.Helper()
		os.Remove(calls)
		file := map[string]any{"name": "shop-prod", "services": map[string]any{"web": map[string]any{"image": image}}}
		err := side.checkImageLabels(file, filepath.Join(dir, "none.json"))
		got, _ := file["services"].(map[string]any)["web"].(map[string]any)["image"].(string)
		return got, err
	}
	// pulled says the start claimed an image: the door's copy of it, which only a claim makes.
	pulled := func() bool { return strings.Contains(readString(calls), "imagetools create ") }
	pin := private + ":1@" + p1
	if got, err := start(private + ":1"); err != nil || !pulled() || got != pin {
		t.Errorf("claimed by its pull (%v), pulled %v, goes to compose as %q, want %q:\n%s", err, pulled(), got, pin, readString(calls))
	}
	// Docker's containerd store names an image by its manifest's digest.
	if got := readString(side.policy.ImagesFile()); got != p1+"\n" {
		t.Errorf("the record: %q", got)
	}
	// The claim copied the manifest's digest, never the name, as the registry serves it, loaded
	// it under its canonical name by that digest,
	// Docker's daemon asking no registry (no pull) and no builder building anything, and named
	// it as asked.
	c := readString(calls)
	if strings.Contains(c, "\npull ") {
		t.Errorf("a claim had Docker's daemon pull:\n%s", c)
	}
	if strings.Contains(c, "buildx build ") {
		t.Errorf("a claim built on the project's builder:\n%s", c)
	}
	for _, want := range []string{"imagetools create --prefer-index=false --tag oci-layout://", "\nload " + private + "@" + p1 + "\n", "\ntag " + private + "@" + p1 + " " + private + ":1\n"} {
		if !strings.Contains(c, want) {
			t.Errorf("a claim with no %q:\n%s", want, c)
		}
	}
	side.policy.Images = claimedImages(side.policy.ImagesFile())
	if got, err := start(private + ":1"); err != nil || pulled() || got != pin {
		t.Errorf("claimed already (%v), copied again %v, goes to compose as %q:\n%s", err, pulled(), got, readString(calls))
	}
	// A copy its registry spoiled (a blob not its hash): refused before anything loads,
	// nothing recorded.
	restore := spoilCopy(t, dir, labelled+"@"+l1, `{"name":"labelled"}`)
	if _, err := start(labelled + ":1"); err == nil || !strings.Contains(err.Error(), "is not its hash") || strings.Contains(readString(calls), "\nload ") {
		t.Errorf("a copy not its hash: %v\n%s", err, readString(calls))
	}
	if got := readString(side.policy.ImagesFile()); strings.Contains(got, l1) {
		t.Errorf("a copy not its hash recorded: %q", got)
	}
	restore()
	if _, err := start(labelled + ":1"); err == nil || !strings.Contains(err.Error(), "devopsy.role") {
		t.Errorf("a claimed image labelled devopsy.role: %v", err)
	}
	for _, image := range []string{"registry.example.com/app:2", "sha256:0123", "blog-prod-web", "blog-prod-web:latest"} {
		if _, err := start(image); err == nil || !strings.Contains(err.Error(), "its registry logins do not pull it") {
			t.Errorf("%s, another's, ran: %v", image, err)
		}
	}
	// A build blog runs is under devopsy's name for it, which only the door writes for blog:
	// never pulled over.
	if _, err := start("devopsy.internal/blog-prod/web"); err == nil || pulled() || !strings.Contains(err.Error(), "a build's name") {
		t.Errorf("blog's build ran or pulled (%v):\n%s", err, readString(calls))
	}
	if _, err := start("registry.example.com/app:devopsy-blog-prod-20260101000000"); err == nil || pulled() {
		t.Errorf("another's hold pulled or ran (%v):\n%s", err, readString(calls))
	}
	// Its own built image, its own hold and devopsy's util image need no claim, and go by the
	// id read: the project's own build or tag in another environment moves the name before
	// compose's create, and a one-off container's run holds nothing for as long as it runs.
	for image, id := range map[string]string{"devopsy.internal/shop-prod/web": "sha256:s1", "busybox:devopsy-shop-prod-20260101000000": "sha256:bb", remote.UtilImage: "sha256:u4"} {
		if got, err := start(image); err != nil || pulled() || got != id {
			t.Errorf("its own %s (%v), pulled %v, goes to compose as %q:\n%s", image, err, pulled(), got, readString(calls))
		}
	}
	// A reference by digest is a pin already: claimed, and kept as given.
	if got, err := start("busybox@" + bb); err != nil || !pulled() || got != "busybox@"+bb {
		t.Errorf("a reference by digest (%v), pulled %v, goes to compose as %q:\n%s", err, pulled(), got, readString(calls))
	}
	// An image the project claimed that has no digest in its repository (loaded here, named
	// by hand) and that its registry does not serve: refused, or pinned by what it has (its
	// id), never started by its plain name, which the check did not pin.
	side.policy.Images = append(side.policy.Images, "sha256:n1")
	if got, err := start("loaded:1"); err == nil && got == "loaded:1" {
		t.Errorf("an image with no digest in its repository goes to compose by its plain name")
	}
	// An index: the claim takes the entry for the daemon's platform (linux amd64 here),
	// copied, loaded and pinned by that manifest's digest, never the index's, which imagetools'
	// copy and Docker's load could each resolve to another entry.
	amd := fakeManifest(t, dir, plainManifest("multi-amd64"), "registry.example.com/multi:amd64")
	arm := fakeManifest(t, dir, plainManifest("multi-arm64"), "registry.example.com/multi:arm64")
	fakeManifest(t, dir, `{"schemaVersion":2,"manifests":[{"digest":"`+arm+`","platform":{"os":"linux","architecture":"arm64","variant":"v8"}},{"digest":"`+amd+`","platform":{"os":"linux","architecture":"amd64"}}]}`, "registry.example.com/multi:1")
	store("registry.example.com/multi@" + amd + "|sha256:ma|registry.example.com/multi@" + amd + "||{}")
	if got, err := start("registry.example.com/multi:1"); err != nil || got != "registry.example.com/multi:1@"+amd || !strings.Contains(readString(calls), " registry.example.com/multi@"+amd+"\n") {
		t.Errorf("an index (%v) goes to compose as %q, want its amd64 entry:\n%s", err, got, readString(calls))
	}
	// One whose entry for the platform is itself an index, or that has none, is refused.
	nested := fakeManifest(t, dir, `{"schemaVersion":2,"manifests":[{"digest":"`+amd+`","platform":{"os":"linux","architecture":"amd64"}}]}`, "registry.example.com/nested:inner")
	fakeManifest(t, dir, `{"schemaVersion":2,"manifests":[{"digest":"`+nested+`","platform":{"os":"linux","architecture":"amd64"}}]}`, "registry.example.com/nested:1")
	fakeManifest(t, dir, `{"schemaVersion":2,"manifests":[{"digest":"`+arm+`","platform":{"os":"linux","architecture":"arm64"}}]}`, "registry.example.com/armonly:1")
	for image, why := range map[string]string{"registry.example.com/nested:1": "no image manifest", "registry.example.com/armonly:1": "no image for linux/amd64"} {
		if _, err := start(image); err == nil || !strings.Contains(err.Error(), why) || pulled() {
			t.Errorf("%s (%v), pulled %v:\n%s", image, err, pulled(), readString(calls))
		}
	}
	// The swap: right after the claim's pull, something else lands on the name (the deploy
	// user's own build of mongo-express:1, or another pull of a tag the registry moved),
	// with a registry digest of its own, as Docker's containerd store gives built images.
	// The pull went by the registry's digest; that is what the pin and the record must be.
	hh := fakeManifest(t, dir, plainManifest("mongo-express"), "mongo-express:1")
	store(
		"mongo-express:1|sha256:mmm|mongo-express@sha256:mmm|mongo-express:1|{}",
		"mongo-express@"+hh+"|sha256:hhh|mongo-express@"+hh+"||{}",
	)
	if got, err := start("mongo-express:1"); err != nil || !pulled() || got != "mongo-express:1@"+hh {
		t.Errorf("a name moved right after its pull (%v), pulled %v, goes to compose as %q, want the registry's digest:\n%s", err, pulled(), got, readString(calls))
	}
	if got := readString(side.policy.ImagesFile()); !strings.Contains(got, hh) || strings.Contains(got, "sha256:mmm") {
		t.Errorf("the record after the swap holds the name's image, not the pull's: %q", got)
	}
}

// What compose receives for a verb that creates a container (up, create, run) is the
// checked file with its pins and pull_policy never on every service, marshalled after the
// image check: marshalled before it, the file carries the plain names the check pinned in its
// map alone, and an image gone after the claim would be Docker's daemon's to pull. Its
// DOCKER_CONFIG is an empty directory of the door's,
// closed to writes, for any verb that needs no logins: unset, compose reads the deploy user's
// own, its logins, helpers and plugins. A day-old work directory goes, as with every other
// request. In a helper process (doorComposeHelper), since the door
// execs compose, here the fake docker, which prints the file it was given.
func TestDoorComposeHandsThePinnedFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	doorImageFake(t, dir)
	// The door reads the project's tree as its user, through sudo: a path is itself.
	write(t, filepath.Join(dir, "sudo"), "#!/bin/sh\nshift 5\n[ \"$1\" = real ] || exit 1\nprintf '%s\\n' \"$2\"\n", 0o755)
	private := "registry.example.com/blog/private"
	p1 := fakeManifest(t, dir, plainManifest("private"), private+":1")
	write(t, filepath.Join(dir, "store"), private+"@"+p1+"|sha256:p1|"+private+"@"+p1+"||{}\n", 0o644)
	empty := filepath.Join(dir, "space", "docker-empty")
	// What a door killed mid-claim left a day ago goes with the next compose request.
	left := filepath.Join(dir, "space", "work", "shop-1")
	write(t, filepath.Join(left, "layout", "index.json"), "{}", 0o644)
	if err := os.Chtimes(left, time.Now().Add(-25*time.Hour), time.Now().Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, verb := range []string{"up -d", "create", "run --rm web true", "down"} {
		c := exec.Command(os.Args[0], "-test.run", "^$")
		c.Env = append(os.Environ(), "DEVOPSY_TEST_DOOR_COMPOSE="+dir, "DEVOPSY_TEST_DOOR_VERB="+verb, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := c.CombinedOutput()
		if err != nil {
			t.Errorf("%s: the door did not exec compose (%v):\n%s", verb, err, out)
			continue
		}
		var file struct {
			Services map[string]struct {
				Image      string
				PullPolicy string `json:"pull_policy"`
			}
		}
		_, body, ok := strings.Cut(string(out), "\n{")
		if !ok || json.Unmarshal([]byte("{"+body), &file) != nil {
			t.Errorf("%s: compose got no file:\n%s", verb, out)
			continue
		}
		want, policy, config := private+":1@"+p1, "never", empty
		if verb == "down" {
			want, policy = private+":1", ""
		}
		if got := file.Services["web"]; got.Image != want || got.PullPolicy != policy {
			t.Errorf("%s: compose got web's image as %q, pull_policy %q, want %q and %q:\n%s", verb, got.Image, got.PullPolicy, want, policy, out)
		}
		if !strings.Contains(string(out), "\nCONFIG: "+config+"\n") {
			t.Errorf("%s: compose's DOCKER_CONFIG is not %s:\n%s", verb, config, out)
		}
	}
	if _, err := os.Stat(left); err == nil {
		t.Errorf("a compose request left a day-old work directory: %s", left)
	}
	// A down by name, a request of its own, too.
	c := exec.Command(os.Args[0], "-test.run", "^$")
	c.Env = append(os.Environ(), "DEVOPSY_TEST_DOOR_COMPOSE="+dir, "DEVOPSY_TEST_DOOR_DOWN=-v", "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, _ := c.CombinedOutput(); !strings.Contains(string(out), "ARGS: compose -p shop-prod down -v\nCONFIG: "+empty+"\nDIR: /\n") {
		t.Errorf("a down by name: compose's DOCKER_CONFIG is not %s, or it runs elsewhere than /, where no compose file is anyone's:\n%s", empty, out)
	}
	if fi, err := os.Stat(empty); err != nil || fi.Mode().Perm() != 0o555 {
		t.Errorf("the empty config: %v %v", fi, err)
	} else if entries, _ := os.ReadDir(empty); len(entries) > 0 {
		t.Errorf("the empty config holds %v", entries)
	}
}

// A restart or a start says which services' containers bind another copy of a read-only part
// of the release than a start would seal now, and offers a release, which makes them again: each
// container keeps the sealed copy it was made with. The release is read and hashed as a seal
// reads it (the same content is no warning), keeping no copy; only the services the verb names
// are compared, only in containers of the file's compose name; no other verb says it; and a
// compare that fails loses only the warning, never the verb. The release is offered as a
// command by the address the caller's session carries, in words without one or with one that
// is no address, and what the session carries never reaches the refusals' log. In a helper
// process (doorComposeHelper), since the door execs compose, here the fake docker.
func TestDoorWarnsOfACopyARestartKeeps(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	doorImageFake(t, dir)
	write(t, filepath.Join(dir, "sudo"), `#!/bin/sh
shift 5
case "$1" in
  real) printf '%s\n' "$2" ;;
  tar) exec tar -C "$(dirname "$2")" -cf - -- "$(basename "$2")" ;;
  *) exit 1 ;;
esac
`, 0o755)
	code := filepath.Join(dir, "prod", "releases", "1", "code")
	write(t, filepath.Join(code, "config", "app.conf"), "one\n", 0o644)
	svc := `{"user":"10001:10001","security_opt":["no-new-privileges"],"volumes":[{"type":"bind","source":"` +
		filepath.Join(code, "config") + `","target":"/etc/app","read_only":true}]}`
	file := `{"name":"shop-prod","services":{"web":` + svc + `,"worker":` + svc + `}}`
	runWith := func(verb string, session ...string) (string, error) {
		t.Helper()
		c := exec.Command(os.Args[0], "-test.run", "^$")
		c.Env = append(append(os.Environ(), "DEVOPSY_TEST_DOOR_COMPOSE="+dir, "DEVOPSY_TEST_DOOR_VERB="+verb, "DEVOPSY_TEST_DOOR_FILE="+file,
			"DEVOPSY_TEST_DOOR_DIR="+code, "DEVOPSY_TEST_DOOR_LOG="+filepath.Join(dir, "refused.log"), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH")), session...)
		out, err := c.CombinedOutput()
		return string(out), err
	}
	run := func(verb string, session ...string) string {
		t.Helper()
		out, err := runWith(verb, session...)
		if err != nil || !strings.Contains(out, "ARGS: compose ") {
			t.Fatalf("%s: compose did not run (%v):\n%s", verb, err, out)
		}
		return out
	}
	// The copy an up seals, which its containers are made with.
	var got struct {
		Services map[string]struct{ Volumes []struct{ Source string } }
	}
	out := run("up -d")
	if _, body, ok := strings.Cut(out, "\n{"); !ok || json.Unmarshal([]byte("{"+body), &got) != nil || len(got.Services["web"].Volumes) != 1 {
		t.Fatalf("up -d: compose got no file:\n%s", out)
	}
	sealedDir := filepath.Join(dir, "space", "sealed", "shop")
	copied := got.Services["web"].Volumes[0].Source
	if filepath.Dir(filepath.Dir(copied)) != sealedDir {
		t.Fatalf("up -d binds %s, no sealed copy", copied)
	}
	container := func(id, project, service, source string) string {
		return id + `|{"com.docker.compose.project":"` + project + `","com.docker.compose.service":"` + service +
			`","devopsy.door":"devopsy-shop"}|[{"Source":"` + source + `","Destination":"/etc/app"}]`
	}
	write(t, filepath.Join(dir, "containers"), container("c1", "shop-prod", "web", copied)+"\n"+
		container("c2", "shop-prod", "worker", copied)+"\n"+
		// Another environment's web, on another copy: none of this file's.
		container("c3", "shop-dev", "web", filepath.Join(sealedDir, "0123456789abcdef01234567", "x"))+"\n", 0o644)
	warning := func(verb string, services ...string) string {
		lines := []string{"devopsy: warning: what these containers bind read-only differs from the release's: " + verb + " keeps the copy each was made with"}
		for _, s := range services {
			lines = append(lines, "devopsy: | "+s+": ./config")
		}
		return strings.Join(append(lines, "devopsy: | to fix it:", "devopsy: | "+sealedFix), "\n") + "\n"
	}
	if out := run("restart"); strings.Contains(out, "devopsy: warning: ") {
		t.Errorf("a restart on the copy the release seals warned:\n%s", out)
	}
	write(t, filepath.Join(code, "config", "app.conf"), "two\n", 0o644)
	for _, verb := range []string{"restart", "start"} {
		if out := run(verb); !strings.Contains(out, warning(verb, "web", "worker")) {
			t.Errorf("%s on a copy the release no longer holds, want:\n%s\ngot:\n%s", verb, warning(verb, "web", "worker"), out)
		}
	}
	if out := run("restart -t 5 web"); !strings.Contains(out, warning("restart", "web")) {
		t.Errorf("restart of web alone, want:\n%s\ngot:\n%s", warning("restart", "web"), out)
	}
	session := []string{remote.CommandEnv + "=devopsy @box restart", remote.AddressEnv + "=box", remote.HintAddressEnv + "=devopsy@vm1:prod"}
	offered := strings.Replace(warning("restart", "web", "worker"), "devopsy: | "+sealedFix+"\n", "devopsy: | "+sealedFix+":\ndevopsy: | $ devopsy @devopsy@vm1:prod --release\n", 1)
	if out := run("restart", session...); !strings.Contains(out, offered) {
		t.Errorf("restart in a session, want:\n%s\ngot:\n%s", offered, out)
	}
	forged := []string{remote.AddressEnv + "=box", remote.HintAddressEnv + "=vm1;reboot:prod"}
	if out := run("restart", forged...); !strings.Contains(out, warning("restart", "web", "worker")) {
		t.Errorf("restart with an address that is none, want:\n%s\ngot:\n%s", warning("restart", "web", "worker"), out)
	}
	// A refusal, its session's values never logged.
	if out, err := runWith("exec -u 0 web sh", session...); err == nil || strings.Contains(out, "ARGS: compose ") {
		t.Fatalf("exec as root ran (%v):\n%s", err, out)
	}
	if log := readString(filepath.Join(dir, "refused.log")); log == "" || strings.Contains(log, "box") || strings.Contains(log, "vm1") {
		t.Errorf("the refusals' log holds what the session carries, or nothing:\n%s", log)
	}
	if out := run("stop"); strings.Contains(out, "devopsy: warning: ") {
		t.Errorf("stop warned:\n%s", out)
	}
	write(t, filepath.Join(dir, "inspectfails"), "", 0o644)
	if out := run("restart"); strings.Contains(out, "devopsy: warning: ") {
		t.Errorf("a compare that failed warned:\n%s", out)
	}
	// The compares kept no copy: only the up's is there.
	entries, _ := os.ReadDir(sealedDir)
	if len(entries) != 1 || entries[0].Name() != filepath.Base(filepath.Dir(copied)) {
		t.Errorf("the sealed copies after the compares: %v, want only %s", entries, filepath.Base(filepath.Dir(copied)))
	}
}

// A request that claims an image (a tag's source, an inspect) then acts on it by the digest
// its claim loaded: the name is anyone's on the server to move right after the load,
// and a tag would make the mover's image a hold of this project's, which it then runs by
// its own name, unpinned. In a helper process (doorComposeHelper), since the door execs
// Docker, here the fake, which logs its calls.
func TestDoorClaimActsByDigest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	doorImageFake(t, dir)
	private := "registry.example.com/blog/private"
	p1 := fakeManifest(t, dir, plainManifest("private"), private+":1")
	write(t, filepath.Join(dir, "store"), private+":1|sha256:mmm|"+private+"@sha256:mmm|"+private+":1|{}\n"+private+"@"+p1+"|sha256:p1|"+private+"@"+p1+"||{}\n", 0o644)
	hold := "busybox:devopsy-shop-prod-20260101000000"
	c := exec.Command(os.Args[0], "-test.run", "^$")
	c.Env = append(os.Environ(), "DEVOPSY_TEST_DOOR_COMPOSE="+dir, "DEVOPSY_TEST_DOOR_REQUEST=tag "+private+":1 "+hold)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("the request (%v):\n%s", err, out)
	}
	if calls := readString(filepath.Join(dir, "calls")); !strings.Contains(calls, "\ntag "+private+"@"+p1+" "+hold+"\n") {
		t.Errorf("the tag is not by the claim's digest:\n%s", calls)
	}
}

// A build context reaches compose only when that compose reads service:<name> as another
// service's build (v2.33.0 on): one before reads it as a path in the project's directory,
// the project user's to make (a link to the deploy user's keys, another project's entries),
// which its client sends to the builder with the deploy user's rights. Unread, refused too.
func TestDoorBuildContextNeedsAComposeThatKnowsIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	doorImageFake(t, dir)
	// The door reads the project's tree as its user, through sudo: a path is itself, and a
	// tar of it is that user's.
	write(t, filepath.Join(dir, "sudo"), `#!/bin/sh
shift 5
case "$1" in
  real) printf '%s\n' "$2" ;;
  tar) exec tar -C "$(dirname "$2")" -cf - -- "$(basename "$2")" ;;
  *) exit 1 ;;
esac
`, 0o755)
	code := filepath.Join(dir, "prod", "releases", "1", "code")
	write(t, filepath.Join(code, "base", "Dockerfile"), "FROM busybox:1.37\n", 0o644)
	write(t, filepath.Join(code, "app", "Dockerfile"), "FROM shop-base\n", 0o644)
	svc := func(build string) string {
		return `{"build":` + build + `,"user":"10001:10001","security_opt":["no-new-privileges"]}`
	}
	file := `{"name":"shop-prod","services":{` +
		`"base":` + svc(`{"context":"`+code+`/base"}`) + `,` +
		`"app":` + svc(`{"context":"`+code+`/app","additional_contexts":{"shop-base":"service:base"}}`) + `}}`
	run := func(version string) (string, error) {
		c := exec.Command(os.Args[0], "-test.run", "^$")
		c.Env = append(os.Environ(), "DEVOPSY_TEST_DOOR_COMPOSE="+dir, "DEVOPSY_TEST_DOOR_VERB=build", "DEVOPSY_TEST_DOOR_FILE="+file,
			"DEVOPSY_TEST_DOOR_DIR="+code, "FAKE_COMPOSE_VERSION="+version, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := c.CombinedOutput()
		return string(out), err
	}
	for _, version := range []string{"2.32.4", "v2.20.2", "garbage"} {
		if out, err := run(version); err == nil || strings.Contains(out, "ARGS: ") || !strings.Contains(out, "a build context needs compose 2.33.0 or later") {
			t.Errorf("compose %s ran a build with a context (%v):\n%s", version, err, out)
		}
	}
	if out, err := run("2.33.0"); err != nil || !strings.Contains(out, `"shop-base":"service:base"`) {
		t.Errorf("the control, compose 2.33.0 (%v):\n%s", err, out)
	}
}

// The compose whose version the door reads is the compose that then builds: Docker's client
// finds its plugins in $DOCKER_CONFIG/cli-plugins first (~/.docker/cli-plugins without the
// variable), then in the system's directories, so the two are one only when both run under
// the same DOCKER_CONFIG.
func TestDoorReadsTheVersionOfTheComposeThatBuilds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	fake := doorImageFake(t, dir)
	if err := os.Rename(fake, fake+".all"); err != nil {
		t.Fatal(err)
	}
	write(t, fake, `#!/bin/sh
if [ "$*" = "compose version --short" ]; then
  if [ -n "$DOCKER_CONFIG" ]; then echo 2.26.1; else echo 2.40.0; fi
  exit 0
fi
case "$1 $DOCKER_CONFIG" in "compose "?*) echo "BUILT BY: 2.26.1" ;; esac
exec `+fake+`.all "$@"
`, 0o755)
	write(t, filepath.Join(dir, "sudo"), `#!/bin/sh
shift 5
case "$1" in
  real) printf '%s\n' "$2" ;;
  tar) exec tar -C "$(dirname "$2")" -cf - -- "$(basename "$2")" ;;
  *) exit 1 ;;
esac
`, 0o755)
	code := filepath.Join(dir, "prod", "releases", "1", "code")
	write(t, filepath.Join(code, "base", "Dockerfile"), "FROM busybox:1.37\n", 0o644)
	write(t, filepath.Join(code, "app", "Dockerfile"), "FROM shop-base\n", 0o644)
	svc := func(build string) string {
		return `{"build":` + build + `,"user":"10001:10001","security_opt":["no-new-privileges"]}`
	}
	file := `{"name":"shop-prod","services":{` +
		`"base":` + svc(`{"context":"`+code+`/base"}`) + `,` +
		`"app":` + svc(`{"context":"`+code+`/app","additional_contexts":{"shop-base":"service:base"}}`) + `}}`
	c := exec.Command(os.Args[0], "-test.run", "^$")
	c.Env = append(os.Environ(), "DEVOPSY_TEST_DOOR_COMPOSE="+dir, "DEVOPSY_TEST_DOOR_VERB=build", "DEVOPSY_TEST_DOOR_FILE="+file,
		"DEVOPSY_TEST_DOOR_DIR="+code, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, _ := c.CombinedOutput()
	if strings.Contains(string(out), "BUILT BY: 2.26.1") && strings.Contains(string(out), `"shop-base":"service:base"`) {
		t.Errorf("the door read another compose's version, and compose 2.26.1 got a build with a context:\n%s", out)
	}
}

// The Dockerfile compose's client reads for a build is a file of the sealed context, with no
// link on the way to it: the client opens <context>/<dockerfile> as the deploy user (its
// directory is the local source it sends the builder, the file the only one sent), and what
// BuildKit cannot parse comes back in the build's error, the line and those around it.
//
// Not parallel: it sets PATH (doorFakes).
func TestDoorBuildReadsNoDockerfileThroughALink(t *testing.T) {
	doorFakes(t)
	tree, space, other := t.TempDir(), t.TempDir(), t.TempDir()
	for _, d := range []*string{&tree, &space, &other} {
		if r, err := filepath.EvalSymlinks(*d); err == nil {
			*d = r
		}
	}
	// Another project's logins, the deploy user's alone to read.
	write(t, filepath.Join(other, "config.json"), `{"auths":{"registry.example.com":{"auth":"c2VjcmV0"}}}`+"\n", 0o600)
	code := filepath.Join(tree, "prod", "releases", "1", "code")
	write(t, filepath.Join(code, "big"), "what keeps tar busy\n", 0o644)
	if err := os.Symlink(other, filepath.Join(code, "sub")); err != nil {
		t.Fatal(err)
	}
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: space, Tree: tree}}
	file := map[string]any{"name": "shop-prod", "services": map[string]any{"web": map[string]any{
		"build": map[string]any{"context": code, "dockerfile": "sub/config.json"},
		"user":  "10001:10001", "security_opt": []any{"no-new-privileges"},
	}}}
	if errs := door.CheckFile(file, side.policy, "build", side); len(errs) != 0 {
		return // refused: nothing is built
	}
	b := file["services"].(map[string]any)["web"].(map[string]any)["build"].(map[string]any)
	context, _ := b["context"].(string)
	dockerfile, _ := b["dockerfile"].(string)
	read, err := filepath.EvalSymlinks(filepath.Join(context, dockerfile))
	if err == nil && !strings.HasPrefix(read, context+"/") {
		got, _ := os.ReadFile(filepath.Join(context, dockerfile))
		t.Errorf("the build's Dockerfile %s in the sealed context is %s, outside it, and the client reads: %s", dockerfile, read, got)
	}
}

// No git of the deploy user's runs in a sealed build context: buildx's client asks git about
// the context it builds (rev-parse, show HEAD, remote get-url, for its provenance, on by
// default), as whoever runs it, and a repository's own config says what git executes.
func TestDoorBuildRunsNoGitInASealedContext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	fake := doorImageFake(t, dir)
	if err := os.Rename(fake, fake+".all"); err != nil {
		t.Fatal(err)
	}
	// Compose as the door execs it says what buildx would read of git's switches.
	write(t, fake, `#!/bin/sh
case "$1 $2" in "compose -p") echo "GIT INFO: ${BUILDX_GIT_INFO-unset} LABELS: ${BUILDX_GIT_LABELS-unset}" ;; esac
exec `+fake+`.all "$@"
`, 0o755)
	write(t, filepath.Join(dir, "sudo"), `#!/bin/sh
shift 5
case "$1" in
  real) printf '%s\n' "$2" ;;
  tar) exec tar -C "$(dirname "$2")" -cf - -- "$(basename "$2")" ;;
  *) exit 1 ;;
esac
`, 0o755)
	code := filepath.Join(dir, "prod", "releases", "1", "code")
	write(t, filepath.Join(code, "Dockerfile"), "FROM busybox:1.37\n", 0o644)
	write(t, filepath.Join(code, ".git", "HEAD"), "ref: refs/heads/main\n", 0o644)
	write(t, filepath.Join(code, ".git", "config"), "[extensions]\n\tpartialClone = origin\n[remote \"origin\"]\n\tpromisor = true\n\tuploadpack = /bin/true\n", 0o644)
	file := `{"name":"shop-prod","services":{"web":{"build":{"context":"` + code + `"},"user":"10001:10001","security_opt":["no-new-privileges"]}}}`
	c := exec.Command(os.Args[0], "-test.run", "^$")
	c.Env = append(os.Environ(), "DEVOPSY_TEST_DOOR_COMPOSE="+dir, "DEVOPSY_TEST_DOOR_VERB=build", "DEVOPSY_TEST_DOOR_FILE="+file,
		"DEVOPSY_TEST_DOOR_DIR="+code, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := c.CombinedOutput()
	if err != nil {
		return // refused: nothing is built
	}
	var got struct {
		Services map[string]struct{ Build struct{ Context string } }
	}
	_, body, ok := strings.Cut(string(out), "\n{")
	if !ok || json.Unmarshal([]byte("{"+body), &got) != nil || got.Services["web"].Build.Context == "" {
		t.Fatalf("compose got no file:\n%s", out)
	}
	_, lerr := os.Lstat(filepath.Join(got.Services["web"].Build.Context, ".git"))
	off := regexp.MustCompile(`GIT INFO: (false|0|f|F|FALSE|False) LABELS: (unset|false|0|f|F|FALSE|False)\n`).MatchString(string(out))
	if lerr == nil && !off {
		line, _, _ := strings.Cut(string(out), "\n")
		t.Errorf("the sealed context %s holds the project user's .git, and buildx asks git about it as the deploy user (%s)", got.Services["web"].Build.Context, line)
	}
}

// doorComposeHelper runs the door's compose on a file naming a pulled image, with the
// fakes in dir and the verb in DEVOPSY_TEST_DOOR_VERB (TestDoorComposeHandsThePinnedFile),
// or the request in DEVOPSY_TEST_DOOR_REQUEST (TestDoorClaimActsByDigest), and exits as it
// does: compose or Docker is exec'd, so what it prints is the fake docker's.
func doorComposeHelper(dir string) {
	doorLog = os.Getenv("DEVOPSY_TEST_DOOR_LOG")
	side := &doorSide{
		policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space"), Tree: dir, ForbiddenIDs: []int{0}},
		claims: testShopClaims, imagesRoot: dir,
		real: filepath.Join(dir, "docker"), env: []string{"PATH=" + os.Getenv("PATH"), "FAKE_COMPOSE_VERSION=" + os.Getenv("FAKE_COMPOSE_VERSION")},
		// As runDoor reads it from what its sudo kept (doorEnvironment).
		asked: remote.SessionFrom(os.Getenv),
	}
	side.policy.Images = claimedImages(side.policy.ImagesFile())
	// A work directory the request made before (TestDoorClaimCutBySignalLeavesNoWork).
	if os.Getenv("DEVOPSY_TEST_DOOR_WORK") != "" {
		if _, err := side.workDir(""); err != nil {
			panic(err)
		}
	}
	if req := os.Getenv("DEVOPSY_TEST_DOOR_REQUEST"); req != "" {
		os.Exit(side.request(filepath.Join(dir, "none.json"), strings.Fields(req)))
	}
	if flags, ok := os.LookupEnv("DEVOPSY_TEST_DOOR_DOWN"); ok {
		os.Exit(side.downProject("shop-prod", strings.Fields(flags)))
	}
	file := map[string]any{"name": "shop-prod", "services": map[string]any{"web": map[string]any{
		"image": "registry.example.com/blog/private:1", "user": "10001:10001", "security_opt": []any{"no-new-privileges"},
	}}}
	// A test's own file (TestDoorBuildContextIsNoPathOfTheProjects).
	if given := os.Getenv("DEVOPSY_TEST_DOOR_FILE"); given != "" {
		file = map[string]any{}
		if err := json.Unmarshal([]byte(given), &file); err != nil {
			panic(err)
		}
	}
	// Its prod's directory, whose claim the side holds (testShopClaims), unless a test says.
	os.Exit(side.compose(filepath.Join(dir, "none.json"), file, os.Getenv("DEVOPSY_TEST_DOOR_PROFILES"), cmp.Or(os.Getenv("DEVOPSY_TEST_DOOR_DIR"), filepath.Join(dir, "prod")), strings.Fields(os.Getenv("DEVOPSY_TEST_DOOR_VERB"))))
}

// The door's clone and mark take only a clone's directory or a take's in it (door.CloneDir):
// below, a clone is a root copy of the project's entries, owners kept, so a directory the
// project user made is its own there, and it could swap what is in it for a link between the
// door's Real and the deploy user's write, which would then land anywhere the deploy user
// writes (its release root, another project's mnt). The fake's real answers what realpath
// said before the swap; on disk the link is already there.
//
// Not parallel: it sets PATH (doorFakes).
func TestDoorCloneRequestsFollowNoLinkInAClone(t *testing.T) {
	doorFakes(t)
	root := t.TempDir()
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(root, "space")}}
	// own is the project user's, made in its entry and copied with its owner.
	own := filepath.Join(side.policy.Clones(), "abc", "1", "mnt", "data", "own")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(root, "releases")
	if err := os.Mkdir(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(own, "sub")); err != nil {
		t.Fatal(err)
	}
	side.clone(filepath.Join(own, "sub", "blog-prod"))
	if _, err := os.Lstat(filepath.Join(victim, "blog-prod")); err == nil {
		t.Errorf("clone made a directory as the deploy user through a link in a clone: %s", filepath.Join(victim, "blog-prod"))
	}
	side.mark(filepath.Join(own, "sub"), "id1")
	if _, err := os.Lstat(filepath.Join(victim, remote.SnapshotComplete)); err == nil {
		t.Errorf("mark wrote as the deploy user through a link in a clone")
	}
}

// devopsy's util image runs as root on the host's files, by its name: no project user's pull
// may write that name (Claimable), as none may write a build's, so it is under the door's own
// host, which no public registry serves.
func TestDoorPullsNoUtilImage(t *testing.T) {
	t.Parallel()
	p := door.Policy{Project: "shop", User: "devopsy-shop"}
	if err := p.Claimable(remote.UtilImage); err == nil {
		t.Errorf("a project user may pull %s, which root runs", remote.UtilImage)
	}
	if _, err := door.Decide([]string{"pull", "-q", remote.UtilImage}, p, nil, doorTestHost{}, doorKnown()); err == nil {
		t.Errorf("the door pulls %s for a project user", remote.UtilImage)
	}
}

// The door runs with no file size limit, whatever its caller's through sudo: under one, its
// append to the refusals' log fails without a word. A soft limit is lifted to the hard one.
//
// Not parallel: it sets the process's file size limit.
func TestDoorLiftsTheFileSizeLimit(t *testing.T) {
	var was unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_FSIZE, &was); err != nil {
		t.Fatal(err)
	}
	if was.Max != unix.RLIM_INFINITY {
		t.Skip("this process runs under a hard file size limit")
	}
	t.Cleanup(func() { _ = unix.Setrlimit(unix.RLIMIT_FSIZE, &was) })
	low := unix.Rlimit{Cur: 1 << 20, Max: was.Max}
	if err := unix.Setrlimit(unix.RLIMIT_FSIZE, &low); err != nil {
		t.Fatal(err)
	}
	if err := doorLimits(); err != nil {
		t.Fatal(err)
	}
	var now unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_FSIZE, &now); err != nil || now.Cur != unix.RLIM_INFINITY {
		t.Errorf("the door's file size limit: %+v (%v)", now, err)
	}
}

// The door ignores its caller's terminal stop signals (SIGTSTP, SIGTTIN, SIGTTOU), so it is
// never held between a check and its use, and gives them back to what it execs: an ignored
// signal stays ignored across an exec, and compose's Ctrl-Z is compose's.
func TestDoorGivesWhatItExecsItsStops(t *testing.T) {
	t.Parallel()
	if _, err := os.Stat("/proc/self/status"); err != nil {
		t.Skip("no /proc here to read a process's ignored signals from")
	}
	c := exec.Command(os.Args[0], "-test.run", "^$")
	c.Env = append(os.Environ(), "DEVOPSY_TEST_EXEC_DECIDED=1")
	out, err := c.Output()
	if err != nil {
		t.Fatalf("the exec (%v): %s", err, out)
	}
	mask, err := strconv.ParseUint(strings.TrimSpace(strings.TrimPrefix(string(out), "SigIgn:")), 16, 64)
	if err != nil {
		t.Fatalf("SigIgn: %q", out)
	}
	for _, sig := range []syscall.Signal{syscall.SIGTSTP, syscall.SIGTTIN, syscall.SIGTTOU} {
		if mask&(1<<(uint(sig)-1)) != 0 {
			t.Errorf("what the door execs ignores %v", sig)
		}
	}
}

// sudo's rule for a project user is `devopsy --door *`, and sudoers matches a command's
// arguments as one string, a wildcard crossing words: one argument "--door x" matches it.
// devopsy refuses a first word that only starts like a door flag before it reads anything,
// so the project's own programs (its env capability) never run as the deploy user.
func TestDoorFlagsTakeNoWordThatOnlyStartsLikeThem(t *testing.T) {
	t.Parallel()
	for _, first := range []string{"--door x", "--door  ", "--door x up", "--door\tx", "--door-read x", "--doorway"} {
		dir := t.TempDir()
		ran := filepath.Join(dir, "helper-ran")
		write(t, filepath.Join(dir, ".devopsy", "config.yaml"), "project: shop\n", 0o644)
		write(t, filepath.Join(dir, ".devopsy", "compose.yaml"), "services:\n  web:\n    image: busybox:1.37\n", 0o644)
		write(t, filepath.Join(dir, ".devopsy", "capabilities", "env", "compute"), "#!/bin/sh\ntouch "+ran+"\n", 0o755)
		out, code := runDevopsy(t, dir, []string{"SUDO_USER=devopsy-shop"}, first, "up")
		if _, err := os.Stat(ran); err == nil {
			t.Errorf("%q: the project's own program ran as whoever sudo ran devopsy as:\n%s", first, out)
		}
		if code == 0 {
			t.Errorf("%q: not refused:\n%s", first, out)
		}
	}
}

// A start of an image pinned by a multi-platform index's digest (repo:tag@sha256:<index>, as a
// registry shows a tag and pinning tools write it) runs the entry its claim checked and loaded
// for the daemon's platform: no image here goes by the index's digest, so compose given it
// would fail under pull_policy never, or run Docker's own choice from an index another's pull
// left, which the door checked nothing of.
func TestDoorStartOfAnIndexPinRunsWhatItLoaded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := doorImageFake(t, dir)
	repo := "registry.example.com/multi"
	amd := fakeManifest(t, dir, plainManifest("multi-amd64"), repo+":amd64")
	index := `{"schemaVersion":2,"manifests":[{"digest":"` + amd + `","platform":{"os":"linux","architecture":"amd64"}}]}`
	idx := fakeManifest(t, dir, index, repo+":1")
	// The registry serves it by its tag and digest too, as a registry does.
	write(t, filepath.Join(dir, "raw", strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(repo+":1@"+idx)), index, 0o644)
	write(t, filepath.Join(dir, "store"), "", 0o644)
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}, real: fake, imagesRoot: dir}
	// With its tag and without.
	for _, image := range []string{repo + ":1@" + idx, repo + "@" + idx} {
		os.Remove(filepath.Join(dir, "calls"))
		file := map[string]any{"name": "shop-prod", "services": map[string]any{"web": map[string]any{"image": image}}}
		if err := side.checkImageLabels(file, filepath.Join(dir, "none.json")); err != nil {
			t.Fatal(err)
		}
		got, _ := file["services"].(map[string]any)["web"].(map[string]any)["image"].(string)
		if _, err := side.inspectImage(got); err != nil || !strings.HasSuffix(got, "@"+amd) {
			t.Errorf("%s: compose gets %q, not the %s@%s the claim loaded:\n%s", image, got, repo, amd, readString(filepath.Join(dir, "store")))
		}
	}
	if !strings.Contains(readString(side.policy.ImagesFile()), amd) {
		t.Errorf("the claim recorded no %s: %q", amd, readString(side.policy.ImagesFile()))
	}
}

// The door makes an entry only from a clean answer of where a bind's source ends (Real): the
// project user's read is its own process, and an answer through a link in its own entry and
// `..` twice reads to filepath.Dir as its own mnt while the kernel resolves another project's,
// which the door would make and give the user.
//
// Not parallel: it sets PATH (doorFakes).
func TestDoorEntriesBelieveOnlyACleanAnswer(t *testing.T) {
	acl, _ := doorFakes(t)
	root := t.TempDir()
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(root, "space")}}
	mnt := side.policy.Mnt("shop-prod")
	entry := filepath.Join(mnt, "data")
	if err := os.MkdirAll(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	// In its entry, which it writes: a link to the entry itself.
	if err := os.Symlink(".", filepath.Join(entry, "l")); err != nil {
		t.Fatal(err)
	}
	binds := func(source string) map[string]any {
		return map[string]any{"name": "shop-prod", "services": map[string]any{"web": map[string]any{"volumes": []any{
			map[string]any{"type": "bind", "source": source, "target": "/x"},
		}}}}
	}
	// The control: an entry of its own mnt is made (the fake's read answers the path asked).
	if side.entries(binds(filepath.Join(mnt, "files"))); !fileExists(filepath.Join(mnt, "files")) {
		t.Fatal("the control: an entry of its mnt was not made")
	}
	// The answer its user forged: another project's mnt, to the kernel.
	theirs := filepath.Join(side.policy.Space, "mnt", "blog-staging")
	side.entries(binds(entry + "/l/../../blog-staging"))
	if fileExists(theirs) {
		t.Errorf("the door made %s, another project's mnt, and gave it to devopsy-shop:\n%s", theirs, readString(acl))
	}
}

// testShopClaims are claims the door tests' project holds: its prod on 10001, the uid their
// compose files run as.
var testShopClaims = []uids.Claim{
	{Owner: uids.Owner{Project: "shop", Env: "prod"}, UID: 10001},
	{Owner: uids.Owner{Project: "shop"}, UID: 10001},
}
