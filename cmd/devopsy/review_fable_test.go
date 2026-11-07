package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Review (Fable), the feature gate: the status script runs the hidden
// --status-facts on the server, so its needs name it, and an older
// server refuses in the prelude rather than failing at the facts.
func TestReviewStatusScriptNeedsFacts(t *testing.T) {
	t.Parallel()
	script := remote.StatusScript(&remote.Target{Host: "vm1", Name: "prod", Path: "shop/prod"})
	if needs := remote.Needs(script); !slices.Contains(needs, "status-facts") {
		t.Errorf("StatusScript needs %v, not status-facts: an older server would run it and fail at the facts", needs)
	}
	// Both ways to the facts, the release's and the disks alone, are
	// exec'd: nothing of the script runs after them.
	if strings.Count(script, "exec devopsy '--status-facts'") != 3 {
		t.Errorf("the facts are not exec'd three times (plain, current, no project):\n%s", script)
	}
}

// Review (Fable): with a target, the capability's warning and its fix
// show under the release and the disks, every key aligned on the
// longest, the services running given to show, exit 3; --json carries
// the target, the server, the release, the disks, the containers with
// their levels and attention, with the same exit code.
func TestReviewStatusTarget(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "capabilities", "status", "show"), `#!/bin/sh
printf 'info\turl\thttps://%s.example\n' "$COMPOSE_PROJECT_NAME"
printf 'info\trunning\t%s\n' "$DEVOPSY_STATUS_RUNNING"
[ -z "${FAKE_SHOW:-}" ] || printf "$FAKE_SHOW"
`, 0o755)
	s.releaseProd(nil)
	id := s.current("prod")[len("releases/"):]
	warn := []string{"FAKE_SHOW=warning\tmaintenance\ton\tdevopsy maintenance off\n"}
	out, code := s.run(warn, "@prod", "--status")
	if code != statusWarned || !strings.Contains(out, "\nrelease      "+id) || !strings.Contains(out, "\ndisk         releases ") ||
		!strings.Contains(out, "\ncontainers\n  app ") || !strings.Contains(out, "\nurl          https://shop-prod.example\n") ||
		!strings.Contains(out, "\nrunning      app db\n") || !strings.Contains(out, "\nmaintenance  on\n             $ devopsy maintenance off\n") {
		t.Errorf("--status with a warning on the server (%d):\n%s", code, out)
	}
	stdout, stderr, code := s.runIO(warn, nil, "@prod", "--status", "--json")
	var r statusReport
	if err := json.Unmarshal(stdout, &r); err != nil {
		t.Fatalf("--status --json (%d, %v):\n%s%s", code, err, stdout, stderr)
	}
	if code != statusWarned || !r.Attention || r.Target != "devopsy@one:prod" || r.Server != "devopsy@one:shop/prod" || r.Project != "shop-prod" ||
		r.Release == nil || r.Release.ID != id || r.Release.Kept != 1 || r.Release.Mode != "image" || len(r.Disks) == 0 || len(r.Containers) != 2 {
		t.Errorf("--status --json (%d): %+v\n%s", code, r, stderr)
	}
	for _, c := range r.Containers {
		if c.Level != remote.StatusInfo || c.State != "running" {
			t.Errorf("a running container's level: %+v", c)
		}
	}
	keys := map[string]remote.StatusEntry{}
	for _, e := range r.Entries {
		keys[e.Key] = e
	}
	if e := keys["maintenance"]; e.Level != remote.StatusWarning || len(e.Fixes) != 1 || strings.Join(e.Fixes[0].Commands, " ") != "devopsy maintenance off" {
		t.Errorf("the capability's warning in JSON: %+v", e)
	}
	if e, ok := keys["disk"]; !ok || e.Level != remote.StatusInfo {
		t.Errorf("the disks in JSON: %+v (%v)", e, ok)
	}
	if e := keys["release"]; e.Level != remote.StatusInfo || len(e.Lines) != 1 || !strings.HasPrefix(e.Lines[0], id) {
		t.Errorf("the release in JSON: %+v", e)
	}
	// Nothing but the JSON on stdout: a script reads it whole.
	if !bytes.HasPrefix(bytes.TrimSpace(stdout), []byte("{")) || !bytes.HasSuffix(bytes.TrimSpace(stdout), []byte("}")) {
		t.Errorf("stdout around the JSON:\n%s", stdout)
	}
}

// Review (Fable): a plain devopsy directory on the server (no releases/)
// says so, and its facts come from that directory: the containers, the
// capability's lines and the disks.
func TestReviewStatusPlainDirectory(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "capabilities", "status", "show"), "#!/bin/sh\nprintf 'info\\turl\\thttps://%s.example\\n' \"$COMPOSE_PROJECT_NAME\"\n", 0o755)
	base := filepath.Join(s.root, "shop", "prod")
	if err := os.CopyFS(base, os.DirFS(s.project)); err != nil {
		t.Fatal(err)
	}
	out, code := s.run(nil, "@prod", "--status")
	if code != 0 || !strings.Contains(out, "\nrelease     none: a plain devopsy directory\n") || !strings.Contains(out, "\ndisk        releases ") ||
		!strings.Contains(out, "\ncontainers\n  app ") || !strings.Contains(out, "\nurl         https://") {
		t.Errorf("--status of a plain directory (%d):\n%s", code, out)
	}
}

