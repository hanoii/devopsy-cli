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
	"github.com/hanoii/devopsy-cli/internal/report"
)

// The status capability's lines: consecutive lines of one key are one
// entry, at the highest of their levels, with every fix they give once;
// a key coming back after another is a second entry; a line of any other
// shape is bad, and empty lines are skipped.
func TestParseStatusShow(t *testing.T) {
	t.Parallel()
	out := "info\turl\thttps://shop.example\n" +
		"info\turl\thttps://www.shop.example\n" +
		"\n" +
		"info\tcron\tlast run failed\tdevopsy drush cron\r\n" +
		"warning\tcron\tdatabase is locked\tdevopsy drush cron\n" +
		"info\turl\thttps://shop.example/admin\n" +
		"url https://shop.example\n" +
		"info\turl\n" +
		"error\turl\tx\n" +
		"info\t\tx\n" +
		"info\ta\tb\tc\td\n"
	entries, bad := parseStatusShow(out)
	if len(entries) != 3 {
		t.Fatalf("entries: %+v", entries)
	}
	if e := entries[0]; e.Key != "url" || e.Level != remote.StatusInfo || strings.Join(e.Lines, "|") != "https://shop.example|https://www.shop.example" || e.Fixes != nil {
		t.Errorf("a key over two lines: %+v", e)
	}
	if e := entries[1]; e.Key != "cron" || e.Level != remote.StatusWarning || strings.Join(e.Lines, "|") != "last run failed|database is locked" || len(e.Fixes) != 1 || strings.Join(e.Fixes[0].Cmds, "|") != "devopsy drush cron" {
		t.Errorf("a warning on its second line, its fix given twice: %+v", e)
	}
	if e := entries[2]; e.Key != "url" || strings.Join(e.Lines, "|") != "https://shop.example/admin" {
		t.Errorf("a key coming back: %+v", e)
	}
	if strings.Join(bad, "|") != "url https://shop.example|info\turl|error\turl\tx|info\t\tx|info\ta\tb\tc\td" {
		t.Errorf("bad lines: %q", bad)
	}
}

// A container needs attention when it restarts, is paused, dead or
// unhealthy, or exited on an error of its own; a stop's exit codes (0,
// SIGKILL's 137, SIGTERM's 143) are none. --server-info judges the same
// (containerTrouble).
func TestContainerLevel(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		c    remote.StatusContainer
		want string
	}{
		{remote.StatusContainer{State: "running"}, remote.StatusInfo},
		{remote.StatusContainer{State: "running", Health: "unhealthy"}, remote.StatusWarning},
		{remote.StatusContainer{State: "restarting"}, remote.StatusWarning},
		{remote.StatusContainer{State: "dead"}, remote.StatusWarning},
		// devopsy never pauses: Traefik drops a paused container's router.
		{remote.StatusContainer{State: "paused"}, remote.StatusWarning},
		{remote.StatusContainer{State: "exited"}, remote.StatusInfo},
		{remote.StatusContainer{State: "exited", ExitCode: 137}, remote.StatusInfo},
		{remote.StatusContainer{State: "exited", ExitCode: 143}, remote.StatusInfo},
		{remote.StatusContainer{State: "exited", ExitCode: 1}, remote.StatusWarning},
		{remote.StatusContainer{State: remote.StatusNoContainer}, remote.StatusInfo},
	} {
		if got := containerLevel(c.c); got != c.want {
			t.Errorf("%+v: %s, want %s", c.c, got, c.want)
		}
	}
}

