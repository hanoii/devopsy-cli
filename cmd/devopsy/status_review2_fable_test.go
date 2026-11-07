package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// A target never released says so and shows the server's disks alone
// (--status-facts --no-project), with no containers table and no project;
// a child names its parent, never synced; exit 0. --json has the same:
// containers an empty array, no project, no release, attention false,
// and no null anywhere, for a script reading it.
func TestStatusUnreleasedTarget(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	out, code := s.run(nil, "@dev", "--status")
	if code != 0 || !strings.Contains(out, "\nrelease     none: not released yet\n") || !strings.Contains(out, "\nparent      ") || !strings.Contains(out, "  never synced\n") ||
		!strings.Contains(out, "\ndisks       releases ") || strings.Contains(out, "containers") {
		t.Errorf("--status of an unreleased child (%d):\n%s", code, out)
	}
	stdout, stderr, code := s.runIO(nil, nil, "@dev", "--status", "--json")
	var r statusReport
	if err := json.Unmarshal(stdout, &r); err != nil || code != 0 {
		t.Fatalf("--status --json (%d, %v):\n%s%s", code, err, stdout, stderr)
	}
	keys := []string{}
	for _, e := range r.Entries {
		keys = append(keys, e.Key)
	}
	if r.Attention || r.Project != "" || r.Release != nil || r.Containers == nil || len(r.Containers) != 0 || len(r.Disks) == 0 || strings.Join(keys, " ") != "release parent disks" {
		t.Errorf("--status --json of an unreleased child: %+v", r)
	}
	if js := string(stdout); !strings.Contains(js, `"containers": []`) || strings.Contains(js, "null") || strings.Contains(js, `"project"`) {
		t.Errorf("the JSON's shape:\n%s", js)
	}
}

// A status capability that prints a line of another shape and then fails
// still has its good lines shown, and gets a warning for each: the lines
// named, quoted, and the failure with its exit status; exit 3. --json
// carries both as capability entries at warning.
func TestStatusCapabilityTrouble(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "capabilities", "status", "show"), "#!/bin/sh\nprintf 'info\\turl\\thttps://shop.example\\n'\necho 'url https://old.example'\nexit 1\n", 0o755)
	out, code := s.run(nil, "--status")
	if code != statusWarned || !strings.Contains(out, "\nurl         https://shop.example\n") ||
		!strings.Contains(out, "\ncapability  the status capability printed lines that are not "+statusShowFormat+":\n            \"url https://old.example\"\n            the status capability failed: exit status 1\n") {
		t.Errorf("--status with a capability in trouble (%d):\n%s", code, out)
	}
	stdout, stderr, code := s.runIO(nil, nil, "--status", "--json")
	var r statusReport
	if err := json.Unmarshal(stdout, &r); err != nil || code != statusWarned {
		t.Fatalf("--status --json (%d, %v):\n%s%s", code, err, stdout, stderr)
	}
	var trouble []remote.StatusEntry
	for _, e := range r.Entries {
		if e.Key == "capability" {
			trouble = append(trouble, e)
		}
	}
	if !r.Attention || len(trouble) != 2 || trouble[0].Level != remote.StatusWarning || trouble[1].Level != remote.StatusWarning ||
		len(trouble[0].Lines) != 2 || trouble[0].Lines[1] != `"url https://old.example"` || !strings.HasPrefix(trouble[1].Lines[0], "the status capability failed: ") {
		t.Errorf("the capability's trouble in JSON: %+v", r.Entries)
	}
}

// A local --status --json of a project with nothing to add has arrays,
// never null, for entries and containers, and none of a target's fields.
func TestStatusLocalJSONShape(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	stdout, stderr, code := s.runIO(nil, nil, "--status", "--json")
	if code != 0 {
		t.Fatalf("--status --json (%d):\n%s%s", code, stdout, stderr)
	}
	js := string(stdout)
	if !strings.Contains(js, `"project": "`) || strings.Contains(js, "null") || !strings.Contains(js, `"entries": []`) ||
		strings.Contains(js, `"target"`) || strings.Contains(js, `"server"`) || strings.Contains(js, `"release"`) || strings.Contains(js, `"disks"`) {
		t.Errorf("the local JSON's shape:\n%s", js)
	}
}

// A server whose devopsy lacks status-facts refuses @<target> --status in
// its prelude, naming the feature and the upgrade, before any fact is
// gathered: the operator sees no half report and exits 1, as every script
// an older server cannot run.
func TestStatusRefusedByOlderServer(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	s.releaseProd(nil)
	bin := filepath.Dir(strings.TrimPrefix(s.env[0], "DEVOPSY_SSH_COMMAND="))
	lacks := filepath.Join(t.TempDir(), "lacks")
	write(t, filepath.Join(lacks, "devopsy"), "#!/bin/sh\nif [ \"$1\" = --release-settings ]; then "+binary+" --release-settings | sed 's/ status-facts / /'; exit; fi\nexec "+binary+" \"$@\"\n", 0o755)
	write(t, filepath.Join(lacks, "lacksssh"), "#!/bin/sh\nPATH="+lacks+":$PATH exec "+filepath.Join(bin, "fakessh")+" \"$@\"\n", 0o755)
	out, code := s.run([]string{"DEVOPSY_SSH_COMMAND=" + filepath.Join(lacks, "lacksssh")}, "@prod", "--status")
	if code != 1 || !strings.Contains(out, "the server's devopsy lacks what this needs (status-facts): upgrade it (devopsy @devopsy@one: --upgrade)") || strings.Contains(out, "containers") || strings.Contains(out, "cannot read") {
		t.Errorf("--status against a server lacking status-facts (%d):\n%s", code, out)
	}
}
