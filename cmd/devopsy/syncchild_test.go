package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// A sync key's forced command does that sync's requests on the child and nothing else:
// only while the sync is open there (its gate, or the record the first write renamed it
// to, still naming it), only the requests a sync makes (never another devopsy call, a
// wipe outside a reset, a spool or import of a service the child does not label custom, an
// entry or volume its labels lack, an unknown field, two at once), each script the
// child's own, run in its release as its compose project.
func TestSyncChild(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	base := filepath.Join(s.root, "shop", "dev")
	id := "1762600000.0a1b2c"
	child := func(req string, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(binary, append([]string{remote.SyncChildFlag, id, base, "vm2:dev"}, args...)...)
		cmd.Dir = t.TempDir()
		// sshd's environment, the request in SSH_ORIGINAL_COMMAND.
		cmd.Env = append(append([]string{}, s.env...), "SSH_ORIGINAL_COMMAND="+req)
		out, _ := cmd.CombinedOutput()
		return string(out), cmd.ProcessState.ExitCode()
	}
	list := `{"exec":["--sync-services","list"]}`
	if out, code := child(list); code == 0 || !strings.Contains(out, "sync "+id+" is not open in "+base) {
		t.Fatalf("no gate (%d):\n%s", code, out)
	}
	gate := filepath.Join(base, remote.SyncOpenFile(id))
	write(t, gate, "vm1:prod "+id+"\n", 0o644)
	if out, code := child(list); code != 0 || !strings.Contains(out, "db\t") || !strings.Contains(out, "app\t") {
		t.Fatalf("the list, the gate open (%d):\n%s", code, out)
	}
	for req, want := range map[string]string{
		`{"exec":["inheritfile"]}`:                      "not a sync's request",
		`{"exec":["--sync-services","wipe"]}`:           "not a sync's request",
		`{"exec":["--sync-services","import","db"]}`:    "no such custom service",
		`{"spool":"db"}`:                                "no such custom service",
		`{"spool":"../x"}`:                              "not a service",
		`{"receive":{"entry":"etc"}}`:                   `no entry "etc"`,
		`{"receive":{"volume":"other"}}`:                `no volume "other"`,
		`{"receive":{"entry":"storage","volume":"db"}}`: "one entry or one volume",
		`{"receive":{}}`:                                "one entry or one volume",
		// The parent names no port: the receive goes over the session, never to a port
		// on the child's loopback, which a root rsync there would reach for anyone.
		`{"receive":{"port":22,"entry":"storage"}}`: "unknown field",
		`{"exec":["--util-image"],"spool":"db"}`:    "one of exec, receive and spool",
		`{"exec":["--util-image"],"run":"sh"}`:      "unknown field",
		`sh -c 'id'`:                                "not a sync's request",
	} {
		if out, code := child(req); code == 0 || !strings.Contains(out, want) {
			t.Errorf("%s: want %q (%d):\n%s", req, want, code, out)
		}
	}
	for _, args := range [][]string{{id + `"`, base, "vm2:dev"}, {id, base + "/../x", "vm2:dev"}, {id, base, "vm2:dev,x"}} {
		cmd := exec.Command(binary, append([]string{remote.SyncChildFlag}, args...)...)
		cmd.Env = append(append([]string{}, s.env...), "SSH_ORIGINAL_COMMAND="+list)
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("%q: taken:\n%s", args, out)
		}
	}
	// The record the first write renamed the gate to still names the sync;
	// once the holder took it, or for another sync, nothing goes.
	started := filepath.Join(base, remote.SyncStartedFile)
	if err := os.Rename(gate, started); err != nil {
		t.Fatal(err)
	}
	if out, code := child(list); code != 0 {
		t.Errorf("after the claim (%d):\n%s", code, out)
	}
	write(t, started, "vm1:prod 1762600000.ffffff\n", 0o644)
	if out, code := child(list); code == 0 || !strings.Contains(out, "is not open") {
		t.Errorf("another sync's record (%d):\n%s", code, out)
	}
	if err := os.Rename(started, filepath.Join(base, remote.SyncTakenFile)); err != nil {
		t.Fatal(err)
	}
	if out, code := child(list); code == 0 || !strings.Contains(out, "is not open") {
		t.Errorf("taken (%d):\n%s", code, out)
	}
	os.Remove(filepath.Join(base, remote.SyncTakenFile))
	// A holder killed outright with its gate open and nothing written: the
	// next lock-taker's settle removes the gate, so its key does nothing.
	write(t, gate, "vm1:prod "+id+"\n", 0o644)
	settle := exec.Command(binary, "--sync-services", "settle")
	settle.Dir = filepath.Join(base, "current")
	settle.Env = append(append([]string{}, s.env...), "COMPOSE_PROJECT_NAME=shop-dev", "PWD="+settle.Dir)
	if out, err := settle.CombinedOutput(); err != nil || fileExists(gate) {
		t.Fatalf("settle over a dead holder's gate (%v), gate there %v:\n%s", err, fileExists(gate), out)
	}
	if out, code := child(list); code == 0 || !strings.Contains(out, "is not open") {
		t.Errorf("a settled gate's key (%d):\n%s", code, out)
	}
	// Whatever takes the lock next settles it: a release, here.
	write(t, gate, "vm1:prod "+id+"\n", 0o644)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 || fileExists(gate) {
		t.Errorf("a release over a dead holder's gate (%d), gate there %v:\n%s", code, fileExists(gate), out)
	}
}

// A sync copies between users of one kind, both project users or both deploy users: a
// release whose child check read the child's kind refuses before the upload, a --sync's
// holder before it stops anything, both ways.
func TestSyncKinds(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	across := []string{"DEVOPSY_SERVER_PROD=devopsy@one"}
	// The child a project user, the parent not: a first release refuses
	// before uploading.
	out, code := s.run(append(across, "FAKE_PROJECT_HOST=two"), "@devopsy@two:dev", "--release")
	if code == 0 || !strings.Contains(out, "the child's user is a project user and the parent's is not") || !strings.Contains(out, "--export <service> | devopsy @<child> --import <service>") || strings.Contains(out, "uploaded") {
		t.Fatalf("a release, a project user's child (%d):\n%s", code, out)
	}
	if out, code := s.run(across, "@devopsy@two:dev", "--release"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	s.logs()
	// The parent a project user, the child not: the holder refuses before
	// its prepare, so nothing stops on either side.
	out, code = s.run(append(across, "FAKE_PROJECT_HOST=one"), "@devopsy@two:dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code == 0 || !strings.Contains(out, "the parent's user is a project user and the child's is not") || strings.Contains(docker, " stop ") || strings.Contains(out, "copying") {
		t.Fatalf("a sync from a project user's parent (%d):\n%s\n%s", code, out, docker)
	}
}