// --status's layout: keys aligned on the longest, a key said once over
// its entry's lines and its next entries', fixes under their entry, a
// container's health only when compose's status does not already end in
// it, a disk past the thresholds warned with its fixes (exit 3).
func TestStatusPrint(t *testing.T) {
	t.Parallel()
	r := statusReport{}
	r.add(remote.StatusInfo, "release", "20261107000000  just now")
	r.addFacts(remote.StatusFacts{
		Project: "shop-prod",
		Containers: []remote.StatusContainer{
			{Service: "app", State: "running", Health: "healthy", Status: "Up 3 hours (healthy)"},
			{Service: "db", State: "running", Health: "healthy"},
			{Service: "worker", State: remote.StatusNoContainer},
		},
		Entries: []remote.StatusEntry{
			{Level: remote.StatusInfo, Key: "url", Lines: []string{"https://shop.example", "https://www.shop.example"}},
			{Level: remote.StatusWarning, Key: "maintenance", Lines: []string{"on"}, Fixes: []report.Remedy{report.Fix("", "devopsy maintenance off")}},
		},
		Disks: []remote.Disk{
			{Paths: []string{"releases=/srv", "/=/"}, Type: "ext4", Size: 100e9, Free: 50e9, Used: 50e9},
			{Paths: []string{"docker=/var/lib/docker"}, Type: "xfs", Size: 100e9, Free: 2e9, Used: 98e9},
		},
	}, "vm1:")
	var out bytes.Buffer
	code := r.print(&out, style{}, "@vm1:prod (vm1:shop/prod)", false)
	want := `@vm1:prod (vm1:shop/prod)
release      20261107000000  just now
disks        releases /srv, /: ext4, 50.0 GB free of 100.0 GB (50% used)
             docker /var/lib/docker: xfs, 2.0 GB free of 100.0 GB (98% used)
             docker /var/lib/docker is 98% full, 2.0 GB free: builds, pulls, uploads, logs and databases fail once it is full
             to fix it:
             see what Docker takes, then empty its build cache, on the server:
             $ devopsy @vm1: --shell-host
             $ docker system df
             $ docker builder prune
containers
  app      running  up 3 hours (healthy)
  db       running  healthy
  worker   no container
url          https://shop.example
             https://www.shop.example
maintenance  on
             to fix it:
             $ devopsy maintenance off
`
	if out.String() != want || code != statusWarned {
		t.Errorf("--status (%d):\n%s\nwant:\n%s", code, out.String(), want)
	}
}

// The script @<target> --status sends names status-facts among its
// needs, so an older server refuses it in its prelude rather than failing
// at the facts; each way to the facts (a plain directory, the current
// release, the disks alone) is exec'd, so nothing of the script runs
// after them.
func TestStatusScriptNeedsFacts(t *testing.T) {
	t.Parallel()
	script := remote.StatusScript(&remote.Target{Host: "vm1", Name: "prod", Path: "shop/prod"})
	if needs := remote.Needs(script); !slices.Contains(needs, "status-facts") {
		t.Errorf("StatusScript needs %v, not status-facts", needs)
	}
	if strings.Count(script, "exec devopsy '--status-facts'") != 3 {
		t.Errorf("the facts are not exec'd three times (plain, current, no project):\n%s", script)
	}
}

// With a target, the capability's warning and its fix show under the
// release and the disks, every key aligned on the longest, the services
// running given to show, exit 3; --json carries the target, the server,
// the release, the disks, the containers with their levels and
// attention, with the same exit code, and nothing else on stdout.
func TestStatusTarget(t *testing.T) {
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
	if code != statusWarned || !strings.Contains(out, "\nrelease      "+id) || !strings.Contains(out, "\ndisks        releases ") ||
		!strings.Contains(out, "\ncontainers\n  app ") || !strings.Contains(out, "\nurl          https://shop-prod.example\n") ||
		!strings.Contains(out, "\nrunning      app db\n") || !strings.Contains(out, "\nmaintenance  on\n             to fix it:\n             $ devopsy maintenance off\n") {
		t.Errorf("--status with a warning on the server (%d):\n%s", code, out)
	}
	stdout, stderr, code := s.runIO(warn, nil, "@prod", "--status", "--json")
	var r statusReport
	if err := json.Unmarshal(stdout, &r); err != nil {
		t.Fatalf("--status --json (%d, %v):\n%s%s", code, err, stdout, stderr)
	}
	if code != statusWarned || !r.Attention || r.Target != "devopsy@one:prod" || r.Server != "devopsy@one:shop/prod" || r.Project != "shop-prod" ||
		r.Release == nil || r.Release.ID != id || r.Release.Kept != 1 || r.Release.Record == nil || r.Release.Mode != "image" || len(r.Disks) == 0 || len(r.Containers) != 2 {
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
	if e := keys["maintenance"]; e.Level != remote.StatusWarning || len(e.Fixes) != 1 || strings.Join(e.Fixes[0].Cmds, " ") != "devopsy maintenance off" {
		t.Errorf("the capability's warning in JSON: %+v", e)
	}
	if e, ok := keys["disks"]; !ok || e.Level != remote.StatusInfo {
		t.Errorf("the disks in JSON: %+v (%v)", e, ok)
	}
	if e := keys["release"]; e.Level != remote.StatusInfo || len(e.Lines) != 1 || !strings.HasPrefix(e.Lines[0], id) {
		t.Errorf("the release in JSON: %+v", e)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(stdout), []byte("{")) || !bytes.HasSuffix(bytes.TrimSpace(stdout), []byte("}")) {
		t.Errorf("stdout around the JSON:\n%s", stdout)
	}
}

// A plain devopsy directory on the server (no releases/) says so, and its
// facts come from that directory: the containers, the capability's lines
// and the disks.
func TestStatusPlainDirectory(t *testing.T) {
	t.Parallel()
	s := newSyncSetup(t)
	write(t, filepath.Join(s.dot, "capabilities", "status", "show"), "#!/bin/sh\nprintf 'info\\turl\\thttps://%s.example\\n' \"$COMPOSE_PROJECT_NAME\"\n", 0o755)
	base := filepath.Join(s.root, "shop", "prod")
	if err := os.CopyFS(base, os.DirFS(s.project)); err != nil {
		t.Fatal(err)
	}
	out, code := s.run(nil, "@prod", "--status")
	if code != 0 || !strings.Contains(out, "\nrelease     none: a plain devopsy directory\n") || !strings.Contains(out, "\ndisks       releases ") ||
		!strings.Contains(out, "\ncontainers\n  app ") || !strings.Contains(out, "\nurl         https://") {
		t.Errorf("--status of a plain directory (%d):\n%s", code, out)
	}
}

// A one-off container (a devopsy run, an import's run --rm) is no
// service's: never listed, so a service has one line and one entry in
// --json, and a service whose only container is a one-off has none.
func TestStatusLeavesOneOffsOut(t *testing.T) {
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
		t.Errorf("a one-off of app listed as a second app: %+v", r.Containers)
	}
	if out, code := s.run([]string{"FAKE_ABSENT=db", "FAKE_ONEOFF=db"}, "--status"); code != 0 || strings.Count(out, "  db ") != 1 || !strings.Contains(out, "  db       no container\n") {
		t.Errorf("a service with a one-off alone (%d):\n%s", code, out)
	}
}

