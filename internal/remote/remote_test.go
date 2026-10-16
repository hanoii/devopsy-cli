package remote

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
	out := "20261005120000\tcurrent\t-\t" + `{"id":"20261005120000","mode":"image","by":"ariel@mac","commit":"0123456789abcdef","branch":"main","dirty":true}` + "\n" +
		"20261004120000\t-\tfailed\t\n"
	got := FormatReleases(out)
	for _, want := range []string{"* 20261005120000", "main@0123456789+dirty", "20261004120000", "FAILED"} {
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
	fake := "#!/bin/sh\nif [ \"$1\" = --release-settings ]; then printf 'root=%s\\nkeep=5\\nmax_keep=5\\n' \"${FAKE_ROOT:-/nowhere}\"; exit; fi\necho \"ran in $PWD: $*\"\n"
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
if [ "$1" = --release-settings ]; then printf 'root=%s\nkeep=5\nmax_keep=5\n' "$FAKE_ROOT"; exit; fi
echo "$* from=${DEVOPSY_SYNC_FROM_TARGET:-} to=${DEVOPSY_SYNC_TO_TARGET:-} args=${DEVOPSY_SYNC_ARGS:-}${DEVOPSY_INTERNAL_INHERIT_FILE:+ file=$DEVOPSY_INTERNAL_INHERIT_FILE}" >> "$FAKE_ROOT/calls"
if [ "$*" = "--sync-services prepare" ] && [ -n "${FAKE_HANGUP:-}" ]; then
  echo PREPARING >&2
  while [ ! -e "$FAKE_ROOT/closed" ]; do sleep 0.05; done
fi
[ "$*" = "--sync-services prepare" ] && exit ${FAIL_PREPARE:-0}
exit 0
`
	write(t, filepath.Join(bin, "devopsy"), fake)
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n")
	write(t, filepath.Join(bin, "mv"), "#!/bin/sh\nif [ \"$1\" = -Tf ]; then rm -f \"$3\"; exec /bin/mv -f \"$2\" \"$3\"; fi\nexec /bin/mv \"$@\"\n")
	for _, f := range []string{"devopsy", "flock", "mv"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
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
	// The capability's hooks run from --sync-services, never the script.
	file := " file=" + filepath.Join(base, ".devopsy-sync.inherit.env")
	if got := calls(root); got != "--check-names "+base+" from= to= args=\n--missing-vars from= to= args=\n--sync-services prepare from=vm1:prod to=vm2:dev args=-v"+file+"\n--sync-services finish go from=vm1:prod to=vm2:dev args=-v"+file+"\ndeploy from=vm1:prod to=vm2:dev args=-v"+file+"\n" {
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

// Bookmarks fit Docker's 128-character tags: readable up to 105
// characters of name, cut and hashed past that, and two long names that
// differ only past the cut stay apart.
func TestBookmarkPrefix(t *testing.T) {
	const id = "20261016120000"
	if got := BookmarkPrefix("drupal11-prod"); got != "devopsy-drupal11-prod-" {
		t.Errorf("short name: %q", got)
	}
	if got := BookmarkPrefix(""); got != "devopsy-" {
		t.Errorf("no name: %q", got)
	}
	at := strings.Repeat("a", 105)
	if got := BookmarkPrefix(at); got != "devopsy-"+at+"-" || len(got+id) != 128 {
		t.Errorf("105 characters: %q (%d)", got, len(got+id))
	}
	long1 := strings.Repeat("a", 110) + "-one"
	long2 := strings.Repeat("a", 110) + "-two"
	p1, p2 := BookmarkPrefix(long1), BookmarkPrefix(long2)
	if p1 == p2 {
		t.Errorf("long names share %q", p1)
	}
	for _, p := range []string{p1, p2} {
		if len(p+id) != 128 || !strings.HasPrefix(p, "devopsy-"+strings.Repeat("a", 96)+"-") {
			t.Errorf("long name: %q (%d)", p, len(p+id))
		}
	}
	if BookmarkPrefix(long1) != p1 {
		t.Errorf("not stable")
	}
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
if [ "$1" = --release-settings ]; then printf 'root=%s\nkeep=5\nmax_keep=5\n' "$FAKE_ROOT"; exit; fi
id=$(pwd -P); id=${id%/code}; id=${id##*/}
case $PWD in */current) at=current ;; *) at=$id ;; esac
echo "$at: $*" >> "$FAKE_ROOT/calls"
mkdir -p "$FAKE_ROOT/images"
case "$*" in
  "-- config --images") printf 'app-prod-app\nmariadb:11\nx@sha256:abc\n' ;;
  "-- pull --ignore-buildable" | "-- pull") echo "$id" > "$FAKE_ROOT/images/mariadb_11" ;;
  "-- build --pull") [ -z "${FAIL_BUILD:-}" ] || exit "$FAIL_BUILD"; echo "$id" > "$FAKE_ROOT/images/app-prod-app" ;;
  deploy) exit ${FAIL_RUN:-0} ;;
esac
exit 0
`
	docker := `#!/bin/sh
f() { echo "$FAKE_ROOT/images/$(echo "$1" | tr '/:' '__')"; }
case "$1 $2" in
  "image inspect") [ -f "$(f "$3")" ] ;;
  "image rm") rm -f "$(f "$3")" ;;
  *) [ "$1" = tag ] && [ -f "$(f "$2")" ] && cp "$(f "$2")" "$(f "$3")" ;;
esac
`
	write(t, filepath.Join(bin, "devopsy"), fake)
	write(t, filepath.Join(bin, "docker"), docker)
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n")
	write(t, filepath.Join(bin, "mv"), "#!/bin/sh\nif [ \"$1\" = -Tf ]; then rm -f \"$3\"; exec /bin/mv -f \"$2\" \"$3\"; fi\nexec /bin/mv \"$@\"\n")
	for _, f := range []string{"devopsy", "docker", "flock", "mv"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
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
	image := func(root, tag string) string {
		data, _ := os.ReadFile(filepath.Join(root, "images", strings.NewReplacer("/", "_", ":", "_").Replace(tag)))
		return strings.TrimSpace(string(data))
	}

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

	// A rollback from 4 to 1 (2 and 3 failed) restores 1's images.
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

// Pruning counts only releases that went live: a failed one between those
// kept stays, with its log, and older ones go.
func TestActivatePruneFailed(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\nif [ \"$1\" = --release-settings ]; then printf 'root=%s\\nkeep=5\\nmax_keep=5\\n' \"$FAKE_ROOT\"; fi\nexit 0\n")
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n")
	write(t, filepath.Join(bin, "mv"), "#!/bin/sh\nif [ \"$1\" = -Tf ]; then rm -f \"$3\"; exec /bin/mv -f \"$2\" \"$3\"; fi\nexec /bin/mv \"$@\"\n")
	for _, f := range []string{"devopsy", "flock", "mv"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
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
