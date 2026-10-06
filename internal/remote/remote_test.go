package remote

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

func TestLoadTarget(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	dir := t.TempDir()
	write(t, filepath.Join(dir, TargetsFile), `
prod:
  host: deploy@203.0.113.10
  path: /srv/app/
build:
  host: vm1
  path: /srv/app-build
  mode: build
bad-mode:
  host: vm1
  path: /srv/x
  mode: rsync
relative:
  host: vm1
  path: srv/x
root:
  host: vm1
  path: /
nohost:
  path: /srv/x
`)
	tg, err := LoadTarget(dir, "prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tg.Mode != ModeImage || tg.Path != "/srv/app" || tg.Name != "prod" {
		t.Fatalf("prod: %+v", tg)
	}
	if tg, err := LoadTarget(dir, "build", nil); err != nil || tg.Mode != ModeBuild {
		t.Fatalf("build: %+v %v", tg, err)
	}
	for _, name := range []string{"bad-mode", "relative", "root", "nohost", "missing"} {
		if _, err := LoadTarget(dir, name, nil); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := LoadTarget(t.TempDir(), "prod", nil); err == nil || !strings.Contains(err.Error(), "no targets defined") {
		t.Errorf("no targets file: %v", err)
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
	write(t, filepath.Join(root, ".devopsy", LocalTargetsFile), "mine: {}\n")
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
		"activate":     ActivateScript(tg, "20261005120000", false, "", "", nil),
		"activate+cmd": ActivateScript(tg, "20261005120000", false, "app", "app", []string{"deploy", "a b"}),
		"rollback+cmd": ActivateScript(tg, "", true, "", "shop", []string{"up", "-d"}),
		"run":          RunScript(tg, "app", []string{"logs", "-f"}),
		"releases":     ReleasesScript(tg),
		"shell":        ShellScript(tg),
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

func TestLoadTargetEnvAndLocal(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	dir := t.TempDir()
	write(t, filepath.Join(dir, TargetsFile), `
prod:
  host: vm1
  path: /srv/app
  env:
    DEVOPSY_DOMAINS: example.org
staging:
  host: vm1
  path: /srv/app-staging
badenv:
  host: vm1
  path: /srv/x
  env:
    "NOT VALID": x
`)
	write(t, filepath.Join(dir, LocalTargetsFile), `
staging:
  host: my-test-vm
  path: /srv/mine
mine:
  host: laptop-vm
  path: /srv/mine
`)
	prod, err := LoadTarget(dir, "prod", nil)
	if err != nil || prod.Env["DEVOPSY_DOMAINS"] != "example.org" {
		t.Fatalf("prod: %+v %v", prod, err)
	}
	staging, err := LoadTarget(dir, "staging", nil)
	if err != nil || staging.Host != "my-test-vm" || staging.Path != "/srv/mine" {
		t.Fatalf("local override: %+v %v", staging, err)
	}
	if _, err := LoadTarget(dir, "mine", nil); err != nil {
		t.Fatalf("local-only target: %v", err)
	}
	if _, err := LoadTarget(dir, "badenv", nil); err == nil {
		t.Fatal("invalid env name: want an error")
	}

	// Only a local file is enough.
	only := t.TempDir()
	write(t, filepath.Join(only, LocalTargetsFile), "x:\n  host: h\n  path: /srv/x\n")
	if _, err := LoadTarget(only, "x", nil); err != nil {
		t.Fatalf("local file only: %v", err)
	}
}

func TestLoadTargetUserFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEVOPSY_HOME", home)
	write(t, filepath.Join(home, TargetsFile), `
vm1-traefik:
  host: devopsy@vm1
  path: /srv/traefik
prod:
  host: user-level
  path: /srv/user-prod
`)
	project := t.TempDir()
	write(t, filepath.Join(project, TargetsFile), "prod:\n  host: vm1\n  path: /srv/app\n")

	prod, err := LoadTarget(project, "prod", nil)
	if err != nil || prod.User || prod.Host != "vm1" {
		t.Fatalf("project target must win: %+v %v", prod, err)
	}
	tr, err := LoadTarget(project, "vm1-traefik", nil)
	if err != nil || !tr.User || tr.File != filepath.Join(home, TargetsFile) {
		t.Fatalf("user target from a project: %+v %v", tr, err)
	}
	if tr, err := LoadTarget("", "vm1-traefik", nil); err != nil || !tr.User {
		t.Fatalf("user target outside a project: %+v %v", tr, err)
	}
	if _, err := LoadTarget("", "missing", nil); err == nil || !strings.Contains(err.Error(), "vm1-traefik") {
		t.Fatalf("missing target should list the others: %v", err)
	}
}

func TestLoadTargetHostVars(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DEVOPSY_HOME", home)
	write(t, filepath.Join(home, TargetsFile), "vm1-traefik:\n  path: /srv/traefik\n")
	dir := t.TempDir()
	write(t, filepath.Join(dir, TargetsFile), `
prod:
  path: /srv/app-prod
staging-eu:
  host: devopsy@staging
  path: /srv/app-staging
`)
	env := map[string]string{}
	projectEnv := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	host := func(name string) string {
		t.Helper()
		tg, err := LoadTarget(dir, name, projectEnv)
		if err != nil {
			return "error: " + err.Error()
		}
		return tg.Host
	}

	if got := HostVar("staging-eu"); got != "DEVOPSY_TARGET_HOST_STAGING_EU" {
		t.Errorf("HostVar: %s", got)
	}
	if got := host("prod"); !strings.Contains(got, "DEVOPSY_TARGET_HOST_PROD or DEVOPSY_TARGET_HOST") {
		t.Errorf("no host should name the variables: %s", got)
	}
	// The default only fills in missing hosts; a target's own variable
	// replaces any.
	env["DEVOPSY_TARGET_HOST"] = "devopsy@default"
	if got := host("prod"); got != "devopsy@default" {
		t.Errorf("default: %s", got)
	}
	if got := host("staging-eu"); got != "devopsy@staging" {
		t.Errorf("default must not replace a host: %s", got)
	}
	env["DEVOPSY_TARGET_HOST_STAGING_EU"] = "devopsy@eu"
	if got := host("staging-eu"); got != "devopsy@eu" {
		t.Errorf("target variable: %s", got)
	}
	// The caller's environment wins over the project's .env.
	t.Setenv("DEVOPSY_TARGET_HOST_STAGING_EU", "devopsy@caller")
	if got := host("staging-eu"); got != "devopsy@caller" {
		t.Errorf("caller: %s", got)
	}
	// User-level targets ignore the project's .env.
	if got := host("vm1-traefik"); !strings.HasPrefix(got, "error: ") {
		t.Errorf("user target used the project's .env: %s", got)
	}
	t.Setenv("DEVOPSY_TARGET_HOST_VM1_TRAEFIK", "devopsy@vm1")
	if got := host("vm1-traefik"); got != "devopsy@vm1" {
		t.Errorf("user target from the caller: %s", got)
	}
}

// fakeDevopsy puts a devopsy on PATH that prints where it runs.
func fakeDevopsy(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "devopsy"), []byte("#!/bin/sh\necho \"ran in $PWD: $*\"\n"), 0o755); err != nil {
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
		"rollback": ActivateScript(tg, "", true, "", "", nil),
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
