package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/door"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Review 5 (Fable), a failing test proving a bug: a start the door holds on its registry's
// copy goes on after a key's removal ended the user. The copy (copyImage, `buildx imagetools
// create`) has no bound, and its registry is one the project names, so whoever holds a leaked
// key parks `docker compose up -d` of a file of its own on a registry that serves the manifest
// and then stalls a blob, detached (setsid): sudo and the door are root's and the deploy
// user's processes, which the end of the user's processes does not reach. The admin removes
// the key (the end runs, the keys file is made anew), releases the project again and resets
// the home, as the doc says; days later the registry lets the blob go, and the door records
// the image (the user has a key left, the kept one), pins it and execs compose on the file it
// checked back then, over the project's containers, volumes and entries. Nothing between the
// door's first look at the keys and its exec looks again: only a sync key's add does
// (doorSide.keysAt), and the record only for a user with no key at all (whileAUser).
//
// The same cause as the sync key added after the end and the logins written back after a
// removal: a door past its sudo outlives the end. This asserts the plainest form, a start
// refused once the user's keys are no longer the file the request found; the end reaching the
// user's doors in flight would do as well (then this test changes with it).
func TestReview5ParkedStartEndsWithAKeysRemoval(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := doorImageFake(t, dir)
	image := "registry.example.com/shop/web"
	fakeManifest(t, dir, plainManifest("web"), image+":1")
	// Its registry holds the copy until the test lets it go.
	write(t, filepath.Join(dir, "slowcopy"), "", 0o644)
	keys := filepath.Join(dir, "keys")
	leaked := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g ci\n"
	kept := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPkVz0SeYpQpDNG2uM1BtNSy2LkZ2w3gYBGsgWAIEtxd laptop\n"
	write(t, keys, leaked+kept, 0o600)
	at, err := os.Stat(keys)
	if err != nil {
		t.Fatal(err)
	}
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(dir, "space")}, real: fake,
		env: []string{"PATH=" + os.Getenv("PATH")}, keysFile: keys, keysAt: at}
	file := map[string]any{"name": "shop-prod", "services": map[string]any{"web": map[string]any{"image": image + ":1"}}}
	// What compose's up runs before the door execs compose (doorSide.compose).
	started := make(chan error, 1)
	go func() { started <- side.checkImageLabels(file, filepath.Join(dir, "none.json")) }()
	for deadline := time.Now().Add(startWait); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(dir, "copying")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the copy never started:\n%s", readString(filepath.Join(dir, "calls")))
		}
	}
	// The leaked key's removal, as KeysRemoveScript and endProjectUser leave the keys: the kept
	// key alone, under a new inode. The end's kill reaches none of the door's processes.
	write(t, keys, kept, 0o600)
	if err := renewKeys(keys); err != nil {
		t.Fatal(err)
	}
	if now, err := os.Stat(keys); err != nil || os.SameFile(now, at) {
		t.Fatalf("the control: the keys file is the one the request found (%v)", err)
	}
	// The registry lets the copy go.
	pid, _ := strconv.Atoi(strings.TrimSpace(readString(filepath.Join(dir, "child"))))
	if pid <= 0 || syscall.Kill(pid, syscall.SIGKILL) != nil {
		t.Fatalf("the control: no copy to let go (%d)", pid)
	}
	select {
	case err := <-started:
		if err == nil {
			svc := file["services"].(map[string]any)["web"].(map[string]any)
			t.Errorf("a start parked on its registry went on after a key's removal ended the user: compose would run %v, the image recorded as the project's (%q)",
				svc["image"], strings.TrimSpace(readString(side.policy.ImagesFile())))
		}
	case <-time.After(startWait):
		t.Fatal("the start never ended")
	}
}

// Review 5 (Fable), a failing test proving a bug: --project-users remove whose end of the
// user's processes failed (Docker restarting, the util image not building, a session cut
// while the end runs) cannot be asked again. The entry and the keys are gone by then, so the
// same command finds "no project user for shop here" and ends nothing, and nothing on the
// server says an end is pending: what the user ran stays, and a later add of the project,
// with a new key, gives those processes the door back. A key's removal marks its end and
// settles it (remote.DoorEndingFile, keysSettle); the user's removal, the same end, does
// neither. This takes either: the removal asked again ends the user, or a mark is left for
// what settles it.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), where the user's keys are.
func TestReview5ProjectUserRemovalEndIsAskedAgain(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	entry := filepath.Join(t.TempDir(), "devopsy-shop.yaml")
	write(t, entry, "project: shop\n", 0o600)
	write(t, remote.DoorKeysFile("devopsy-shop"), "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEuaxIKUCdcH/5b0njntAmDSby4e2SdCbfB+x/kDNl8g op@laptop\n", 0o600)
	fails := errors.New("no docker")
	ended := 0
	end := func(string) error {
		if fails == nil {
			ended++
		}
		return fails
	}
	// The control: an end that fails fails the removal.
	if code := removeProjectUser(nil, "shop", "devopsy-shop", entry, end); code == 0 {
		t.Fatal("the control: a removal whose end failed passed")
	}
	// Docker is back: the admin asks again.
	fails = nil
	code := removeProjectUser(nil, "shop", "devopsy-shop", entry, end)
	_, err := os.Stat(remote.DoorEndingFile("devopsy-shop"))
	if ended == 0 && err != nil {
		t.Errorf("the removal asked again (exit %d) ended nothing, and no mark says an end is pending: what the user ran stays, for a later add to hand the door back to", code)
	}
}

