package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/images"
)

// profileFake is the sync fake docker with a worker whose profile is
// honored as compose honors it: compose config lists a service of a
// profile only with --profile * (or COMPOSE_PROFILES naming it), and the
// worker mounts an entry of mnt (work) so that a copy carries its data.
// FAKE_WORKER_VERSION is its devopsy.data.version (any by default).
func profileFake(t *testing.T, s *syncSetup) {
	t.Helper()
	fake := syncFakeDocker
	from := `,"depends_on":{"db":{"condition":"service_started"}}}' "$(lab "$FAKE_WORKER")" "$wprofile")`
	to := `,"depends_on":{"db":{"condition":"service_started"}},"volumes":[{"type":"bind","source":"%s/mnt/work"}]}' "$(lab "$FAKE_WORKER" | sed "s/\"devopsy.data.version\":\"any\"/\"devopsy.data.version\":\"${FAKE_WORKER_VERSION:-any}\"/")" "$wprofile" "$dir")`
	if !strings.Contains(fake, from) {
		t.Fatal("the fake's worker changed")
	}
	fake = strings.Replace(fake, from, to, 1)
	from = "  *\" config --format json\")\n"
	to = from + "    case \" $* \" in *\" --profile * \"*) ;; *) [ -z \"${FAKE_WORKER_PROFILE:-}\" ] || worker= ;; esac\n"
	if !strings.Contains(fake, from) {
		t.Fatal("the fake's config changed")
	}
	fake = strings.Replace(fake, from, to, 1)
	s.fake("docker", fake)
}

// Bug: a copy writes every profile's data (the push's and a take's set
// come from compose config --profile '*', cli.SyncServices), but the
// window sent with it comes from images.ListCompose, compose config
// without profiles: a data service in a profile that is off is copied
// with no window. The receiving side then keeps its own stale window
// (a sync) or lands it as data of no known version (a restore, the
// manifest naming it), so a later release that turns the profile on is
// judged against the wrong window: a lower label allowed on newer data.
// --export shares the same function (parentData): here prod's worker
// (profile tools, off) ran 12 on its data, and the export of it carries
// no history at all, where app's export carries its line.
func TestExportProfileOffCarriesNoWindow(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	profileFake(t, s)
	env := []string{"FAKE_WORKER=mnt", "FAKE_WORKER_PROFILE=tools", "FAKE_WORKER_VERSION=12"}
	s.releaseProd(env)
	base := filepath.Join(s.root, "shop", "prod")
	write(t, filepath.Join(base, images.HistoryFile), "1\tworker\tran\tbusybox:12\tsha256:w12\tbusybox@sha256:w12\t12\t12\n", 0o644)
	write(t, filepath.Join(s.mnt("prod"), "work", "file.txt"), "from prod\n", 0o644)
	out, errs, code := s.runIO(env, nil, "@prod", "--export", "worker")
	if code != 0 {
		t.Fatalf("@prod --export worker (%d):\n%s", code, errs)
	}
	h, err := tar.NewReader(bytes.NewReader(out)).Next()
	if err != nil {
		t.Fatalf("the export's first header: %v", err)
	}
	if h.Typeflag != tar.TypeXGlobalHeader || !strings.HasPrefix(h.PAXRecords[images.ExportKey], "data\tworker\nran\t1\tworker\tran\tbusybox:12\t") {
		t.Errorf("the export of a data service in a profile that is off carries no window of its data (its history says 12): header %+v, record %q", h, h.PAXRecords[images.ExportKey])
	}
	if _, err := os.Stat(filepath.Join(s.log+".x", "work", "file.txt")); err != nil {
		t.Errorf("the export did not carry the worker's entry: %v", err)
	}
	// The control: the same worker in no profile exports its window.
	s.logs()
	env = []string{"FAKE_WORKER=mnt", "FAKE_WORKER_VERSION=12"}
	out, errs, code = s.runIO(env, nil, "@prod", "--export", "worker")
	if code != 0 {
		t.Fatalf("@prod --export worker, in no profile (%d):\n%s", code, errs)
	}
	if h, err := tar.NewReader(bytes.NewReader(out)).Next(); err != nil || h.Typeflag != tar.TypeXGlobalHeader || !strings.HasPrefix(h.PAXRecords[images.ExportKey], "data\tworker\nran\t1\tworker\tran\tbusybox:12\t") {
		t.Fatalf("the control, the worker in no profile (%v): %+v", err, h)
	}
}
