package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// Review of feat/reflink-syncs: each test here names the bug it proves
// and fails on the branch as it is.

// reviewLine is the first line of log starting with prefix and holding
// sub, with its index, or -1.
func reviewLine(log, prefix, sub string) (string, int) {
	at := 0
	for _, l := range strings.Split(log, "\n") {
		if strings.HasPrefix(l, prefix) && strings.Contains(l, sub) {
			return l, at
		}
		at += len(l) + 1
	}
	return "", -1
}

// Bug: with the clone, a custom service NOT in the set is exported after
// the parent's set is started again (runSyncPush: restore(), then
// copyStreams), so a service labeled devopsy.data.stop=always runs
// while another service's data is exported. The README's table says an
// `always` service is down while "another service's data exported", and
// the label's text "while any data is exported or imported". Here: db is
// custom with stop=never (a hot dump, the recipe), app is volumes with
// stop=always (its files must match the dump): the clone takes app's
// files, app is started, and db is dumped while app writes it, so the
// dump no longer matches the files the child gets, which is the one
// thing `always` buys. Before the branch the dump ran with app down.
func TestReviewFableAlwaysDownDuringStreamExport(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	caps := filepath.Join(s.dot, "capabilities", "data")
	write(t, filepath.Join(caps, "export"), "#!/bin/sh\necho \"dump of $1\"\n", 0o755)
	write(t, filepath.Join(caps, "import"), "#!/bin/sh\ncat >/dev/null\n", 0o755)
	s.releaseProd(nil)
	if out, code := s.run(nil, "@dev", "--release-no-sync"); code != 0 {
		t.Fatalf("dev (%d):\n%s", code, out)
	}
	s.logs()
	s.labels("FAKE_APP=mnt+always\nFAKE_DB=stream")
	out, code := s.run([]string{"FAKE_REFLINK=1"}, "@dev", "--sync", "--yes")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(out, "the data clones by reflinks") {
		t.Fatalf("the sync (%d):\n%s", code, out)
	}
	// app (always) is stopped, cloned... and must still be down when db
	// is exported: the export before the start.
	if _, at := reviewLine(docker, "shop-prod ", " stop app"); at < 0 {
		t.Fatalf("app (devopsy.data.stop=always) not stopped on the parent:\n%s", docker)
	}
	export, start := strings.Index(out, "exporting db"), strings.Index(out, "starting app")
	if export < 0 || start < 0 {
		t.Fatalf("no export or no start:\n%s", out)
	}
	if start < export {
		t.Errorf("app (devopsy.data.stop=always) was started before db was exported: the dump does not match the files the child gets:\n%s", out)
	}
}

// Bug: the reflink check and the space check run while the parent's
// services are stopped (syncClone, called after stopServices: a docker
// run for `reflinks`, then duData's `du -sk` over every entry and
// volume, then the clone), so the downtime the clone was meant to make
// "seconds whatever the size" includes a du over the whole data set,
// minutes cold on a large tree, for a warning. A snapshot's take runs
// both before anything stops (snapshotCheck, takeCopy); the push should
// too: only the clone belongs between the stop and the start.
func TestReviewFableOnlyTheCloneWhileStopped(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	out, code := s.run([]string{"FAKE_REFLINK=1"}, "@dev", "--release")
	docker, _ := s.logs()
	if code != 0 || !strings.Contains(out, "the data clones by reflinks") {
		t.Fatalf("first release (%d):\n%s", code, out)
	}
	_, stop := reviewLine(docker, "shop-prod ", " stop db")
	_, start := reviewLine(docker, "shop-prod ", " start db")
	if stop < 0 || start < stop {
		t.Fatalf("no stop and start of db on the parent:\n%s", docker)
	}
	down := docker[stop:start]
	if !strings.Contains(down, "-clone ") {
		t.Fatalf("the clone not made while db was stopped:\n%s", down)
	}
	if strings.Contains(down, " du -sk ") {
		t.Errorf("du over the whole data set ran while the parent's services were stopped (downtime for a warning):\n%s", down)
	}
	if strings.Contains(down, "-reflink ") {
		t.Errorf("the reflink check ran while the parent's services were stopped, not before:\n%s", down)
	}
}