// Review (Fable): a one-off container (a devopsy run) is its service's
// by name only. statusFacts skips it for the services with none and for
// DEVOPSY_STATUS_RUNNING, yet lists it among the containers as its
// service, so the table shows "app running" twice and --json's
// containers carry no name and no one-off mark: a reader cannot tell
// the service's own container from a run's.
// BUG: a one-off is listed as a second container of its service, told
// apart by nothing (cmd/devopsy/status.go, statusFacts, and
// remote.StatusContainer, which has no Name or OneOff).
func TestReviewStatusOneOffToldApart(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	stdout, stderr, code := s.runIO([]string{"FAKE_ONEOFF=app"}, nil, "--status", "--json")
	var r statusReport
	if err := json.Unmarshal(stdout, &r); err != nil || code != 0 {
		t.Fatalf("--status --json (%d, %v):\n%s%s", code, err, stdout, stderr)
	}
	apps := 0
	for _, c := range r.Containers {
		if c.Service == "app" {
			apps++
		}
	}
	if apps != 1 {
		t.Errorf("a one-off of app is a second app, with nothing telling it apart: %+v", r.Containers)
	}
	if out, _ := s.run([]string{"FAKE_ONEOFF=app"}, "--status"); strings.Count(out, "\n  app ") != 1 {
		t.Errorf("the table lists app twice:\n%s", out)
	}
}

// Review (Fable): the header's warnings (a cut activation, a mark, a
// sync that did not complete, a failed first release) name their
// commands inside the line, while the disks' and the capability's are
// StatusFix entries: --json gives fixes for the latter and none for the
// former, and the formatter shows "$ <command>" lines only for the
// latter.
// BUG: addHeader glues remote.ImportFixes, RestoreFixes and
// Activating.Cut's commands into the entry's line instead of its Fixes
// (cmd/devopsy/status.go, addHeader).
func TestReviewStatusHeaderFixesAreFields(t *testing.T) {
	t.Parallel()
	tgt := &remote.Target{Host: "vm1", Name: "prod", Path: "shop/prod"}
	for _, c := range []struct{ name, header string }{
		{"a sync cut", "release\t20261107000000\t{\"mode\":\"image\",\"time\":\"2026-11-07T00:00:00Z\",\"by\":\"me\"}\nkept\t1\npartial\t\n"},
		{"a failed first release", "none\t20261107000000\n"},
	} {
		var r statusReport
		r.addHeader(parseStatus(c.header), tgt)
		warned := false
		for _, e := range r.Entries {
			if e.Level != remote.StatusWarning {
				continue
			}
			warned = true
			if len(e.Fixes) == 0 {
				t.Errorf("%s: the warning's commands are in its line, not its fixes: %+v", c.name, e)
			}
		}
		if !warned {
			t.Errorf("%s: no warning: %+v", c.name, r.Entries)
		}
	}
}

// Review (Fable), the layout's colors: the heading cyan, a warning's
// lines yellow whole (padded first, so the columns hold), a command
// bold, a running container green, one needing attention yellow, a
// service with no container dim, and info lines as they are.
func TestReviewStatusPrintColors(t *testing.T) {
	t.Parallel()
	r := statusReport{}
	r.add(remote.StatusWarning, "release", "the release of 1 past its switch: devopsy @vm1:prod --release releases it again")
	r.addFacts(remote.StatusFacts{
		Project: "shop-prod",
		Containers: []remote.StatusContainer{
			{Service: "app", State: "running", Health: "healthy", Status: "Up 1 hour (healthy)"},
			{Service: "db", State: "exited", ExitCode: 1},
			{Service: "worker", State: remote.StatusNoContainer},
		},
		Entries: []remote.StatusEntry{
			{Level: remote.StatusInfo, Key: "url", Lines: []string{"https://shop.example"}},
			{Level: remote.StatusWarning, Key: "maintenance", Lines: []string{"on"}, Fixes: []remote.StatusFix{{Text: "bring it back:", Commands: []string{"devopsy maintenance off"}}}},
		},
	}, "vm1:")
	var out bytes.Buffer
	code := r.print(&out, style{on: true}, "@vm1:prod (vm1:shop/prod)", false)
	want := "\033[0;36m@vm1:prod (vm1:shop/prod)\033[0m\n" +
		"\033[0;33mrelease      the release of 1 past its switch: devopsy @vm1:prod --release releases it again\033[0m\n" +
		"containers\n" +
		"  app      \033[0;32mrunning\033[0m  up 1 hour (healthy)\n" +
		"  db       \033[0;33mexited\033[0m\n" +
		"  worker   \033[2mno container\033[0m\n" +
		"url          https://shop.example\n" +
		"\033[0;33mmaintenance  on\033[0m\n" +
		"\033[0;33m             bring it back:\033[0m\n" +
		"\033[1m             $ devopsy maintenance off\033[0m\n"
	if out.String() != want || code != statusWarned {
		t.Errorf("--status in color (%d):\n%q\nwant:\n%q", code, out.String(), want)
	}
	// --json never colors, and exits the same.
	out.Reset()
	if code := r.print(&out, style{on: true}, "", true); code != statusWarned || bytes.Contains(out.Bytes(), []byte("\033")) {
		t.Errorf("--json (%d):\n%s", code, out.String())
	}
}
