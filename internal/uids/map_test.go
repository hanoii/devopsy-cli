package uids

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/testfile"
)

// One map, two renderers: rsync's maps take the environment range to the environment's uid
// and both project ranges to the project's, users and groups alike, the ranges as FROM,
// and read back as written; a map script finds exactly the same ranges, uid and gid apart.
// Nothing else is a map's word.
func TestMapRenders(t *testing.T) {
	t.Parallel()
	m := Map{Env: 2000000123, Project: 1950000007}
	want := []string{"--usermap=2000000000-2099999999:2000000123,1900000000-1999999999:1950000007", "--groupmap=2000000000-2099999999:2000000123,1900000000-1999999999:1950000007"}
	if got := m.RsyncArgs(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("rsync's: %q", got)
	}
	for _, a := range m.RsyncArgs() {
		if env, project, ok := ParseRsyncMap(a); !ok || env != "2000000123" || project != "1950000007" {
			t.Errorf("%s read back: %q %q %v", a, env, project, ok)
		}
	}
	for _, a := range []string{"--usermap=0-4294967294:2000000123", "--usermap=2000000000-2099999999:1,1900000000-1999999999:2,*:3", "--chown=1:1", "--usermap=1900000000-2099999999:1,1900000000-1999999999:2"} {
		if _, _, ok := ParseRsyncMap(a); ok {
			t.Errorf("%s taken for a map", a)
		}
	}
	if got := strings.Join(m.Args(), " "); got != "2000000123 1950000007" {
		t.Errorf("the script's arguments: %q", got)
	}
	// The ranges the script finds by, find's +N and -N being strict.
	for _, bounds := range []string{"-uid chown 1999999999 2100000000 \"$2\"", "-gid chgrp 1999999999 2100000000 \"$2\"", "-uid chown 1899999999 2000000000 \"$3\"", "-gid chgrp 1899999999 2000000000 \"$3\""} {
		if !strings.Contains(MapScript, bounds) {
			t.Errorf("the script lacks %q:\n%s", bounds, MapScript)
		}
	}
}

// The map script parses as POSIX sh (dash on servers), maps nothing for "-", and asks find
// for only the files of a range not on their target yet, chown and chgrp on the link itself:
// a fake find here logs what it is asked and answers what a pass found. A pass that fails
// (a file the service removed meanwhile) fails the map only when something is left on a
// mapped uid.
func TestMapScriptRuns(t *testing.T) {
	t.Parallel()
	if out, err := exec.Command("sh", "-n", "-c", MapScript).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v %s", err, out)
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	if err := testfile.Write(filepath.Join(bin, "find"), []byte("#!/bin/sh\necho \"find $*\" >>"+log+"\ncase \"$*\" in *-exec*) [ -z \"${FAIL_EXEC:-}\" ] || { echo vanished >&2; exit 1; } ;; *-print*) [ -z \"${LEFT:-}\" ] || echo /mnt/data/x ;; esac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	runEnv := func(extra []string, env, project string) (string, error) {
		t.Helper()
		os.Remove(log)
		c := exec.Command("sh", "-c", MapScript+`map_owners /mnt/data "$1" "$2"`, "sh", env, project)
		c.Env = append(append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH")), extra...)
		_, err := c.CombinedOutput()
		data, _ := os.ReadFile(log)
		return string(data), err
	}
	run := func(env, project string) string {
		t.Helper()
		got, err := runEnv(nil, env, project)
		if err != nil {
			t.Fatalf("map_owners: %v\n%s", err, got)
		}
		return got
	}
	if got, err := runEnv([]string{"FAIL_EXEC=1"}, "2000000123", "1950000007"); err != nil || strings.Count(got, "-print -quit") != 4 {
		t.Errorf("a pass failing with nothing left: %v\n%s", err, got)
	}
	if got, err := runEnv([]string{"FAIL_EXEC=1", "LEFT=1"}, "2000000123", "1950000007"); err == nil {
		t.Errorf("a pass failing with a file left on a mapped uid passed:\n%s", got)
	}
	if got := run("-", "-"); got != "" {
		t.Errorf("mapped with no uids:\n%s", got)
	}
	got := run("2000000123", "1950000007")
	for _, want := range []string{
		"find /mnt/data -xdev -ignore_readdir_race ( -type f -o -type d -o -type l ) -uid +1999999999 -uid -2100000000 ! -uid 2000000123 -exec chown -h 2000000123 {} +\n",
		"find /mnt/data -xdev -ignore_readdir_race ( -type f -o -type d -o -type l ) -gid +1999999999 -gid -2100000000 ! -gid 2000000123 -exec chgrp -h 2000000123 {} +\n",
		"find /mnt/data -xdev -ignore_readdir_race ( -type f -o -type d -o -type l ) -uid +1899999999 -uid -2000000000 ! -uid 1950000007 -exec chown -h 1950000007 {} +\n",
		"find /mnt/data -xdev -ignore_readdir_race ( -type f -o -type d -o -type l ) -gid +1899999999 -gid -2000000000 ! -gid 1950000007 -exec chgrp -h 1950000007 {} +\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
