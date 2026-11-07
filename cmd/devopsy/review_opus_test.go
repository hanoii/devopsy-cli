package main

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Bug (review round 1, Opus): a paused container is a warning in
// --server-info ("devopsy never pauses: Traefik drops a paused
// container's router") but info in --status, which then exits 0 on a
// site Traefik no longer routes. The two judgments of a container are
// written twice and disagree.
func TestReviewOpusStatusPausedWarns(t *testing.T) {
	t.Parallel()
	if got := containerLevel(remote.StatusContainer{State: "paused"}); got != remote.StatusWarning {
		t.Errorf("a paused container: %s, want %s as --server-info warns it", got, remote.StatusWarning)
	}
}

// The script @<target> --status sends needs status-facts, so an older
// server refuses it in its prelude, naming the feature.
func TestReviewOpusStatusScriptNeedsFacts(t *testing.T) {
	t.Parallel()
	needs := remote.Needs(remote.StatusScript(&remote.Target{Host: "vm1", Path: "shop/prod"}))
	if !slices.Contains(needs, "status-facts") {
		t.Errorf("StatusScript's needs: %v", needs)
	}
}

// @<target> --status --json: the same report as fields, the release's
// id and record, its containers judged, the server's disks, the
// capability's entries, and exit 3 with attention once a container
// restarts; a plain devopsy directory reports its project too.
func TestReviewOpusRemoteStatusJSON(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "capabilities", "status", "show"), "#!/bin/sh\nprintf 'info\\turl\\thttps://%s.example\\n' \"$COMPOSE_PROJECT_NAME\"\n", 0o755)
	s.releaseProd(nil)
	stdout, stderr, code := s.runIO(nil, nil, "@prod", "--status", "--json")
	var r statusReport
	if err := json.Unmarshal(stdout, &r); err != nil || code != 0 || r.Attention {
		t.Fatalf("--status --json (%d, %v):\n%s%s", code, err, stdout, stderr)
	}
	id := strings.TrimPrefix(s.current("prod"), "releases/")
	if r.Target != "devopsy@one:prod" || r.Server == "" || r.Project != "shop-prod" || r.Release == nil || r.Release.ID != id || r.Release.Kept != 1 || r.Release.Mode == "" {
		t.Errorf("the release: %+v %+v\n%s", r, r.Release, stdout)
	}
	if len(r.Disks) == 0 || len(r.Containers) != 2 {
		t.Errorf("disks and containers: %+v\n%s", r, stdout)
	}
	for _, c := range r.Containers {
		if c.Level != remote.StatusInfo {
			t.Errorf("a running container's level: %+v", c)
		}
	}
	keys := []string{}
	for _, e := range r.Entries {
		keys = append(keys, e.Key)
	}
	if !slices.Contains(keys, "release") || !slices.Contains(keys, "disk") || !slices.Contains(keys, "url") {
		t.Errorf("entries' keys: %v\n%s", keys, stdout)
	}

	// A container restarting: attention, exit 3, the same with and
	// without --json.
	write(t, s.log+".shop-prod", "FAKE_DB_RESTARTING=1\n", 0o644)
	stdout, stderr, code = s.runIO(nil, nil, "@prod", "--status", "--json")
	r = statusReport{}
	if err := json.Unmarshal(stdout, &r); err != nil || code != statusWarned || !r.Attention {
		t.Errorf("--status --json, db restarting (%d, %v):\n%s%s", code, err, stdout, stderr)
	}
	if out, code := s.run(nil, "@prod", "--status"); code != statusWarned || !strings.Contains(out, "  db ") {
		t.Errorf("--status, db restarting (%d):\n%s", code, out)
	}
}

// A plain devopsy directory on a server (a .devopsy, no releases): its
// project's containers, the disks, and release none.
func TestReviewOpusRemoteStatusPlain(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	base := filepath.Join(s.root, "shop", "prod")
	write(t, filepath.Join(base, ".devopsy", "compose.yaml"), "services:\n  app:\n    image: x\n", 0o644)
	write(t, filepath.Join(base, ".devopsy", "config.yaml"), "project: shop\n", 0o644)
	out, code := s.run(nil, "@prod", "--status")
	if code != 0 || !strings.Contains(out, "none: a plain devopsy directory") || !strings.Contains(out, "\ndisk ") || !strings.Contains(out, "containers\n") {
		t.Errorf("a plain directory's --status (%d):\n%s", code, out)
	}
}

// Bug (review round 1, Opus): a one-off container (a hook's run --rm)
// is listed under its service's name, unmarked, as if it were the
// service's: a service whose only container is a one-off shows twice,
// "db running" and "db no container", and the JSON cannot tell them
// apart; a one-off left exited on an error (a run without --rm) warns
// as the service failing.
func TestReviewOpusStatusOneOffService(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	out, code := s.run([]string{"FAKE_ABSENT=db", "FAKE_ONEOFF=db"}, "--status")
	if code != 0 || strings.Count(out, "  db ") != 1 {
		t.Errorf("a service with a one-off alone (%d):\n%s", code, out)
	}
}

// Bug (review round 1, Opus): --status --json prints its commands as
// they are, an address holding an IPv6 address in brackets unquoted,
// while the text quotes it (report.Copyable): a script or an agent
// running a fix from the JSON gets zsh's "no matches found".
func TestReviewOpusStatusJSONCopyable(t *testing.T) {
	t.Parallel()
	var r statusReport
	r.addFacts(remote.StatusFacts{
		Project: "shop-prod",
		Disks:   []remote.Disk{{Paths: []string{"docker=/var/lib/docker"}, Type: "xfs", Size: 100e9, Free: 2e9, Used: 98e9}},
	}, "devopsy@[::1]:22:")
	var text, js strings.Builder
	r.print(&text, style{}, "", false)
	if !strings.Contains(text.String(), "$ devopsy @'devopsy@[::1]:22:' --shell-host") {
		t.Fatalf("the text's command is not quoted:\n%s", text.String())
	}
	r.print(&js, style{}, "", true)
	var got statusReport
	if err := json.Unmarshal([]byte(js.String()), &got); err != nil {
		t.Fatal(err)
	}
	var cmds []string
	for _, e := range got.Entries {
		for _, f := range e.Fixes {
			cmds = append(cmds, f.Commands...)
		}
	}
	if !slices.Contains(cmds, "devopsy @'devopsy@[::1]:22:' --shell-host") {
		t.Errorf("the JSON's commands, not copyable: %q", cmds)
	}
}

// Bug (review round 1, Opus): the header's warnings (a failed first
// release, a cut activation, a mark) carry their commands inside the
// line, never in fixes, while the disks' and the status capability's
// are fields: --status --json gives a script no command to run for
// exactly the states that need one.
func TestReviewOpusStatusHeaderFixes(t *testing.T) {
	t.Parallel()
	var r statusReport
	r.addHeader(envState{none: true, failed: "20261107000000"}, &remote.Target{Address: "vm1:prod", Name: "prod", Host: "vm1"})
	if len(r.Entries) != 1 || r.Entries[0].Level != remote.StatusWarning {
		t.Fatalf("entries: %+v", r.Entries)
	}
	if e := r.Entries[0]; len(e.Fixes) == 0 {
		t.Errorf("a failed first release's warning has no fixes, its command in its line: %+v", e)
	}
}
