package remote

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// runVarsScript runs a --vars script in the test, as sh on a server would,
// with a no-op flock (macOS has none) and root as the release root the fake
// server devopsy reports.
func runVarsScript(t *testing.T, root, script, stdin string) string {
	t.Helper()
	// As devopsy sends it, gated: the lines, then the end line.
	out, err := runVarsRaw(t, root, script, GatedInput(stdin))
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, script)
	}
	return strings.ReplaceAll(out, LockedMarker+"\n", "")
}

// varsBin is the fakes' directory, written once, since macOS takes
// seconds, under the parallel tests, for the first exec of each new
// script. Every test shares it, so it is read-only, as fakeBin's in
// cmd/devopsy: a test that writes there fails at once.
var varsBin = sync.OnceValues(func() (string, error) {
	bin, err := os.MkdirTemp(filepath.Dir(realDevopsy), "vars-bin")
	if err != nil {
		return "", err
	}
	if err := testfile.Write(filepath.Join(bin, "flock"), []byte("#!/bin/sh\nexit 0\n"), 0o555); err != nil {
		return "", err
	}
	settings := "#!/bin/sh\n" + sayCase() + "[ \"$1\" != --stdin-guard ] || exec cat\nprintf 'root=%s\\nimages=%s\\nkeep=5\\nmax_keep=5\\n' \"$FAKE_ROOT\" \"$FAKE_ROOT\"\n"
	if err := testfile.Write(filepath.Join(bin, "devopsy"), []byte(settings), 0o555); err != nil {
		return "", err
	}
	return bin, os.Chmod(bin, 0o555)
})

