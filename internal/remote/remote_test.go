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
	write(t, filepath.Join(root, ".devopsy", TargetEnvFile), "STRAY=1\n")
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
	extra := map[string][]byte{RecordFile: []byte(`{"id":"x"}`), ".devopsy/" + TargetEnvFile: []byte("A='1'\n")}
	if err := Pack(&buf, root, files, extra); err != nil {
		t.Fatal(err)
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
	if h := entries["link.php"]; h == nil || h.Typeflag != tar.TypeSymlink || h.Linkname != "app.php" {
		t.Fatalf("symlink not kept: %+v", h)
	}
	if contents[RecordFile] != `{"id":"x"}` || contents[".devopsy/target.env"] != "A='1'\n" || contents["app.php"] != "<?php\n" {
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
	tg := &Target{Name: "prod", Host: "h", Path: "/srv/it's app", Mode: ModeImage}
	scripts := map[string]string{
		"upload":       UploadScript(tg, "20261005120000"),
		"activate":     ActivateScript(tg, "20261005120000", false, "", Phases{}),
		"activate+cmd": ActivateScript(tg, "20261005120000", false, "app", Phases{Before: [][]string{{"x"}}, Prepare: [][]string{{"secrets"}}, Run: []string{"deploy", "a b"}, Restart: []string{"deploy"}, After: [][]string{{"warm"}}}),
		"rollback+cmd": ActivateScript(tg, "", true, "", Phases{Run: []string{"up", "-d"}}),
		"run":          RunScript(tg, "app", []string{"logs", "-f"}),
		"releases":     ReleasesScript(tg),
		"shell":        ShellScript(tg),
		"vars":         VarsReadScript(tg, nil),
		"vars set":     VarsSetScript(tg, nil),
		"vars unset":   VarsUnsetScript(tg, &Level{Name: "project", Dir: "app"}),
		"destroy":      DestroyScript(tg, "app", []string{"destroy"}),
		"environments": EnvironmentsScript(&Target{Name: "prod", Path: "app/prod", Project: &Project{Name: "app"}}),
		"role holders": RoleHoldersScript(tg, "proxy"),
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
	run := func(script string) (string, error) {
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = []string{path}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	plain := t.TempDir()
	if err := os.MkdirAll(filepath.Join(plain, ".devopsy"), 0o755); err != nil {
		t.Fatal(err)
	}
	tg := &Target{Name: "vm1-traefik", Host: "h", Path: plain}
	if out, err := run(RunScript(tg, "", []string{"proxies"})); err != nil || !strings.Contains(out, "ran in "+plain+": proxies") {
		t.Fatalf("plain run: %v\n%s", err, out)
	}
	for name, script := range map[string]string{
		"upload":   UploadScript(tg, "20261006000000"),
		"rollback": ActivateScript(tg, "", true, "", Phases{}),
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

	released := t.TempDir()
	if err := os.MkdirAll(filepath.Join(released, "current"), 0o755); err != nil {
		t.Fatal(err)
	}
	tr := &Target{Name: "prod", Host: "h", Path: released}
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

	empty := &Target{Name: "new", Host: "h", Path: t.TempDir()}
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
		write(t, filepath.Join(rel, ".devopsy", "compose.yaml"), c.compose)
		if c.override != "" {
			write(t, filepath.Join(rel, ".devopsy", "compose.override.yaml"), c.override)
		}
		if c.fail {
			write(t, filepath.Join(rel, ".devopsy", "FAIL"), "")
		}
		cmd := exec.Command("sh", "-c", "set -eu\nbase="+Quote(base)+"\nid=1\nprev=releases/0\nrel="+Quote(rel)+"\n"+script+"echo after\n")
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
		out, err := cmd.CombinedOutput()
		_, ranErr := os.Stat(filepath.Join(rel, "ran"))
		if (ranErr == nil) != c.runs {
			t.Errorf("%s: ran %v, want %v", name, ranErr == nil, c.runs)
		}
		_, failedErr := os.Stat(filepath.Join(rel, ".devopsy-failed"))
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
		write(t, filepath.Join(rel, ".devopsy", "compose.yaml"), "services:\n  a:\n    labels: [devopsy.role=proxy]\n")
		write(t, filepath.Join(base, "shared", ".env"), "")
		if secrets {
			write(t, filepath.Join(base, "shared", ".env"), "TOKEN=x\n")
		}
		cmd := exec.Command("sh", "-c", "set -eu\nbase="+Quote(base)+"\nid=1\nprev=\nrel="+Quote(rel)+"\n"+script)
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
