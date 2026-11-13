package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --project-users setup says a server is ready for project users, and offers --project-users
// add, only where add then goes: on a server --server-setup never ran on (its release root
// missing), it refuses before anything changes, naming --server-setup on the address as typed,
// as add refuses; with the release root made, it sets the server up and add goes past it.
func TestProjectUsersSetupReadyOnlyWhereAddGoes(t *testing.T) {
	t.Parallel()
	s, host, space := doorSetupServer(t)
	if err := os.Remove(s.root); err != nil {
		t.Fatal(err)
	}
	before := readString(configFile(s))
	out, code := s.run(nil, "@devopsy@one:", "--project-users", "setup", space, "--yes")
	entries, _ := os.ReadDir(filepath.Join(host, "etc"))
	if code != 1 || !strings.Contains(out, "devopsy: error: release root: the release root "+s.root+" is missing\ndevopsy: | to fix it:\ndevopsy: | set the server up:\ndevopsy: | $ devopsy @devopsy@one: --server-setup\n") ||
		strings.Contains(out, "ready for project users") || len(entries) > 0 || fileExists(space) || readString(configFile(s)) != before {
		t.Errorf("no release root (%d):\n%s", code, out)
	}
	if out, code := s.run(nil, "@devopsy@one:", "--project-users", "setup", "--yes"); code != 1 || strings.Contains(out, "ready for project users") {
		t.Errorf("a second run, no release root (%d):\n%s", code, out)
	}
	if err := os.Mkdir(s.root, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, code := s.run(nil, "@devopsy@one:", "--project-users", "setup", space, "--yes"); code != 0 || !strings.Contains(out, "devopsy: this server is ready for project users\n") {
		t.Fatalf("the setup (%d):\n%s", code, out)
	}
	if add, _ := s.run(nil, "@devopsy@one:", "--project-users", "add", "shop"); strings.Contains(add, "the release root") {
		t.Errorf("add refuses for the release root after the setup said ready:\n%s", add)
	}
}

// A refusal of the directory door: space: names in the config offers a fix that works there:
// what to change of that directory, or to move it by hand and name the new path in the config,
// never to name another directory, which is refused while the config names this one.
func TestProjectUsersSetupFixForTheConfigsDirectory(t *testing.T) {
	t.Parallel()
	s, _, space := doorSetupServer(t)
	if err := os.Symlink(t.TempDir(), space); err != nil {
		t.Fatal(err)
	}
	setupConfig(t, s, "door:\n  space: "+space+"\n")
	out, code := s.run(nil, "@devopsy@one:", "--project-users", "setup", "--yes")
	if code != 1 || !strings.Contains(out, "devopsy: error: project users' directory: "+space+" is a symbolic link, and devopsy acts as root only on a path no other user can change\n"+
		"devopsy: | to fix it:\ndevopsy: | make every part of its path a directory, none of them a link, each above it root's or the deploy user's and writable by no one else, "+
		"or move it by hand to such a path and set door: space: in "+configFile(s)+" to that path\n") {
		t.Errorf("a link the config names (%d):\n%s", code, out)
	}
}

// A --project-users setup that exits 1 ends on an error saying what is left, as one whose
// refusal stopped a change does: a /proc hidden another way with no gid= is refused, the
// directory beside it set up, and the run's last words say the server is not ready.
//
// Bug: the apply's last error names only the steps a refusal stopped that had a change to make
// (setup.DoorFacts.Stopped), and a refused /proc set another way has none, so the run ends on
// its last change ("Docker now waits ...") and exits 1 with no word after the changes.
func TestProjectUsersSetupFailsWithItsLastWords(t *testing.T) {
	t.Parallel()
	s, host, space := doorSetupServer(t)
	write(t, filepath.Join(host, procMounts), "proc /proc proc rw,nosuid,nodev,noexec,relatime,hidepid=invisible 0 0\n", 0o644)
	out, code := s.run(nil, "@devopsy@one:", "--project-users", "setup", space, "--yes")
	_, after, applied := strings.Cut(out, "devopsy: added the project users' directory to "+configFile(s)+"\n")
	if code != 1 || !applied || !strings.Contains(out, "devopsy: error: processes: /proc hides other users' processes (hidepid=invisible) with no gid=") {
		t.Fatalf("a /proc hidden with no gid= (%d):\n%s", code, out)
	}
	if !strings.HasSuffix(after, "devopsy: error: this server is not ready for project users\n") {
		t.Errorf("the run exits 1 and says no error after its changes:\n%s", out)
	}
}

// What the apply says of the deploy user's new group is true of the /proc it leaves: where
// systemd did not remount /proc, every user still sees every process, so no line says the
// deploy user sees them only from its next login.
//
// Bug: the line goes by the run having put the deploy user in the group alone, so it is said
// beside "systemd did not remount /proc" too, of a /proc that hides nothing.
func TestProjectUsersSetupNextLoginOnlyOnceHidden(t *testing.T) {
	t.Parallel()
	s, _, space := doorSetupServer(t)
	bin := filepath.Join(filepath.Dir(s.root), "bin")
	write(t, filepath.Join(bin, "systemctl-real"), readString(filepath.Join(bin, "systemctl")), 0o755)
	write(t, filepath.Join(bin, "systemctl"), "#!/bin/sh\n[ \"$*\" != 'restart devopsy-proc.service' ] || exit 1\nexec \"$(dirname \"$0\")/systemctl-real\" \"$@\"\n", 0o755)
	out, code := s.run(nil, "@devopsy@one:", "--project-users", "setup", space, "--yes")
	if code != 1 || !strings.Contains(out, "devopsy: error: systemd did not remount /proc\n") {
		t.Fatalf("a /proc systemd does not remount (%d):\n%s", code, out)
	}
	if strings.Contains(out, " sees every process from its next login\n") {
		t.Errorf("/proc is not hidden, and the deploy user is said to see every process only from its next login:\n%s", out)
	}
}
