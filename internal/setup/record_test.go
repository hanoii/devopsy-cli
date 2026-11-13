package setup

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestRecordText keeps the record's text: the header, the devopsy that last applied here, the one
// that last found Docker at its floor, Parts' order, each value single-quoted canonical JSON, a part no longer wanted kept after them.
func TestRecordText(t *testing.T) {
	t.Parallel()
	r := Record{Version: "0.65.0", Floor: "0.66.0", Daemon: map[string]Entry{
		"default-address-pools": {Kept: `{"default-address-pools":[{"base":"10.200.0.0/16","size":24}]}`},
		"live-restore":          {Applied: `{"live-restore":true}`},
		"logs":                  {Applied: `{"log-driver":"local","log-opts":{"max-file":"5","max-size":"20m"}}`},
		"gone":                  {Applied: `{"gone":"it's"}`},
	}}
	got, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	want := `# Written by devopsy (--server-setup, --upgrade): what it applied here. A record, not
# settings: change daemon.json, never this file.
devopsy: 0.65.0
docker-floor: 0.66.0
daemon.json:
  logs:
    applied: '{"log-driver":"local","log-opts":{"max-file":"5","max-size":"20m"}}'
  live-restore:
    applied: '{"live-restore":true}'
  default-address-pools:
    kept: '{"default-address-pools":[{"base":"10.200.0.0/16","size":24}]}'
  gone:
    applied: '{"gone":"it''s"}'
`
	if string(got) != want {
		t.Errorf("record:\n%s\nwant:\n%s", got, want)
	}
	back, err := ParseRecord(got)
	if err != nil || !reflect.DeepEqual(back, r) {
		t.Errorf("read back %+v, %v; want %+v", back, err, r)
	}
	if _, err := ParseRecord([]byte("daemon.json:\n  logs:\n    applied: '{}'\n    kept: '{}'\n")); err == nil {
		t.Error("an entry both applied and kept read")
	}
	// No version line: none recorded, read as another devopsy's; no floor line, the floor
	// never found met.
	if r, err := ParseRecord([]byte("daemon.json:\n  logs:\n    applied: '{}'\n")); err != nil || r.Version != "" || r.Floor != "" || r.Daemon["logs"].Applied != "{}" {
		t.Errorf("no version line: %+v, %v", r, err)
	}
}

// TestRecordFile keeps the record's round trip on disk, beside the config file, mode
// 0600, written by rename with nothing left beside it, none read as no record.
func TestRecordFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := RecordPath(filepath.Join(dir, "devopsy", "config.yaml"))
	if path != filepath.Join(dir, "devopsy", RecordFile) || LockPath(path) != filepath.Join(dir, "devopsy", LockFile) {
		t.Fatalf("paths %s, %s", path, LockPath(path))
	}
	if RecordPath("") != "" || LockPath("") != "" {
		t.Error("a path with no config file")
	}
	r, exists, err := ReadRecord(path)
	if err != nil || exists || len(r.Daemon) != 0 {
		t.Fatalf("no record: %+v, %v, %v", r, exists, err)
	}
	if err := WriteRecord(path, r); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := ReadRecord(path); err != nil || !exists {
		t.Fatalf("an empty record: %v, %v", exists, err)
	}
	r = r.Set("logs", Entry{Applied: Parts[0].Want}).Set("live-restore", Entry{Kept: `{}`})
	if err := WriteRecord(path, r); err != nil {
		t.Fatal(err)
	}
	back, exists, err := ReadRecord(path)
	if err != nil || !exists || !reflect.DeepEqual(back, r) {
		t.Errorf("read back %+v, %v, %v; want %+v", back, exists, err, r)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, %v", fi.Mode(), err)
	}
	if es, _ := os.ReadDir(filepath.Dir(path)); len(es) != 1 {
		t.Errorf("left beside the record: %v", es)
	}
	if err := os.WriteFile(path, []byte("daemon.json: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadRecord(path); err == nil {
		t.Error("a broken record read")
	}
}

// TestWriteFile keeps a whole file's write: by rename, its mode set, nothing left beside it
// whether it went or failed, and a symbolic link written at the file it names, the link kept.
func TestWriteFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "home", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "real", "config.yaml"), link); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(link, []byte("new\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link is no link: %v", err)
	}
	if data, _ := os.ReadFile(real); string(data) != "new\n" {
		t.Errorf("the file the link names: %q", data)
	}
	if fi, _ := os.Stat(real); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %v", fi.Mode())
	}
	// A rename that fails (a directory in the file's place) leaves nothing beside it.
	if err := os.Remove(real); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(link, []byte("new\n"), 0o600); err == nil {
		t.Error("a write over a directory went")
	}
	for _, d := range []string{filepath.Dir(real), filepath.Dir(link)} {
		if es, _ := os.ReadDir(d); len(es) != 1 {
			t.Errorf("left in %s: %v", d, es)
		}
	}
}

// TestCutBeforeRecord keeps the order an apply writes in, the file then the record: an
// apply cut between them leaves the part in sync whatever the record said before, so it
// needs no record, and only a keep answer is lost.
func TestCutBeforeRecord(t *testing.T) {
	t.Parallel()
	pools, _ := PartNamed("default-address-pools")
	for _, before := range []struct {
		name string
		file string
		r    Entry
	}{
		{"no record, nothing there", `{}`, Entry{}},
		{"an older want applied", `{"default-address-pools":[{"base":"10.0.0.0/8","size":24}]}`,
			Entry{Applied: `{"default-address-pools":[{"base":"10.0.0.0/8","size":24}]}`}},
		{"a kept value replaced", `{"default-address-pools":[{"base":"10.200.0.0/16","size":24}]}`,
			Entry{Kept: `{"default-address-pools":[{"base":"10.200.0.0/16","size":24}]}`}},
	} {
		d := mustDaemon(t, before.file)
		out, changed, err := d.Merge([]Part{pools})
		if err != nil || !changed {
			t.Fatalf("%s: %v, %v", before.name, changed, err)
		}
		// Cut here: daemon.json is put, the record still the one before.
		r := Record{Daemon: map[string]Entry{}}
		if before.r != (Entry{}) {
			r = r.Set(pools.Name, before.r)
		}
		for _, s := range Judge(mustDaemon(t, string(out)), r, nil) {
			if s.Part.Name == pools.Name && s.State != InSync {
				t.Errorf("%s: after the cut %s, want in sync", before.name, s.State)
			}
		}
	}
}
