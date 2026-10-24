package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// runVarsScript runs a --vars script locally, as sh on a server would, with
// a no-op flock (macOS has none) and root as the release root the fake
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

// varsBin is the fakes' directory, written once: they never change, and
// macOS takes seconds, under the parallel tests, for the first exec of
// each new script.
var varsBin = sync.OnceValues(func() (string, error) {
	bin, err := os.MkdirTemp(filepath.Dir(realDevopsy), "vars-bin")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(bin, "flock"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		return "", err
	}
	settings := "#!/bin/sh\n" + sayCase() + "[ \"$1\" != --stdin-guard ] || exec cat\nprintf 'root=%s\\nkeep=5\\nmax_keep=5\\nprotocol=8\\n' \"$FAKE_ROOT\"\n"
	return bin, os.WriteFile(filepath.Join(bin, "devopsy"), []byte(settings), 0o755)
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
	if out := runVarsScript(t, root, VarsSetScript(tg, nil), "A='1'\n"); strings.TrimSpace(out) != file {
		t.Fatalf("set printed %q", out)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("new .env: %v %v", fi, err)
	}

	// Existing keys are replaced in place, duplicates dropped, comments and
	// other lines kept, new keys appended in order.
	write(t, file, "# secrets\nexport A=old\nB=keep\nA=dup\nC = spaced\n\n")
	runVarsScript(t, root, VarsSetScript(tg, nil), "C='new c'\nA='new a'\nD='d'\n")
	got, _ := os.ReadFile(file)
	want := "# secrets\nA='new a'\nB=keep\nC='new c'\n\nD='d'\n"
	if string(got) != want {
		t.Fatalf("set\n got %q\nwant %q", got, want)
	}
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
	runVarsScript(t, root, VarsSetScript(tm, nil), "CERT='new'\nA='new a'\n")
	if got, _ := os.ReadFile(multi); string(got) != "B=keep\nCERT='new'\nNOTE=\"first\nA=inside\"\nA='new a'\n" {
		t.Errorf("set over a value of several lines:\n%s", got)
	}
	write(t, multi, "B=keep\nCERT=\"-----BEGIN\nabc\n-----END\"\n")
	runVarsScript(t, root, VarsUnsetScript(tm, nil), "CERT\n")
	if got, _ := os.ReadFile(multi); string(got) != "B=keep\n" {
		t.Errorf("unset of a value of several lines:\n%s", got)
	}
	// compose's parser reads KEY: value as KEY=value: such a line is the
	// key's too.
	write(t, multi, "A: old\nB=keep\nC: x=y\n")
	runVarsScript(t, root, VarsUnsetScript(tm, nil), "A\n")
	runVarsScript(t, root, VarsSetScript(tm, nil), "C='new'\n")
	if got, _ := os.ReadFile(multi); string(got) != "B=keep\nC='new'\n" {
		t.Errorf("KEY: value lines:\n%s", got)
	}
	// A session cut before its end line (gone while the script waited for
	// the lock, its values sent before) changes nothing.
	if out, err := runVarsRaw(t, root, VarsSetScript(tg, nil), "A='cut'\n"); err == nil || !strings.Contains(out, "nothing changed") {
		t.Errorf("a cut set (%v):\n%s", err, out)
	}
	if got, _ := os.ReadFile(file); string(got) != want {
		t.Errorf("a cut set changed the file: %q", got)
	}

	runVarsScript(t, root, VarsUnsetScript(tg, nil), "A\nD\nMISSING\n")
	got, _ = os.ReadFile(file)
	if want := "# secrets\nB=keep\nC='new c'\n\n"; string(got) != want {
		t.Fatalf("unset\n got %q\nwant %q", got, want)
	}

	out := runVarsScript(t, root, VarsReadScript(tg, nil), "")
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
	runVarsScript(t, root, VarsSetScript(tp, nil), "TOKEN='x'\n")
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
	if out := runVarsScript(t, root, VarsSetScript(tl, level), "SHARED='1'\n"); strings.TrimSpace(out) != filepath.Join(root, "app", ".env") {
		t.Fatalf("level set printed %q", out)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "app", ".env")); string(got) != "SHARED='1'\n" {
		t.Fatalf("level .env %q", got)
	}
	if out := runVarsScript(t, root, VarsSetScript(tl, nil), "OWN='1'\n"); strings.TrimSpace(out) != filepath.Join(root, "app", "prod", "shared", ".env") {
		t.Fatalf("own .env printed %q", out)
	}

	// Reading a level brings the configs of every release under it, for
	// their secrets: (with and without instances).
	write(t, filepath.Join(root, "app", "prod", "current", ".devopsy", "config.yaml"), "secrets: [A]\n")
	write(t, filepath.Join(root, "app", "site", "dev", "current", ".devopsy", "config.yaml"), "secrets: [B]\n")
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

// The inherited variables (the parent's secrets) are never readable by
// others on their way into place: before, they sat 644 in the
// temporary files until the chmod.
func TestInheritWriteMode(t *testing.T) {
	t.Parallel()
	root, bin := t.TempDir(), t.TempDir()
	tg := &Target{Name: "dev", Host: "h", Path: "app/dev"}
	in := filepath.Join(root, "app", "dev", "shared", InheritFile+".tmp.in")
	modes := filepath.Join(root, "modes")
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n")
	// The guard copies stdin, then says what mode its output has.
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\n"+sayCase()+"if [ \"$1\" = --stdin-guard ]; then cat; ls -ln \""+in+"\" | cut -c1-10 >\""+modes+"\"; exit; fi\nprintf 'root=%s\\nkeep=5\\nmax_keep=5\\nprotocol=8\\n' \""+root+"\"\n")
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
	if got := strings.TrimSpace(readFile(modes)); got != "-rw-------" {
		t.Errorf("the inherited values on their way: %q", got)
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
	write(t, filepath.Join(bin, "devopsy"), "#!/bin/sh\n"+sayCase()+"[ \"$1\" != --stdin-guard ] || exec cat\nprintf 'root=%s\\nkeep=5\\nmax_keep=5\\nprotocol=8\\n' \""+root+"\"\n")
	// cat fails on the edit's result, as a full disk would.
	write(t, filepath.Join(bin, "cat"), "#!/bin/sh\ncase \"$*\" in *.tmp) exit 1 ;; esac\nexec /bin/cat \"$@\"\n")
	for _, f := range []string{"flock", "devopsy", "cat"} {
		if err := os.Chmod(filepath.Join(bin, f), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", VarsSetScript(tg, nil))
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	cmd.Stdin = strings.NewReader(GatedInput("A='new'\n"))
	out, _ := cmd.CombinedOutput()
	if got := readFile(real); got != "A='old'\n" && got != "A='new'\n" {
		t.Errorf("the file after an edit that failed writing: %q\n%s", got, out)
	}
	// Without the failing cat: the edit lands, mode and link kept.
	os.Remove(filepath.Join(bin, "cat"))
	cmd = exec.Command("sh", "-c", VarsSetScript(tg, nil))
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