// runVarsRaw runs script with stdin as given.
func runVarsRaw(t *testing.T, root, script, stdin string) (string, error) {
	t.Helper()
	bin, err := varsBin()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestVarsScripts(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	// A release environment before its first release: shared/.env is created.
	base := filepath.Join(root, "app's prod")
	tg := &Target{Name: "prod", Host: "h", Path: "app's prod"}
	file := filepath.Join(base, "shared", ".env")
	// Its path, what it did to each key, then what applies it: nothing is released
	// there yet.
	if out := runVarsScript(t, root, VarsSetScript(tg, nil, nil), "A='1'\n"); out != file+"\n"+VarsKey+"added\tA\n"+VarsApplied+"\tfirst\n" {
		t.Fatalf("set printed %q", out)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("new .env: %v %v", fi, err)
	}

	// Existing keys are replaced in place, duplicates dropped, comments and
	// other lines kept, new keys appended in order. Each key's outcome is said: a key
	// whose one line is already the line sent is the same, any other line of it (a
	// duplicate, a value of several lines) changed.
	write(t, file, "# secrets\nexport A=old\nB=keep\nA=dup\nC = spaced\nS='s'\nT='t'\nT='t'\n\n")
	out := runVarsScript(t, root, VarsSetScript(tg, nil, nil), "C='new c'\nA='new a'\nD='d'\nS='s'\nT='t'\n")
	got, _ := os.ReadFile(file)
	want := "# secrets\nA='new a'\nB=keep\nC='new c'\nS='s'\nT='t'\n\nD='d'\n"
	if string(got) != want {
		t.Fatalf("set\n got %q\nwant %q", got, want)
	}
	if keys := ParseVarsKeys(out); fmt.Sprint(keys) != fmt.Sprint(map[string]string{"A": "changed", "C": "changed", "D": "added", "S": "same", "T": "changed"}) {
		t.Errorf("set's outcomes %v\n%s", keys, out)
	}
	want = "# secrets\nA='new a'\nB=keep\nC='new c'\n\nD='d'\n"
	write(t, file, want)
	// A value over several lines, in double quotes, as compose's parser
	// reads it (a certificate written by hand): set and unset replace or
	// drop all of its lines, and a line inside it is no key.
	//
	// Bug: the edit goes line by line, so set replaces only CERT's first
	// line and keeps the rest (the file no longer parses: `-----END"`
	// opens a quote), and set A takes the A=inside line of NOTE's value
	// for A, dropping the real A=old as a duplicate.
	multi := filepath.Join(root, "multi", "shared", ".env")
	tm := &Target{Name: "prod", Host: "h", Path: "multi"}
	write(t, multi, "B=keep\nCERT=\"-----BEGIN\nabc\n-----END\"\nNOTE=\"first\nA=inside\"\nA=old\n")
	out = runVarsScript(t, root, VarsSetScript(tm, nil, nil), "CERT='new'\nA='new a'\n")
	if got, _ := os.ReadFile(multi); string(got) != "B=keep\nCERT='new'\nNOTE=\"first\nA=inside\"\nA='new a'\n" {
		t.Errorf("set over a value of several lines:\n%s", got)
	}
	if keys := ParseVarsKeys(out); keys["CERT"] != "changed" || keys["A"] != "changed" {
		t.Errorf("set's outcomes over a value of several lines %v", keys)
	}
	write(t, multi, "B=keep\nCERT=\"-----BEGIN\nabc\n-----END\"\n")
	runVarsScript(t, root, VarsUnsetScript(tm, nil, nil), "CERT\n")
	if got, _ := os.ReadFile(multi); string(got) != "B=keep\n" {
		t.Errorf("unset of a value of several lines:\n%s", got)
	}
	// compose's parser reads KEY: value as KEY=value: such a line is the
	// key's too.
	write(t, multi, "A: old\nB=keep\nC: x=y\n")
	runVarsScript(t, root, VarsUnsetScript(tm, nil, nil), "A\n")
	runVarsScript(t, root, VarsSetScript(tm, nil, nil), "C='new'\n")
	if got, _ := os.ReadFile(multi); string(got) != "B=keep\nC='new'\n" {
		t.Errorf("KEY: value lines:\n%s", got)
	}
	// A session cut before its end line (gone while the script waited for
	// the lock, its values sent before) changes nothing.
	if out, err := runVarsRaw(t, root, VarsSetScript(tg, nil, nil), "A='cut'\n"); err == nil || !strings.Contains(out, "nothing changed") {
		t.Errorf("a cut set (%v):\n%s", err, out)
	}
	if got, _ := os.ReadFile(file); string(got) != want {
		t.Errorf("a cut set changed the file: %q", got)
	}

	out = runVarsScript(t, root, VarsUnsetScript(tg, nil, nil), "A\nD\nMISSING\n")
	got, _ = os.ReadFile(file)
	if want := "# secrets\nB=keep\nC='new c'\n\n"; string(got) != want {
		t.Fatalf("unset\n got %q\nwant %q", got, want)
	}
	if keys := ParseVarsKeys(out); fmt.Sprint(keys) != fmt.Sprint(map[string]string{"A": "removed", "D": "removed", "MISSING": "absent"}) {
		t.Errorf("unset's outcomes %v\n%s", keys, out)
	}

	out = runVarsScript(t, root, VarsReadScript(tg, nil), "")
	read, err := ReadVarsOutput(out)
	if err != nil || read.File != file || read.Content != string(got) || len(read.Configs) != 0 {
		t.Fatalf("read %q: %+v (%v)", out, read, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(file)); len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}

	// A plain directory: its .devopsy/.env, and nothing else is created.
	plain := filepath.Join(root, "traefik", "main")
	if err := os.MkdirAll(filepath.Join(plain, ".devopsy"), 0o755); err != nil {
		t.Fatal(err)
	}
	tp := &Target{Name: "vm1-traefik", Host: "h", Path: "traefik/main"}
	runVarsScript(t, root, VarsSetScript(tp, nil, nil), "TOKEN='x'\n")
	if got, _ := os.ReadFile(filepath.Join(plain, ".devopsy", ".env")); string(got) != "TOKEN='x'\n" {
		t.Fatalf("plain .env %q", got)
	}
	if _, err := os.Stat(filepath.Join(plain, "shared")); err == nil {
		t.Fatal("plain directory got a shared/")
	}

	// Reading a target with nothing yet creates nothing.
	empty := filepath.Join(root, "new")
	if out := runVarsScript(t, root, VarsReadScript(&Target{Path: "new"}, nil), ""); out != empty+"/shared/.env\n" {
		t.Fatalf("read empty %q", out)
	}
	if _, err := os.Stat(empty); err == nil {
		t.Fatal("read created the target directory")
	}

	// A level: the project's .env under the release root, shared.
	level := &Level{Name: "project", Dir: "app", Link: "project.env"}
	tl := &Target{Name: "prod", Path: "app/prod"}
	if out := runVarsScript(t, root, VarsSetScript(tl, level, nil), "SHARED='1'\n"); out != filepath.Join(root, "app", ".env")+"\n"+VarsKey+"added\tSHARED\n" {
		t.Fatalf("level set printed %q", out)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "app", ".env")); string(got) != "SHARED='1'\n" {
		t.Fatalf("level .env %q", got)
	}
	if out := runVarsScript(t, root, VarsSetScript(tl, nil, nil), "OWN='1'\n"); out != filepath.Join(root, "app", "prod", "shared", ".env")+"\n"+VarsKey+"added\tOWN\n"+VarsApplied+"\tfirst\n" {
		t.Fatalf("own .env printed %q", out)
	}

	// Reading a level brings the configs of every current release under it, for
	// their secrets: (with and without instances).
	for env, config := range map[string]string{"prod": "secrets: [A]\n", "site/dev": "secrets: [B]\n"} {
		write(t, filepath.Join(root, "app", env, "releases", "1", CodeDir, ".devopsy", "config.yaml"), config)
		if err := os.Symlink(filepath.Join("releases", "1", CodeDir), filepath.Join(root, "app", env, "current")); err != nil {
			t.Fatal(err)
		}
	}
	read, err = ReadVarsOutput(runVarsScript(t, root, VarsReadScript(tl, level), ""))
	if err != nil || read.Content != "SHARED='1'\n" || strings.Join(read.Configs, "") != "secrets: [A]\nsecrets: [B]\n" {
		t.Fatalf("level read: %+v (%v)", read, err)
	}
	read, err = ReadVarsOutput(runVarsScript(t, root, VarsReadScript(tl, nil), ""))
	if err != nil || strings.Join(read.Configs, "") != "secrets: [A]\n" {
		t.Fatalf("own read: %+v (%v)", read, err)
	}
	if s, ok := ConfigSecrets([]byte("project: x\nnewer_key: 1\nsecrets: [A, \"*_KEY\"]\n")); !ok || strings.Join(s, " ") != "A *_KEY" {
		t.Fatalf("ConfigSecrets: %q %v", s, ok)
	}
}

