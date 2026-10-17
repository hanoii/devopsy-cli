package remote

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// gitProject makes a git repository with a .devopsy/ project and some state
// that must never be released.
func gitProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, ".gitignore"), "/ignored.txt\n/.devopsy/.env\n/.devopsy/mnt\n")
	write(t, filepath.Join(root, "app.php"), "<?php\n")
	write(t, filepath.Join(root, "src", "lib.php"), "<?php\n")
	write(t, filepath.Join(root, "ignored.txt"), "no\n")
	write(t, filepath.Join(root, "untracked.txt"), "yes\n")
	write(t, filepath.Join(root, ".devopsy", "compose.yaml"), "services: {}\n")
	write(t, filepath.Join(root, ".devopsy", "commands", "deploy"), "#!/bin/sh\n")
	write(t, filepath.Join(root, ".devopsy", ".env"), "SECRET=1\n")
	write(t, filepath.Join(root, ".devopsy", "compose.override.yaml"), "services: {}\n")
	write(t, filepath.Join(root, ".devopsy", "mnt", "data", "db"), "data\n")
	write(t, filepath.Join(root, ".devopsy", LocalConfigFile), "targets: {mine: {}}\n")
	if err := os.Symlink("app.php", filepath.Join(root, "link.php")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", ".gitignore", "app.php", "src", ".devopsy/compose.yaml", ".devopsy/commands", "link.php"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	return root
}

