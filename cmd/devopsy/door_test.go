package main

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// utilScripts are the util containers' scripts by name, as the code calls them: what
// a project user's door must take, or its release fails at runtime.
var utilScripts = map[string]string{
	"exportTarScript": exportTarScript, "reflinkProbeScript": reflinkProbeScript, "snapshotTarScript": snapshotTarScript,
	"spaceProbeScript": spaceProbeScript, "ownEntry": ownEntry, "emptyAndExtract": emptyAndExtract, "shareScript": shareScript,
	"rsyncDaemonScript": rsyncDaemonScript, "restoreStreamScript": restoreStreamScript, "receiveScript": receiveScript,
}

// TestDoorKnowsDevopsysUtilScripts fails when devopsy runs a util container whose
// script the door does not take. A script is one of doorKnown's, by value.
func TestDoorKnowsDevopsysUtilScripts(t *testing.T) {
	t.Parallel()
	known := doorKnown()
	scripts := []string{known.Owners, known.Export, known.Import, known.ReflinkProbe, known.SnapshotTar, known.Space, known.Share, known.Daemon, known.Restore, known.Receive}
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
	"buildx": "--context-hash, local", "info": "--server-info, the admin's", "system": "--server-info, the admin's",
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
	p := door.Policy{Project: "shop", User: "devopsy-shop", Space: "/space", Tree: "/home/devopsy-shop/shop"}
	daemon := []string{"run", "--rm", "-d", "--network", "none", "-e", "RSYNCD_CONF=" + rsyncdConf([]string{"storage", "app.ini"}, []string{"app.ini"}, []string{"db"}),
		"-v", "/space/mnt/shop-prod:/mnt:ro", "-v", "shop-prod_db:/vol/db:ro", remote.UtilImage, "sh", "-c", rsyncDaemonScript}
	if _, err := door.Decide(daemon, p, nil, doorTestHost{}, doorKnown()); err != nil {
		t.Errorf("the door does not take devopsy's own rsync daemon: %v", err)
	}
	child := []childService{{Dirs: []string{"storage", "app.ini"}, Volumes: []string{"db"}, Owners: map[string]string{"storage": "1:2"}}}
	c := container{remote.SyncContainerPrefix + "1.abcdef-receive", "shop-dev", remote.SyncTask("1.abcdef")}
	for _, r := range []syncReceive{{Entry: "storage"}, {Entry: "app.ini", File: true}, {Volume: "db"}} {
		plan, err := planReceive(&r, child, "/space/mnt/shop-dev", "shop-dev")
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
func (doorTestHost) IsDir(string) bool                       { return false }
func (doorTestHost) CopyIn(p string, _ bool) (string, error) { return p, nil }
func (doorTestHost) WorkDir() (string, error)                { return "/space/work/x", nil }

// TestRunDoorRead is the project user's side, on real files.
func TestRunDoorRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	f := filepath.Join(dir, "f")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := runDoorRead([]string{"nothing-known", f}); code != 2 {
		t.Errorf("an unknown read: %d", code)
	}
	if code := runDoorRead([]string{"isdir", dir}); code != 0 {
		t.Errorf("isdir of a directory: %d", code)
	}
	if code := runDoorRead([]string{"isdir", f}); code != 1 {
		t.Errorf("isdir of a file: %d", code)
	}
	if code := runDoorRead([]string{"remove", f}); code != 0 {
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
	_, _ = duData(context.Background(), container{"devopsy-du-1", "shop-prod", remote.TaskExport}, d)
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
// project user's release.
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
	p := door.Policy{Project: "shop", User: "devopsy-shop", Space: space, Tree: "/home/devopsy-shop/shop"}
	c := container{"devopsy-x-1", "shop-prod", remote.TaskExport}
	ctx := context.Background()
	d := dataSet{mnt: mnt, entries: []string{"storage"}, volumes: []cli.Volume{{Key: "db", Name: "shop-prod_db"}}}
	inClone := d.in(clone)
	for name, run := range map[string]func(){
		"du":                 func() { _, _ = duData(ctx, c, d) },
		"du of a clone":      func() { _, _ = duData(ctx, c, inClone) },
		"probe":              func() { reflinks(ctx, c, clone, d) },
		"clone":              func() { _ = cloneData(ctx, c, clone, d) },
		"rsync an entry":     func() { _ = rsyncEntry(ctx, c, mnt, dev, "storage", false) },
		"rsync with owners":  func() { _ = rsyncEntry(ctx, c, mnt, dev, "storage", false, "--chown=1:2") },
		"rsync a file":       func() { _ = rsyncEntry(ctx, c, mnt, dev, "app.ini", true) },
		"rsync from a clone": func() { _ = rsyncEntry(ctx, c, inClone.mnt, dev, "storage", false) },
		"rsync a volume":     func() { _ = rsyncVolume(ctx, c, "shop-prod_db:/src/db:ro", "shop-dev_db:/dst/db", "db") },
		"a tar": func() {
			_ = tarInto(ctx, c, tree+"/snapshots/1.tmp/data/mnt.storage.tar.zst", []string{"-v", clone + ":/m:ro"}, "mnt", "storage")
		},
		"a share": func() { shareInto(ctx, c, mnt+"/storage", dev+"/storage", "mnt/storage") },
	} {
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
		if _, err := door.Decide(argv, p, nil, doorTestHost{}, doorKnown()); err != nil {
			t.Errorf("%s: %v\n%q", name, err, argv)
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

// A start holds the project's image lock past its exec, into compose: a build, pull or tag
// of the project's would otherwise change an image between the label check and compose's
// create. A door that refuses lets it go.
func TestDoorImageLockHoldsTheProject(t *testing.T) {
	t.Parallel()
	side := &doorSide{policy: door.Policy{Project: "shop", Space: t.TempDir()}}
	unlock, err := side.imageLock()
	if err != nil {
		t.Fatal(err)
	}
	other, err := os.Open(filepath.Join(side.policy.Space, "locks", "shop"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		t.Fatal("the lock is not held")
	}
	if side.lock == nil {
		t.Error("execKeeping has no lock to keep")
	}
	unlock()
	// A parallel test's fork shares the lock's open file until its exec closes it, so the
	// lock may stay a moment after unlock: polled, bounded by a second.
	free := false
	for end := time.Now().Add(time.Second); !free && time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		free = syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
	}
	if !free {
		t.Error("the lock stays after unlock")
	}
}

// What a start execs, compose, holds the image lock it took: a lock let go at the exec
// would leave compose's create open to a build of the project's.
func TestDoorImageLockOutlivesItsExec(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("no flock(1) here")
	}
	space := t.TempDir()
	c := exec.Command(os.Args[0], "-test.run", "^$")
	c.Env = append(os.Environ(), "DEVOPSY_TEST_LOCK_EXEC="+space)
	out, err := c.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "held" {
		t.Errorf("after the exec: %s %v", out, err)
	}
}

// lockExecHelper takes the image lock and execs a shell that tries it from another open
// file, as a build of the project's would.
func lockExecHelper(space string) {
	side := &doorSide{policy: door.Policy{Project: "shop", Space: space}, real: "/bin/sh"}
	if _, err := side.imageLock(); err != nil {
		panic(err)
	}
	lock := filepath.Join(space, "locks", "shop")
	panic(side.execKeeping([]string{"sh", "-c", `flock -n "$1" true && echo free || echo held`, "sh", lock}, os.Environ()))
}

// A start runs the image the door read: what a service builds is checked by compose's name
// for it, and an image still missing refuses, since compose would pull or build one past
// the check.
func TestDoorChecksWhatAServiceBuilds(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	docker := filepath.Join(bin, "docker")
	if err := testfile.Write(docker, []byte(`#!/bin/sh
case "$*" in
  *" shop-prod-labelled") echo '{"Config":{"Labels":{"devopsy.role":"proxy"}}}' ;;
  *" shop-prod-plain") echo '{"Config":{"Labels":{"a":"b"}}}' ;;
  *) exit 1 ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}
	side := &doorSide{policy: door.Policy{Project: "shop"}, real: docker}
	file := func(svc string) map[string]any {
		return map[string]any{"name": "shop-prod", "services": map[string]any{svc: map[string]any{"build": map[string]any{"context": "."}}}}
	}
	if err := side.checkImageLabels(file("plain"), ""); err != nil {
		t.Errorf("the control: a plain build is refused: %v", err)
	}
	if err := side.checkImageLabels(file("labelled"), ""); err == nil || !strings.Contains(err.Error(), "devopsy.role") {
		t.Errorf("a build labelled devopsy.role: %v", err)
	}
	if err := side.checkImageLabels(file("missing"), ""); err == nil || !strings.Contains(err.Error(), "not here") {
		t.Errorf("a build not here yet, which compose would build at the start: %v", err)
	}
}

// A project's build runs on a BuildKit of its own, made at its first build: on one builder,
// a cache mount is shared across projects by its id. Its settings stay the door's.
func TestDoorBuildsOnTheProjectsBuilder(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log := filepath.Join(dir, "docker.log")
	docker := filepath.Join(dir, "docker")
	if err := testfile.Write(docker, []byte(`#!/bin/sh
echo "$BUILDX_CONFIG $*" >>`+log+`
case "$*" in
  "buildx inspect "*) [ -e `+dir+`/made ] ;;
  "buildx create "*) touch `+dir+`/made ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}
	side := &doorSide{policy: door.Policy{Project: "shop", Space: filepath.Join(dir, "space")}, real: docker}
	for range 2 {
		env, err := side.builder()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(env, "BUILDX_BUILDER=devopsy-shop") || !slices.Contains(env, "BUILDX_CONFIG="+filepath.Join(dir, "space", "buildx")) {
			t.Errorf("a build's environment: %q", env)
		}
	}
	got, _ := os.ReadFile(log)
	space := filepath.Join(dir, "space", "buildx")
	want := space + " buildx inspect devopsy-shop\n" + space + " buildx create --name devopsy-shop --driver docker-container\n" + space + " buildx inspect devopsy-shop\n"
	if string(got) != want {
		t.Errorf("docker was asked:\n%s\nwant:\n%s", got, want)
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
	want := "run --rm --network none --cap-drop MKNOD --security-opt no-new-privileges -v " + data + ":/e/0 -v " + ini + ":/e/1 " + remote.UtilImage +
		" find /e/0 /e/1 -xdev ( -type d -o -type f ) ! -perm -g=w -exec setfacl -m u:10002:rwX {} +\n"
	if string(got) != want {
		t.Errorf("docker was asked:\n%s\nwant:\n%s", got, want)
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
	// devopsy's own names are under devopsy-: the util image (devopsy-util, root with host
	// mounts, which a project named devopsy could build and remove), the project users, the
	// group, the builders.
	for project, clash := range map[string]bool{
		"shop": false, "shopadmin": false, "admin-shop": false, "blog": true, "shop-admin": true, "traefik-x": true, "blog-admin": false, "blog-admin-eu": true, "devopsy": true, "devopsy-util": true,
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
// buildx imagetools inspect answers; a pull of a ref listed in dir/fails fails. It logs
// every call in dir/calls, lists no containers, and answers compose with its arguments
// and the file it was given, for a door that execs it (doorComposeHelper). Image
// inspects answer the two formats the door uses today and `{{json .}}`, as
// images.CLI.Inspect reads it.
func doorImageFake(t *testing.T, dir string) string {
	t.Helper()
	in := func(name string) string { return filepath.Join(dir, name) }
	docker := in("docker")
	write(t, docker, `#!/bin/sh
all="$*"
echo "$all" >>`+in("calls")+`
for a; do ref=$a; done
entry() { awk -F'|' -v r="$1" '$1==r' `+in("store")+` 2>/dev/null | head -1; }
field() { printf '%s\n' "$1" | cut -d'|' -f"$2"; }
tojson() { printf '['; first=1; for x in $1; do [ $first = 1 ] || printf ','; printf '"%s"' "$x"; first=0; done; printf ']'; }
registry() { awk -F'|' -v r="$1" '$1==r{print $2}' `+in("registry")+` 2>/dev/null; }
case "$all" in
  'pull -q '*|'pull '*)
    grep -qxF -- "$ref" `+in("fails")+` 2>/dev/null && exit 1
    case "$all" in
      'pull -q '*) echo "$ref" ;;
      *) echo "$ref: Pulling from a registry"; d=$(registry "$ref"); [ -n "$d" ] && echo "Digest: $d"; echo "Status: Downloaded newer image for $ref" ;;
    esac ;;
  'buildx imagetools inspect '*) registry "$ref" ;;
  'ps -a '*) ;;
  'compose '*)
    f=; while [ $# -gt 0 ]; do [ "$1" = -f ] && f=$2; shift; done
    echo "ARGS: $all"
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

// A start runs an image of this project's by name (a build's, a hold of its own, devopsy's
// util image: names only this project or the deploy user writes, under this project's
// lock), or one it claims first: pulled with its own registry logins, the id then recorded
// in its space, so the next start asks the registry nothing. One already on disk that
// another project pulled (a private one with that project's login), built (blog-prod-web)
// or holds, or an id alone, is refused when this project's pull of it fails, and a hold of
// another's is never pulled at all.
//
// A claimed image goes to compose pinned, `<image as given>@<digest>`, the digest the
// image has in the service's repository (as the devopsy.digest label chooses it, the tag
// kept so docker ps shows it, a reference by digest kept as given): the name is anyone's
// on the server to move between this check and compose's create (a project user's build of
// a Docker Hub name, mongo's mongo-express:1, under mongo's lock alone), and Docker
// resolves repo:tag@digest by the digest. The digest, the labels read and the id recorded
// are the image the project's pull got, read by digest or id, never by the name again
// after the pull; an image with no digest in its repository never goes by its plain name.
func TestDoorStartsItsOwnOrClaimedImages(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := doorImageFake(t, dir)
	store := func(lines ...string) { write(t, filepath.Join(dir, "store"), strings.Join(lines, "\n")+"\n", 0o644) }
	private := "registry.example.com/blog/private"
	store(
		// Pulled under two repositories, a mirror's digest first: the pin is this one's.
		private+":1|sha256:p1|other.example.com/mirror@sha256:zz "+private+"@sha256:p1||{}",
		private+"@sha256:p1|sha256:p1|"+private+"@sha256:p1||{}",
		"registry.example.com/blog/labelled:1|sha256:l1|registry.example.com/blog/labelled@sha256:l1||{\"devopsy.role\":\"proxy\"}",
		"registry.example.com/blog/labelled@sha256:l1|sha256:l1|registry.example.com/blog/labelled@sha256:l1||{\"devopsy.role\":\"proxy\"}",
		"blog-prod-web|sha256:b1||blog-prod-web blog-prod-web:latest|{}",
		"blog-prod-web:latest|sha256:b1||blog-prod-web blog-prod-web:latest|{}",
		"registry.example.com/app:2|sha256:a2|registry.example.com/app@sha256:a2||{}",
		"registry.example.com/app:devopsy-blog-prod-20260101000000|sha256:a2|registry.example.com/app@sha256:a2||{}",
		"sha256:0123|sha256:0123|||{}",
		"shop-prod-web|sha256:s1||shop-prod-web|{}",
		"busybox:devopsy-shop-prod-20260101000000|sha256:bb|busybox@sha256:bb|busybox:devopsy-shop-prod-20260101000000|{}",
		"busybox@sha256:bb|sha256:bb|busybox@sha256:bb||{}",
		remote.UtilImage+"|sha256:u4|||{}",
		"loaded:1|sha256:n1||loaded:1|{}",
	)
	write(t, filepath.Join(dir, "registry"), private+":1|sha256:p1\nregistry.example.com/blog/labelled:1|sha256:l1\nbusybox@sha256:bb|sha256:bb\nmongo-express:1|sha256:hhh\n", 0o644)
	write(t, filepath.Join(dir, "fails"), "blog-prod-web\nblog-prod-web:latest\nregistry.example.com/app:2\nsha256:0123\nloaded:1\n", 0o644)
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: dir, Others: []string{"blog"}}, real: fake}
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
	pulled := func() bool { return strings.Contains(readString(calls), "pull ") }
	pin := private + ":1@sha256:p1"
	if got, err := start(private + ":1"); err != nil || !pulled() || got != pin {
		t.Errorf("claimed by its pull (%v), pulled %v, goes to compose as %q, want %q:\n%s", err, pulled(), got, pin, readString(calls))
	}
	if got := readString(side.policy.ImagesFile()); got != "sha256:p1\n" {
		t.Errorf("the record: %q", got)
	}
	side.policy.Images = claimedImages(side.policy.ImagesFile())
	if got, err := start(private + ":1"); err != nil || pulled() || got != pin {
		t.Errorf("claimed already (%v), pulled again %v, goes to compose as %q:\n%s", err, pulled(), got, readString(calls))
	}
	if _, err := start("registry.example.com/blog/labelled:1"); err == nil || !strings.Contains(err.Error(), "devopsy.role") {
		t.Errorf("a claimed image labelled devopsy.role: %v", err)
	}
	for _, image := range []string{"registry.example.com/app:2", "sha256:0123"} {
		if _, err := start(image); err == nil || !strings.Contains(err.Error(), "its registry logins do not pull it") {
			t.Errorf("%s, another's, ran: %v", image, err)
		}
	}
	// One blog builds is under blog's names, which only blog writes: never pulled over.
	for _, image := range []string{"blog-prod-web", "blog-prod-web:latest"} {
		if _, err := start(image); err == nil || pulled() || !strings.Contains(err.Error(), "under project blog's names") {
			t.Errorf("%s, blog's build, ran or pulled (%v):\n%s", image, err, readString(calls))
		}
	}
	if _, err := start("registry.example.com/app:devopsy-blog-prod-20260101000000"); err == nil || pulled() {
		t.Errorf("another's hold pulled or ran (%v):\n%s", err, readString(calls))
	}
	// Its own built image, its own hold and devopsy's util image need no claim and no pin.
	for _, image := range []string{"shop-prod-web", "busybox:devopsy-shop-prod-20260101000000", remote.UtilImage} {
		if got, err := start(image); err != nil || pulled() || got != image {
			t.Errorf("its own %s (%v), pulled %v, goes to compose as %q:\n%s", image, err, pulled(), got, readString(calls))
		}
	}
	// A reference by digest is a pin already: claimed, and kept as given.
	if got, err := start("busybox@sha256:bb"); err != nil || !pulled() || got != "busybox@sha256:bb" {
		t.Errorf("a reference by digest (%v), pulled %v, goes to compose as %q:\n%s", err, pulled(), got, readString(calls))
	}
	// An image the project claimed that has no digest in its repository (loaded here, named
	// by hand) and that its registry does not pull: refused, or pinned by what it has (its
	// id), never started by its plain name, which the check did not pin.
	side.policy.Images = append(side.policy.Images, "sha256:n1")
	if got, err := start("loaded:1"); err == nil && got == "loaded:1" {
		t.Errorf("an image with no digest in its repository goes to compose by its plain name")
	}
	// The swap: right after the claim's pull, another project's build lands on the name
	// (mongo's mongo-express:1, a Docker Hub name under mongo's names), with a registry
	// digest of its own, as Docker's containerd store gives built images. The name now
	// resolves to mongo's image; the pull got the registry's, and that is what the pin
	// and the record must be.
	store(
		"mongo-express:1|sha256:mmm|mongo-express@sha256:mmm|mongo-express:1|{}",
		"mongo-express@sha256:hhh|sha256:hhh|mongo-express@sha256:hhh||{}",
		"sha256:hhh|sha256:hhh|mongo-express@sha256:hhh||{}",
	)
	if got, err := start("mongo-express:1"); err != nil || !pulled() || got != "mongo-express:1@sha256:hhh" {
		t.Errorf("a name moved right after its pull (%v), pulled %v, goes to compose as %q, want the digest the pull got:\n%s", err, pulled(), got, readString(calls))
	}
	if got := readString(side.policy.ImagesFile()); !strings.Contains(got, "sha256:hhh") || strings.Contains(got, "sha256:mmm") {
		t.Errorf("the record after the swap holds the name's image, not the pull's: %q", got)
	}
}

// What compose receives for a verb that creates a container (up, create, run) is the
// checked file with its pins, marshalled after the image check: marshalled before it, the
// file carries the plain names the check pinned in its map alone. A pull keeps the names:
// it is what moves them. In a helper process (doorComposeHelper), since the door execs
// compose, here the fake docker, which prints the file it was given.
func TestDoorComposeHandsThePinnedFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	doorImageFake(t, dir)
	// The door reads the project's tree as its user, through sudo: a path is itself.
	write(t, filepath.Join(dir, "sudo"), "#!/bin/sh\nshift 5\n[ \"$1\" = real ] || exit 1\nprintf '%s\\n' \"$2\"\n", 0o755)
	private := "registry.example.com/blog/private"
	write(t, filepath.Join(dir, "store"), private+":1|sha256:p1|"+private+"@sha256:p1||{}\n"+private+"@sha256:p1|sha256:p1|"+private+"@sha256:p1||{}\n", 0o644)
	write(t, filepath.Join(dir, "registry"), private+":1|sha256:p1\n", 0o644)
	for _, verb := range []string{"up -d", "create", "run --rm web true", "pull"} {
		c := exec.Command(os.Args[0], "-test.run", "^$")
		c.Env = append(os.Environ(), "DEVOPSY_TEST_DOOR_COMPOSE="+dir, "DEVOPSY_TEST_DOOR_VERB="+verb, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		out, err := c.CombinedOutput()
		if err != nil {
			t.Errorf("%s: the door did not exec compose (%v):\n%s", verb, err, out)
			continue
		}
		var file struct {
			Services map[string]struct{ Image string }
		}
		_, body, ok := strings.Cut(string(out), "\n{")
		if !ok || json.Unmarshal([]byte("{"+body), &file) != nil {
			t.Errorf("%s: compose got no file:\n%s", verb, out)
			continue
		}
		want := private + ":1@sha256:p1"
		if verb == "pull" {
			want = private + ":1"
		}
		if got := file.Services["web"].Image; got != want {
			t.Errorf("%s: compose got web's image as %q, want %q:\n%s", verb, got, want, out)
		}
	}
}

// A request that claims an image (a tag's source, an inspect) then acts on it by the digest
// its claim's pull got: the name is anyone's on the server to move right after the pull,
// and a tag would make the mover's image a hold of this project's, which it then runs by
// its own name, unpinned. In a helper process (doorComposeHelper), since the door execs
// Docker, here the fake, which logs its calls.
func TestDoorClaimActsByDigest(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	doorImageFake(t, dir)
	private := "registry.example.com/blog/private"
	write(t, filepath.Join(dir, "store"), private+":1|sha256:mmm|"+private+"@sha256:mmm|"+private+":1|{}\n"+private+"@sha256:p1|sha256:p1|"+private+"@sha256:p1||{}\n", 0o644)
	write(t, filepath.Join(dir, "registry"), private+":1|sha256:p1\n", 0o644)
	hold := "busybox:devopsy-shop-prod-20260101000000"
	c := exec.Command(os.Args[0], "-test.run", "^$")
	c.Env = append(os.Environ(), "DEVOPSY_TEST_DOOR_COMPOSE="+dir, "DEVOPSY_TEST_DOOR_REQUEST=tag "+private+":1 "+hold)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("the request (%v):\n%s", err, out)
	}
	if calls := readString(filepath.Join(dir, "calls")); !strings.Contains(calls, "\ntag "+private+"@sha256:p1 "+hold+"\n") {
		t.Errorf("the tag is not by the claim's digest:\n%s", calls)
	}
}

// doorComposeHelper runs the door's compose on a file naming a pulled image, with the
// fakes in dir and the verb in DEVOPSY_TEST_DOOR_VERB (TestDoorComposeHandsThePinnedFile),
// or the request in DEVOPSY_TEST_DOOR_REQUEST (TestDoorClaimActsByDigest), and exits as it
// does: compose or Docker is exec'd, so what it prints is the fake docker's.
func doorComposeHelper(dir string) {
	side := &doorSide{
		policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space"), Tree: dir, ForbiddenIDs: []int{0}},
		real:   filepath.Join(dir, "docker"), env: []string{"PATH=" + os.Getenv("PATH")},
	}
	side.policy.Images = claimedImages(side.policy.ImagesFile())
	if req := os.Getenv("DEVOPSY_TEST_DOOR_REQUEST"); req != "" {
		os.Exit(side.request(filepath.Join(dir, "none.json"), strings.Fields(req)))
	}
	file := map[string]any{"name": "shop-prod", "services": map[string]any{"web": map[string]any{
		"image": "registry.example.com/blog/private:1", "user": "10001:10001", "security_opt": []any{"no-new-privileges"},
	}}}
	os.Exit(side.compose(filepath.Join(dir, "none.json"), file, "", dir, strings.Fields(os.Getenv("DEVOPSY_TEST_DOOR_VERB"))))
}
