package main

import (
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// Bug (review 1b): a level's edit that changed nothing, with one environment failing,
// still says for every other environment what applies the change ("running containers
// keep their old values until a reload recreates them"), though nothing changed and
// nothing is to apply: only the failing environment's detail follows the head.
//
// Not parallel: it sets report's default logger.
func TestReview1bLevelEditThatChangedNothingSaysNoReload(t *testing.T) {
	old := report.Default()
	defer report.SetDefault(old)
	var said strings.Builder
	report.SetDefault(report.New(&said, false, 0))
	tg := &remote.Target{Name: "prod", Host: "vm1", Address: "vm1:prod", Path: "shop/prod"}
	level := &remote.Level{Name: "project", Dir: "shop", Link: "project.env"}
	edit := cli.Edit{File: "project.env", Known: true, Gone: map[string]bool{}, Unchanged: true}
	sayApplied(tg, level, "X was not set in "+varsScope(tg, level)+", so nothing changed", edit, []remote.Applied{
		{Dir: "dev", Word: "0"},
		{Dir: "prod", Word: "4", Lines: (&cli.Missing{Names: []string{"X"}}).Lines()},
	})
	got := said.String()
	if !strings.Contains(got, "but prod's next reload or release will still fail, as before") || !strings.Contains(got, "devopsy: | prod: compose needs X") {
		t.Fatalf("the failing environment is not said:\n%s", got)
	}
	if strings.Contains(got, "devopsy: | dev: ") {
		t.Errorf("an environment with nothing to apply gets a detail after an edit that changed nothing:\n%s", got)
	}
}
