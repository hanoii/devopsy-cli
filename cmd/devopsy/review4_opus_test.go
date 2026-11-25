package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Bug (review 4): with --borders a project's heading is put in the key
// column (entryTable.boxed turns its Span off), so its name is measured
// into the sections' shared key column: a project named longer than every
// key widens the key column of every box of the report, and its prose
// wraps in what is left. Without borders the heading spans and the key
// column stays as the keys need. The key column of a boxed report should
// not depend on a project's name that holds no facts on its line.
func TestReview4BoxedKeyColumnIgnoresAProjectHeading(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	// keyColumn is the report's system line up to its value.
	keyColumn := func(info remote.ServerInfo, borders bool) string {
		var b strings.Builder
		tab := newEntryTable(&b, style{})
		tab.borders = borders
		tab.print(serverInfoReport(info, "vm1", "vm1:", "0.53.0", now))
		for _, l := range strings.Split(b.String(), "\n") {
			if i := strings.Index(l, "Debian"); i >= 0 && strings.Contains(l, "system") {
				return l[:i]
			}
		}
		t.Fatalf("no system line in:\n%s", b.String())
		return ""
	}
	long := healthyServerInfo()
	long.Environments = append(long.Environments, remote.Environment{Path: "storefront-checkout-service/prod",
		Status: "release\t20261027115000\t{\"mode\":\"image\",\"time\":\"2026-10-27T11:50:00Z\",\"by\":\"ana\"}\nkept\t1\nsynced\t\n"})
	for _, borders := range []bool{false, true} {
		if short, wide := keyColumn(healthyServerInfo(), borders), keyColumn(long, borders); short != wide {
			t.Errorf("borders %v: a project's name moved the value column: %q, with a long project name %q", borders, short, wide)
		}
	}
}

// Bug (review 4): --snapshots checks --borders before it checks its
// words (runSnapshots), where --releases checks the words first
// (runReleases): a word that is no subcommand with --borders is said as
// "--snapshots <word> takes no --borders", as though the word were one,
// instead of the usage error the same word gets without --borders. The
// same check written twice, in two orders.
func TestReview4SnapshotsBordersAfterItsWords(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	plain, plainCode := s.run(nil, "@prod", "--snapshots", "bogus")
	boxed, boxedCode := s.run(nil, "@prod", "--snapshots", "bogus", "--borders")
	if plain != boxed || plainCode != boxedCode {
		t.Errorf("a word that is no subcommand, said otherwise with --borders:\nwithout (%d):\n%s\nwith (%d):\n%s", plainCode, plain, boxedCode, boxed)
	}
}

// Bug (review 4): docs/servers.md lists what each --json entry of
// --server-info carries (section, group, head, cells, name) but not
// instance, which this branch added (remote.StatusEntry.Instance: an
// environment's row, a claim's and an instance's heading under their
// instance): a script reading --json meets a field no doc names, and the
// docs no longer say how an instance's prod differs from the project's.
func TestReview4ServerInfoDocsNameEveryEntryField(t *testing.T) {
	t.Parallel()
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "servers.md"))
	if err != nil {
		t.Fatal(err)
	}
	// What every entry has, which the docs say in words (its level, key,
	// lines and fixes).
	plain := map[string]bool{"level": true, "key": true, "lines": true, "remedies": true}
	ty := reflect.TypeOf(remote.StatusEntry{})
	for i := range ty.NumField() {
		name, _, _ := strings.Cut(ty.Field(i).Tag.Get("json"), ",")
		if !plain[name] && !strings.Contains(string(doc), "`"+name+"`") {
			t.Errorf("docs/servers.md names no `%s`, a field of --server-info --json's entries", name)
		}
	}
}

// Bug (review 4): --ssh-keys quotes a key's comment and options as they
// show (report.Copyable) but writes its type as the file has it, though
// the same line holds all three: a type that starts with ssh- passes the
// parse with a control sequence in it, and reaches the terminal raw.
func TestReview4SSHKeysTypeShownEscaped(t *testing.T) {
	t.Parallel()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	write(t, filepath.Join(bin, "fakessh"), "#!/bin/sh\nfor a; do s=$a; done\nexec sh -c \"$s\"\n", 0o755)
	write(t, filepath.Join(bin, "flock"), "#!/bin/sh\nexit 0\n", 0o755)
	project := filepath.Join(tmp, "app")
	write(t, filepath.Join(project, ".devopsy", "compose.yaml"), "services: {}\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", "config.yaml"), "project: app\nenvironments:\n  prod: {}\n", 0o644)
	write(t, filepath.Join(project, ".devopsy", ".env"), "DEVOPSY_SERVER=devopsy@server\n", 0o644)
	write(t, filepath.Join(tmp, ".ssh", "authorized_keys"),
		"ssh-\x1b[2Jed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g sample@test\n", 0o600)
	cmd := exec.Command(binary, "@prod", "--ssh-keys")
	cmd.Dir = project
	cmd.Env = []string{"DEVOPSY_HOME=" + t.TempDir(), "DEVOPSY_SSH_COMMAND=" + filepath.Join(bin, "fakessh"), "PATH=" + bin + ":" + fakeBin(t) + ":/usr/bin:/bin", "HOME=" + tmp}
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "sample@test") {
		t.Fatalf("the control: the key not listed:\n%s", out)
	}
	if !strings.Contains(string(out), `\x1b[2Jed25519`) {
		t.Errorf("a key's type not shown escaped:\n%q", out)
	}
}
