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
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	// A fake devopsy says what scripts say (--say) as the real one does,
	// so the calls it records are the scripts' own.
	if base := filepath.Base(path); (base == "devopsy" || base == "devopsy.fake") && strings.HasPrefix(content, "#!/bin/sh\n") {
		content = "#!/bin/sh\n" + sayCase() + strings.TrimPrefix(content, "#!/bin/sh\n")
	}
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

// Pack carries git's modes, whatever the checkout's: a 0600 file (ddev's
// Mutagen writes those) as 0644, an executable one as 0755.
func TestPack(t *testing.T) {
	root := gitProject(t)
	for name, mode := range map[string]os.FileMode{"app.php": 0o600, "src/lib.php": 0o664, ".devopsy/commands/deploy": 0o700} {
		if err := os.Chmod(filepath.Join(root, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	files, err := Files(root, ModeBuild)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	extra := map[string][]byte{RecordFile: []byte(`{"mode":"x"}`), TargetEnvFile: []byte("A='1'\n")}
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
	if contents[RecordFile] != `{"mode":"x"}` || contents[TargetEnvFile] != "A='1'\n" || contents["code/app.php"] != "<?php\n" {
		t.Fatalf("contents: %q", contents)
	}
	for name, want := range map[string]int64{"code/app.php": 0o644, "code/src/lib.php": 0o644, "code/.devopsy/commands/deploy": 0o755, "code/.devopsy/compose.yaml": 0o644} {
		if h := entries[name]; h == nil || h.Mode != want {
			t.Errorf("%s: mode %o, want %o", name, h.Mode, want)
		}
	}
}

func TestNewRecord(t *testing.T) {
	root := gitProject(t)
	r := NewRecord(root, ModeBuild, time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	if r.Time != "2026-10-05T12:00:00Z" || r.Commit == "" || r.Branch == "" || !r.Dirty {
		t.Fatalf("%+v", r)
	}
	if r := NewRecord(t.TempDir(), ModeImage, time.Now()); r.Commit != "" {
		t.Fatalf("outside git: %+v", r)
	}
}

// A new release is newer than every release there: the server's second,
// or one past the newest when two land in one second or older ids came
// from a clock ahead of it.
func TestNextReleaseID(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 500, time.UTC)
	for _, c := range []struct {
		ids  []string
		want string
	}{
		{nil, "20261020120000"},
		{[]string{"20261020115959", "x.tmp", "notes"}, "20261020120000"},
		{[]string{"20261020120000"}, "20261020120001"},
		{[]string{"20261020120000", "20261231235959", "20261020110000"}, "20270101000000"},
	} {
		if got := NextReleaseID(now, c.ids); got != c.want {
			t.Errorf("%v: %s, want %s", c.ids, got, c.want)
		}
	}
}

// Every file a sync leaves in an environment's directory starts with
// .devopsy-sync., so they read as one family there.
func TestSyncFilesPrefixed(t *testing.T) {
	tg := &Target{Name: "prod", Host: "h", Path: "app/prod", Project: &Project{Name: "app"}}
	scripts := HolderScript(tg, "app-prod", &SyncOptions{From: "vm1:prod"}, true) + PushScript(tg, "app-prod", nil) +
		ExportScript(tg, "app-prod", "db") + ImportScript(tg, "app-prod", "db", "end") + StatusScript(tg) +
		ClaimWriteScript("$base", "1.2") + SyncReceiveScript("/x/current/.devopsy", "$base", "1.2", "1", []string{"a"}, nil, nil, nil, true, "")
	for _, m := range regexp.MustCompile(`\$base/(\.[A-Za-z0-9_.-]*sync[A-Za-z0-9_.-]*)`).FindAllStringSubmatch(scripts, -1) {
		if !strings.HasPrefix(m[1], ".devopsy-sync.") {
			t.Errorf("a sync's file not prefixed: %s", m[1])
		}
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

// allScripts is every script sent to a server, by name.
func allScripts() map[string]string {
	tg := &Target{Name: "prod", Host: "h", Path: "it's app/prod", Mode: ModeImage}
	return map[string]string{
		"upload":          UploadScript(tg, false),
		"upload sync":     UploadScript(tg, true),
		"activate":        ActivateScript(tg, "20261005120000", false, "", Phases{Run: []string{"deploy"}}, nil),
		"activate+cmd":    ActivateScript(tg, "20261005120000", false, "app", Phases{Before: [][]string{{"x"}}, Prepare: [][]string{{"secrets"}}, Run: []string{"deploy", "a b"}, After: [][]string{{"warm"}}}, nil),
		"rollback+cmd":    ActivateScript(tg, "", true, "", Phases{Run: []string{"up", "-d"}}, nil),
		"run":             RunScript(tg, "app", []string{"logs", "-f"}),
		"releases":        ReleasesScript(tg),
		"releases remove": ReleasesRemoveScript(tg, "app", []string{"1", "2"}, false),
		"shell":           ShellScript(tg),
		"vars":            VarsReadScript(tg, nil),
		"vars set":        VarsSetScript(tg, nil),
		"vars unset":      VarsUnsetScript(tg, &Level{Name: "project", Dir: "app"}),
		"inherit read":    InheritReadScript(&Target{Path: "app/a/prod", Levels: []Level{{Name: "project", Dir: "app"}, {Name: "instance", Dir: "app/a"}}}),
		"inherit write":   InheritWriteScript(tg, []string{"A", "B"}, ""),
		"inherit pending": InheritWriteScript(tg, []string{"A", "B"}, "1.2"),
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
		"activate+sync":   ActivateScript(tg, "20261005120000", false, "app", Phases{Prepare: [][]string{{"secrets"}}, Run: []string{"deploy"}}, &SyncOptions{From: "vm1:prod", Args: []string{"--exclude=it's"}, ID: "1.2"}),
		"sync only":       ActivateScript(tg, "20261005120000", false, "app", Phases{Run: []string{"deploy"}}, &SyncOptions{From: "vm1:prod", ID: "1.2"}),
		"holder":          HolderScript(tg, "app-dev", &SyncOptions{From: "vm1:prod", Args: []string{"-v"}, Inherit: true, ID: "1.2"}, true),
		"holder plain":    HolderScript(tg, "app-dev", &SyncOptions{From: "vm1:prod", ID: "1.2"}, true),
		"push":            PushScript(tg, "app-prod", map[string]string{SyncToEnv: "devopsy@h2", SyncToDirEnv: "/srv/it's/current/.devopsy"}),
		"current":         CurrentExistsScript(tg, nil),
		"current parent":  CurrentExistsScript(tg, []string{"sync-push", DataLabelsFeature}),
		"receive":         SyncReceiveScript("/srv/it's/current/.devopsy", "/srv/it's", "1.2", "20001", []string{"storage", "logs"}, nil, map[string]string{"storage": "10001:10001"}, []string{"--exclude=x y"}, true, ""),
		"receive reset":   SyncReceiveScript("/srv/it's/current/.devopsy", "/srv/it's", "1.2", "20001", []string{"storage"}, nil, nil, nil, true, SyncExecScript("/srv/it's/current/.devopsy", "app-dev", nil, []string{"--sync-services", "wipe"})),
		"receive bare":    SyncReceiveScript("/srv/app/current/.devopsy", "/srv/app", "1.2", "20001", []string{"storage", "app.ini"}, []string{"app.ini"}, map[string]string{"app.ini": "1:2"}, nil, false, ""),
		"exec":            SyncExecScript("/srv/it's/current/.devopsy", "app-dev", map[string]string{SyncArgsEnv: "--x 'y'", SyncFromTargetEnv: "vm1:prod"}, []string{"--sync-services", "import", "db"}),
		"exec list":       SyncExecScript("/srv/it's/current/.devopsy", "app-dev", nil, []string{"--sync-services", "list"}),
		"child check":     ChildCheck("devopsy@h2:dev", "cat > x", []string{"say", "sync-services"}),
		"export":          ExportScript(tg, "app-prod", "it's db"),
		"import":          ImportScript(tg, "app-prod", "it's db", "\nend 0a1b\n"),
		"resolved check":  ResolvedCheckScript(tg),
		"resolved":        ResolvedScript(tg, "app-prod", "it's db"),
	}
}

// The devopsy.data labels are a named feature: exactly the scripts that
// read labels need it (an older devopsy reads them as none), so an older
// server refuses those before anything changes, and runs the rest:
// settle, which every activation calls, stays out.
func TestDataLabelsFeature(t *testing.T) {
	need := []string{"activate+sync", "export", "exec", "exec list", "holder", "holder plain", "import", "push", "resolved", "sync only"}
	for name, s := range allScripts() {
		got := slices.Contains(Needs(s), DataLabelsFeature)
		if got != slices.Contains(need, name) {
			t.Errorf("%s needs %s: %v", name, DataLabelsFeature, got)
		}
	}
	// A project's remote step calling --export, rendered inside with_tty's
	// quotes, reads labels too; settle alone does not.
	steps := ActivateScript(&Target{Name: "prod", Host: "h", Path: "app/prod"}, "20261005120000", false, "app", Phases{Before: [][]string{{"--export", "db"}}, Run: []string{"deploy"}}, nil)
	if !slices.Contains(Needs(steps), DataLabelsFeature) {
		t.Errorf("a step calling --export: %v", Needs(steps))
	}
	if got := Needs("devopsy '--sync-services' 'settle'"); slices.Contains(got, DataLabelsFeature) {
		t.Errorf("settle: %v", got)
	}
	// The parent's check carries the push's needs.
	if s := CurrentExistsScript(&Target{Path: "app/prod"}, Needs(PushScript(&Target{Path: "app/prod"}, "app-prod", nil))); !strings.Contains(s, needsEnv+`="${`+needsEnv+`:-} `) || !strings.Contains(s, DataLabelsFeature) {
		t.Errorf("the parent's check:\n%s", s)
	}
}

// devopsy.mnt.owner in copies is a named feature: the push (whose rsync
// applies the child's owners), the child's list (which says them) and
// imports (which give what they extract away) need it; a release's
// owners and mounts check are flags of their own.
func TestMntOwnerFeature(t *testing.T) {
	need := []string{"exec list", "import", "push", "resolved"}
	for name, s := range allScripts() {
		if got := slices.Contains(Needs(s), MntOwnerFeature); got != slices.Contains(need, name) {
			t.Errorf("%s needs %s: %v", name, MntOwnerFeature, got)
		}
	}
	for _, name := range []string{"activate", "activate+sync", "rollback+cmd"} {
		if got := Needs(allScripts()[name]); !slices.Contains(got, "mnt-owners") || !slices.Contains(got, "check-mounts") {
			t.Errorf("%s: %v", name, got)
		}
	}
}

// The receive's rsync gives an entry the child owns to its owner
// (--chown), and keeps the others' (--numeric-ids).
func TestSyncReceiveOwners(t *testing.T) {
	s := allScripts()["receive"]
	if !strings.Contains(s, "copy 'storage' 'storage/' 'storage/' '--chown=10001:10001'\n") || !strings.Contains(s, "copy 'logs' 'logs/' 'logs/' ''\n") || !strings.Contains(s, "rsync -a --delete --numeric-ids --modify-window=-1 $4 ") {
		t.Errorf("receive:\n%s", s)
	}
	if s := allScripts()["receive bare"]; !strings.Contains(s, "copy 'app.ini' 'app.ini/app.ini' '' '--inplace --chown=1:2'\n") {
		t.Errorf("a file's owner:\n%s", s)
	}
}

// A reset needs its feature on both servers: the parent's push wipes at
// its first write (an older one would sync without wiping), the child's
// devopsy runs the wipe. A sync needs neither.
func TestResetFeature(t *testing.T) {
	tg := &Target{Path: "app/prod"}
	for name, s := range allScripts() {
		if name != "receive reset" && slices.Contains(Needs(s), ResetFeature) {
			t.Errorf("%s needs %s", name, ResetFeature)
		}
	}
	push := PushScript(tg, "app-prod", map[string]string{SyncResetEnv: "1", SyncIDEnv: "1.2"})
	if !slices.Contains(Needs(push), ResetFeature) || strings.Contains(push, SyncResetEnv) || !strings.Contains(push, "'--sync-push' 'reset'") {
		t.Errorf("a reset's push: %v\n%s", Needs(push), push)
	}
	if got := Needs(allScripts()["receive reset"]); !slices.Contains(got, ResetFeature) {
		t.Errorf("a reset's receive: %v", got)
	}
}

func TestScriptsParse(t *testing.T) {
	for name, s := range allScripts() {
		if out, err := exec.Command("sh", "-n", "-c", WithNeeds(s)).CombinedOutput(); err != nil {
			t.Errorf("%s: %v\n%s\n%s", name, err, out, s)
		}
	}
}

// Every devopsy flag a script calls is a feature this devopsy reports
// (featureFlags), so a server's devopsy that lacks one refuses the
// scripts that call it, by name, and only those.
func TestFeaturesCoverScripts(t *testing.T) {
	have := " " + FeaturesList() + " "
	for name, s := range allScripts() {
		for _, f := range flagsCalled(s) {
			if !strings.Contains(have, " "+f+" ") {
				t.Errorf("%s calls devopsy --%s, which is no feature (featureFlags)", name, f)
			}
		}
	}
	// The operator's own command is no need of the script's.
	if got := Needs(RunScript(&Target{Path: "app/prod"}, "app", []string{"--env"})); slices.Contains(got, "env") {
		t.Errorf("a command's own flag read as a need: %v", got)
	}
	// Nor is a word of it after a service named devopsy (compose's
	// logs devopsy --follow).
	if got := Needs(RunScript(&Target{Path: "app/prod"}, "app", []string{"logs", "devopsy", "--follow"})); slices.Contains(got, "follow") {
		t.Errorf("a command's own argument read as a need: %v", got)
	}
	// Nor the project's steps: compose flags may go first (--profile web
	// up -d), and a release of them must not need --profile of the
	// server's devopsy, which no devopsy reports: every server would
	// refuse it.
	steps := ActivateScript(&Target{Name: "prod", Host: "h", Path: "app/prod"}, "20261005120000", false, "app", Phases{Before: [][]string{{"--profile", "tools", "run", "--rm", "jq"}}, Prepare: [][]string{{"--env-file", "x", "config"}}, Run: []string{"--profile", "web", "up", "-d"}}, nil)
	for _, f := range Needs(steps) {
		if !strings.Contains(have, " "+f+" ") {
			t.Errorf("a release step's own flag read as a need: --%s", f)
		}
	}
	// Quoted, inside with_tty's quotes too; once each; the data labels'
	// feature by what reads them, quoted or not.
	if got := Needs("devopsy --say x; with_tty 'COMPOSE_PROJECT_NAME=a devopsy '\\''--release-steps'\\'' run'; devopsy --say y; devopsy '--images' x"); !slices.Equal(got, []string{"images", "release-steps", "say"}) {
		t.Errorf("needs: %v", got)
	}
	if got := Needs("with_tty 'devopsy '\\''--export'\\'' db'; devopsy '--sync-services' 'list'"); !slices.Equal(got, []string{DataLabelsFeature, "export", MntOwnerFeature, "sync-services"}) {
		t.Errorf("needs, quoted: %v", got)
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
	fake := "#!/bin/sh\n" + sayCase() + "if [ \"$1\" = --release-settings ]; then printf 'root=%s\\nkeep=5\\nmax_keep=5\\nprotocol=8\\n' \"${FAKE_ROOT:-/nowhere}\"; exit; fi\necho \"ran in $PWD: $*\"\n"
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
		"upload":   UploadScript(tg, false),
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
	fake := "#!/bin/sh\n" + sayCase() + "pwd > ran\n[ ! -f .devopsy/FAIL ]\n"
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

// A first release that fails before going live is marked failed, its
// directory kept, whatever shared/ has: --destroy removes it.
func TestPrepareScriptFirstRelease(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\nexit 1\n")
	if err := os.Chmod(filepath.Join(bin, "devopsy"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := NotLive + fmt.Sprintf(PrepareScript, "devopsy --prepare-release")
	root := t.TempDir()
	base := filepath.Join(root, "shop", "b", "prod")
	rel := filepath.Join(base, "releases", "1")
	write(t, filepath.Join(rel, CodeDir, ".devopsy", "compose.yaml"), "services:\n  a:\n    labels: [devopsy.role=proxy]\n")
	write(t, filepath.Join(base, "shared", ".env"), "")
	cmd := exec.Command("sh", "-c", "set -eu\nbase="+Quote(base)+"\nid=1\nprev=\nrel="+Quote(rel)+"\ncode="+Quote(filepath.Join(rel, CodeDir))+"\n"+script)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err == nil || !fileExists(filepath.Join(rel, FailedFile)) || !strings.Contains(string(out), "release 1 not made current") {
		t.Errorf("first release failing (%v):\n%s", err, out)
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
if [ "$1" = --release-settings ]; then printf 'root=%s\nkeep=5\nmax_keep=5\nprotocol=8\n' "$FAKE_ROOT"; exit; fi
echo "$* from=${DEVOPSY_SYNC_FROM_TARGET:-} to=${DEVOPSY_SYNC_TO_TARGET:-} args=${DEVOPSY_SYNC_ARGS:-}${DEVOPSY_INTERNAL_INHERIT_FILE:+ file=$DEVOPSY_INTERNAL_INHERIT_FILE}" >> "$FAKE_ROOT/calls"
if [ "${1:-} ${2:-}" = "--sync-services prepare" ] && [ -n "${FAKE_HANGUP:-}" ]; then
  echo PREPARING >&2
  while [ ! -e "$FAKE_ROOT/closed" ]; do sleep 0.05; done
fi
# What the push writes into the child, as if during the copy.
if [ "${1:-} ${2:-}" = "--sync-services prepare" ] && [ -n "${FAKE_WRITE:-}" ]; then
  # started claims the gate; done and other-done append the copy's last
  # write to the record, this sync's or another's.
  rec="$FAKE_ROOT/app/dev/.devopsy-sync.started"
  for f in $FAKE_WRITE; do
    case $f in
      started) echo "vm1:prod $DEVOPSY_INTERNAL_SYNC_ID" >"$rec" ;;
      done) echo "done $DEVOPSY_INTERNAL_SYNC_ID" >>"$rec" ;;
      other-done) echo "done 123.45" >>"$rec" ;;
    esac
  done
fi
[ "$*" = "--sync-services finish partial" ] && { [ -e "$FAKE_ROOT/app/dev/.devopsy-sync.incomplete" ] || : >"$FAKE_ROOT/app/dev/.devopsy-sync.incomplete"; }
[ "${1:-} ${2:-}" = "--sync-services prepare" ] && exit ${FAIL_PREPARE:-0}
[ "$*" = --mnt-owners ] && exit ${FAIL_OWNERS:-0}
exit 0
`
	write(t, filepath.Join(bin, "devopsy"), fake)
	// flock, mv -T, a fake docker, and the real --images.
	fakeImageTools(t, bin)
	tg := &Target{Name: "dev", Address: "vm2:dev", Path: "app/dev"}
	phases := Phases{Run: []string{"deploy"}}
	opts := &SyncOptions{From: "vm1:prod", Args: []string{"-v"}, ID: "1.2"}
	run := func(root, id, stdin string, extra ...string) (stdout, stderr string, err error) {
		t.Helper()
		write(t, filepath.Join(root, tg.Path, "releases", id, CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
		cmd := exec.Command("sh", "-c", ActivateScript(tg, id, false, "app-dev", phases, opts))
		cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}, extra...)
		// As devopsy does: stdin a pipe it keeps open until the script
		// ends, the answer in it; none means devopsy is gone.
		r, w, perr := os.Pipe()
		if perr != nil {
			t.Fatal(perr)
		}
		defer w.Close()
		cmd.Stdin = r
		_, _ = w.WriteString(stdin)
		if stdin == "" {
			w.Close()
		}
		var o, e strings.Builder
		cmd.Stdout, cmd.Stderr = &o, &e
		err = cmd.Run()
		r.Close()
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
	// The marker on stderr, which is all the operator reads for it:
	// nothing on stdout.
	if err != nil || out != "" || !strings.Contains(errOut, SyncMarker+base+"\t") || current(root) != "releases/1/code" {
		t.Fatalf("go (%v), current %q:\nstdout %q\n%s", err, current(root), out, errOut)
	}
	// The capability's hooks run from --sync-services, never the script;
	// the run step after it is no sync's: none of its variables.
	file := " file=" + filepath.Join(base, SyncInheritPending("1.2"))
	if got := calls(root); got != "--missing-vars from= to= args=\n--check-names "+base+" from= to= args=\n--check-mounts from= to= args=\n-- pull --ignore-buildable from= to= args=\n-- build --pull from= to= args=\n-- config --images from= to= args=\n--mnt-owners from= to= args=\n--sync-services prepare first from=vm1:prod to=vm2:dev args=-v"+file+"\n--sync-services finish go from=vm1:prod to=vm2:dev args=-v"+file+"\ndeploy from= to= args=\n" {
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

	// The owners failing after the switch: no sync (nothing stopped,
	// no marker), back to the previous release.
	_, errOut, err = run(root, "21", "go\n", "FAIL_OWNERS=6")
	if err == nil || current(root) != "releases/1/code" || strings.Contains(errOut, SyncMarker) || !strings.Contains(errOut, "giving the entries of mnt to their owners failed (6), current is back to 1") {
		t.Fatalf("failed owners (%v), current %q:\n%s", err, current(root), errOut)
	}
	if got := calls(root); strings.Contains(got, "--sync-services") || strings.Contains(got, "deploy from") {
		t.Errorf("failed owners calls:\n%s", got)
	}

	// abort on a first release: nothing current, the directory kept
	// (the data is partly there).
	root = t.TempDir()
	_, errOut, err = run(root, "1", "abort\n")
	if err == nil || current(root) != "" || !strings.Contains(errOut, "no previous release to go back to") {
		t.Fatalf("first abort (%v), current %q:\n%s", err, current(root), errOut)
	}
	if got := calls(root); !strings.Contains(got, "--sync-services finish") || strings.Contains(got, "deploy") {
		t.Errorf("first abort calls:\n%s", got)
	}
	// The files decide, not the answer: a copy that completed goes on
	// whatever devopsy said; one written in part is partial, its data
	// run on by nothing, the previous release not restarted.
	root = t.TempDir()
	if _, errOut, err := run(root, "1", "go\n"); err != nil {
		t.Fatalf("first (%v):\n%s", err, errOut)
	}
	calls(root)
	if _, errOut, err := run(root, "2", "abort\n", "FAKE_WRITE=started done"); err != nil || current(root) != "releases/2/code" || !strings.Contains(errOut, "the copy had completed") {
		t.Fatalf("abort after a complete copy (%v), current %q:\n%s", err, current(root), errOut)
	}
	if got := calls(root); !strings.Contains(got, "--sync-services finish go") || !strings.Contains(got, "deploy from") {
		t.Errorf("abort after a complete copy, calls:\n%s", got)
	}
	// A done this sync's push never wrote (another sync's, 123.45: a push
	// that outlived its holder finishing its copy now) never makes this one
	// go on: that push wrote into the child meanwhile, so it is partial.
	other := t.TempDir()
	if _, errOut, err := run(other, "1", "go\n"); err != nil {
		t.Fatalf("first (%v):\n%s", err, errOut)
	}
	calls(other)
	if _, errOut, err := run(other, "2", "abort\n", "FAKE_WRITE=started other-done"); err == nil || current(other) != "releases/1/code" || strings.Contains(errOut, "the copy had completed") || !fileExists(filepath.Join(other, tg.Path, SyncIncompleteFile)) {
		t.Errorf("another sync's done (%v), current %q:\n%s", err, current(other), errOut)
	}
	_, errOut, err = run(root, "3", "go\n", "FAKE_WRITE=started")
	if err == nil || current(root) != "releases/2/code" || !strings.Contains(errOut, "the copy was cut") || !strings.Contains(errOut, "not restarting 2") {
		t.Fatalf("a partial copy (%v), current %q:\n%s", err, current(root), errOut)
	}
	if got := calls(root); !strings.Contains(got, "--sync-services finish partial") || strings.Contains(got, "--release-steps restart run") || strings.Contains(got, "deploy from") {
		t.Errorf("a partial copy, calls:\n%s", got)
	}
	for _, f := range []string{SyncStartedFile, SyncTakenFile} {
		if fileExists(filepath.Join(root, tg.Path, f)) {
			t.Errorf("%s left", f)
		}
	}
	// The mark names the parent that wrote, never the sync's id, and why.
	mark := readFile(filepath.Join(root, tg.Path, SyncIncompleteFile))
	if _, from, code, why := ParseIncomplete(mark); from != "vm1:prod" || code != IncompleteCopy || !strings.Contains(why, "was cut") {
		t.Errorf("the mark: %q", mark)
	}
	// Nothing but a sync runs on it.
	write(t, filepath.Join(root, tg.Path, "releases", "4", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
	plain := exec.Command("sh", "-c", ActivateScript(tg, "4", false, "app-dev", phases, nil))
	plain.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}
	if out, err := plain.CombinedOutput(); err == nil || !strings.Contains(string(out), "did not complete") || current(root) != "releases/2/code" || !fileExists(filepath.Join(root, tg.Path, "releases", "4", FailedFile)) {
		t.Errorf("a release on a partial copy (%v):\n%s", err, out)
	}
	calls(root)
	// A sync that writes nothing (the parent's labels cover nothing now)
	// leaves the earlier sync's partial copy as it was: the mark stays.
	if _, errOut, err := run(root, "5", "go\n"); err == nil || current(root) != "releases/2/code" || !strings.Contains(errOut, "nothing was copied") || !fileExists(filepath.Join(root, tg.Path, SyncIncompleteFile)) {
		t.Errorf("a sync writing nothing on a partial copy (%v), current %q:\n%s", err, current(root), errOut)
	}
	calls(root)

	// devopsy gone before the switch (its stdin ends): the watcher
	// cancels the release before anything of the sync runs.
	root = t.TempDir()
	if _, errOut, err := run(root, "1", ""); err == nil || current(root) != "" {
		t.Fatalf("devopsy gone (%v), current %q:\n%s", err, current(root), errOut)
	}
	if got := calls(root); strings.Contains(got, "--sync-services") || strings.Contains(got, "deploy") {
		t.Errorf("devopsy gone, calls:\n%s", got)
	}

	// A holder whose session is gone before it starts reading (its
	// watcher reads EOF at once and leaves): no answer, never a wait on
	// the fifo, nothing left in $base.
	root = t.TempDir()
	write(t, filepath.Join(root, tg.Path, "releases", "1", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
	if err := os.Symlink("releases/1/code", filepath.Join(root, tg.Path, "current")); err != nil {
		t.Fatal(err)
	}
	gone := exec.Command("sh", "-c", HolderScript(tg, "app-dev", opts, true))
	gone.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}
	var goneOut bytes.Buffer
	gone.Stdout, gone.Stderr = &goneOut, &goneOut
	if err := gone.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- gone.Wait() }()
	// Either its watcher ends it before it holds the lock (nothing
	// prepared), or it gets there first and finds no answer.
	select {
	case err := <-waited:
		noAnswer := strings.Contains(readFile(filepath.Join(root, tg.Path, ".devopsy-sync.log")), "no answer from devopsy")
		if err == nil || (!noAnswer && strings.Contains(calls(root), "--sync-services prepare")) {
			t.Errorf("holder with its session gone (%v):\n%s", err, goneOut.String())
		}
	case <-time.After(30 * time.Second):
		_ = gone.Process.Kill()
		t.Fatalf("holder with its session gone hangs:\n%s", goneOut.String())
	}
	if left, _ := filepath.Glob(filepath.Join(root, tg.Path, ".devopsy-sync.answer*")); len(left) > 0 {
		t.Errorf("left in $base: %v", left)
	}

	// A failing prepare prints no marker, restores, and does not go live.
	root = t.TempDir()
	out, errOut, err = run(root, "1", "go\n", "FAIL_PREPARE=3")
	if err == nil || strings.Contains(out+errOut, SyncMarker) || current(root) != "" || !strings.Contains(errOut, "preparing the sync failed (3)") {
		t.Fatalf("failing prepare (%v), current %q:\nstdout %q\n%s", err, current(root), out, errOut)
	}
	if got := calls(root); !strings.Contains(got, "--sync-services finish") || strings.Contains(got, "deploy") {
		t.Errorf("failing prepare calls:\n%s", got)
	}

	// A re-sync's pending inherited variables: read by the sync (the
	// variable names them), moved into shared/inherit.env on go and linked
	// into the release, removed on abort; another sync's (its own id) is
	// never read.
	root = t.TempDir()
	base = filepath.Join(root, tg.Path)
	pending := filepath.Join(base, SyncInheritPending("1.2"))
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
	write(t, filepath.Join(base, SyncInheritPending("9.9")), "STALE=1\n")
	if _, errOut, err := run(root, "3", "go\n"); err != nil || readFile(filepath.Join(base, "shared", "inherit.env")) != "SYNC_DB='stream'\n" {
		t.Errorf("another sync's pending (%v): %q\n%s", err, readFile(filepath.Join(base, "shared", "inherit.env")), errOut)
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
		// stdin ends with the session, as devopsy's pipe does.
		in, inW, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer in.Close()
		cmd.Stdin = in
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
		inW.Close()
		write(t, filepath.Join(root, "closed"), "")
		_ = cmd.Wait()
	}
	for _, fail := range []string{"0", "3"} {
		root := t.TempDir()
		write(t, filepath.Join(root, tg.Path, "releases", "1", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
		hangup(root, ActivateScript(tg, "1", false, "app-dev", phases, &SyncOptions{From: "vm1:prod", ID: "1.2"}), "FAIL_PREPARE="+fail)
		if got := calls(root); !strings.Contains(got, "--sync-services finish") || strings.Contains(got, "deploy") {
			t.Errorf("release, session gone, prepare exits %s: calls:\n%s", fail, got)
		}
		root = t.TempDir()
		write(t, filepath.Join(root, tg.Path, "releases", "1", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
		if err := os.Symlink("releases/1/code", filepath.Join(root, tg.Path, "current")); err != nil {
			t.Fatal(err)
		}
		hangup(root, HolderScript(tg, "app-dev", &SyncOptions{From: "vm1:prod", ID: "1.2"}, true), "FAIL_PREPARE="+fail)
		if got := calls(root); !strings.Contains(got, "--sync-services finish") || strings.Contains(got, "deploy") {
			t.Errorf("--sync, session gone, prepare exits %s: calls:\n%s", fail, got)
		}
	}
}

// A sync's holder arms its watcher before it waits for the child's
// release lock, so its file is there while a release holds that lock; a
// release that takes the lock meanwhile (the activation after its upload,
// another release's upload) must leave it alone: the holder's session is
// alive, and once the lock comes the sync goes on. Once prelude removed
// every watcher's file as stale, the waiting holder's included, and the
// holder then took its watcher for fired: "the session ended while this
// waited for the release lock", its operator there.
func TestHolderArmedThroughRelease(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, "devopsy"), `#!/bin/sh
if [ "$1" = --release-settings ]; then printf 'root=%s\nkeep=5\nmax_keep=5\nprotocol=8\n' "$FAKE_ROOT"; exit; fi
echo "$*" >> "$FAKE_ROOT/calls"
exit 0
`)
	fakeImageTools(t, bin)
	// The holder's release lock is busy until $FAKE_ROOT/free exists;
	// everything else's is free.
	write(t, filepath.Join(bin, "flock"), `#!/bin/sh
if [ -n "${FAKE_WAIT:-}" ]; then
  case " $* " in
    *" -n 9 ") exit 1 ;;
    *" -w 600 9 ") : >"$FAKE_ROOT/waiting"; while [ ! -e "$FAKE_ROOT/free" ]; do sleep 0.05; done ;;
  esac
fi
exit 0
`)
	if err := os.Chmod(filepath.Join(bin, "flock"), 0o755); err != nil {
		t.Fatal(err)
	}
	tg := &Target{Name: "dev", Address: "vm2:dev", Path: "app/dev"}
	root := t.TempDir()
	base := filepath.Join(root, tg.Path)
	write(t, filepath.Join(base, "releases", "1", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
	if err := os.Symlink("releases/1/code", filepath.Join(base, "current")); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}

	holder := exec.Command("sh", "-c", HolderScript(tg, "app-dev", &SyncOptions{From: "vm1:prod", ID: "1.2"}, true))
	holder.Env = append(env, "FAKE_WAIT=1")
	// The operator's pipe, open for as long as the holder runs, the
	// answer in it.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	holder.Stdin = r
	var out bytes.Buffer
	holder.Stdout, holder.Stderr = &out, &out
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	r.Close()
	_, _ = w.WriteString("go\n")
	done := make(chan error, 1)
	go func() { done <- holder.Wait() }()
	for i := 0; !fileExists(filepath.Join(root, "waiting")); i++ {
		if i > 400 {
			_ = holder.Process.Kill()
			t.Fatalf("the holder never waited for the lock:\n%s", out.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
	if armed, _ := filepath.Glob(filepath.Join(base, ".devopsy-watch.*")); len(armed) != 1 {
		t.Fatalf("the holder's watcher is not armed: %v", armed)
	}

	// A release takes the lock meanwhile (its prelude), then frees it.
	release := exec.Command("sh", "-c", prelude(tg, false)+"echo released\n")
	release.Env = env
	if o, err := release.CombinedOutput(); err != nil || !strings.Contains(string(o), "released") {
		t.Fatalf("the release's prelude (%v):\n%s", err, o)
	}
	write(t, filepath.Join(root, "free"), "")

	select {
	case err := <-done:
		if err != nil || strings.Contains(out.String(), "the session ended while this waited") || !strings.Contains(readFile(filepath.Join(root, "calls")), "--sync-services prepare") {
			t.Errorf("the holder, its session alive, after a release took the lock (%v):\n%s", err, out.String())
		}
	case <-time.After(30 * time.Second):
		_ = holder.Process.Kill()
		t.Fatalf("the holder hangs:\n%s", out.String())
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
# FAKE_RELEASE_BUSY: a pending release's lock (fd 7) is held.
[ -z "${FAKE_RELEASE_BUSY:-}" ] || case " $* " in *" 7 ") exit 1 ;; esac
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
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\ncase \"$1\" in --images|"+WatchSessionFlag+") exec "+Quote(realDevopsy)+" \"$@\" ;; esac\nexec \"$(dirname \"$0\")/devopsy.fake\" \"$@\"\n")
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
if [ "$1" = --release-settings ]; then printf 'root=%s\nkeep=5\nmax_keep=5\nprotocol=8\n' "$FAKE_ROOT"; exit; fi
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
  --mnt-owners*) [ "$id" != "${FAIL_OWNERS:-}" ] || exit 5 ;;
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
	if got, want := calls(root), "1: --missing-vars\n1: --check-names "+base+"\n1: --check-mounts\n1: -- pull --ignore-buildable\n1: -- build --pull\n1: -- config --images\ncurrent: --mnt-owners\ncurrent: deploy\n"; got != want {
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

	// A failed run step: what its containers were left as reported, then
	// release 1's images restored, nothing built or pulled for it, then
	// its restart.
	if out, err := release("3", "FAIL_RUN=4"); err == nil || current() != "releases/1/code" || !strings.Contains(out, "images of 1 restored") {
		t.Fatalf("failed run (%v), current %q:\n%s", err, current(), out)
	}
	if got, want := calls(root), "3: --missing-vars\n3: --check-names "+base+"\n3: --check-mounts\n3: -- pull --ignore-buildable\n3: -- build --pull\n3: -- config --images\ncurrent: --mnt-owners\ncurrent: deploy\ncurrent: -- --profile * ps -a --format {{.Name}} {{.Service}} {{.State}} {{.ExitCode}} {{.Health}}\ncurrent: --mnt-owners restored\ncurrent: --release-steps restart run\n"; got != want {
		t.Errorf("failed run calls:\n%s\nwant:\n%s", got, want)
	}
	if image(root, "app-prod-app") != "1" || image(root, "mariadb:11") != "1" {
		t.Errorf("after a failed run the images are not release 1's: %q %q", image(root, "app-prod-app"), image(root, "mariadb:11"))
	}

	// Giving mnt's entries to their owners fails: a failed run step,
	// never run, and the previous release back, its own owners first.
	if out, err := release("32", "FAIL_OWNERS=32"); err == nil || current() != "releases/1/code" || !strings.Contains(out, "giving the entries of mnt to their owners failed (5), current is back to 1") {
		t.Fatalf("failed owners (%v), current %q:\n%s", err, current(), out)
	}
	if got := calls(root); strings.Contains(got, "current: deploy") || !strings.HasSuffix(got, "current: --mnt-owners restored\ncurrent: --release-steps restart run\n") {
		t.Errorf("failed owners calls:\n%s", got)
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
	// Its owners from its own labels, as a restored release: labels it
	// cannot read only warn (cmd/devopsy's TestMntOwnersRelease).
	if got := calls(root); strings.Contains(got, "pull") || strings.Contains(got, "build") || !strings.Contains(got, "current: --mnt-owners restored\n") {
		t.Errorf("rollback fetched images, or gave owners as a release:\n%s", got)
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

	// A first release whose build fails stays, marked failed, the images
	// it got bookmarked: they go with it (--destroy, --releases remove).
	first := func(env ...string) (string, string, error) {
		root := t.TempDir()
		write(t, filepath.Join(root, build.Path, "releases", "1", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
		out, err := run(root, script(build, "1", false), append([]string{"FAIL_BUILD=3"}, env...)...)
		return root, out, err
	}
	r, out, err := first()
	if _, statErr := os.Stat(filepath.Join(r, build.Path, "releases", "1", FailedFile)); err == nil || statErr != nil || image(r, "mariadb:devopsy-app-prod-1") != "1" {
		t.Errorf("failed first release (%v):\n%s", err, out)
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
	if err == nil || !fileExists(filepath.Join(rel1, FailedFile)) || fileExists(filepath.Join(rel1, FetchingFile)) || image(r, "mariadb:devopsy-app-prod-1") != "1" {
		t.Errorf("failed first release, lock taken after the fetch (%v):\n%s", err, out)
	}

	image2 := &Target{Name: "prod", Path: "app/prod", Mode: ModeImage}
	root = t.TempDir()
	write(t, filepath.Join(root, image2.Path, "releases", "1", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
	if out, err := run(root, script(image2, "1", false)); err != nil {
		t.Fatalf("image (%v):\n%s", err, out)
	}
	if got, want := calls(root), "1: --missing-vars\n1: --check-names "+filepath.Join(root, image2.Path)+"\n1: --check-mounts\n1: -- pull\n1: -- config --images\ncurrent: --mnt-owners\ncurrent: deploy\n"; got != want {
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
if [ "$1" = --release-settings ]; then printf 'root=%s\nkeep=1\nmax_keep=5\nprotocol=8\n' "$FAKE_ROOT"; exit; fi
echo "$*" >>"$FAKE_ROOT/calls"
mkdir -p "$FAKE_ROOT/images"
case "$*" in
  "--stdin-guard") exec cat ;;
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
		// The ack a gated removal takes once it holds the lock.
		cmd.Stdin = strings.NewReader(AckInput)
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
		out, err := run(ReleasesRemoveScript(target("a"), "", ids, false))
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
	if out, err := run(ReleasesRemoveScript(target("a"), "", []string{"1"}, false), "FAKE_IN_USE=mariadb:devopsy-app-a-prod-1"); err != nil || exists("app/a/prod/releases/1") || !strings.Contains(out, "removed release 1") || !strings.Contains(out, "mariadb:devopsy-app-a-prod-1 stays: conflict") || !strings.Contains(out, "removed registry/app:def, left by release 3 of app-b-prod") || strings.Contains(out, "other:1") || !exists(filepath.Join("other", ImagesLeftFile)) {
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
	// A release never made current, cut off mid-fetch: in progress while
	// its activation holds its marker's lock, else failed (a fetch cut
	// off).
	write(t, filepath.Join(root, "app/a/prod/releases/5", PendingFile), "")
	write(t, filepath.Join(root, "app/a/prod/releases/5", FetchingFile), "")
	for env, want := range map[string]string{"FAKE_UNUSED=1": "FAILED", "FAKE_RELEASE_BUSY=1": "IN PROGRESS"} {
		out, err := run(ReleasesScript(target("a")), env)
		if rs := ParseReleases(out); err != nil || len(rs) != 2 || !strings.HasSuffix(rs[0].Line(), "  "+want) {
			t.Errorf("pending release with %s (%v): %+v\n%s", env, err, rs, out)
		}
	}
	// Pending alone, nothing running it: incomplete.
	os.Remove(filepath.Join(root, "app/a/prod/releases/5", FetchingFile))
	if out, err := run(ReleasesScript(target("a"))); err != nil || !strings.HasSuffix(ParseReleases(out)[0].Line(), "  INCOMPLETE") {
		t.Errorf("pending alone (%v):\n%s", err, out)
	}
	os.RemoveAll(filepath.Join(root, "app/a/prod/releases/5"))

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
	if out, err := run(ReleasesRemoveScript(target("a"), "", []string{"7"}, false), "FAKE_IN_USE=mariadb:devopsy-app-a-prod-1"); err != nil || fakeImage(root, "old:1") != "" || exists("app/a/prod/releases/7") {
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
if [ "$1" = --release-settings ]; then printf 'root=%s\nkeep=5\nmax_keep=5\nprotocol=8\n' "$FAKE_ROOT"; exit; fi
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
if [ "$1" = --release-settings ]; then printf 'root=%s\nkeep=5\nmax_keep=5\nprotocol=8\n' "$FAKE_ROOT"; exit; fi
id=$(pwd -P); id=${id%/code}; id=${id##*/}
mkdir -p "$FAKE_ROOT/images"
case "$*" in
  "-- config --images") printf 'app-prod-app\nmariadb:11\n' ;;
  "-- pull --ignore-buildable") echo "$id" >"$FAKE_ROOT/images/mariadb:11" ;;
  "-- build --pull")
    if [ -n "${FAKE_HANG:-}" ]; then touch "$FAKE_ROOT/building"; sleep 30; exit 1; fi
    if [ -n "${FAKE_DROP:-}" ]; then
      touch "$FAKE_ROOT/building"
      while [ ! -e "$FAKE_ROOT/dropped" ]; do sleep 0.05; done
    fi
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

	// Release 3a's session drops while it builds, with no signal (-T):
	// the build ends well and img bookmark ignores SIGPIPE, so the shell
	// must die at a write before the marker goes and current switches.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	dropped := command("3a", "FAKE_DROP=1")
	dropped.Stdout, dropped.Stderr = w, w
	os.Remove(filepath.Join(root, "building"))
	if err := dropped.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	for i := 0; i < 100 && !fileExists(filepath.Join(root, "building")); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	r.Close()
	write(t, filepath.Join(root, "dropped"), "")
	if err := dropped.Wait(); err == nil {
		t.Errorf("release 3a went on without its session")
	}
	if l, _ := os.Readlink(filepath.Join(base, "current")); l != "releases/3/code" || !exists("releases/3a/"+FetchingFile) || fakeImage(root, "app-prod-app:devopsy-app-prod-3a") != "3a" {
		t.Errorf("dropped release 3a: current %q, marker %v, bookmark %q", l, exists("releases/3a/"+FetchingFile), fakeImage(root, "app-prod-app:devopsy-app-prod-3a"))
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

	// Release 4b gets Ctrl-C right after its fetch (here as the fetching
	// marker goes): ignored until the run step, so nothing of the switch
	// dies halfway, and the release goes live with its names.
	// As a terminal's Ctrl-C does: SIGINT to the whole group, rm too.
	write(t, filepath.Join(bin, "rm"), "#!/bin/sh\ncase \"$*\" in *"+FetchingFile+"*) [ -z \"${FAKE_INT_AT_RM:-}\" ] || kill -INT 0 ;; esac\nexec /bin/rm \"$@\"\n")
	if err := os.Chmod(filepath.Join(bin, "rm"), 0o755); err != nil {
		t.Fatal(err)
	}
	late := command("4b", "FAKE_INT_AT_RM=1")
	late.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	outLate, errLate := late.CombinedOutput()
	os.Remove(filepath.Join(bin, "rm"))
	if l, _ := os.Readlink(filepath.Join(base, "current")); errLate != nil || l != "releases/4b/code" || exists("releases/4b/"+FailedFile) || fakeImage(root, "mariadb:11") != "4b" {
		t.Errorf("Ctrl-C right after the fetch (%v): current %q:\n%s", errLate, l, outLate)
	}

	// Release 5's -T session drops while it builds (watchSession): its
	// stdin ends, the script's group gets SIGTERM, and the fetch is
	// undone at once, not a build later. Setpgid: the shell leads its
	// group, as under sshd.
	r, w, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatal(pipeErr)
	}
	watched := command("5", "FAKE_HANG=1", WatchEnv+"=1")
	watched.Stdin = r
	var buf5 bytes.Buffer
	watched.Stdout, watched.Stderr = &buf5, &buf5
	watched.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	os.Remove(filepath.Join(root, "building"))
	if err := watched.Start(); err != nil {
		t.Fatal(err)
	}
	r.Close()
	for i := 0; i < 100 && !fileExists(filepath.Join(root, "building")); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	start := time.Now()
	w.Close()
	_ = watched.Wait()
	if code := watched.ProcessState.ExitCode(); code != 130 || time.Since(start) > 10*time.Second || !exists("releases/5/"+FailedFile) || exists("releases/5/"+FetchingFile) {
		t.Errorf("release 5 after its session ended: exit %d in %v, failed %v, fetching %v:\n%s", code, time.Since(start), exists("releases/5/"+FailedFile), exists("releases/5/"+FetchingFile), buf5.String())
	}
	if l, _ := os.Readlink(filepath.Join(base, "current")); l != "releases/4b/code" || fakeImage(root, "mariadb:11") != "4b" {
		t.Errorf("after release 5's cancel: current %q, mariadb:11 %q", l, fakeImage(root, "mariadb:11"))
	}
	// Nobody reads the session: the undo wrote to the release's server
	// log, never to it.
	if o := readFile(filepath.Join(base, "releases/5", ServerLogFile)); !strings.Contains(o, LogSection) || !strings.Contains(o, "the session is gone: undoing the fetch") || strings.Contains(buf5.String(), "undoing the fetch") {
		t.Errorf("release 5's undo, its log:\n%s\nits session:\n%s", o, buf5.String())
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
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\nif [ \"$1\" = --release-settings ]; then printf 'root=%s\\nkeep=5\\nmax_keep=5\\nprotocol=8\\n' \"$FAKE_ROOT\"; fi\nexit 0\n")
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
	cmd := exec.Command("sh", "-c", ActivateScript(tg, "6", false, "", Phases{Run: []string{"deploy"}}, nil))
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
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\nprintf 'root=%s\\nkeep=5\\nmax_keep=5\\nprotocol=8\\n' \"$FAKE_ROOT\"\n")
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

// The upload takes the release lock: --destroy removes $base meanwhile.
// Every release links inherit.env, so
// --inherit reaches one released before the parent had inherit:. A sync's
// variables stop at its end.
func TestScriptsLockAndLinks(t *testing.T) {
	tg := &Target{Name: "prod", Path: "app/prod", Mode: ModeImage, Project: &Project{Name: "app"}}
	if s := UploadScript(tg, false); !strings.Contains(s, `exec 9>"$base/.lock"`) {
		t.Errorf("upload without the lock:\n%s", s)
	}
	// The upgrade hint names an alias whatever its source: --upgrade
	// needs only a server.
	if s := basePrelude(&Target{Name: "prod", Alias: "vm1-reg", Address: "vm1:prod", Host: "vm1"}); !strings.Contains(s, "devopsy @vm1-reg --upgrade") {
		t.Errorf("upgrade hint without the alias:\n%s", s)
	}
	// Hints in scripts name the alias as typed.
	aliased := &Target{Name: "prod", Alias: "vm1-traefik", Source: "~/traefik", Address: "vm1:prod", Path: "traefik/prod"}
	if s := HolderScript(aliased, "x", &SyncOptions{}, false); !strings.Contains(s, "devopsy @vm1-traefik --release") {
		t.Errorf("hint without the alias:\n%s", s)
	}
	// A release is pending until its switch, and cut uploads go.
	if s := UploadScript(tg, false); !strings.Contains(s, `: >"$rel.tmp/`+PendingFile+`"`) || !strings.Contains(s, `rm -rf "$base"/releases/*.tmp`) {
		t.Errorf("upload without its pending marker or the stale uploads' cleanup:\n%s", s)
	}
	act := ActivateScript(tg, "1", false, "app-prod", Phases{Run: []string{"deploy"}}, &SyncOptions{From: "devopsy@one:prod"})
	if !strings.Contains(act, `ln -sfn "$base/shared/`+InheritFile+`" "$code/.devopsy/`+InheritFile+`"`) {
		t.Errorf("inherit.env not always linked")
	}
	if !strings.Contains(act, "unset "+SyncFromTargetEnv) {
		t.Errorf("the sync's variables reach the run step")
	}
}

// A .lock removed while its lock was awaited (--destroy, a failed first
// release removing $base) is opened again: a lock on the removed file
// keeps nothing out. Without create, a $base gone fails.
func TestReleaseLockReopens(t *testing.T) {
	bin, root := t.TempDir(), t.TempDir()
	base := filepath.Join(root, "app", "prod")
	// The first flock "waits" while the file goes; later ones lock.
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\necho flock >> \"$ROOT/calls\"\n[ -e \"$ROOT/once\" ] || { : > \"$ROOT/once\"; rm -rf \"$BASE\"; mkdir -p \"$BASE\"; }\nexit 0\n")
	// The scripts say things through the server's devopsy.
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\n")
	for _, f := range []string{"flock", "devopsy"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run := func(create bool) (string, error) {
		cmd := exec.Command("sh", "-c", "set -eu\nbase=\"$BASE\"\n"+releaseLock(false, create)+"echo locked\n")
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "ROOT=" + root, "BASE=" + base}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	// Twice where /dev/fd names the open file (Linux, the servers); once
	// on macOS, which cannot tell and never loops.
	want := 2
	if runtime.GOOS == "darwin" {
		want = 1
	}
	if out, err := run(true); err != nil || !strings.Contains(out, "locked") || strings.Count(readFile(filepath.Join(root, "calls")), "flock") != want {
		t.Errorf("reopen (%v), calls %q:\n%s", err, readFile(filepath.Join(root, "calls")), out)
	}
	os.RemoveAll(base)
	if out, err := run(false); err == nil || !strings.Contains(out, "is gone") {
		t.Errorf("gone (%v):\n%s", err, out)
	}

	// removeBase moves $base aside before rm -rf runs, so a release
	// arriving while rm -rf is under way creates and locks a fresh $base
	// instead of putting files inside the directory rm -rf is emptying.
	write(t, filepath.Join(base, "old"), "1")
	concurrent := "set -eu\nbase=\"$BASE\"\n" + releaseLock(false, true) + ": >\"$base/new\"\n"
	write(t, filepath.Join(bin, "rm"), "#!/bin/sh\nsh -c "+Quote(concurrent)+"\nexec /bin/rm \"$@\"\n")
	if err := os.Chmod(filepath.Join(bin, "rm"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "set -eu\nbase=\"$BASE\"\n"+removeBase+"\n")
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "ROOT=" + root, "BASE=" + base}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("removeBase (%v):\n%s", err, out)
	}
	if fileExists(filepath.Join(base, "old")) || !fileExists(filepath.Join(base, "new")) {
		t.Errorf("after removeBase: old=%v, new=%v", fileExists(filepath.Join(base, "old")), fileExists(filepath.Join(base, "new")))
	}

	// What rm -rf could not remove goes back to $base, so running it
	// again finds it, unless a new $base exists already.
	moved := func() []string { m, _ := filepath.Glob(base + ".rm.*"); return m }
	for _, concurrently := range []bool{false, true} {
		os.RemoveAll(base)
		for _, m := range moved() {
			os.RemoveAll(m)
		}
		write(t, filepath.Join(base, "old"), "1")
		fake := "#!/bin/sh\nexit 1\n"
		if concurrently {
			fake = "#!/bin/sh\nsh -c " + Quote(concurrent) + "\nexit 1\n"
		}
		write(t, filepath.Join(bin, "rm"), fake)
		cmd := exec.Command("sh", "-c", "set -eu\nbase=\"$BASE\"\n"+removeBase+"\n")
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "ROOT=" + root, "BASE=" + base}
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("removeBase with rm failing (concurrently %v) succeeded:\n%s", concurrently, out)
		}
		if concurrently {
			if !fileExists(filepath.Join(base, "new")) || fileExists(filepath.Join(base, "old")) || len(moved()) != 1 {
				t.Errorf("rm failing beside a new $base: new %v, old %v, aside %v", fileExists(filepath.Join(base, "new")), fileExists(filepath.Join(base, "old")), moved())
			}
		} else if !fileExists(filepath.Join(base, "old")) || len(moved()) != 0 {
			t.Errorf("rm failing: old back %v, aside %v", fileExists(filepath.Join(base, "old")), moved())
		}
	}
}

// From the switch on, a release finishes or goes back whatever happens
// to its session (followOutput): one gone without a signal mid-run kills
// only the tails, and the run step ends well; so does one that gets
// Ctrl-C, which nothing there takes (script in a session of its own).
// GNU's tail only: as before without it.
func TestAfterSwitchSurvives(t *testing.T) {
	// The script's PATH, not the test's: a GNU tail from Homebrew is not
	// the one it finds.
	if exec.Command("/bin/sh", "-c", "PATH=/usr/bin:/bin tail --version").Run() != nil {
		t.Skip("needs GNU tail (--pid)")
	}
	bin := t.TempDir()
	write(t, filepath.Join(bin, "devopsy"), `#!/bin/sh
if [ "$1" = --release-settings ]; then printf 'root=%s\nkeep=5\nmax_keep=5\nprotocol=8\n' "$FAKE_ROOT"; exit; fi
case "$*" in
  "-- config --images") printf 'app-prod-app\n' ;;
  "-- build --pull") id=$(pwd -P); id=${id%/code}; echo "${id##*/}" >"$FAKE_ROOT/images/app-prod-app" ;;
  deploy)
    if [ -n "${FAKE_WAIT:-}" ]; then
      touch "$FAKE_ROOT/running"
      while [ ! -e "$FAKE_ROOT/go" ]; do sleep 0.05; done
    fi
    [ -z "${FAKE_FAIL:-}" ] || exit 1
    echo "deployed $(pwd -P)"; echo "deploy progress" >&2 ;;
  "--release-steps restart run") echo restarted >>"$FAKE_ROOT/calls" ;;
esac
exit 0
`)
	fakeImageTools(t, bin)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	tg := &Target{Name: "prod", Path: "app/prod", Mode: ModeBuild}
	base := filepath.Join(root, tg.Path)
	command := func(id string, env ...string) *exec.Cmd {
		write(t, filepath.Join(base, "releases", id, CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
		cmd := exec.Command("sh", "-c", ActivateScript(tg, id, false, "app-prod", Phases{Run: []string{"deploy"}}, nil))
		cmd.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}, env...)
		return cmd
	}
	current := func() string { l, _ := os.Readlink(filepath.Join(base, "current")); return l }
	out, err := command("1").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "deployed") || !strings.Contains(string(out), "deploy progress") {
		t.Fatalf("release 1 (%v), its output through the tails:\n%s", err, out)
	}

	// running starts cmd, waits for its run step, does what it is given
	// and lets the step go on.
	running := func(cmd *exec.Cmd, during func()) {
		t.Helper()
		os.Remove(filepath.Join(root, "running"))
		os.Remove(filepath.Join(root, "go"))
		for i := 0; i < 200 && !fileExists(filepath.Join(root, "running")); i++ {
			time.Sleep(50 * time.Millisecond)
		}
		during()
		write(t, filepath.Join(root, "go"), "")
		_ = cmd.Wait()
	}

	// Release 2's session goes mid-run, no signal.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	// With the watcher (WatchEnv, its stdin ending with the session): past
	// the switch it is disarmed, so the drop cancels nothing.
	in, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	dropped := command("2", "FAKE_WAIT=1", WatchEnv+"=1")
	dropped.Stdin = in
	dropped.Stdout, dropped.Stderr = w, w
	dropped.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := dropped.Start(); err != nil {
		t.Fatal(err)
	}
	w.Close()
	in.Close()
	running(dropped, func() { r.Close(); inW.Close() })
	if code := dropped.ProcessState.ExitCode(); code != 0 || current() != "releases/2/code" || fileExists(filepath.Join(base, "releases/2", FailedFile)) {
		t.Errorf("release 2 after its session went: exit %d, current %q", code, current())
	}
	if o := readFile(filepath.Join(base, "releases/2", ServerLogFile)); !strings.Contains(o, "deployed") {
		t.Errorf("release 2's output file: %q", o)
	}

	// Release 3 gets Ctrl-C mid-run: SIGINT to the session's group, the
	// run step under script (a terminal's size set), which would read it
	// even ignored and end its child, exiting 0.
	var buf bytes.Buffer
	interrupted := command("3", "FAKE_WAIT=1", "DEVOPSY_INTERNAL_TTY=80 24")
	interrupted.Stdout, interrupted.Stderr = &buf, &buf
	interrupted.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	os.Remove(filepath.Join(root, "calls"))
	if err := interrupted.Start(); err != nil {
		t.Fatal(err)
	}
	running(interrupted, func() {
		if err := syscall.Kill(-interrupted.Process.Pid, syscall.SIGINT); err != nil {
			t.Fatal(err)
		}
	})
	if interrupted.ProcessState.ExitCode() != 0 || current() != "releases/3/code" || fileExists(filepath.Join(base, "releases/3", FailedFile)) || fileExists(filepath.Join(root, "calls")) || !strings.Contains(readFile(filepath.Join(base, "releases/3", ServerLogFile)), "deployed") {
		t.Errorf("release 3 after Ctrl-C: exit %d, current %q, calls %q:\n%s", interrupted.ProcessState.ExitCode(), current(), readFile(filepath.Join(root, "calls")), buf.String())
	}

	// --log after the drop: no log saved, the output file instead.
	logCmd := exec.Command("sh", "-c", LogReadScript(tg, "2"))
	logCmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}
	if out, err := logCmd.CombinedOutput(); err != nil || !strings.Contains(string(out), LogServerPart+"\n"+LogSection) || !strings.Contains(string(out), "deployed") {
		t.Errorf("--log of release 2 (%v):\n%s", err, out)
	}

	// On a terminal (a pty, as on -t), the last lines reach it: the pty
	// hangs up as the shell exits, so the shell waits for the tail.
	pty := command("4", "FAKE_FAIL=1")
	pty.Args = []string{"script", "-qec", "sh -c " + Quote(pty.Args[2]), "/dev/null"}
	pty.Path, _ = exec.LookPath("script")
	if pty.Path == "" {
		t.Skip("needs script")
	}
	if out, _ := pty.CombinedOutput(); !strings.Contains(string(out), "restarting 3 with its own run step") {
		t.Errorf("release 4 on a pty, its last line lost:\n%s", out)
	}

	// A rollback to 2 appends to 2's output, after its own.
	rollback := exec.Command("sh", "-c", ActivateScript(tg, "", true, "app-prod", Phases{}, nil))
	rollback.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}
	rbOut, err := rollback.CombinedOutput()
	if err != nil {
		t.Fatalf("rollback (%v):\n%s", err, rbOut)
	}
	if o := readFile(filepath.Join(base, "releases/2", ServerLogFile)); strings.Count(o, LogSection) != 2 || !strings.Contains(o, "deployed") {
		t.Errorf("release 2's output after a rollback to it:\n%s\nthe rollback's session:\n%s", o, rbOut)
	}
}

// A failed first release is marked failed and leaves the other releases
// (one uploaded meanwhile waits for the lock); an activation whose
// release or environment went while it waited fails, and never makes
// $base again.
func TestActivateGoneAndNeighbours(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, "devopsy"), `#!/bin/sh
if [ "$1" = --release-settings ]; then printf 'root=%s\nkeep=5\nmax_keep=5\nprotocol=8\n' "$FAKE_ROOT"; exit; fi
case "$*" in
  fail) exit 3 ;;
  "-- config --images") printf 'app-prod-app\n' ;;
esac
exit 0
`)
	fakeImageTools(t, bin)
	root := t.TempDir()
	tg := &Target{Name: "prod", Path: "app/prod", Mode: ModeBuild}
	base := filepath.Join(root, tg.Path)
	run := func(id string) (string, error) {
		cmd := exec.Command("sh", "-c", ActivateScript(tg, id, false, "app-prod", Phases{Prepare: [][]string{{"fail"}}, Run: []string{"deploy"}}, nil))
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for _, id := range []string{"1", "2"} {
		write(t, filepath.Join(base, "releases", id, CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
	}
	if out, err := run("1"); err == nil || !fileExists(filepath.Join(base, "releases", "2", CodeDir)) || !fileExists(filepath.Join(base, "releases", "1", FailedFile)) {
		t.Errorf("first release failing beside another (%v):\n%s", err, out)
	}
	os.RemoveAll(base)
	if out, err := run("3"); err == nil || !strings.Contains(out, "is gone") || fileExists(base) {
		t.Errorf("activation of a gone environment (%v), base made again %v:\n%s", err, fileExists(base), out)
	}
	write(t, filepath.Join(base, "releases", "4", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
	if out, err := run("5"); err == nil || !strings.Contains(out, "release 5 is gone") {
		t.Errorf("activation of a gone release (%v):\n%s", err, out)
	}
}

// --status's last sync is the last copy that went through (SyncCopied),
// whatever its run step did: a failed copy after it does not count.
func TestStatusLastSync(t *testing.T) {
	bin, root := t.TempDir(), t.TempDir()
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\nif [ \"$1\" = --release-settings ]; then printf 'root=%s\\nkeep=5\\nmax_keep=5\\nprotocol=8\\n' \"$FAKE_ROOT\"; fi\nexit 0\n")
	if err := os.Chmod(filepath.Join(bin, "devopsy"), 0o755); err != nil {
		t.Fatal(err)
	}
	tg := &Target{Name: "dev", Path: "app/dev"}
	base := filepath.Join(root, tg.Path)
	write(t, filepath.Join(base, "releases", "1", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
	if err := os.Symlink("releases/1/code", filepath.Join(base, "current")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(base, "releases", "1", LogFile), ""+
		"2026-10-01 10:00:00 devopsy release: release of @dev\n"+
		"2026-10-01 10:00:01 Syncing @prod's data into it before its run step\n"+
		"2026-10-01 10:00:05 "+SyncCopied+"prod\n"+
		"2026-10-01 10:00:09 devopsy: release exited 0\n"+
		"\n"+
		"2026-10-02 10:00:00 devopsy dev: sync of @dev\n"+
		"2026-10-02 10:00:03 "+SyncCopied+"prod\n"+
		"2026-10-02 10:00:05 devopsy: sync exited 1\n"+
		"\n"+
		"2026-10-03 10:00:00 devopsy dev: sync of @dev\n"+
		"2026-10-03 10:00:05 devopsy: sync exited 1\n")
	cmd := exec.Command("sh", "-c", StatusScript(tg))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "synced\t2026-10-02 10:00:03\n") {
		t.Errorf("last sync (%v):\n%s", err, out)
	}
}

// A step's text is printed as it is: a $VAR, a quote or a $(...) in it
// is neither expanded nor run by the line that says it runs, and the step
// itself gets its arguments as written.
func TestStepTextQuoted(t *testing.T) {
	bin, root := t.TempDir(), t.TempDir()
	write(t, filepath.Join(bin, "devopsy"), `#!/bin/sh
if [ "$1" = --release-settings ]; then printf 'root=%s\nkeep=5\nmax_keep=5\nprotocol=8\n' "$FAKE_ROOT"; exit; fi
case "$1" in deploy|prep) printf '%s|' "$@" >>"$FAKE_ROOT/args"; echo >>"$FAKE_ROOT/args" ;; esac
exit 0
`)
	fakeImageTools(t, bin)
	tg := &Target{Name: "prod", Path: "app/prod", Mode: ModeImage}
	base := filepath.Join(root, tg.Path)
	write(t, filepath.Join(base, "releases", "1", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
	pwned := filepath.Join(root, "pwned")
	odd := []string{"--tag=$TAG", `say "hi`, "$(touch " + pwned + ")"}
	script := ActivateScript(tg, "1", false, "app-prod", Phases{Prepare: [][]string{append([]string{"prep"}, odd...)}, Run: append([]string{"deploy"}, odd...)}, nil)
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}
	out, err := cmd.CombinedOutput()
	if err != nil || fileExists(pwned) || !strings.Contains(string(out), "devopsy: running 'devopsy deploy --tag=$TAG say \"hi $(touch ") {
		t.Fatalf("odd step text (%v), pwned %v:\n%s", err, fileExists(pwned), out)
	}
	want := "prep|--tag=$TAG|say \"hi|$(touch " + pwned + ")|\ndeploy|--tag=$TAG|say \"hi|$(touch " + pwned + ")|\n"
	if got := readFile(filepath.Join(root, "args")); got != want {
		t.Errorf("steps' arguments:\n%q\nwant\n%q", got, want)
	}
	if l, _ := os.Readlink(filepath.Join(base, "current")); l != "releases/1/code" {
		t.Errorf("current %q", l)
	}
}