// Review 5 (Fable), a failing test proving a bug: a project user's `docker compose --help`
// (and -h), compose's own help with no daemon behind it, is refused as a global flag "not
// taken for a project user, whose door runs the command without it". composeHelp reads the
// verb's flags only, and the loop over compose's global flags refuses the two before it.
func TestReview5DoorClientGivesComposesOwnHelp(t *testing.T) {
	t.Parallel()
	if syscall.Access(dockerSocket, 2) == nil {
		t.Skip("this user reaches Docker's socket: its docker is no door")
	}
	dir := t.TempDir()
	bin, realBin := filepath.Join(dir, "bin"), filepath.Join(dir, "real")
	direct := filepath.Join(dir, "direct")
	write(t, filepath.Join(realBin, "docker"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >>"+direct+"\n", 0o755)
	write(t, filepath.Join(bin, "sudo"), "#!/bin/sh\nexit 1\n", 0o755)
	if err := os.Symlink(binary, filepath.Join(bin, "docker")); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (string, string) {
		os.Remove(direct)
		cmd := exec.Command(filepath.Join(bin, "docker"), args...)
		cmd.Env = []string{"PATH=" + bin + ":" + realBin + ":/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir}
		cmd.Dir = dir
		out, _ := cmd.CombinedOutput()
		return strings.TrimSpace(readString(direct)), string(out)
	}
	// The control: a verb's help goes to the real docker.
	if real, out := run("compose", "up", "--help"); real != "compose up --help" {
		t.Fatalf("the control: a verb's help did not go to the real docker: %q\n%s", real, out)
	}
	for _, flag := range []string{"--help", "-h"} {
		if real, out := run("compose", flag); real != "compose "+flag {
			t.Errorf("docker compose %s: the real docker was asked %q:\n%s", flag, real, out)
		}
	}
}

// Review 5 (Fable), a failing test proving a hole: `entries`, which takes a file not yet
// checked, believes its user's answer about where a bind's source ends when that answer is
// not clean. The read runs as the project user, whose process it is to trace (doorRead says
// so), so the answer is that user's word, and checkBind and classify believe one only when it
// is clean; bindEntries asks only that its parent, as filepath.Dir cleans it, be the mnt. An
// answer through a link the user made in an entry of its own (data/l -> .), then `..` twice,
// reads to filepath.Dir as <space>/mnt/shop-prod/<name> and to the kernel as
// <space>/mnt/<name>: the deploy user makes that directory and gives the project user rwX on
// it, and on all made in it after (makeEntry's default ACL). With <name> another project's
// environment to come (blog-staging), that project's entries are then this user's to read,
// write and replace; with one of its own (shop-x), it replaces an entry under a root util
// container's bind, which Docker resolves at the start (an import's tar then lands in
// /etc/cron.d). The same answer ending in <space> itself makes <space>/clones or
// <space>/sealed where no project made one yet.
//
// Not parallel: it sets PATH (doorFakes).
func TestReview5EntriesBelieveOnlyACleanAnswer(t *testing.T) {
	acl, _ := doorFakes(t)
	root := t.TempDir()
	side := &doorSide{policy: door.Policy{Project: "shop", User: "devopsy-shop", Space: filepath.Join(root, "space")}}
	mnt := side.policy.Mnt("shop-prod")
	entry := filepath.Join(mnt, "data")
	if err := os.MkdirAll(entry, 0o755); err != nil {
		t.Fatal(err)
	}
	// In its entry, which it writes: a link to the entry itself.
	if err := os.Symlink(".", filepath.Join(entry, "l")); err != nil {
		t.Fatal(err)
	}
	binds := func(source string) map[string]any {
		return map[string]any{"name": "shop-prod", "services": map[string]any{"web": map[string]any{"volumes": []any{
			map[string]any{"type": "bind", "source": source, "target": "/x"},
		}}}}
	}
	// The control: an entry of its own mnt is made (the fake's read answers the path asked).
	if side.entries(binds(filepath.Join(mnt, "files"))); !fileExists(filepath.Join(mnt, "files")) {
		t.Fatal("the control: an entry of its mnt was not made")
	}
	// The answer its user forged: another project's mnt, to the kernel.
	theirs := filepath.Join(side.policy.Space, "mnt", "blog-staging")
	side.entries(binds(entry + "/l/../../blog-staging"))
	if fileExists(theirs) {
		t.Errorf("the door made %s, another project's mnt, and gave it to devopsy-shop:\n%s", theirs, readString(acl))
	}
}
