package remote

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/door"
)

// BUG: on a deploy user's child, the receive mounts the child's whole mnt into a root rsync
// (-v "$mnt:/mnt" in SyncReceiveScript), and the parent's daemon decides what lands under
// it. The parent names which entries are files (syncReceive.Files, its own word: the child's
// list says nothing of it), so the child runs rsync --inplace from <entry>/<entry> into
// /mnt/, and a daemon serving that name as a symlink replaces the child's entry with the
// symlink, a non-empty directory included (with --delete, rsync removes a directory that a
// non-directory replaces; checked below with the real rsync where there is one). The
// child's next up binds mnt/<entry>, which Docker resolves on the host: the parent chose
// the host path the child's container mounts (/, /root/.ssh, Docker's socket), and a
// database image's entrypoint chowns its data directory recursively as root. That is beyond
// the copy, which the sync key was made to bound. The door narrows the same request to the
// one entry it writes (TestDecideRunReceiveIntoAVolumeHoldsNoMnt, the control here): there
// the entry is a mount point rsync cannot unlink. The fix reaches the cause when the script
// mounts "$mnt/<entry>" alone for each copy, as the door does.
func TestReceiveHoldsOnlyTheEntryItWrites(t *testing.T) {
	t.Parallel()
	script := SyncReceiveScript("/srv/app/current/.devopsy", "/srv/app", "1.2", "20001", []string{"data", "app.ini"}, []string{"app.ini"}, nil, "app-dev", nil, true, "", true)
	if strings.Contains(script, `-v "$mnt:/mnt"`) {
		t.Errorf("the receive mounts the child's whole mnt rw into a root rsync, so the parent's daemon can replace an entry:\n%s", script)
	}

	// The control: the door, asked for the same rsync by a project user, hands the
	// container the one entry it writes, never the mnt.
	mnt := "/space/mnt/shop-prod"
	argv := slices.Concat([]string{"run", "--network", "host", "-e", "RSYNC_PASSWORD", "-v", mnt + ":/mnt", UtilImage}, RsyncCopy, []string{"--inplace", "rsync://127.0.0.1:20001/app.ini/app.ini", "/mnt/"})
	a, err := door.Decide(argv, reviewPolicy, reviewDaemon{}, &reviewHost{}, door.Known{Image: UtilImage, RsyncCopy: RsyncCopy})
	if err != nil {
		t.Fatalf("the control: the door refused the receive it takes from a sync: %v", err)
	}
	if slices.Contains(a.Docker, mnt+":/mnt") || !slices.Contains(a.Docker, mnt+"/app.ini:/mnt/app.ini") {
		t.Fatalf("the control: the door did not narrow the receive to its entry: %q", a.Docker)
	}

	// What rsync does with what the parent serves: its daemon's <entry>/<entry> a symlink,
	// the child's entry a directory with data in it, RsyncCopy with --inplace as the
	// receive runs for a file entry. Only the real rsync (the util image's, the servers');
	// macOS ships openrsync.
	if out, err := exec.Command("rsync", "--version").Output(); err != nil || !strings.Contains(string(out), "rsync  version 3") {
		t.Skip("no rsync 3 here to show what the daemon's symlink does to the entry")
	}
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "mod"), filepath.Join(dir, "mnt")
	for _, d := range []string{src, filepath.Join(dst, "data")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dst, "data", "f"), []byte("the child's data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(src, "data")); err != nil {
		t.Fatal(err)
	}
	copy := exec.Command(RsyncCopy[0], slices.Concat(RsyncCopy[1:], []string{"--inplace", filepath.Join(src, "data"), dst + "/"})...)
	if out, err := copy.CombinedOutput(); err != nil {
		t.Fatalf("rsync: %v: %s", err, out)
	}
	if fi, err := os.Lstat(filepath.Join(dst, "data")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the demonstration: rsync left the entry as it was (%v): the bug needs it replaced", err)
	}
	t.Logf("rsync replaced the child's entry, data in it, with the daemon's symlink: the next up binds its target")
}

// reviewPolicy, reviewDaemon and reviewHost are the door's fixtures for the control above:
// a project user whose mnt is under /space.
var reviewPolicy = door.Policy{Project: "shop", User: "devopsy-shop", UID: "1001", Space: "/space", Tree: "/home/devopsy-shop/shop"}

type reviewDaemon struct{}

func (reviewDaemon) Inspect(string) (string, map[string]string, error) {
	return "", nil, errors.New("no such container")
}
func (reviewDaemon) Containers() ([]string, error) { return nil, nil }
func (reviewDaemon) Image(string) (string, []string, error) {
	return "", nil, errors.New("no such image")
}

type reviewHost struct{}

func (*reviewHost) Real(path string) (string, error) { return filepath.Clean(path), nil }
func (*reviewHost) Seal(path string) (string, error) { return path, nil }
func (*reviewHost) IsDir(string) bool                { return true }
func (*reviewHost) CopyIn(path string, _ bool) (string, error) {
	return "/space/work/in/" + filepath.Base(path), nil
}
func (*reviewHost) WorkDir() (string, error) { return "/space/work/out", nil }