// The inherited variables (the parent's secrets) are never in a file on their way into
// place, and their file is the deploy user's alone: a spool beside them, left by a cut
// session, would be linked into every later release (activation.links), and before it
// sat 644 until the chmod.
func TestInheritWriteMode(t *testing.T) {
	t.Parallel()
	root, bin := t.TempDir(), t.TempDir()
	tg := &Target{Name: "dev", Host: "h", Path: "app/dev"}
	shared := filepath.Join(root, "app", "dev", "shared")
	during := filepath.Join(root, "during")
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n")
	// The guard copies stdin, then lists what the environment holds meanwhile.
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\n"+sayCase()+"if [ \"$1\" = --stdin-guard ]; then cat; ls -A "+shared+" >\""+during+"\"; exit; fi\nprintf 'root=%s\\nimages=%s\\nkeep=5\\nmax_keep=5\\n' \""+root+"\" \""+root+"\"\n")
	for _, f := range []string{"flock", "devopsy"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", "umask 022\n"+InheritWriteScript(tg, []string{"A"}, ""))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	cmd.Stdin = strings.NewReader(GatedInput("A='secret'\n"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := strings.TrimSpace(readFile(during)); got != "" {
		t.Errorf("files in shared/ while the values were read: %q", got)
	}
	entries, _ := os.ReadDir(shared)
	if len(entries) != 1 || entries[0].Name() != InheritFile {
		t.Errorf("shared/ after the write: %v", entries)
	}
	if fi, err := os.Stat(filepath.Join(shared, InheritFile)); err != nil || fi.Mode().Perm() != 0o600 || readFile(filepath.Join(shared, InheritFile)) != "A='secret'\n" {
		t.Errorf("the inherited values' file: %v %v %q", fi, err, readFile(filepath.Join(shared, InheritFile)))
	}
}

