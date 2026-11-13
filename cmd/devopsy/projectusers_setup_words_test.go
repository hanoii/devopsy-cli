package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --project-users setup says a server is ready for project users, and offers --project-users
// add, only where add then goes: on a server --server-setup never ran on (its release root
// missing), add refuses for the release root, so the setup's last line names that instead.
//
// Bug: the setup judges only its own items (the tools, Docker, the directory, the config, the
// processes), never the release root add refuses on, so on a new server it ends "this server
// is ready for project users" with add to copy, and the add copied refuses.
func TestProjectUsersSetupReadyOnlyWhereAddGoes(t *testing.T) {
	t.Parallel()
	s, _, space := doorSetupServer(t)
	out, code := s.run(nil, "@devopsy@one:", "--project-users", "setup", space, "--yes")
	if code != 0 {
		t.Fatalf("the setup (%d):\n%s", code, out)
	}
	ready := strings.Contains(out, "devopsy: this server is ready for project users\n") && strings.Contains(out, "$ devopsy @devopsy@one: --project-users add ")
	add, code := s.run(nil, "@devopsy@one:", "--project-users", "add", "shop")
	if ready && code != 0 && strings.Contains(add, "the release root "+s.root+" is missing: no project user made") {
		t.Errorf("the setup says the server is ready and offers add, which refuses:\n%s\nadd (%d):\n%s", out, code, add)
	}
	// A second run says it again, with nothing to change.
	out, _ = s.run(nil, "@devopsy@one:", "--project-users", "setup", "--yes")
	if strings.Contains(out, "nothing to change: devopsy@one is ready for project users") {
		t.Errorf("a second run says the server is ready, its release root missing:\n%s", out)
	}
}

// A refusal of the directory door: space: names in the config offers a fix that works there:
// another directory named is refused (devopsy never moves it), so the fix never says to name
// one.
//
// Bug: the refusals of a path another user could swap (a link, a parent others own or may
// write) and of a path with other characters say "name a directory ...", written for a
// directory typed on the command; where the config names it, naming one is refused in turn,
// and the admin is sent from one refusal to the other.
func TestProjectUsersSetupFixForTheConfigsDirectory(t *testing.T) {
	t.Parallel()
	s, _, space := doorSetupServer(t)
	if err := os.Symlink(t.TempDir(), space); err != nil {
		t.Fatal(err)
	}
	setupConfig(t, s, "door:\n  space: "+space+"\n")
	out, code := s.run(nil, "@devopsy@one:", "--project-users", "setup", "--yes")
	if code != 1 || !strings.Contains(out, space+" is a symbolic link") {
		t.Fatalf("a link the config names (%d):\n%s", code, out)
	}
	other := filepath.Join(filepath.Dir(space), "other")
	named, _ := s.run(nil, "@devopsy@one:", "--project-users", "setup", other, "--yes")
	if strings.Contains(out, "devopsy: | name a directory") && strings.Contains(named, "(door: space:), not "+other) {
		t.Errorf("the fix says to name a directory, and one named is refused:\n%s\nwith %s named:\n%s", out, other, named)
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
	if !strings.Contains(after, "devopsy: error: ") {
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
