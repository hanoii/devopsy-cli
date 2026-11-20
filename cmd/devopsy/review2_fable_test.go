package main

import (
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/cli"
	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/report"
)

// A level's fix offers each command once. BUG (review 2, fable): sayApplied merges the
// environments' keys list by list, so a key one environment sets again (the project's
// value was its own) and another only sets (its own empty line won before too) lands in
// both SetAgain and SetThem, and varsFix says "set X again, then set X" with the same
// command twice.
//
// Not parallel: it sets report's default logger.
func TestLevelFixOffersEachCommandOnce(t *testing.T) {
	old := report.Default()
	defer report.SetDefault(old)
	var said strings.Builder
	report.SetDefault(report.New(&said, false, 0))
	tg := &remote.Target{Name: "prod", Host: "vm1", Address: "vm1:prod", Path: "shop/prod"}
	level := &remote.Level{Name: "project", Dir: "shop", Link: "project.env"}
	before := func(set bool) []string {
		return (&cli.Missing{Names: []string{"X"}, Before: &cli.MissingBefore{Set: map[string]bool{"X": set}, Computed: map[string]bool{}, Changed: map[string]string{"X": "removed"}}}).Lines()
	}
	sayApplied(tg, level, "unset X in "+varsScope(tg, level), []string{"X"}, []remote.Applied{
		{Dir: "dev", Word: "4", Lines: before(true)},
		{Dir: "prod", Word: "4", Lines: before(false)},
	})
	cmd := "$ devopsy @vm1:prod --vars-project set X\n"
	if got := said.String(); strings.Count(got, cmd) != 1 || strings.Contains(got, "set X again, then set X:") {
		t.Errorf("the fix repeats a command:\n%s", got)
	}
}
