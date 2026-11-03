package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/images"
	"github.com/hanoii/devopsy-cli/internal/remote"
)

// A landing can fail with a fix that names a release (images.Set.unhold),
// but three of its callers never tell the Set the address (--resolved,
// a reset's wipe, a restore's data step: only an import and img do), so
// the line to copy is "$ --release", which is no command.
func TestLandingFixNamesTheAddress(t *testing.T) {
	t.Parallel()
	s := newImportSetup(t)
	link, err := os.Readlink(filepath.Join(s.base, "current"))
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(s.base, filepath.Dir(link))
	write(t, filepath.Join(rel, images.RecordFile), `{"mode":"build"}`, 0o644)
	list := filepath.Join(rel, images.ListFile)
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(readString(list), "\n"), "\n") {
		if f := strings.Split(l, "\t"); f[2] == "app" {
			// Held on HELD, the data's 11.8; its own, 11, gone.
			l = strings.Join([]string{"shop-prod-app", "HELD", "app", "own", "", "", "", "11.8", "", "shop-prod-app", "GONE", "", "", "11", ""}, "\t")
		}
		lines = append(lines, l)
	}
	write(t, list, strings.Join(lines, "\n")+"\n", 0o644)
	write(t, s.log+".shop-prod", "FAKE_DB=stream\nFAKE_APP=mnt+import\nFAKE_WORKER=none\nFAKE_APP_BUILD=1\nFAKE_APP_VERSION=11\nFAIL_PULL=1\nFAKE_NAMES='shop-prod-app=HELD HELD=HELD'\n", 0o644)
	write(t, filepath.Join(s.base, remote.SyncIncompleteFile), "2026-11-03 10:00:00\tapp\t"+remote.IncompleteImport+"\tcut\n", 0o644)
	_, out, code := s.runIO(nil, nil, "@prod", "--import", "app", "--resolved", "--yes")
	if code == 0 || !strings.Contains(out, "own image of it, built by the project, is gone") {
		t.Fatalf("--resolved with its own build gone (%d):\n%s", code, out)
	}
	if strings.Contains(out, "$ --release\n") || !strings.Contains(out, "$ devopsy @devopsy@one:prod --release\n") {
		t.Errorf("the fix cannot be copied:\n%s", out)
	}
}

// An environment marked by an import, held, its own build gone (nothing
// names it once held: any pruning on the server takes it) has no way out:
// --resolved refuses, naming a release; a release refuses on the mark,
// naming an import or --resolved; an import refuses, naming a release,
// and --accept-image does not help it. Only --destroy is left, or a
// snapshot: at least one of the commands they name has to go through.
func TestMarkedAndOwnBuildGoneHasAWayOut(t *testing.T) {
	t.Parallel()
	s := newImportSetup(t)
	link, err := os.Readlink(filepath.Join(s.base, "current"))
	if err != nil {
		t.Fatal(err)
	}
	rel := filepath.Join(s.base, filepath.Dir(link))
	write(t, filepath.Join(rel, images.RecordFile), `{"mode":"build"}`, 0o644)
	list := filepath.Join(rel, images.ListFile)
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(readString(list), "\n"), "\n") {
		if f := strings.Split(l, "\t"); f[2] == "app" {
			l = strings.Join([]string{"shop-prod-app", "HELD", "app", "own", "", "", "", "11.8", "", "shop-prod-app", "GONE", "", "", "11", ""}, "\t")
		}
		lines = append(lines, l)
	}
	write(t, list, strings.Join(lines, "\n")+"\n", 0o644)
	write(t, s.log+".shop-prod", "FAKE_DB=stream\nFAKE_APP=mnt+import\nFAKE_WORKER=none\nFAKE_APP_BUILD=1\nFAKE_APP_VERSION=11\nFAIL_PULL=1\nFAKE_NAMES='shop-prod-app=HELD HELD=HELD'\n", 0o644)
	write(t, filepath.Join(s.base, remote.SyncIncompleteFile), "2026-11-03 10:00:00\tapp\t"+remote.IncompleteImport+"\tcut\n", 0o644)

	var tarred bytes.Buffer
	tw := tar.NewWriter(&tarred)
	body := []byte("x\n")
	if err := tw.WriteHeader(&tar.Header{Name: "storage/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "storage/f", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	tw.Write(body)
	tw.Close()

	_, resolved, resolvedCode := s.runIO(nil, nil, "@prod", "--import", "app", "--resolved", "--yes")
	imported, importedCode := "", 0
	{
		_, out, code := s.runIO(s.env, tarred.Bytes(), "@prod", "--import", "app", "--accept-image", "app", "--yes")
		imported, importedCode = out, code
	}
	released, releasedCode := s.run(nil, "@prod", "--release")
	if resolvedCode != 0 && importedCode != 0 && releasedCode != 0 {
		t.Errorf("no way out of the mark:\n--resolved (%d):\n%s\n--import (%d):\n%s\n--release (%d):\n%s", resolvedCode, resolved, importedCode, imported, releasedCode, released)
	}
}