// An edit that fails while it writes the result never leaves the file
// empty or cut (cat into it truncated it first), and the file keeps its
// mode, and a link stays a link.
func TestVarsEditInPlace(t *testing.T) {
	t.Parallel()
	root, bin := t.TempDir(), t.TempDir()
	tg := &Target{Name: "prod", Host: "h", Path: "app/prod"}
	shared := filepath.Join(root, "app", "prod", "shared")
	real := filepath.Join(root, "app", "prod", "real.env")
	write(t, real, "A='old'\n")
	if err := os.Chmod(real, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(shared, ".env")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n")
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\n"+sayCase()+"[ \"$1\" != --stdin-guard ] || exec cat\nprintf 'root=%s\\nimages=%s\\nkeep=5\\nmax_keep=5\\n' \""+root+"\" \""+root+"\"\n")
	// cat fails on the edit's result, as a full disk would.
	write(t, filepath.Join(bin, "cat"), "#!/bin/sh\ncase \"$*\" in *.tmp) exit 1 ;; esac\nexec /bin/cat \"$@\"\n")
	for _, f := range []string{"flock", "devopsy", "cat"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", VarsSetScript(tg, nil, nil))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	cmd.Stdin = strings.NewReader(GatedInput("A='new'\n"))
	out, _ := cmd.CombinedOutput()
	if got := readFile(real); got != "A='old'\n" && got != "A='new'\n" {
		t.Errorf("the file after an edit that failed writing: %q\n%s", got, out)
	}
	// Without the failing cat: the edit lands, mode and link kept.
	os.Remove(filepath.Join(bin, "cat"))
	cmd = exec.Command("sh", "-c", VarsSetScript(tg, nil, nil))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	cmd.Stdin = strings.NewReader(GatedInput("A='new'\n"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	fi, err := os.Lstat(filepath.Join(shared, ".env"))
	if rfi, _ := os.Stat(real); err != nil || fi.Mode()&os.ModeSymlink == 0 || readFile(real) != "A='new'\n" || rfi.Mode().Perm() != 0o640 {
		t.Errorf("the edit: link %v, %q, mode %v", fi.Mode(), readFile(real), rfi.Mode())
	}
}

// A .env another user owns goes into place in one rename too, with its
// mode (once, a cat wrote over it in place, so a reader could find it
// empty or cut). Only root can give the file away to set it up: Linux
// in Docker, as Checks runs it.
func TestVarsRenamesForeignFile(t *testing.T) {
	t.Parallel()
	if os.Geteuid() != 0 {
		t.Skip("needs root to make a file of another user's")
	}
	root := t.TempDir()
	tg := &Target{Name: "prod", Host: "h", Path: "app/prod"}
	file := filepath.Join(root, "app", "prod", "shared", ".env")
	write(t, file, "A=old\n")
	if err := os.Chmod(file, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(file, 12345, 12345); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	runVarsScript(t, root, VarsSetScript(tg, nil, nil), "A='new'\n")
	after, err := os.Stat(file)
	if err != nil || readFile(file) != "A='new'\n" || after.Mode().Perm() != 0o640 {
		t.Fatalf("%q, %v, %v", readFile(file), after, err)
	}
	if os.SameFile(before, after) {
		t.Errorf("written over in place, not renamed")
	}
}

// An instance's or a project's edit traces and checks each environment under it that has
// a current release (each_env), never a directory a cut removal left (.rm.) nor one inside
// an environment (its shared/, the project's, holding a current/), each check under that
// environment's lock taken without waiting: one held is busy, never waited for; one with
// no .lock (removed meanwhile) is skipped; the take removes a killed holder's file, as
// releaseLock does. Each check gets nothing on its stdin (the session's is the operator's
// values), and the edit keeps what the operator sent in memory, never beside the
// environments' files.
func TestVarsApplyLevel(t *testing.T) {
	t.Parallel()
	root, bin := t.TempDir(), t.TempDir()
	base := filepath.Join(root, "app")
	for _, env := range []string{"old", "prod", "qa", "qa.rm.12"} {
		write(t, filepath.Join(base, env, "releases", "1", CodeDir, ".devopsy", "compose.yaml"), "services: {}\n")
		if err := os.Symlink(filepath.Join("releases", "1", CodeDir), filepath.Join(base, env, "current")); err != nil {
			t.Fatal(err)
		}
		if env != "old" {
			write(t, filepath.Join(base, env, ".lock"), "")
		}
	}
	write(t, filepath.Join(base, "qa", SyncHoldingFile), "")
	write(t, filepath.Join(base, "prod", "shared", "current", ".devopsy", "compose.yaml"), "services: {}\n")
	write(t, filepath.Join(base, "prod", "shared", ".lock"), "")
	old := strings.Repeat("BIG='"+strings.Repeat("x", 1000)+"'\n", 100)
	write(t, filepath.Join(base, ".env"), old)
	during := filepath.Join(root, "during")
	// The first try of an environment's lock fails (prod's: the glob's order); each
	// check saves its stdin, finds X missing.
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nif [ \"$*\" = '-n 7' ] && [ ! -e \""+root+"/tried\" ]; then : > \""+root+"/tried\"; exit 1; fi\nexit 0\n")
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\n"+sayCase()+`case "$1" in
  --stdin-guard) cat; ls -A "$FAKE_ROOT/app" "$FAKE_ROOT/app/qa" > "$FAKE_ROOT/during"; exit 0 ;;
  --env-trace) echo traced; exit 0 ;;
  --prepare-release) exit 0 ;;
  --check-images) env=$(pwd); env=${env%/current}; cat > "$FAKE_ROOT/stdin.${env##*/}"; echo "missing X"; exit 4 ;;
esac
printf 'root=%s\nimages=%s\nkeep=5\nmax_keep=5\n' "$FAKE_ROOT" "$FAKE_ROOT"
`)
	for _, f := range []string{"flock", "devopsy"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	level := &Level{Name: "project", Dir: "app", Link: "project.env"}
	cmd := exec.Command("sh", "-c", VarsSetScript(&Target{Name: "prod", Path: "app/prod"}, level, []string{"NEW"}))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + root}
	cmd.Stdin = strings.NewReader(GatedInput("NEW='1'\n"))
	var stdout strings.Builder
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v\n%s", err, stdout.String())
	}
	file, applied := ParseVarsApplied(stdout.String())
	want := []Applied{{Dir: "prod", Word: "busy"}, {Dir: "qa", Word: "4", Lines: []string{"missing X"}}}
	if file != filepath.Join(base, ".env") || fmt.Sprint(applied) != fmt.Sprint(want) {
		t.Errorf("file %q, applied %+v, want %+v\n%s", file, applied, want, stdout.String())
	}
	if traces := ParseVarsTraces(stdout.String()); fmt.Sprint(traces) != fmt.Sprint(map[string]string{"old": "traced", "prod": "traced", "qa": "traced"}) {
		t.Errorf("traced %v", traces)
	}
	if got, err := os.ReadFile(filepath.Join(root, "stdin.qa")); err != nil || len(got) != 0 {
		t.Errorf("qa's check's stdin: %q %v", got, err)
	}
	if fileExists(filepath.Join(base, "qa", SyncHoldingFile)) {
		t.Errorf("a killed holder's file stays once the check took the lock")
	}
	if got := readFile(during); strings.Contains(got, ".vars") || strings.Contains(got, ".in") {
		t.Errorf("files beside the environments' while the values were read:\n%s", got)
	}
	if got := readFile(filepath.Join(base, ".env")); got != old+"NEW='1'\n" {
		t.Errorf("the project's .env after the edit: %d bytes", len(got))
	}
}

// A level's listing reads the configs (their secrets:) of the environments each_env
// lists, no other directory's.: never what a cut removal left (.rm.) nor a directory inside an
// environment (its shared/current), whose secrets: would mask the listing.
func TestVarsReadLevelListsEachEnv(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	base := filepath.Join(root, "app")
	config := func(dir, name string) {
		write(t, filepath.Join(dir, "releases", "1", CodeDir, ".devopsy", "config.yaml"), "project: app\nsecrets: ["+name+"]\n")
		if err := os.Symlink(filepath.Join("releases", "1", CodeDir), filepath.Join(dir, "current")); err != nil {
			t.Fatal(err)
		}
	}
	config(filepath.Join(base, "prod"), "OF_PROD")
	config(filepath.Join(base, "qa.rm.12"), "OF_A_CUT_REMOVAL")
	write(t, filepath.Join(base, "prod", "shared", "current", ".devopsy", "config.yaml"), "project: app\nsecrets: [OF_SHARED]\n")
	write(t, filepath.Join(base, ".env"), "A='1'\n")
	level := &Level{Name: "project", Dir: "app", Link: "project.env"}
	out, err := runVarsRaw(t, root, VarsReadScript(&Target{Name: "prod", Host: "h", Path: "app/prod"}, level), "")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "OF_PROD") {
		t.Fatalf("prod's config not read:\n%s", out)
	}
	for _, name := range []string{"OF_A_CUT_REMOVAL", "OF_SHARED"} {
		if strings.Contains(out, name) {
			t.Errorf("the config of what is no environment read (%s):\n%s", name, out)
		}
	}
}