// --status --json's lines and commands are copyable as the text's are:
// an address holding an IPv6 address in brackets quoted, else zsh takes
// it for a glob.
func TestStatusJSONCopyable(t *testing.T) {
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
			cmds = append(cmds, f.Cmds...)
		}
	}
	if !slices.Contains(cmds, "devopsy @'devopsy@[::1]:22:' --shell-host") {
		t.Errorf("the JSON's commands, not copyable: %q", cmds)
	}
}

// The header's warnings carry their commands as fixes, each a text and
// its command, as the disks' and the capability's do: --json gives a
// script the command to run, and the text shows it on a $ line.
func TestStatusHeaderFixes(t *testing.T) {
	t.Parallel()
	tgt := &remote.Target{Address: "vm1:prod", Host: "vm1", Name: "prod", Path: "shop/prod"}
	for _, c := range []struct{ name, header, cmd string }{
		{"a sync cut", "release\t20261107000000\t{\"mode\":\"image\",\"time\":\"2026-11-07T00:00:00Z\",\"by\":\"me\"}\nkept\t1\npartial\t\n", "--sync"},
		{"a failed first release", "none\t20261107000000\n", "--log"},
		{"a cut rollback", "release\t20261107000000\t{}\ncut\t20261107000000\trollback\t\t\n", "--rollback"},
		{"an import that did not complete", "release\t20261107000000\t{}\npartial\t" + remote.IncompleteLine("2026-11-07 00:00:00", "db", remote.IncompleteImport, "it was cut") + "\n", "--import db --resolved"},
	} {
		var r statusReport
		r.addHeader(parseStatus(c.header), tgt)
		var warned []remote.StatusEntry
		for _, e := range r.Entries {
			if e.Level == remote.StatusWarning {
				warned = append(warned, e)
			}
		}
		if len(warned) != 1 {
			t.Errorf("%s: warnings %+v", c.name, r.Entries)
			continue
		}
		e := warned[0]
		found := false
		for _, f := range e.Fixes {
			for _, cmd := range f.Cmds {
				found = found || strings.HasSuffix(cmd, " "+c.cmd)
			}
		}
		if !found || strings.Contains(e.Lines[0], "devopsy @") {
			t.Errorf("%s: no fix running %s, or a command in the line: %+v", c.name, c.cmd, e)
		}
	}
}

