package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/uids"
)

// Review round 2 (Opus), bug: --server-info drops what --gc's count could not read. leftFacts
// fills GCFacts.Unread from gcScope.find (the dangling images' listing failing, a release
// snapshot whose restore mark does not read, the project users' names), and leftEntries builds
// its "gc" line from the counts alone, never Unread: with nothing counted and a read failed,
// no line is said, which reads as nothing to remove, unknown taken for none.
func TestReview2ServerInfoSaysWhatGCCouldNotRead(t *testing.T) {
	t.Parallel()
	info := remote.ServerInfo{GC: &remote.GCFacts{Unread: []string{"the dangling images: docker image ls: exit status 1"}}}
	if got := strings.Join(leftSays(info, "devopsy@vm1", "vm1:"), "\n\n"); !strings.Contains(got, "the dangling images") {
		t.Errorf("what --gc's count could not read is not said:\n%s", got)
	}
}

// Review round 2 (Opus), bug: a project user whose --project-users remove --destroy did not
// complete is also said as a project user with no key, whose hint offers a key added to take
// it back. While the destroy's mark is there the keys scripts refuse the user (keysSettle)
// and sshd takes none of its keys, so that way out does not work; the destroy's own line
// (destroysPending) already says how to complete it. keylessUsers, shared by --project-users
// list and --server-info, reads no mark.
func TestReview2KeylessLeavesADestroyPending(t *testing.T) {
	t.Parallel()
	home, root := t.TempDir(), t.TempDir()
	write(t, filepath.Join(home, "config.yaml"), "releases:\n  root: "+root+"\ndoor:\n  space: "+t.TempDir()+"\n  users:\n    devopsy-shop: {project: shop}\n", 0o644)
	if _, err := uids.StoreAt(root).Lookup(uids.Owner{Project: "shop", Env: "prod"}); err != nil {
		t.Fatal(err)
	}
	// The destroy's mark (remote.DoorDestroyingFile), its project on its first line.
	write(t, filepath.Join(home, "door", "keys", "devopsy-shop.destroying"), "shop\nhome /home/devopsy-shop\nuid 50000\n", 0o600)
	bin := t.TempDir()
	write(t, filepath.Join(bin, "docker"), "#!/bin/sh\nexit 0\n", 0o755)
	out, code := runDevopsy(t, t.TempDir(), []string{"DEVOPSY_HOME=" + home, "PATH=" + bin + ":" + fakeBin(t) + ":/usr/bin:/bin"}, "--project-users-step", "list")
	if code != 0 || !strings.Contains(out, "devopsy-shop: the destroy of shop's project user did not complete") {
		t.Fatalf("list (%d):\n%s", code, out)
	}
	if strings.Contains(out, "devopsy-shop has no key") || strings.Contains(out, "a key added for it") {
		t.Errorf("a destroy pending is said as a keyless user a key takes back:\n%s", out)
	}
}