func TestFilesImageMode(t *testing.T) {
	root := gitProject(t)
	got, err := Files(root, ModeImage)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{".devopsy/commands/deploy", ".devopsy/compose.yaml"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestFilesBuildMode(t *testing.T) {
	root := gitProject(t)
	got, err := Files(root, ModeBuild)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		".devopsy/commands/deploy", ".devopsy/compose.yaml", ".gitignore",
		"app.php", "link.php", "src/lib.php", "untracked.txt",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if _, err := Files(t.TempDir(), ModeBuild); err == nil {
		t.Fatal("build mode outside git: want an error")
	}
}

func TestPack(t *testing.T) {
	root := gitProject(t)
	files, err := Files(root, ModeBuild)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	extra := map[string][]byte{RecordFile: []byte(`{"id":"x"}`), TargetEnvFile: []byte("A='1'\n")}
	var read int64
	if err := Pack(&buf, root, files, extra, func(n int64) { read += n }); err != nil {
		t.Fatal(err)
	}
	if size, err := Size(root, files); err != nil || size == 0 || read != size {
		t.Fatalf("progress counted %d bytes, Size %d (%v)", read, size, err)
	}
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	entries := map[string]*tar.Header{}
	contents := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entries[hdr.Name] = hdr
		b, _ := io.ReadAll(tr)
		contents[hdr.Name] = string(b)
	}
	if len(entries) != len(files)+2 {
		t.Fatalf("%d entries for %d files", len(entries), len(files))
	}
	if h := entries["code/link.php"]; h == nil || h.Typeflag != tar.TypeSymlink || h.Linkname != "app.php" {
		t.Fatalf("symlink not kept: %+v", h)
	}
	if contents[RecordFile] != `{"id":"x"}` || contents[TargetEnvFile] != "A='1'\n" || contents["code/app.php"] != "<?php\n" {
		t.Fatalf("contents: %q", contents)
	}
}

func TestNewRecord(t *testing.T) {
	root := gitProject(t)
	r := NewRecord(root, ModeBuild, time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	if r.ID != "20261005120000" || r.Commit == "" || r.Branch == "" || !r.Dirty {
		t.Fatalf("%+v", r)
	}
	if r := NewRecord(t.TempDir(), ModeImage, time.Now()); r.Commit != "" {
		t.Fatalf("outside git: %+v", r)
	}
}

func TestQuote(t *testing.T) {
	for _, s := range []string{"plain", "with space", "it's", `a"b$c\d`, "", "-n"} {
		out, err := exec.Command("sh", "-c", "printf '%s' "+Quote(s)).Output()
		if err != nil || string(out) != s {
			t.Errorf("%q: got %q (%v)", s, out, err)
		}
	}
}

func TestScriptsParse(t *testing.T) {
	tg := &Target{Name: "prod", Host: "h", Path: "it's app/prod", Mode: ModeImage}
	scripts := map[string]string{
		"upload":          UploadScript(tg, "20261005120000"),
		"activate":        ActivateScript(tg, "20261005120000", false, "", Phases{}, nil),
		"activate+cmd":    ActivateScript(tg, "20261005120000", false, "app", Phases{Before: [][]string{{"x"}}, Prepare: [][]string{{"secrets"}}, Run: []string{"deploy", "a b"}, After: [][]string{{"warm"}}}, nil),
		"rollback+cmd":    ActivateScript(tg, "", true, "", Phases{Run: []string{"up", "-d"}}, nil),
		"run":             RunScript(tg, "app", []string{"logs", "-f"}),
		"releases":        ReleasesScript(tg),
		"releases remove": ReleasesRemoveScript(tg, "app", []string{"1", "2"}),
		"shell":           ShellScript(tg),
		"vars":            VarsReadScript(tg, nil),
		"vars set":        VarsSetScript(tg, nil),
		"vars unset":      VarsUnsetScript(tg, &Level{Name: "project", Dir: "app"}),
		"inherit read":    InheritReadScript(&Target{Path: "app/a/prod", Levels: []Level{{Name: "project", Dir: "app"}, {Name: "instance", Dir: "app/a"}}}),
		"inherit write":   InheritWriteScript(tg, []string{"A", "B"}, false),
		"inherit pending": InheritWriteScript(tg, []string{"A", "B"}, true),
		"keys read":       KeysReadScript(),
		"status":          StatusScript(tg),
		"keys add":        KeysAddScript(),
		"keys remove":     KeysRemoveScript(),
		"destroy":         DestroyScript(tg, "app"),
		"environments":    EnvironmentsScript(&Target{Host: "h", Project: &Project{Name: "app"}}),
		"environments+i":  EnvironmentsScript(&Target{Host: "h", Project: &Project{Name: "app", Instances: true}}),
		"role holders":    RoleHoldersScript(tg, "proxy"),
		"root shell":      RootShellScript(&Target{Host: "h"}),
		"upgrade":         UpgradeScript([]string{"v1.2.3"}),
		"activate+sync":   ActivateScript(tg, "20261005120000", false, "app", Phases{Prepare: [][]string{{"secrets"}}, Run: []string{"deploy"}}, &SyncOptions{From: "vm1:prod", Args: []string{"--exclude=it's"}}),
		"sync only":       ActivateScript(tg, "20261005120000", false, "app", Phases{}, &SyncOptions{From: "vm1:prod"}),
		"holder":          HolderScript(tg, "app-dev", &SyncOptions{From: "vm1:prod", Args: []string{"-v"}, Inherit: true}, []string{"deploy"}),
		"holder stale":    HolderScript(tg, "app-dev", &SyncOptions{From: "vm1:prod"}, []string{"deploy"}),
		"push":            PushScript(tg, "app-prod", map[string]string{SyncToEnv: "devopsy@h2", SyncToDirEnv: "/srv/it's/current/.devopsy"}),
		"current":         CurrentExistsScript(tg),
		"receive":         SyncReceiveScript("/srv/it's/current/.devopsy", "20001", []string{"storage", "."}, nil, []string{"--exclude=x y"}),
		"receive bare":    SyncReceiveScript("/srv/app/current/.devopsy", "20001", []string{"storage", "app.ini"}, []string{"app.ini"}, nil),
		"exec":            SyncExecScript("/srv/it's/current/.devopsy", "app-dev", map[string]string{SyncArgsEnv: "--x 'y'", SyncFromTargetEnv: "vm1:prod"}, []string{"--sync-services", "import", "db"}),
		"export":          ExportScript(tg, "app-prod", "it's db"),
		"import":          ImportScript(tg, "app-prod", "it's db", "\nend 0a1b\n"),
	}
	for name, s := range scripts {
		if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s\n%s", name, err, out, s)
		}
	}
}

func TestFormatReleases(t *testing.T) {
	out := "20261005120000\tcurrent\t-\tregistry/app:abc mariadb:11 \t" + `{"id":"20261005120000","mode":"image","by":"ariel@mac","commit":"0123456789abcdef","branch":"main","dirty":true}` + "\n" +
		"20261004120000\t-\tfailed\t\t\n"
	got := FormatReleases(out)
	for _, want := range []string{"* 20261005120000", "main@0123456789+dirty", "\n    registry/app:abc mariadb:11\n  20261004120000", "FAILED"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if FormatReleases("") != "No releases yet.\n" {
		t.Error("empty output")
	}
}

// fakeDevopsy puts a devopsy on PATH that prints where it runs.
func fakeDevopsy(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	fake := "#!/bin/sh\nif [ \"$1\" = --release-settings ]; then printf 'root=%s\\nkeep=5\\nmax_keep=5\\nprotocol=1\\n' \"${FAKE_ROOT:-/nowhere}\"; exit; fi\necho \"ran in $PWD: $*\"\n"
	if err := os.WriteFile(filepath.Join(bin, "devopsy"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	return "PATH=" + bin + ":/usr/bin:/bin"
}

func TestPlainDirectories(t *testing.T) {
	path := fakeDevopsy(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	run := func(script string) (string, error) {
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = []string{path, "FAKE_ROOT=" + root}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	plain := filepath.Join(root, "traefik", "main")
	if err := os.MkdirAll(filepath.Join(plain, ".devopsy"), 0o755); err != nil {
		t.Fatal(err)
	}
	tg := &Target{Name: "vm1-traefik", Host: "h", Path: "traefik/main"}
	if out, err := run(RunScript(tg, "", []string{"proxies"})); err != nil || !strings.Contains(out, "ran in "+plain+": proxies") {
		t.Fatalf("plain run: %v\n%s", err, out)
	}
	for name, script := range map[string]string{
		"upload":   UploadScript(tg, "20261006000000"),
		"rollback": ActivateScript(tg, "", true, "", Phases{}, nil),
		"releases": ReleasesScript(tg),
	} {
		out, err := run(script)
		if err == nil || !strings.Contains(out, "plain devopsy directory") {
			t.Errorf("%s on a plain directory must refuse: %v\n%s", name, err, out)
		}
	}
	if _, err := os.Stat(filepath.Join(plain, "releases")); err == nil {
		t.Error("refusing must not create releases/")
	}

	released := filepath.Join(root, "app", "prod")
	if err := os.MkdirAll(filepath.Join(released, "current"), 0o755); err != nil {
		t.Fatal(err)
	}
	tr := &Target{Name: "prod", Host: "h", Path: "app/prod"}
	if out, err := run(RunScript(tr, "", []string{"ps"})); err != nil || !strings.Contains(out, "ran in "+released+"/current: ps") {
		t.Fatalf("release run: %v\n%s", err, out)
	}

	// --shell opens $SHELL where commands run.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "fakeshell"), []byte("#!/bin/sh\necho \"shell $* in $PWD\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for dir, tg := range map[string]*Target{plain: tg, released + "/current": tr} {
		if out, err := run("SHELL=" + bin + "/fakeshell; " + ShellScript(tg)); err != nil || !strings.Contains(out, "shell -l in "+dir) {
			t.Errorf("shell in %s: %v\n%s", dir, err, out)
		}
	}

	if err := os.MkdirAll(filepath.Join(root, "app", "new"), 0o755); err != nil {
		t.Fatal(err)
	}
	empty := &Target{Name: "new", Host: "h", Path: "app/new"}
	if out, err := run(RunScript(empty, "", []string{"ps"})); err == nil || !strings.Contains(out, "no release and no .devopsy/") {
		t.Fatalf("empty: %v\n%s", err, out)
	}
}

func TestDirtyOutsideDevopsy(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	root := filepath.Join(repo, "app")
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	write(t, filepath.Join(root, ".devopsy", "compose.yaml"), "services: {}\n")
	write(t, filepath.Join(root, "index.php"), "<?php\n")
	write(t, filepath.Join(repo, "elsewhere.txt"), "x\n")
	git("add", "-A")
	git("commit", "-q", "-m", "init")

	if DirtyOutsideDevopsy(root) {
		t.Error("clean tree reported dirty")
	}
	write(t, filepath.Join(root, ".devopsy", "compose.yaml"), "services: {web: {}}\n")
	write(t, filepath.Join(repo, "elsewhere.txt"), "changed\n")
	if DirtyOutsideDevopsy(root) {
		t.Error("changes in .devopsy/ or outside the project count as dirty")
	}
	write(t, filepath.Join(root, "index.php"), "<?php echo 1;\n")
	if !DirtyOutsideDevopsy(root) {
		t.Error("a changed project file is not reported")
	}
}

// A new release runs devopsy --prepare-release before it becomes current,
// only with role or import labels, and stops there when it fails.
func TestPrepareScript(t *testing.T) {
	bin := t.TempDir()
	// The fake devopsy records where it ran, and fails in a FAIL directory.
	fake := "#!/bin/sh\npwd > ran\n[ ! -f .devopsy/FAIL ]\n"
	if err := os.WriteFile(filepath.Join(bin, "devopsy"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	script := NotLive + fmt.Sprintf(PrepareScript, "devopsy --prepare-release")
	for name, c := range map[string]struct {
		compose, override string
		fail, runs        bool
	}{
		"no labels":       {compose: "services: {}\n"},
		"import":          {compose: "services:\n  a:\n    labels: [devopsy.import.X=proxy/X]\n", runs: true},
		"role":            {compose: "services:\n  a:\n    labels: {devopsy.role: proxy}\n", runs: true},
		"in the override": {compose: "services: {}\n", override: "services:\n  a:\n    labels: [devopsy.role=proxy]\n", runs: true},
		"fails":           {compose: "services:\n  a:\n    labels: [devopsy.role=proxy]\n", runs: true, fail: true},
		"in a comment":    {compose: "# devopsy.role=proxy\nservices: {}\n"},
	} {
		base := t.TempDir()
		rel := filepath.Join(base, "releases", "1")
		write(t, filepath.Join(rel, CodeDir, ".devopsy", "compose.yaml"), c.compose)
		if c.override != "" {
			write(t, filepath.Join(rel, CodeDir, ".devopsy", "compose.override.yaml"), c.override)
		}
		if c.fail {
			write(t, filepath.Join(rel, CodeDir, ".devopsy", "FAIL"), "")
		}
		cmd := exec.Command("sh", "-c", "set -eu\nbase="+Quote(base)+"\nid=1\nprev=releases/0\nrel="+Quote(rel)+"\ncode="+Quote(filepath.Join(rel, CodeDir))+"\n"+script+"echo after\n")
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
		out, err := cmd.CombinedOutput()
		_, ranErr := os.Stat(filepath.Join(rel, CodeDir, "ran"))
		if (ranErr == nil) != c.runs {
			t.Errorf("%s: ran %v, want %v", name, ranErr == nil, c.runs)
		}
		_, failedErr := os.Stat(filepath.Join(rel, FailedFile))
		if c.fail != (err != nil) || c.fail != (failedErr == nil) || c.fail == strings.Contains(string(out), "after") {
			t.Errorf("%s: err %v, marked failed %v:\n%s", name, err, failedErr == nil, out)
		}
	}
}

// A first release that fails before going live, with nothing in shared/,
// leaves no directory; with secrets already set, it stays.
func TestPrepareScriptFirstRelease(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(filepath.Join(bin, "devopsy"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := NotLive + fmt.Sprintf(PrepareScript, "devopsy --prepare-release")
	for _, secrets := range []bool{false, true} {
		root := t.TempDir()
		base := filepath.Join(root, "shop", "b", "prod")
		rel := filepath.Join(base, "releases", "1")
		write(t, filepath.Join(rel, CodeDir, ".devopsy", "compose.yaml"), "services:\n  a:\n    labels: [devopsy.role=proxy]\n")
		write(t, filepath.Join(base, "shared", ".env"), "")
		if secrets {
			write(t, filepath.Join(base, "shared", ".env"), "TOKEN=x\n")
		}
		cmd := exec.Command("sh", "-c", "set -eu\nbase="+Quote(base)+"\nid=1\nprev=\nrel="+Quote(rel)+"\ncode="+Quote(filepath.Join(rel, CodeDir))+"\n"+script)
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("want a failure:\n%s", out)
		}
		_, baseErr := os.Stat(base)
		_, instanceErr := os.Stat(filepath.Dir(base))
		if secrets && baseErr != nil {
			t.Errorf("removed despite shared/.env:\n%s", out)
		}
		if !secrets && (baseErr == nil || instanceErr == nil || !strings.Contains(string(out), "did not go live")) {
			t.Errorf("left behind: base %v, instance %v:\n%s", baseErr, instanceErr, out)
		}
	}
}

// A release with a sync: once current, the services are prepared, the
// marker printed, and devopsy's answer decides: go finishes and runs the
// run step; abort, or a failing prepare, restores and does not go live.
func TestActivateSync(t *testing.T) {
	bin := t.TempDir()
	// The fake devopsy logs its calls (cwd basename and arguments), and fails
	// --sync-services prepare when told.
	fake := `#!/bin/sh
if [ "$1" = --release-settings ]; then printf 'root=%s\nkeep=5\nmax_keep=5\nprotocol=1\n' "$FAKE_ROOT"; exit; fi
echo "$* from=${DEVOPSY_SYNC_FROM_TARGET:-} to=${DEVOPSY_SYNC_TO_TARGET:-} args=${DEVOPSY_SYNC_ARGS:-}${DEVOPSY_INTERNAL_INHERIT_FILE:+ file=$DEVOPSY_INTERNAL_INHERIT_FILE}" >> "$FAKE_ROOT/calls"
if [ "$*" = "--sync-services prepare" ] && [ -n "${FAKE_HANGUP:-}" ]; then
  echo PREPARING >&2
  while [ ! -e "$FAKE_ROOT/closed" ]; do sleep 0.05; done
fi
[ "$*" = "--sync-services prepare" ] && exit ${FAIL_PREPARE:-0}
exit 0
`
	write(t, filepath.Join(bin, "devopsy"), fake)
	// flock, mv -T, a fake docker, and the real --images.
	fakeImageTools(t, bin)
	tg := &Target{Name: "dev", Address: "vm2:dev", Path: "app/dev"}
	phases := Phases{Run: []string{"deploy"}}
	opts := &SyncOptions{From: "vm1:prod", Args: []string{"-v"}}
	run := func(root, id, stdin string, extra ...string) (stdout, stderr string, err error) {
		t.Helper()
		write(t, filepath.Join(root, tg.Path, "releases", id, CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
		cmd := exec.Command("sh", "-c", ActivateScript(tg, id, false, "app-dev", phases, opts))
		cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}, extra...)
		cmd.Stdin = strings.NewReader(stdin)
		var o, e strings.Builder
		cmd.Stdout, cmd.Stderr = &o, &e
		err = cmd.Run()
		return o.String(), e.String(), err
	}
	calls := func(root string) string {
		data, _ := os.ReadFile(filepath.Join(root, "calls"))
		os.Remove(filepath.Join(root, "calls"))
		return string(data)
	}
	current := func(root string) string {
		l, _ := os.Readlink(filepath.Join(root, tg.Path, "current"))
		return l
	}

	// go, on a first release.
	root := t.TempDir()
	out, errOut, err := run(root, "1", "go\n")
	base := filepath.Join(root, tg.Path)
	if err != nil || out != SyncMarker+base+"\n" || current(root) != "releases/1/code" {
		t.Fatalf("go (%v), current %q:\nstdout %q\n%s", err, current(root), out, errOut)
	}
	// The capability's hooks run from --sync-services, never the script;
	// the run step after it is no sync's: none of its variables.
	file := " file=" + filepath.Join(base, ".devopsy-sync.inherit.env")
	if got := calls(root); got != "--check-names "+base+" from= to= args=\n--missing-vars from= to= args=\n-- pull --ignore-buildable from= to= args=\n-- build --pull from= to= args=\n-- config --images from= to= args=\n--sync-services prepare from=vm1:prod to=vm2:dev args=-v"+file+"\n--sync-services finish go from=vm1:prod to=vm2:dev args=-v"+file+"\ndeploy from= to= args=\n" {
		t.Errorf("go calls:\n%s", got)
	}

	// abort, on a later release: back to the previous one, restarted.
	_, errOut, err = run(root, "2", "abort\n")
	if err == nil || current(root) != "releases/1/code" || !strings.Contains(errOut, "sync failed (1), current is back to 1") {
		t.Fatalf("abort (%v), current %q:\n%s", err, current(root), errOut)
	}
	if _, err := os.Stat(filepath.Join(base, "releases", "2", FailedFile)); err != nil {
		t.Errorf("not marked failed: %v", err)
	}
	if got := calls(root); !strings.Contains(got, "--sync-services finish abort") || !strings.Contains(got, "--release-steps restart run") || strings.Contains(got, "deploy from") {
		t.Errorf("abort calls:\n%s", got)
	}

	// abort, or no answer, on a first release: nothing current, the
	// directory kept (the data is partly there).
	for _, stdin := range []string{"abort\n", ""} {
		root := t.TempDir()
		_, errOut, err := run(root, "1", stdin)
		if err == nil || current(root) != "" || !strings.Contains(errOut+readFile(filepath.Join(root, tg.Path, ".devopsy-sync.log")), "no previous release to go back to") {
			t.Fatalf("first abort %q (%v), current %q:\n%s", stdin, err, current(root), errOut)
		}
		if got := calls(root); !strings.Contains(got, "--sync-services finish") || strings.Contains(got, "deploy") {
			t.Errorf("first abort %q calls:\n%s", stdin, got)
		}
	}

	// A failing prepare prints no marker, restores, and does not go live.
	root = t.TempDir()
	out, errOut, err = run(root, "1", "go\n", "FAIL_PREPARE=3")
	if err == nil || out != "" || current(root) != "" || !strings.Contains(errOut, "preparing the sync failed (3)") {
		t.Fatalf("failing prepare (%v), current %q:\nstdout %q\n%s", err, current(root), out, errOut)
	}
	if got := calls(root); !strings.Contains(got, "--sync-services finish") || strings.Contains(got, "deploy") {
		t.Errorf("failing prepare calls:\n%s", got)
	}

	// A re-sync's pending inherited variables: read by the sync (the
	// variable names them), moved into shared/inherit.env on go and linked
	// into the release, removed on abort; a stale one, from a sync that
	// wrote none this time, goes before the sync reads anything.
	root = t.TempDir()
	base = filepath.Join(root, tg.Path)
	pending := filepath.Join(base, ".devopsy-sync.inherit.env")
	opts.Inherit = true
	write(t, pending, "SYNC_DB='stream'\n")
	if _, errOut, err := run(root, "1", "go\n"); err != nil {
		t.Fatalf("pending go (%v):\n%s", err, errOut)
	}
	if got := readFile(filepath.Join(base, "shared", "inherit.env")); got != "SYNC_DB='stream'\n" {
		t.Errorf("not moved into place: %q", got)
	}
	if got := readFile(filepath.Join(base, "current", ".devopsy", "inherit.env")); got != "SYNC_DB='stream'\n" {
		t.Errorf("not linked into the release: %q", got)
	}
	if _, err := os.Stat(pending); err == nil {
		t.Errorf("the pending file stays after go")
	}
	write(t, pending, "SYNC_DB='rsync'\n")
	if _, _, err := run(root, "2", "abort\n"); err == nil {
		t.Fatal("abort succeeded")
	}
	if _, err := os.Stat(pending); err == nil || readFile(filepath.Join(base, "shared", "inherit.env")) != "SYNC_DB='stream'\n" {
		t.Errorf("abort: pending %v, shared %q", err, readFile(filepath.Join(base, "shared", "inherit.env")))
	}
	opts.Inherit = false
	write(t, pending, "STALE=1\n")
	if _, errOut, err := run(root, "3", "go\n"); err != nil || readFile(filepath.Join(base, "shared", "inherit.env")) != "SYNC_DB='stream'\n" {
		t.Errorf("stale pending (%v): %q\n%s", err, readFile(filepath.Join(base, "shared", "inherit.env")), errOut)
	}
	if got := calls(root); !strings.Contains(got, "--sync-services prepare from=vm1:prod to=vm2:dev args=-v file="+pending) {
		t.Errorf("the pending file's variable:\n%s", got)
	}

	// devopsy goes away while the services are being stopped (its session
	// closed: writes to it fail): they are restored all the same, prepare
	// failing or not, in a release and in --sync.
	hangup := func(root, script string, extra ...string) {
		t.Helper()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root, "FAKE_HANGUP=1"}, extra...)
		cmd.Stdin = strings.NewReader("")
		cmd.Stdout, cmd.Stderr = w, w
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		w.Close()
		buf := make([]byte, 4096)
		var seen string
		for !strings.Contains(seen, "PREPARING") {
			n, err := r.Read(buf)
			if err != nil {
				t.Fatalf("no PREPARING: %v\n%s", err, seen)
			}
			seen += string(buf[:n])
		}
		r.Close()
		write(t, filepath.Join(root, "closed"), "")
		_ = cmd.Wait()
	}
	for _, fail := range []string{"0", "3"} {
		root := t.TempDir()
		write(t, filepath.Join(root, tg.Path, "releases", "1", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
		hangup(root, ActivateScript(tg, "1", false, "app-dev", phases, &SyncOptions{From: "vm1:prod"}), "FAIL_PREPARE="+fail)
		if got := calls(root); !strings.Contains(got, "--sync-services finish") || strings.Contains(got, "deploy") {
			t.Errorf("release, session gone, prepare exits %s: calls:\n%s", fail, got)
		}
		root = t.TempDir()
		write(t, filepath.Join(root, tg.Path, "releases", "1", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
		if err := os.Symlink("releases/1/code", filepath.Join(root, tg.Path, "current")); err != nil {
			t.Fatal(err)
		}
		hangup(root, HolderScript(tg, "app-dev", &SyncOptions{From: "vm1:prod"}, []string{"deploy"}), "FAIL_PREPARE="+fail)
		if got := calls(root); !strings.Contains(got, "--sync-services finish") || strings.Contains(got, "deploy") {
			t.Errorf("--sync, session gone, prepare exits %s: calls:\n%s", fail, got)
		}
	}
}

func readFile(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

// fakeImageTools writes into bin a docker whose image store is a file per
// tag under $FAKE_ROOT/images, holding the image's id (the id of the
// release that built or pulled it): tag, inspect (with the Id and RepoTags
// formats), rm (refused for the tags in $FAKE_IN_USE, as for a container's
// image), image prune (logged in $FAKE_ROOT/pruned) and ps -a --filter
// ancestor=<id> (the compose projects in $FAKE_ROOT/users/<id>), every command
// failing as with the daemon down when $FAKE_DOCKER_DOWN is set; plus a
// flock that takes every lock, unless $FAKE_LOCKED fails the image lock's
// (fd 8) exclusive ones (all: shared ones too) or $FAKE_ROOT/locked
// exists (every image lock), and the mv -T the scripts need.
func fakeImageTools(t *testing.T, bin string) {
	t.Helper()
	write(t, filepath.Join(bin, "docker"), `#!/bin/sh
f() { echo "$FAKE_ROOT/images/$(echo "$1" | tr '/' '_')"; }
if [ -n "${FAKE_DOCKER_DOWN:-}" ]; then
  echo "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?" >&2
  exit 1
fi
[ "$1" = version ] && exit 0
case "$1 $2" in
  "ps -a") sed 's/^/c /' "$FAKE_ROOT/users/${4#ancestor=}" 2>/dev/null; exit 0 ;;
  "image inspect")
    if [ "$3" != --format ]; then
      [ -f "$(f "$3")" ] || { echo "Error response from daemon: No such image: $3" >&2; exit 1; }
    elif [ "$4" = '{{.Id}}' ]; then
      [ -f "$(f "$5")" ] || { echo "Error response from daemon: No such image: $5" >&2; exit 1; }
      cat "$(f "$5")"
    else
      for t in "$FAKE_ROOT"/images/*; do
        [ -f "$t" ] && [ "$(cat "$t")" = "$5" ] || continue
        n=${t##*/}
        echo "$(echo "${n%:*}" | tr '_' '/'):${n##*:}"
      done
      exit 0
    fi ;;
  "image rm")
    case " ${FAKE_IN_USE:-} " in *" $3 "*) echo "conflict: a container uses $3" >&2; exit 1 ;; esac
    [ -f "$(f "$3")" ] || { echo "Error response from daemon: No such image: $3" >&2; exit 1; }
    rm -f "$(f "$3")" ;;
  "image prune") echo prune >>"$FAKE_ROOT/pruned"; echo "Total reclaimed space: 1MB" ;;
  *)
    [ "$1" = tag ] || exit 1
    # From a name, or from an image id (the content of some name's file).
    if [ -f "$(f "$2")" ]; then
      cp "$(f "$2")" "$(f "$3")"
    else
      for t in "$FAKE_ROOT"/images/*; do
        [ -f "$t" ] && [ "$(cat "$t")" = "$2" ] && { echo "$2" >"$(f "$3")"; exit 0; }
      done
      exit 1
    fi ;;
esac
`)
	write(t, filepath.Join(bin, "flock"), `#!/bin/sh
case " $* " in *[!8]" ") exit 0 ;; esac
[ ! -e "$FAKE_ROOT/locked" ] || exit 1
case " $* " in
  *" -s "*) [ "${FAKE_LOCKED:-}" != all ] ;;
  *) [ -z "${FAKE_LOCKED:-}" ] ;;
esac
`)
	write(t, filepath.Join(bin, "mv"), "#!/bin/sh\nif [ \"$1\" = -Tf ]; then rm -f \"$3\"; exec /bin/mv -f \"$2\" \"$3\"; fi\nexec /bin/mv \"$@\"\n")
	// The rules are devopsy's own: --images goes to the real one, built
	// for the tests, the rest to the test's fake.
	if err := os.Rename(filepath.Join(bin, "devopsy"), filepath.Join(bin, "devopsy.fake")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\nif [ \"$1\" = --images ]; then exec "+Quote(realDevopsy)+" \"$@\"; fi\nexec \"$(dirname \"$0\")/devopsy.fake\" \"$@\"\n")
	for _, f := range []string{"devopsy", "devopsy.fake", "docker", "flock", "mv"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeImage is the id a tag holds in fakeImageTools' store, "" without
// the tag.
func fakeImage(root, tag string) string {
	data, _ := os.ReadFile(filepath.Join(root, "images", strings.ReplaceAll(tag, "/", "_")))
	return strings.TrimSpace(string(data))
}

// A release gets its images by mode before it goes live and bookmarks
// them: a failure there puts the live release's names back, a failed run
// step and a rollback restore a release's own images without building or
// pulling, and pruning drops a release's bookmarks.
func TestActivateImages(t *testing.T) {
	bin := t.TempDir()
	// The fake devopsy logs its calls (cwd basename and arguments); its
	// pull and build put the release's id in the image store, and fail
	// when told. The fake docker's store is a file per tag.
	fake := `#!/bin/sh
if [ "$1" = --release-settings ]; then printf 'root=%s\nkeep=5\nmax_keep=5\nprotocol=1\n' "$FAKE_ROOT"; exit; fi
id=$(pwd -P); id=${id%/code}; id=${id##*/}
case $PWD in */current) at=current ;; *) at=$id ;; esac
echo "$at: $*" >> "$FAKE_ROOT/calls"
mkdir -p "$FAKE_ROOT/images"
case "$*" in
  "-- config --images") printf 'app-prod-app\nmariadb:11\nx@sha256:abc\n' ;;
  "-- pull --ignore-buildable" | "-- pull") echo "$id${PULLED:-}" > "$FAKE_ROOT/images/mariadb:11" ;;
  "-- build --pull")
    if [ -n "${FAIL_BUILD:-}" ]; then
      # Another release takes the image lock meanwhile.
      [ -z "${LOCK_AFTER:-}" ] || : >"$FAKE_ROOT/locked"
      exit "$FAIL_BUILD"
    fi
    echo "$id" > "$FAKE_ROOT/images/app-prod-app" ;;
  deploy) exit ${FAIL_RUN:-0} ;;
esac
exit 0
`
	write(t, filepath.Join(bin, "devopsy"), fake)
	fakeImageTools(t, bin)
	phases := Phases{Run: []string{"deploy"}}
	script := func(tg *Target, id string, rollback bool) string {
		return ActivateScript(tg, id, rollback, "app-prod", phases, nil)
	}
	run := func(root, script string, extra ...string) (string, error) {
		t.Helper()
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}, extra...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	calls := func(root string) string {
		data, _ := os.ReadFile(filepath.Join(root, "calls"))
		os.Remove(filepath.Join(root, "calls"))
		return string(data)
	}
	// image is what a tag holds: the id of the release that built or
	// pulled it, "" without the tag.
	image := func(root, tag string) string { return fakeImage(root, tag) }

	build := &Target{Name: "prod", Path: "app/prod", Mode: ModeBuild}
	root := t.TempDir()
	base := filepath.Join(root, build.Path)
	current := func() string {
		l, _ := os.Readlink(filepath.Join(base, "current"))
		return l
	}
	release := func(id string, extra ...string) (string, error) {
		t.Helper()
		write(t, filepath.Join(base, "releases", id, CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
		return run(root, script(build, id, false), extra...)
	}

	if out, err := release("1"); err != nil || current() != "releases/1/code" {
		t.Fatalf("build (%v):\n%s", err, out)
	}
	if got, want := calls(root), "1: --check-names "+base+"\n1: --missing-vars\n1: -- pull --ignore-buildable\n1: -- build --pull\n1: -- config --images\ncurrent: deploy\n"; got != want {
		t.Errorf("build calls:\n%s\nwant:\n%s", got, want)
	}
	if image(root, "app-prod-app:devopsy-app-prod-1") != "1" || image(root, "mariadb:devopsy-app-prod-1") != "1" {
		t.Errorf("release 1 not bookmarked")
	}

	// A failed build: not live, no run step, and the pull it made before
	// failing is undone: mariadb:11 is release 1's again.
	if out, err := release("2", "FAIL_BUILD=3"); err == nil || current() != "releases/1/code" || !strings.Contains(out, "release 2 not made current") {
		t.Fatalf("failed build (%v), current %q:\n%s", err, current(), out)
	}
	if got := calls(root); strings.Contains(got, "deploy") {
		t.Errorf("failed build ran the run step:\n%s", got)
	}
	if got := image(root, "mariadb:11"); got != "1" {
		t.Errorf("after a failed build mariadb:11 is %q's, want 1's", got)
	}
	// What it got is bookmarked, so it goes with the release.
	if image(root, "mariadb:devopsy-app-prod-2") != "2" {
		t.Errorf("the failed build's pull is not bookmarked")
	}

	// A failed run step: release 1's images restored, nothing built or
	// pulled for it, then its restart.
	if out, err := release("3", "FAIL_RUN=4"); err == nil || current() != "releases/1/code" || !strings.Contains(out, "images of 1 restored") {
		t.Fatalf("failed run (%v), current %q:\n%s", err, current(), out)
	}
	if got, want := calls(root), "3: --check-names "+base+"\n3: --missing-vars\n3: -- pull --ignore-buildable\n3: -- build --pull\n3: -- config --images\ncurrent: deploy\ncurrent: --release-steps restart run\n"; got != want {
		t.Errorf("failed run calls:\n%s\nwant:\n%s", got, want)
	}
	if image(root, "app-prod-app") != "1" || image(root, "mariadb:11") != "1" {
		t.Errorf("after a failed run the images are not release 1's: %q %q", image(root, "app-prod-app"), image(root, "mariadb:11"))
	}

	// Another failed run, while another compose project runs the
	// mariadb:11 it pulled: that name stays on it, the rest go back.
	write(t, filepath.Join(root, "users", "35m"), "stage-prod\napp-prod\n")
	if out, err := release("35", "FAIL_RUN=4", "PULLED=m"); err == nil || current() != "releases/1/code" || !strings.Contains(out, "mariadb:11 stays on its newer image, which other environments use (stage-prod): 1 runs it too") {
		t.Fatalf("failed run with mariadb:11 shared (%v):\n%s", err, out)
	}
	if image(root, "app-prod-app") != "1" || image(root, "mariadb:11") != "35m" {
		t.Errorf("after a failed run with a shared name: app-prod-app %q, mariadb:11 %q", image(root, "app-prod-app"), image(root, "mariadb:11"))
	}

	// A rollback from 4 to 1 (2, 3 and 35 failed) restores 1's images.
	if out, err := release("4"); err != nil || image(root, "app-prod-app") != "4" {
		t.Fatalf("release 4 (%v):\n%s", err, out)
	}
	calls(root)
	if out, err := run(root, script(build, "", true)); err != nil || current() != "releases/1/code" || image(root, "app-prod-app") != "1" {
		t.Fatalf("rollback (%v), current %q, app %q:\n%s", err, current(), image(root, "app-prod-app"), out)
	}
	if got := calls(root); strings.Contains(got, "pull") || strings.Contains(got, "build") {
		t.Errorf("rollback fetched images:\n%s", got)
	}

	// Pruning drops the bookmarks of the releases it removes.
	keep1 := *build
	keep1.Keep = 1
	write(t, filepath.Join(base, "releases", "5", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
	if out, err := run(root, script(&keep1, "5", false)); err != nil {
		t.Fatalf("release 5 (%v):\n%s", err, out)
	}
	if image(root, "app-prod-app:devopsy-app-prod-1") != "" || image(root, "app-prod-app:devopsy-app-prod-4") != "" || image(root, "app-prod-app:devopsy-app-prod-5") != "5" {
		t.Errorf("pruning kept bookmarks of pruned releases, or dropped 5's")
	}

	// A first release whose build fails leaves no directory, and the
	// images it got go first: the directory was their only record. With
	// the server's image lock busy, it stays, marked failed.
	first := func(env ...string) (string, string, error) {
		root := t.TempDir()
		write(t, filepath.Join(root, build.Path, "releases", "1", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
		out, err := run(root, script(build, "1", false), append([]string{"FAIL_BUILD=3"}, env...)...)
		return root, out, err
	}
	r, out, err := first()
	if _, statErr := os.Stat(filepath.Join(r, build.Path)); err == nil || statErr == nil || image(r, "mariadb:devopsy-app-prod-1") != "" || image(r, "mariadb:11") != "" {
		t.Errorf("failed first release (%v):\n%s", err, out)
	}
	r, out, err = first("FAKE_LOCKED=1")
	if _, statErr := os.Stat(filepath.Join(r, build.Path, "releases", "1", FailedFile)); err == nil || statErr != nil || image(r, "mariadb:devopsy-app-prod-1") != "1" || !strings.Contains(out, "keeping "+filepath.Join(r, build.Path)) {
		t.Errorf("failed first release, image lock busy (%v):\n%s", err, out)
	}
	// Never got the lock to fetch: nothing to undo, so no second wait.
	r, out, err = first("FAKE_LOCKED=all")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 75 || strings.Count(out, "waiting for the server's images") != 1 || strings.Contains(out, "-- pull") {
		t.Errorf("release with the image lock taken (%v):\n%s", err, out)
	}
	if _, statErr := os.Stat(filepath.Join(r, build.Path, "releases", "1", FetchingFile)); statErr == nil {
		t.Errorf("a release that never fetched kept its marker")
	}
	// The fetch failed and another release takes the lock as soon as it
	// goes: the undo ran before (the fetch's pull bookmarked, no marker
	// left), and the directory stays, failed, for a later removal.
	r, out, err = first("LOCK_AFTER=1")
	rel1 := filepath.Join(r, build.Path, "releases", "1")
	if err == nil || !fileExists(filepath.Join(rel1, FailedFile)) || fileExists(filepath.Join(rel1, FetchingFile)) || image(r, "mariadb:devopsy-app-prod-1") != "1" || !strings.Contains(out, "keeping "+filepath.Join(r, build.Path)) {
		t.Errorf("failed first release, lock taken after the fetch (%v):\n%s", err, out)
	}

	image2 := &Target{Name: "prod", Path: "app/prod", Mode: ModeImage}
	root = t.TempDir()
	write(t, filepath.Join(root, image2.Path, "releases", "1", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
	if out, err := run(root, script(image2, "1", false)); err != nil {
		t.Fatalf("image (%v):\n%s", err, out)
	}
	if got, want := calls(root), "1: --check-names "+filepath.Join(root, image2.Path)+"\n1: --missing-vars\n1: -- pull\n1: -- config --images\ncurrent: deploy\n"; got != want {
		t.Errorf("image calls:\n%s\nwant:\n%s", got, want)
	}
}

// Images go with the last release that holds them: two instances share
// an image, and pruning one's release only removes its bookmark; the
// other's pruning removes the names it listed that still point to the
// image, never one that moved on, and never one a container uses.
// Pruning skips while the server's image lock is taken, naming who holds
// it; --destroy then fails having removed nothing.
func TestImagesRemoved(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, "devopsy"), `#!/bin/sh
if [ "$1" = --release-settings ]; then printf 'root=%s\nkeep=1\nmax_keep=5\nprotocol=1\n' "$FAKE_ROOT"; exit; fi
echo "$*" >>"$FAKE_ROOT/calls"
mkdir -p "$FAKE_ROOT/images"
case "$*" in
  "-- config --images") printf 'registry/app:%s\nmariadb:11\n' "$TAG" ;;
  "-- pull") echo "$IMG" >"$FAKE_ROOT/images/registry_app:$TAG"; echo "$IMG" >"$FAKE_ROOT/images/mariadb:11" ;;
esac
exit 0
`)
	fakeImageTools(t, bin)
	root := t.TempDir()
	target := func(instance string) *Target {
		return &Target{Name: "prod", Instance: instance, Path: "app/" + instance + "/prod", Mode: ModeImage, Project: &Project{Name: "app", Instances: true}}
	}
	run := func(script string, env ...string) (string, error) {
		t.Helper()
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}, env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	release := func(instance, id, tag, img string, env ...string) string {
		t.Helper()
		tg := target(instance)
		write(t, filepath.Join(root, tg.Path, "releases", id, CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
		out, err := run(ActivateScript(tg, id, false, "", Phases{Run: []string{"deploy"}}, nil), append([]string{"TAG=" + tag, "IMG=" + img}, env...)...)
		if err != nil {
			t.Fatalf("release %s of %s (%v):\n%s", id, instance, err, out)
		}
		return out
	}
	exists := func(path string) bool {
		_, err := os.Stat(filepath.Join(root, path))
		return err == nil
	}

	release("a", "1", "abc", "X")
	release("b", "1", "abc", "X")
	if fakeImage(root, "registry/app:devopsy-app-a-prod-1") != "X" || fakeImage(root, "registry/app:devopsy-app-b-prod-1") != "X" {
		t.Fatalf("instances not bookmarked")
	}

	// a's next release prunes a's first: b's bookmark holds X.
	if out := release("a", "2", "def", "Y"); !strings.Contains(out, "the images of 1 that other releases hold stay: registry/app:abc, mariadb:11") {
		t.Errorf("pruning a release whose images others hold says nothing:\n%s", out)
	}
	if exists("app/a/prod/releases/1") || fakeImage(root, "registry/app:devopsy-app-a-prod-1") != "" {
		t.Errorf("a's release 1 not pruned")
	}
	if fakeImage(root, "registry/app:abc") != "X" {
		t.Errorf("registry/app:abc went while b's release held it")
	}

	// The server's image lock taken: pruning skips, naming the holder.
	holders := filepath.Join(root, ImagesHolders)
	write(t, filepath.Join(holders, strconv.Itoa(os.Getpid())), fmt.Sprintf("%d other-prod fetching\n", time.Now().Unix()-75))
	write(t, filepath.Join(holders, "999999"), fmt.Sprintf("%d gone-prod fetching\n", time.Now().Unix()))
	out := release("b", "2", "def", "Y", "FAKE_LOCKED=1")
	if !strings.Contains(out, "pruning skipped: the server's images are in use by other-prod (fetching, 1m1") || !strings.Contains(out, "the next release prunes") || !exists("app/b/prod/releases/1") {
		t.Errorf("pruning with the lock taken:\n%s", out)
	}
	if exists(filepath.Join(ImagesHolders, "999999")) {
		t.Errorf("a gone holder's file stays")
	}
	os.RemoveAll(holders)

	// b's next release prunes both of b's earlier ones: X loses its last
	// bookmark, so registry/app:abc goes; mariadb:11 moved on to Y and
	// stays, and Y is held by a's and b's live releases.
	out = release("b", "3", "def", "Y")
	if exists("app/b/prod/releases/1") || exists("app/b/prod/releases/2") {
		t.Errorf("b's releases 1 and 2 not pruned:\n%s", out)
	}
	if fakeImage(root, "registry/app:abc") != "" || !strings.Contains(out, "removed registry/app:abc, which only 1 held") {
		t.Errorf("registry/app:abc stays with no release holding it:\n%s", out)
	}
	if fakeImage(root, "mariadb:11") != "Y" || fakeImage(root, "registry/app:def") != "Y" {
		t.Errorf("names of the live releases went")
	}
	// Once per pruning that removed releases: a's and this one.
	if got := readFile(filepath.Join(root, "pruned")); got != "prune\nprune\n" {
		t.Errorf("image prune ran %q", got)
	}

	// --destroy removes every release's images; a name a container uses
	// stays, with Docker's reason. b's release 3 runs Z alone here.
	write(t, filepath.Join(root, "app/b/prod/releases/3", ImagesFile), "registry/app:def\tZ\n")
	write(t, filepath.Join(root, "images", "registry_app:devopsy-app-b-prod-3"), "Z\n")
	write(t, filepath.Join(root, "images", "registry_app:def"), "Z\n")
	// A release whose fetch was cut off is recovered first, so what it
	// got goes too.
	write(t, filepath.Join(root, "app/b/prod/releases/4", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
	write(t, filepath.Join(root, "app/b/prod/releases/4", FetchingFile), "")
	out, err := run(DestroyScript(target("b"), ""), "FAKE_IN_USE=registry/app:def", "TAG=ghi")
	if err != nil || !strings.Contains(out, "registry/app:def stays: conflict: a container uses registry/app:def") || exists("app/b/prod") {
		t.Errorf("destroy b (%v):\n%s", err, out)
	}

	if !strings.Contains(out, "fetch of release 4 was interrupted") {
		t.Errorf("destroy did not recover release 4:\n%s", out)
	}
	// What Docker refused is recorded in the project's directory, which
	// outlives the environment.
	left := filepath.Join(root, "app", ImagesLeftFile)
	if got := readFile(left); !strings.Contains(got, "\tapp-b-prod\t3\tregistry/app:def\tZ\tconflict: a container uses registry/app:def\n") {
		t.Errorf("registry/app:def not recorded: %q", got)
	}

	// --releases remove checks again under the lock: the current release,
	// or one gone meanwhile, refuses them all.
	write(t, filepath.Join(root, "app/a/prod/releases/1", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
	for _, ids := range [][]string{{"1", "2"}, {"1", "9"}} {
		out, err := run(ReleasesRemoveScript(target("a"), "", ids))
		if err == nil || !strings.Contains(out, "nothing removed") || !exists("app/a/prod/releases/1") {
			t.Errorf("remove %v (%v):\n%s", ids, err, out)
		}
	}
	// A bookmark Docker refuses to remove (the image's last name, a
	// stopped container using it) says so: nothing lists it again.
	write(t, filepath.Join(root, "app/a/prod/releases/1", ImagesFile), "mariadb:10\n")
	write(t, filepath.Join(root, "images", "mariadb:10"), "W\n")
	write(t, filepath.Join(root, "images", "mariadb:devopsy-app-a-prod-1"), "W\n")
	// Another project's record is its own: left alone.
	write(t, filepath.Join(root, "other", ImagesLeftFile), "1\tother-prod\t9\tother:1\tV\tconflict\n")
	write(t, filepath.Join(root, "images", "other:1"), "V\n")
	// The recorded registry/app:def is retried: no container uses it now.
	if out, err := run(ReleasesRemoveScript(target("a"), "", []string{"1"}), "FAKE_IN_USE=mariadb:devopsy-app-a-prod-1"); err != nil || exists("app/a/prod/releases/1") || !strings.Contains(out, "removed release 1") || !strings.Contains(out, "mariadb:devopsy-app-a-prod-1 stays: conflict") || !strings.Contains(out, "removed registry/app:def, left by release 3 of app-b-prod") || strings.Contains(out, "other:1") || !exists(filepath.Join("other", ImagesLeftFile)) {
		t.Errorf("remove 1 (%v):\n%s", err, out)
	}
	if got := readFile(left); strings.Contains(got, "registry/app:def") || strings.Count(got, "\n") != 1 || !strings.Contains(got, "\tapp-a-prod\t1\tmariadb:devopsy-app-a-prod-1\tW\t") {
		t.Errorf("record after the retry: %q", got)
	}
	out, err = run(ReleasesScript(target("a")))
	if err != nil {
		t.Fatalf("releases (%v):\n%s", err, out)
	}
	if l := ParseLeft(out); len(l) != 1 || l[0].Ref != "mariadb:devopsy-app-a-prod-1" || l[0].Name != "app-a-prod" || l[0].Release != "1" || !strings.HasPrefix(l[0].Why, "conflict") {
		t.Errorf("left parsed: %+v\n%s", l, out)
	}
	if rs := ParseReleases(out); len(rs) != 1 || rs[0].ID != "2" {
		t.Errorf("releases parsed: %+v", rs)
	}
	if f := FormatReleases(out); !strings.Contains(f, "Images the project's removals left") || !strings.Contains(f, "  mariadb:devopsy-app-a-prod-1  app-a-prod, release 1, ") {
		t.Errorf("formatted:\n%s", f)
	}

	// --destroy with the lock taken: nothing removed.
	os.Remove(filepath.Join(root, "calls"))
	out, err = run(DestroyScript(target("a"), ""), "FAKE_LOCKED=1")
	if err == nil || !strings.Contains(out, "stays as it was") || !exists("app/a/prod/releases/2") || fakeImage(root, "registry/app:devopsy-app-a-prod-2") != "Y" {
		t.Errorf("destroy with the image lock taken (%v):\n%s", err, out)
	}
	if strings.Contains(readFile(filepath.Join(root, "calls")), "--destroy-data") {
		t.Errorf("destroy took data down without the image lock")
	}

	// A removal cut off after untagging is finished by the next one: the
	// bookmark went, the listing keeps the image's id.
	write(t, filepath.Join(root, "app/a/prod/releases/7", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
	write(t, filepath.Join(root, "app/a/prod/releases/7", ImagesFile), "old:1\tO\n")
	write(t, filepath.Join(root, "images", "old:1"), "O\n")
	if out, err := run(ReleasesRemoveScript(target("a"), "", []string{"7"}), "FAKE_IN_USE=mariadb:devopsy-app-a-prod-1"); err != nil || fakeImage(root, "old:1") != "" || exists("app/a/prod/releases/7") {
		t.Errorf("remove 7, cut off before (%v):\n%s", err, out)
	}

	// With Docker not answering, nothing is removed, the record included.
	out, err = run(DestroyScript(target("a"), ""), "FAKE_DOCKER_DOWN=1")
	if err == nil || !strings.Contains(out, "Docker is not answering") || !exists("app/a/prod/releases/2") || !exists(filepath.Join("app", ImagesLeftFile)) {
		t.Errorf("destroy with Docker down (%v):\n%s", err, out)
	}

	// Once its container is gone, the next removal removes what was left,
	// and the record goes with its last line, even one cut short of its
	// newline.
	write(t, left, strings.TrimSuffix(readFile(left), "\n"))
	if out, err := run(ReleasesScript(target("a"))); err != nil || len(ParseReleases(out)) != 1 || len(ParseLeft(out)) != 1 {
		t.Errorf("releases with a record cut short (%v):\n%s", err, out)
	}
	// Holder files are for messages only: one that cannot be written
	// (a full disk) does not stop the removal.
	os.RemoveAll(filepath.Join(root, ImagesHolders))
	write(t, filepath.Join(root, ImagesHolders), "")
	out, err = run(DestroyScript(target("a"), ""))
	if err != nil || !strings.Contains(out, "removed mariadb:devopsy-app-a-prod-1, left by release 1 of app-a-prod") || exists(filepath.Join("app", ImagesLeftFile)) {
		t.Errorf("destroy a (%v):\n%s", err, out)
	}
}

// Two tags of one repository in a release get a bookmark each, so a
// rollback puts each back on its own image, and a ref by digest gets one
// too, so docker image prune -f leaves a rollback's image alone.
func TestBookmarksPerRef(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, "devopsy"), `#!/bin/sh
if [ "$1" = --release-settings ]; then printf 'root=%s\nkeep=5\nmax_keep=5\nprotocol=1\n' "$FAKE_ROOT"; exit; fi
mkdir -p "$FAKE_ROOT/images"
case "$*" in
  "-- config --images") printf 'postgres:15\npostgres:16\nx@sha256:abc\n' ;;
  "-- pull") echo "A$IMG" >"$FAKE_ROOT/images/postgres:15"; echo "B$IMG" >"$FAKE_ROOT/images/postgres:16"; echo "D$IMG" >"$FAKE_ROOT/images/x@sha256:abc" ;;
esac
exit 0
`)
	fakeImageTools(t, bin)
	root := t.TempDir()
	tg := &Target{Name: "prod", Path: "app/prod", Mode: ModeImage, Project: &Project{Name: "app"}}
	run := func(script string, env ...string) {
		t.Helper()
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}, env...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v:\n%s", err, out)
		}
	}
	for _, id := range []string{"1", "2"} {
		write(t, filepath.Join(root, tg.Path, "releases", id, CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
		run(ActivateScript(tg, id, false, "", Phases{Run: []string{"deploy"}}, nil), "IMG="+id)
	}
	cksum := func(ref string) string {
		out, err := exec.Command("sh", "-c", "printf '%s' \"$0\" | cksum | cut -d ' ' -f 1", ref).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	if fakeImage(root, "postgres:devopsy-app-prod-1-"+cksum("postgres:15")) != "A1" || fakeImage(root, "postgres:devopsy-app-prod-1-"+cksum("postgres:16")) != "B1" {
		t.Errorf("postgres:15 and postgres:16 do not have a bookmark each")
	}
	if fakeImage(root, "x:devopsy-app-prod-1") != "D1" {
		t.Errorf("the ref by digest has no bookmark")
	}
	run(ActivateScript(tg, "", true, "", Phases{}, nil))
	if fakeImage(root, "postgres:15") != "A1" || fakeImage(root, "postgres:16") != "B1" {
		t.Errorf("rollback: postgres:15 is %q, postgres:16 %q", fakeImage(root, "postgres:15"), fakeImage(root, "postgres:16"))
	}
}

// An interrupted fetch leaves no stray images: a signal runs the trap
// (what was fetched bookmarked, the live release's names back, the release
// marked failed, 130), and a fetch whose session dropped without one leaves
// its marker, which the next release recovers the same way.
func TestFetchInterrupted(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, "devopsy"), `#!/bin/sh
if [ "$1" = --release-settings ]; then printf 'root=%s\nkeep=5\nmax_keep=5\nprotocol=1\n' "$FAKE_ROOT"; exit; fi
id=$(pwd -P); id=${id%/code}; id=${id##*/}
mkdir -p "$FAKE_ROOT/images"
case "$*" in
  "-- config --images") printf 'app-prod-app\nmariadb:11\n' ;;
  "-- pull --ignore-buildable") echo "$id" >"$FAKE_ROOT/images/mariadb:11" ;;
  "-- build --pull")
    if [ -n "${FAKE_HANG:-}" ]; then touch "$FAKE_ROOT/building"; sleep 30; exit 1; fi
    echo "$id" >"$FAKE_ROOT/images/app-prod-app" ;;
esac
exit 0
`)
	fakeImageTools(t, bin)
	root := t.TempDir()
	tg := &Target{Name: "prod", Path: "app/prod", Mode: ModeBuild}
	base := filepath.Join(root, tg.Path)
	command := func(id string, env ...string) *exec.Cmd {
		write(t, filepath.Join(base, "releases", id, CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
		cmd := exec.Command("sh", "-c", ActivateScript(tg, id, false, "app-prod", Phases{Run: []string{"deploy"}}, nil))
		cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}, env...)
		return cmd
	}
	exists := func(path string) bool {
		_, err := os.Stat(filepath.Join(base, path))
		return err == nil
	}
	if out, err := command("1").CombinedOutput(); err != nil {
		t.Fatalf("release 1 (%v):\n%s", err, out)
	}

	// Release 2's session dropped mid-fetch: its marker stays and the
	// names point at what it got.
	write(t, filepath.Join(base, "releases", "2", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
	write(t, filepath.Join(base, "releases", "2", FetchingFile), "")
	write(t, filepath.Join(root, "images", "app-prod-app"), "2\n")
	write(t, filepath.Join(root, "images", "mariadb:11"), "2\n")
	out, err := command("3").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "the images' fetch of release 2 was interrupted") {
		t.Fatalf("release 3 (%v):\n%s", err, out)
	}
	if !exists("releases/2/"+FailedFile) || exists("releases/2/"+FetchingFile) || fakeImage(root, "app-prod-app:devopsy-app-prod-2") != "2" {
		t.Errorf("release 2 not recovered:\n%s", out)
	}
	if exists("releases/3/" + FetchingFile) {
		t.Errorf("release 3 kept its marker")
	}

	// interrupt runs cmd until its build hangs, checks what it can only
	// see then, and sends SIGTERM to its process group.
	interrupt := func(cmd *exec.Cmd, during func()) (string, int) {
		t.Helper()
		os.Remove(filepath.Join(root, "building"))
		var buf bytes.Buffer
		cmd.Stdout, cmd.Stderr = &buf, &buf
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 100 && !fileExists(filepath.Join(root, "building")); i++ {
			time.Sleep(50 * time.Millisecond)
		}
		during()
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()
		return buf.String(), cmd.ProcessState.ExitCode()
	}

	// Release 4 gets SIGTERM while it builds, after pulling mariadb:11.
	out4, code := interrupt(command("4", "FAKE_HANG=1"), func() {})
	if code != 130 || !strings.Contains(out4, "the images' fetch was interrupted") || !strings.Contains(out4, "release 4 not made current") {
		t.Fatalf("interrupted release 4 (exit %d):\n%s", code, out4)
	}
	if !exists("releases/4/"+FailedFile) || exists("releases/4/"+FetchingFile) {
		t.Errorf("release 4 not marked failed, or kept its marker")
	}
	if fakeImage(root, "mariadb:devopsy-app-prod-4") != "4" || fakeImage(root, "mariadb:11") != "3" {
		t.Errorf("release 4's pull not bookmarked, or mariadb:11 not 3's again: %q", fakeImage(root, "mariadb:11"))
	}

	// A rollback to 1 (2 and 4 failed) whose bookmarks are gone fetches
	// again; interrupted, nothing changes: 3 stays current with its
	// images, and 1 is neither marked fetching nor failed, so a later
	// rollback still finds it, nor bookmarked from the names.
	for _, ref := range []string{"app-prod-app", "mariadb"} {
		os.Remove(filepath.Join(root, "images", ref+":devopsy-app-prod-1"))
	}
	rollback := exec.Command("sh", "-c", ActivateScript(tg, "", true, "app-prod", Phases{}, nil))
	rollback.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root, "FAKE_HANG=1"}
	outR, code := interrupt(rollback, func() {
		if exists("releases/1/" + FetchingFile) {
			t.Errorf("the rollback marked release 1 as fetching")
		}
	})
	if code != 130 || !strings.Contains(outR, "rolling back to 1") || !strings.Contains(outR, "the images' fetch was interrupted") || !strings.Contains(outR, "nothing changed: current is still 3") {
		t.Fatalf("interrupted rollback (exit %d):\n%s", code, outR)
	}
	if l, _ := os.Readlink(filepath.Join(base, "current")); l != "releases/3/code" {
		t.Errorf("current is %q after an interrupted rollback", l)
	}
	if exists("releases/1/"+FailedFile) || exists("releases/1/"+FetchingFile) {
		t.Errorf("release 1 marked failed or fetching by an interrupted rollback")
	}
	if fakeImage(root, "mariadb:11") != "3" || fakeImage(root, "app-prod-app") != "3" {
		t.Errorf("3's images not back after the rollback: mariadb:11 %q, app-prod-app %q", fakeImage(root, "mariadb:11"), fakeImage(root, "app-prod-app"))
	}
	// 1 keeps the bookmarks it had: what the names held when interrupted
	// (3's app, not built again yet) is not what 1 ran.
	for _, ref := range []string{"app-prod-app", "mariadb"} {
		if img := fakeImage(root, ref+":devopsy-app-prod-1"); img != "" {
			t.Errorf("the interrupted rollback bookmarked %s as release 1's: %q", ref, img)
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Pruning counts only releases that went live: a failed one between those
// kept stays, with its log, and older ones go.
func TestActivatePruneFailed(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\nif [ \"$1\" = --release-settings ]; then printf 'root=%s\\nkeep=5\\nmax_keep=5\\nprotocol=1\\n' \"$FAKE_ROOT\"; fi\nexit 0\n")
	// flock, mv -T, a fake docker, and the real --images.
	fakeImageTools(t, bin)
	root := t.TempDir()
	tg := &Target{Name: "prod", Path: "app/prod", Keep: 3}
	base := filepath.Join(root, tg.Path)
	for _, id := range []string{"1", "2", "3", "4", "5", "6"} {
		write(t, filepath.Join(base, "releases", id, CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
	}
	for _, id := range []string{"2", "4"} {
		write(t, filepath.Join(base, "releases", id, FailedFile), "")
	}
	if err := os.Symlink("releases/5/code", filepath.Join(base, "current")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", ActivateScript(tg, "6", false, "", Phases{}, nil))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("release (%v):\n%s", err, out)
	}
	entries, _ := os.ReadDir(filepath.Join(base, "releases"))
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if got := strings.Join(left, " "); got != "3 4 5 6" {
		t.Errorf("releases left: %s, want 3 4 5 6", got)
	}
}

// A project's directory left with its .env and the record of images its
// removals left is a leftover too; removing it says what is forgotten.
func TestLeftoverImagesRecord(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\nprintf 'root=%s\\nkeep=5\\nmax_keep=5\\nprotocol=1\\n' \"$FAKE_ROOT\"\n")
	if err := os.Chmod(filepath.Join(bin, "devopsy"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	write(t, filepath.Join(root, "app", ".env"), "")
	write(t, filepath.Join(root, "app", ImagesLeftFile), "1\tapp-prod\t1\tmariadb:devopsy-app-prod-1\tW\tconflict\n")
	tg := &Target{Name: "prod", Path: "app/prod", Project: &Project{Name: "app"}, Levels: []Level{{Name: "project", Dir: "app", Link: "project.env"}}}
	run := func(script string) string {
		t.Helper()
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v:\n%s", err, out)
		}
		return string(out)
	}
	if out := run(LeftoverScript(tg, false)); strings.TrimSpace(out) != filepath.Join(root, "app") {
		t.Errorf("leftovers: %q", out)
	}
	if out := run(LeftoverScript(tg, true)); !strings.Contains(out, "forgetting the images "+filepath.Join(root, "app")+"'s removals left: mariadb:devopsy-app-prod-1") || fileExists(filepath.Join(root, "app")) {
		t.Errorf("purge:\n%s", out)
	}
}

// A release fetched again whose listing changed loses the bookmarks the
// new one no longer makes, and one repository written two ways counts
// as one: each name gets its own bookmark.
func TestRebookmark(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\n[ \"$*\" = \"-- config --images\" ] && printf \"$LISTING\"\nexit 0\n")
	fakeImageTools(t, bin)
	root := t.TempDir()
	rel := filepath.Join(root, "app", "prod", "releases", "1")
	write(t, filepath.Join(rel, CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
	for tag, id := range map[string]string{"ghcr.io_x_app:1": "X", "library_nginx:1": "N1", "nginx:2": "N2", "ghcr.io_y_app:1": "Y"} {
		write(t, filepath.Join(root, "images", tag), id+"\n")
	}
	tg := &Target{Name: "prod", Path: "app/prod", Mode: ModeImage, Project: &Project{Name: "app"}}
	run := func(listing string) {
		t.Helper()
		cmd := exec.Command("sh", "-c", "set -eu\nroot=$FAKE_ROOT\n"+releaseImages("", tg, ModeImage)+`img bookmark "$REL"`)
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root, "REL=" + rel, "LISTING=" + listing}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v:\n%s", err, out)
		}
	}
	bookmarks := func() []string {
		var bs []string
		entries, _ := os.ReadDir(filepath.Join(root, "images"))
		for _, e := range entries {
			if strings.Contains(e.Name(), ":devopsy-") {
				bs = append(bs, e.Name())
			}
		}
		return bs
	}
	run(`ghcr.io/x/app:1\nlibrary/nginx:1\nnginx:2\n`)
	bs := bookmarks()
	if len(bs) != 3 || !slices.Contains(bs, "ghcr.io_x_app:devopsy-app-prod-1") {
		t.Fatalf("bookmarks: %v", bs)
	}
	for _, b := range bs {
		if strings.Contains(b, "nginx") && !strings.HasPrefix(strings.TrimPrefix(strings.TrimPrefix(b, "library_"), "nginx:"), "devopsy-app-prod-1-") {
			t.Errorf("%s has no suffix: library/nginx and nginx are one repository", b)
		}
	}
	run(`ghcr.io/y/app:1\n`)
	if bs := bookmarks(); len(bs) != 1 || bs[0] != "ghcr.io_y_app:devopsy-app-prod-1" {
		t.Errorf("bookmarks after the listing changed: %v", bs)
	}
}

// The upload takes the release lock: a first release failing meanwhile
// removes $base, --destroy all of it. Every release links inherit.env, so
// --inherit reaches one released before the parent had inherit:. A sync's
// variables stop at its end.
func TestScriptsLockAndLinks(t *testing.T) {
	tg := &Target{Name: "prod", Path: "app/prod", Mode: ModeImage, Project: &Project{Name: "app"}}
	if s := UploadScript(tg, "1"); !strings.Contains(s, `exec 9>"$base/.lock"`) {
		t.Errorf("upload without the lock:\n%s", s)
	}
	act := ActivateScript(tg, "1", false, "app-prod", Phases{Run: []string{"deploy"}}, &SyncOptions{From: "devopsy@one:prod"})
	if !strings.Contains(act, `ln -sfn "$base/shared/`+InheritFile+`" "$code/.devopsy/`+InheritFile+`"`) {
		t.Errorf("inherit.env not always linked")
	}
	if !strings.Contains(act, "unset "+SyncFromTargetEnv) {
		t.Errorf("the sync's variables reach the run step")
	}
}