// The layout's colors: the heading cyan, a warning's lines yellow whole
// (padded first, so the columns hold), a command bold, a running
// container green, one needing attention yellow, a service with no
// container dim, info lines as they are; --json never colors and exits
// the same.
func TestStatusPrintColors(t *testing.T) {
	t.Parallel()
	r := statusReport{}
	r.add(remote.StatusWarning, "release", "the release of 1 was cut off")
	r.addFacts(remote.StatusFacts{
		Project: "shop-prod",
		Containers: []remote.StatusContainer{
			{Service: "app", State: "running", Health: "healthy", Status: "Up 1 hour (healthy)"},
			{Service: "db", State: "exited", ExitCode: 1},
			{Service: "worker", State: remote.StatusNoContainer},
		},
		Entries: []remote.StatusEntry{
			{Level: remote.StatusInfo, Key: "url", Lines: []string{"https://shop.example"}},
			{Level: remote.StatusWarning, Key: "maintenance", Lines: []string{"on"}, Fixes: []report.Remedy{report.Fix("bring it back:", "devopsy maintenance off")}},
		},
	}, "vm1:")
	var out bytes.Buffer
	code := r.print(&out, style{on: true}, "@vm1:prod (vm1:shop/prod)", false)
	want := "\033[0;36m@vm1:prod (vm1:shop/prod)\033[0m\n" +
		"\033[0;33mrelease      the release of 1 was cut off\033[0m\n" +
		"containers\n" +
		"  app      \033[0;32mrunning\033[0m  up 1 hour (healthy)\n" +
		"  db       \033[0;33mexited\033[0m\n" +
		"  worker   \033[2mno container\033[0m\n" +
		"url          https://shop.example\n" +
		"\033[0;33mmaintenance  on\033[0m\n" +
		"\033[0;33m             to fix it:\033[0m\n" +
		"\033[0;33m             bring it back:\033[0m\n" +
		"             \033[1m$ devopsy maintenance off\033[0m\n"
	if out.String() != want || code != statusWarned {
		t.Errorf("--status in color (%d):\n%q\nwant:\n%q", code, out.String(), want)
	}
	out.Reset()
	if code := r.print(&out, style{on: true}, "", true); code != statusWarned || bytes.Contains(out.Bytes(), []byte("\033")) {
		t.Errorf("--json (%d):\n%s", code, out.String())
	}
}

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

// The header's fixes to release, roll back, sync or restore name the
// target as a release's hint does (remote.HintAddress), as every hint
// does: a user-level alias without source: refuses a release or a
// rollback, so a fix naming it would not run.
func TestStatusHeaderFixesHintAddress(t *testing.T) {
	t.Parallel()
	// A user-level alias without source, a child of prod.
	tgt := &remote.Target{Alias: "a-shop", User: true, Address: "vm1:dev", Host: "vm1", Name: "dev", Path: "shop/dev", Parent: "prod"}
	hint := "devopsy @" + remote.HintAddress(tgt) + " "
	if targetRef(tgt) == remote.HintAddress(tgt) {
		t.Fatalf("the case needs targetRef %q and HintAddress %q to differ", targetRef(tgt), remote.HintAddress(tgt))
	}
	rel := "release\t20261107000000\t{}\n"
	for _, c := range []struct{ name, header string }{
		{"a cut rollback", rel + "cut\t20261107000000\trollback\t20261108000000\t\n"},
		{"a cut restore", rel + "cut\t20261107000000\trestore\t20261106000000\t20261105000000\n"},
		{"a cut release with a sync's partial copy", rel + "cut\t20261107000000\trelease\t20261106000000\t\npartial\t\n"},
		{"a restore that did not complete", rel + "partial\t" + remote.IncompleteLine("2026-11-07 00:00:00", "20261105000000", remote.IncompleteRestore, "it was cut")},
		{"an import that did not complete, on a child", rel + "partial\t" + remote.IncompleteLine("2026-11-07 00:00:00", "db", remote.IncompleteImport, "it was cut")},
		{"a sync that did not complete", rel + "partial\t\n"},
	} {
		var r statusReport
		r.addHeader(parseStatus(c.header), tgt)
		for _, e := range r.Entries {
			for _, f := range e.Fixes {
				for _, cmd := range f.Cmds {
					for _, verb := range []string{" --rollback", " --sync", " --snapshots restore ", " --release"} {
						if strings.Contains(cmd+" ", verb) && !strings.HasPrefix(cmd, hint) {
							t.Errorf("%s: %q does not name %q (HintAddress)", c.name, cmd, remote.HintAddress(tgt))
						}
					}
				}
			}
		}
	}
}