// A sync's files with its lock held are a sync running, not leftovers:
// nothing refuses for them.
func TestIncompleteCheckBusy(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("needs flock")
	}
	base := t.TempDir()
	check := func() string {
		out, _ := exec.Command("sh", "-c", "base="+Quote(base)+"\n"+IncompleteCheck).CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	write(t, filepath.Join(base, ".lock"), "")
	write(t, filepath.Join(base, SyncStartedFile), "")
	if got := check(); got != "incomplete" {
		t.Errorf("leftovers: %q", got)
	}
	holder := exec.Command("flock", filepath.Join(base, ".lock"), "sleep", "5")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Process.Kill(); _ = holder.Wait() }()
	time.Sleep(300 * time.Millisecond)
	if got := check(); got != "" {
		t.Errorf("a sync running: %q", got)
	}
	write(t, filepath.Join(base, SyncIncompleteFile), "x\n")
	if got := check(); got != "incomplete\tx" {
		t.Errorf("marked: %q", got)
	}
}

// --status's sync lines: "syncing" while a sync's holder holds the
// child's lock (its holding file there), never for a holder killed
// outright once the lock is free; "source" while a sync from it (or an
// export) holds its sync lock. --releases marks the current release.
func TestSyncStatusLines(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("needs flock")
	}
	base := t.TempDir()
	status := func() string {
		out, _ := exec.Command("sh", "-c", "base="+Quote(base)+"\n"+strings.ReplaceAll(partialStatus, "%%", "%")).CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	hold := func(lock string) func() {
		t.Helper()
		write(t, filepath.Join(base, lock), "")
		// One process holds it: killed, the lock goes.
		h := exec.Command("sh", "-c", `exec 9>"$1"; flock 9; exec sleep 5`, "sh", filepath.Join(base, lock))
		if err := h.Start(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond)
		return func() { _ = h.Process.Kill(); _ = h.Wait() }
	}
	write(t, filepath.Join(base, SyncHoldingFile), "")
	if got := status(); got != "" {
		t.Errorf("a holder's file, the lock free: %q", got)
	}
	release := hold(".lock")
	if got := status(); got != "syncing" {
		t.Errorf("a holder holding the lock: %q", got)
	}
	release()
	release = hold(SyncLockFile)
	if got := status(); got != "source" {
		t.Errorf("a push holding the sync lock: %q", got)
	}
	release()
	rs := ParseReleases("20261020000000\tcurrent\tsyncing\t\t{}\n")
	if len(rs) != 1 || !rs[0].Syncing || !strings.HasSuffix(rs[0].Line(), "  SYNCING") {
		t.Errorf("a syncing release: %+v %q", rs, rs[0].Line())
	}
}

