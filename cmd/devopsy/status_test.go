package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
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
	if e := entries[1]; e.Key != "cron" || e.Level != remote.StatusWarning || strings.Join(e.Lines, "|") != "last run failed|database is locked" || len(e.Fixes) != 1 || strings.Join(e.Fixes[0].Commands, "|") != "devopsy drush cron" {
		t.Errorf("a warning on its second line, its fix given twice: %+v", e)
	}
	if e := entries[2]; e.Key != "url" || strings.Join(e.Lines, "|") != "https://shop.example/admin" {
		t.Errorf("a key coming back: %+v", e)
	}
	if strings.Join(bad, "|") != "url https://shop.example|info\turl|error\turl\tx|info\t\tx|info\ta\tb\tc\td" {
		t.Errorf("bad lines: %q", bad)
	}
}

// A container needs attention when it restarts, is dead or unhealthy, or
// exited on an error of its own; a stop's exit codes (0, SIGKILL's 137,
// SIGTERM's 143) are none.
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
			{Level: remote.StatusWarning, Key: "maintenance", Lines: []string{"on"}, Fixes: []remote.StatusFix{{Commands: []string{"devopsy maintenance off"}}}},
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
disk         releases /srv, /: ext4, 50.0 GB free of 100.0 GB (50% used)
             docker /var/lib/docker: xfs, 2.0 GB free of 100.0 GB (98% used)
             docker /var/lib/docker is 98% full, 2.0 GB free: builds, pulls, uploads, logs and databases fail once it is full
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
             $ devopsy maintenance off
`
	if out.String() != want || code != statusWarned {
		t.Errorf("--status (%d):\n%s\nwant:\n%s", code, out.String(), want)
	}
}
