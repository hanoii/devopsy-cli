// Package sources finds the repository's own files for the tests that check them (what devopsy
// says, how tests run, the rules' names, the vocabulary): one walk, so every check reads the same
// files.
//
// Rules: the Checks in AGENTS.md.
package sources

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Root is the repository's root: the nearest directory holding go.mod, up from the working
// directory, which is a test's package.
func Root(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		up := filepath.Dir(dir)
		if up == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = up
	}
}

// File is one of the repository's Go files.
type File struct {
	// Path is slash separated from the root (for example internal/cli/cli.go).
	Path string
	Data []byte
}

// Test says whether f is a test file.
func (f File) Test() bool { return strings.HasSuffix(f.Path, "_test.go") }

// Go is every Go file under cmd/ and internal/, tests included only when tests is set.
func Go(t testing.TB, tests bool) []File {
	t.Helper()
	root := Root(t)
	var out []File
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
				return err
			}
			f := File{Path: rel(root, p)}
			if f.Test() && !tests {
				return nil
			}
			if f.Data, err = os.ReadFile(p); err != nil {
				return err
			}
			out = append(out, f)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(out) < 2 {
		t.Fatal("no Go files found")
	}
	return out
}

// Dirs are the directories under tops (for example cmd, internal, e2e) that hold a Go file,
// slash separated from the root.
func Dirs(t testing.TB, tops ...string) []string {
	t.Helper()
	root := Root(t)
	var dirs []string
	for _, top := range tops {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return err
			}
			if gos, _ := filepath.Glob(filepath.Join(p, "*.go")); len(gos) > 0 {
				dirs = append(dirs, rel(root, p))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(dirs) < 2 {
		t.Fatal("no Go packages found")
	}
	return dirs
}

// rel is p slash separated from root.
func rel(root, p string) string {
	r, _ := filepath.Rel(root, p)
	return filepath.ToSlash(r)
}