// A watcher killed outright (SIGKILL, the OOM killer) never takes the
// .next it was asked to: the script waits past every quiet time, then
// takes the session for gone itself, and goes the cancel way, rather
// than waiting for good with its locks held.
func TestWaitBeatWithoutWatcher(t *testing.T) {
	dir := t.TempDir()
	script := "set -eu\nws_armed=" + Quote(filepath.Join(dir, ".devopsy-watch.1")) + "\n: >\"$ws_armed\"\n" +
		waitBeat(`{ echo gone; exit 3; }`) + "echo went on\n"
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), HeartbeatEnv+"=1")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(40 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("still waiting for a watcher that is gone:\n%s", out.String())
	}
	if code := cmd.ProcessState.ExitCode(); code != 3 || strings.Contains(out.String(), "went on") || !fileExists(filepath.Join(dir, ".devopsy-watch.1.fired")) {
		t.Errorf("without a watcher (%d), fired %v:\n%s", code, fileExists(filepath.Join(dir, ".devopsy-watch.1.fired")), out.String())
	}
}

// An import's mark refuses what runs on its data with its own words and
// ways out (the service, --import again, --resolved; --sync only on a
// child), never only --sync, which a target without a parent cannot do:
// a release before its upload (no tar asked for, no release left), an
// activation, a push from it; an import of another service, or any import
// over a sync's mark, before devopsy reads stdin. A release that syncs
// goes on: the sync is a way out.
func TestImportMarkRefusals(t *testing.T) {
	bin := t.TempDir()
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\n"+sayCase()+"if [ \"$1\" = --release-settings ]; then printf 'root=%s\\nkeep=5\\nmax_keep=5\\nprotocol=8\\n' \"$FAKE_ROOT\"; exit; fi\necho \"ran: $*\"\n")
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n")
	for _, f := range []string{"devopsy", "flock"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	tg := &Target{Name: "prod", Address: "vm1:prod", Host: "vm1", Path: "app/prod"}
	base := filepath.Join(root, tg.Path)
	write(t, filepath.Join(base, "releases", "1", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
	if err := os.Symlink("releases/1/"+CodeDir, filepath.Join(base, "current")); err != nil {
		t.Fatal(err)
	}
	mark := filepath.Join(base, SyncIncompleteFile)
	run := func(script, stdin string) (string, error) {
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	importMark := IncompleteLine("2026-10-22 10:00:00", "db", IncompleteImport, "it failed or was cut after its first write: its data may be partial")
	write(t, mark, importMark)
	wantImport := []string{"the import of db into @vm1:prod did not complete", "devopsy: | $ devopsy @vm1:prod --import db < <file>", "devopsy: | $ devopsy @vm1:prod --import db --resolved"}
	for name, script := range map[string]string{
		"upload":   UploadScript(tg, false),
		"activate": ActivateScript(tg, "1", false, "app-prod", Phases{Run: []string{"deploy"}}, nil),
		"push":     PushScript(tg, "app-prod", nil),
	} {
		out, err := run(script, "")
		if err == nil || strings.Contains(out, "--sync") || strings.Contains(out, LockedMarker) || strings.Contains(out, "ran: --sync-push") {
			t.Errorf("%s over an import's mark (%v):\n%s", name, err, out)
		}
		for _, w := range wantImport {
			if !strings.Contains(out, w) {
				t.Errorf("%s over an import's mark, missing %q:\n%s", name, w, out)
			}
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(base, "releases")); len(entries) != 1 {
		t.Errorf("a refused upload left a release: %v", entries)
	}
	// On a child, a sync is a way out too; a release that syncs goes on to
	// its upload.
	child := &Target{Name: "prod", Address: "vm1:prod", Host: "vm1", Path: "app/prod", Parent: "live"}
	if out, _ := run(UploadScript(child, false), ""); !strings.Contains(out, "devopsy: | $ devopsy @vm1:prod --sync") {
		t.Errorf("a child's upload:\n%s", out)
	}
	if out, _ := run(UploadScript(tg, true), ""); !strings.Contains(out, LockedMarker) {
		t.Errorf("an upload that syncs:\n%s", out)
	}
	// Imports: only db's own, before stdin is read.
	if out, err := run(ImportScript(tg, "app-prod", "files", "END"), "data"); err == nil || strings.Contains(out, "ran: --import") || !strings.Contains(out, "the import of db into @vm1:prod did not complete, and only db can be imported until one does: nothing imported") || !strings.Contains(out, "--import db --resolved") {
		t.Errorf("another service's import (%v):\n%s", err, out)
	}
	if out, err := run(ImportScript(tg, "app-prod", "db", "END"), "data"); err != nil || !strings.Contains(out, "ran: --import db --yes") {
		t.Errorf("its own import (%v):\n%s", err, out)
	}
	if fileExists(filepath.Join(base, SyncImportingFile)) {
		t.Errorf("the holding file stayed")
	}
	// A sync's mark: only a sync.
	write(t, mark, IncompleteLine("2026-10-22 10:00:00", "vm1:live", IncompleteCopy, "the copy was cut"))
	if out, err := run(ImportScript(tg, "app-prod", "db", "END"), "data"); err == nil || strings.Contains(out, "ran: --import") || !strings.Contains(out, "only a sync completes it: nothing imported") || !strings.Contains(out, "$ devopsy @vm1:prod --sync") {
		t.Errorf("an import over a sync's mark (%v):\n%s", err, out)
	}
	if out, err := run(UploadScript(tg, false), ""); err == nil || !strings.Contains(out, "data is from a sync that did not complete") || !strings.Contains(out, "--sync") {
		t.Errorf("an upload over a sync's mark (%v):\n%s", err, out)
	}

	// What reads the mark without the lock: the mark parsed, an import
	// running only while its holding file is there with the lock held
	// (never for another holder of the lock), the server's features.
	write(t, mark, importMark)
	cur := ParseCurrent("current\nincomplete\t" + importMark + "importing\n" + DataLabelsFeature + "\n")
	if !cur.Current || !cur.Importing || !cur.DataLabels || !cur.Mark.IsImport() || cur.Mark.From != "db" {
		t.Errorf("parsed: %+v %+v", cur, cur.Mark)
	}
	if cur := ParseCurrent("none\nincomplete\n"); cur.Current || cur.Mark == nil || cur.Mark.Code != "" || cur.Mark.IsImport() {
		t.Errorf("a sync's files without a mark: %+v", cur)
	}
	busy := func(locked bool) string {
		write(t, filepath.Join(bin, "flock"), map[bool]string{true: "#!/bin/sh\nexit 1\n", false: "#!/bin/sh\nexit 0\n"}[locked])
		out, _ := run("base="+Quote(base)+"\nif "+ImportRunning+"; then echo importing; fi\n", "")
		return out
	}
	write(t, filepath.Join(base, ".lock"), "")
	if busy(true) != "" {
		t.Errorf("another holder of the lock read as an import")
	}
	write(t, filepath.Join(base, SyncImportingFile), "")
	if busy(false) != "" || busy(true) != "importing\n" {
		t.Errorf("an import's holding file")
	}
	// Left by a shell killed outright: whatever takes the lock next
	// removes them, or they read as running whenever the lock is held.
	write(t, filepath.Join(base, SyncHoldingFile), "")
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n")
	if out, err := run("base="+Quote(base)+"\n"+releaseLock(true, false), ""); err != nil || fileExists(filepath.Join(base, SyncImportingFile)) || fileExists(filepath.Join(base, SyncHoldingFile)) {
		t.Errorf("stale holding files after the lock (%v):\n%s", err, out)
	}
	write(t, filepath.Join(base, SyncStateFile), "stop db\nstop app\nwriter 123\n")
	if out, err := run(ResolvedCheckScript(tg), ""); err != nil || out != "mark\t"+importMark+"stop db\nstop app\n" {
		t.Errorf("--resolved's check (%v): %q", err, out)
	}
}
