package images

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// layout writes releases under base: each id, with markers by suffix
// (f failed, b rolled back, x fetching, p pending), current pointing at
// cur.
func layout(t *testing.T, cur string, ids ...string) string {
	t.Helper()
	base := filepath.Join(t.TempDir(), "app", "prod")
	for _, spec := range ids {
		id := strings.TrimRight(spec, "fbxp")
		dir := filepath.Join(base, "releases", id)
		if err := os.MkdirAll(filepath.Join(dir, codeDir), 0o755); err != nil {
			t.Fatal(err)
		}
		for mark, file := range map[string]string{"f": failedFile, "b": RolledBackFile, "x": FetchingFile, "p": PendingFile} {
			if strings.Contains(spec[len(id):], mark) {
				if err := os.WriteFile(filepath.Join(dir, file), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(base, "releases", "9.tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("releases", cur, codeDir), filepath.Join(base, "current")); err != nil {
		t.Fatal(err)
	}
	return base
}

// Pruning keeps the keep newest live releases: failed and rolled-back
// ones never count, and stay while newer than the oldest kept; current
// and a release still fetching always stay.
func TestToPrune(t *testing.T) {
	base := layout(t, "5", "1", "2f", "3", "4b", "5", "6x", "7")
	if got := ToPrune(base, 2); !slices.Equal(got, []string{"4", "3", "2", "1"}) {
		t.Errorf("keep 2: %v", got)
	}
	if got := ToPrune(base, 3); !slices.Equal(got, []string{"2", "1"}) {
		t.Errorf("keep 3: %v", got)
	}
	if got := ToPrune(base, 5); got != nil {
		t.Errorf("fewer live than keep: %v", got)
	}
	// A pending one (never made current) counts as a failed one does:
	// never toward keep, kept while newer than the oldest kept.
	base = layout(t, "5", "1", "3", "4p", "5")
	if got := ToPrune(base, 2); !slices.Equal(got, []string{"1"}) {
		t.Errorf("pending: %v", got)
	}
	// After a rollback from 3 to 2, 3 is rolled back: kept, but never
	// counted, so 1 stays as the next rollback's target.
	base = layout(t, "2", "1", "2", "3b")
	if got := ToPrune(base, 2); got != nil {
		t.Errorf("after a rollback: %v", got)
	}
}

func TestRollbackTarget(t *testing.T) {
	if got := RollbackTarget(layout(t, "5", "1", "2", "3f", "4b", "4p", "5", "6")); got != "2" {
		t.Errorf("target %q, want 2", got)
	}
	if got := RollbackTarget(layout(t, "1", "1", "2b")); got != "" {
		t.Errorf("target %q, want none", got)
	}
}

// Every id is checked before anything goes; then each with its images.
func TestRemoveReleases(t *testing.T) {
	d := newFake()
	var out bytes.Buffer
	s := &Set{Docker: d, Name: "app-prod", Left: filepath.Join(t.TempDir(), LeftFile), Out: &out}
	base := layout(t, "3", "1", "2", "3")
	for _, ids := range [][]string{{"1", "3"}, {"1", "8"}} {
		if err := s.RemoveReleases(base, ids, false, false); err == nil || !exists(filepath.Join(base, "releases", "1")) {
			t.Errorf("%v: not refused (%v)\n%s", ids, err, out.String())
		}
	}
	if err := s.RemoveReleases(base, []string{"1", "2"}, false, false); err != nil || exists(filepath.Join(base, "releases", "1")) || !strings.Contains(out.String(), "removed release 2") {
		t.Errorf("remove (%v)\n%s", err, out.String())
	}
	// failed's ids, one taken by a pruning meanwhile: passed over.
	base = layout(t, "3", "1", "2", "3")
	if err := s.RemoveReleases(base, []string{"8", "2"}, false, true); err != nil || exists(filepath.Join(base, "releases", "2")) {
		t.Errorf("gone ok (%v)\n%s", err, out.String())
	}
}
