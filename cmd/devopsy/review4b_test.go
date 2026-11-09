package main

import (
	"os"
	"path/filepath"
	"testing"
)

// BUG: a sealed copy is named by what it holds (contentHash), and a directory's mode is not
// in it, only a file's. A release that opens a directory a container could not read (700 to
// 755, or the reverse to close one) seals to the same name, and Seal hands back the copy
// already there, its use renewed at each start: the container keeps the old modes for good.
func TestReview4bContentHashSeesADirectorysMode(t *testing.T) {
	t.Parallel()
	sum := func(mode os.FileMode) string {
		t.Helper()
		root := filepath.Join(t.TempDir(), "x")
		if err := os.MkdirAll(filepath.Join(root, "d"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "d", "f"), []byte("a"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(root, "d"), mode); err != nil {
			t.Fatal(err)
		}
		s, err := contentHash(root)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if sum(0o755) != sum(0o755) {
		t.Fatal("the control: the same content, two sums")
	}
	if sum(0o700) == sum(0o755) {
		t.Error("a directory closed to others (700) and one open to them (755): the same sum, so the same sealed copy")
	}
}
