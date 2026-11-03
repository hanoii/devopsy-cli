package images

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Bug: the fix of a refused copy that needs a build keeps what the operator
// ran for a sync (ReleaseSync, its --from) but not for a --no-code restore
// --with-env: its labels were judged with the snapshot's variables, and the
// full restore it names, without --with-env, runs under the environment's as
// they are, another version, another image. --check-images' own line for the
// same restore keeps it ("--snapshots restore <id> --with-env").
func TestCopyNeedsFixKeepsWithEnv(t *testing.T) {
	t.Parallel()
	d := newFake()
	var out bytes.Buffer
	m := newSim(t, d, "app-qa", &out)
	m.e.set.Snapshots = filepath.Join(m.base(), "snapshots")
	r1, err := m.release(false, builtDB("app-qa", "11.8"))
	if err != nil {
		m.fail("r1: %v", err)
	}
	own := m.runs("database")
	dir := m.snapshot("20261102000001")
	if _, err := m.failRun(false, builtDB("app-qa", "12.1")); err != nil {
		m.fail("r2: %v", err)
	}
	if m.cur != r1 {
		m.fail("setup: current %s", m.cur)
	}
	// Its own build gone, nothing but a build gives it back: only what the
	// snapshot's bookmark held kept it.
	for n, id := range d.names {
		if id == own {
			delete(d.names, n)
		}
	}
	delete(d.images, own)
	m.e.set.Again = "--snapshots restore 20261102000001 --no-code --with-env"
	out.Reset()
	data, _ := os.ReadFile(filepath.Join(dir, "data", "images"))
	err = m.e.set.CheckLanding(m.e.rel(m.cur), m.e.set.RestoreLanding(string(data), "20261102000001", nil))
	if !errors.Is(err, ErrRefused) {
		m.fail("its own build is gone: %v", err)
	}
	if !strings.Contains(out.String(), "--snapshots restore 20261102000001 --with-env") {
		t.Errorf("the fix drops the --with-env the operator ran:\n%s", out.String())
	}
}
