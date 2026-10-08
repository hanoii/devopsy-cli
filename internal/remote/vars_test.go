package remote

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// varsRoot is the release root the fake server devopsy reports.
var varsRoot = os.TempDir()

// runVarsScript runs a --vars script locally, as sh on a server would, with
// a no-op flock (macOS has none).
func runVarsScript(t *testing.T, script, stdin string) string {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "flock"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := "#!/bin/sh\nprintf 'root=%s\\nkeep=5\\nmax_keep=5\\n' \"$FAKE_ROOT\"\n"
	if err := os.WriteFile(filepath.Join(bin, "devopsy"), []byte(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "FAKE_ROOT=" + varsRoot}
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, script)
	}
	return string(out)
}

func TestVarsScripts(t *testing.T) {
	// A release environment before its first release: shared/.env is created.
	base := filepath.Join(t.TempDir(), "app's prod")
	tg := &Target{Name: "prod", Host: "h", Path: base}
	file := filepath.Join(base, "shared", ".env")
	if out := runVarsScript(t, VarsSetScript(tg, nil), "A='1'\n"); strings.TrimSpace(out) != file {
		t.Fatalf("set printed %q", out)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("new .env: %v %v", fi, err)
	}

	// Existing keys are replaced in place, duplicates dropped, comments and
	// other lines kept, new keys appended in order.
	write(t, file, "# secrets\nexport A=old\nB=keep\nA=dup\nC = spaced\n\n")
	runVarsScript(t, VarsSetScript(tg, nil), "C='new c'\nA='new a'\nD='d'\n")
	got, _ := os.ReadFile(file)
	want := "# secrets\nA='new a'\nB=keep\nC='new c'\n\nD='d'\n"
	if string(got) != want {
		t.Fatalf("set\n got %q\nwant %q", got, want)
	}

	runVarsScript(t, VarsUnsetScript(tg, nil), "A\nD\nMISSING\n")
	got, _ = os.ReadFile(file)
	if want := "# secrets\nB=keep\nC='new c'\n\n"; string(got) != want {
		t.Fatalf("unset\n got %q\nwant %q", got, want)
	}

	out := runVarsScript(t, VarsReadScript(tg, nil), "")
	if out != file+"\n"+string(got) {
		t.Fatalf("read %q", out)
	}
	if entries, _ := os.ReadDir(filepath.Dir(file)); len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}

	// A plain directory: its .devopsy/.env, and nothing else is created.
	plain := t.TempDir()
	if err := os.MkdirAll(filepath.Join(plain, ".devopsy"), 0o755); err != nil {
		t.Fatal(err)
	}
	tp := &Target{Name: "vm1-traefik", Host: "h", Path: plain}
	runVarsScript(t, VarsSetScript(tp, nil), "TOKEN='x'\n")
	if got, _ := os.ReadFile(filepath.Join(plain, ".devopsy", ".env")); string(got) != "TOKEN='x'\n" {
		t.Fatalf("plain .env %q", got)
	}
	if _, err := os.Stat(filepath.Join(plain, "shared")); err == nil {
		t.Fatal("plain directory got a shared/")
	}

	// Reading a target with nothing yet creates nothing.
	empty := filepath.Join(t.TempDir(), "new")
	if out := runVarsScript(t, VarsReadScript(&Target{Path: empty}, nil), ""); out != empty+"/shared/.env\n" {
		t.Fatalf("read empty %q", out)
	}
	if _, err := os.Stat(empty); err == nil {
		t.Fatal("read created the target directory")
	}

	// A level: the project's .env under the release root, shared.
	root := t.TempDir()
	varsRoot = root
	defer func() { varsRoot = os.TempDir() }()
	level := &Level{Name: "project", Dir: "app", Link: "project.env"}
	tl := &Target{Name: "prod", Path: "app/prod"}
	if out := runVarsScript(t, VarsSetScript(tl, level), "SHARED='1'\n"); strings.TrimSpace(out) != filepath.Join(root, "app", ".env") {
		t.Fatalf("level set printed %q", out)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "app", ".env")); string(got) != "SHARED='1'\n" {
		t.Fatalf("level .env %q", got)
	}
	// And a relative path resolves under the root.
	if out := runVarsScript(t, VarsSetScript(tl, nil), "OWN='1'\n"); strings.TrimSpace(out) != filepath.Join(root, "app", "prod", "shared", ".env") {
		t.Fatalf("relative path printed %q", out)
	}
}
