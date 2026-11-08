package remote

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// Bug (review round 2): a target with no server is refused with its
// command in the message's words, "no server for @prod: devopsy
// @<server>:prod, or DEVOPSY_SERVER (or DEVOPSY_SERVER_PROD)"
// (resolveTarget, and ServerTarget's "devopsy @<server>, or ..."), where
// a parent with no server gets a report.Failure with its fix: the
// command is a fix, after "$ ", and the variables its text.
//
// Not parallel: it sets DEVOPSY_HOME (t.Setenv), as TestLoadTarget does.
func TestNoServerOffersTheAddressAsAFix(t *testing.T) {
	t.Setenv("DEVOPSY_HOME", t.TempDir())
	dir := t.TempDir()
	write(t, filepath.Join(dir, ConfigFile), "project: shop\nenvironments:\n  prod: {}\n")
	_, err := LoadTarget(dir, "prod", nil)
	if err == nil || !strings.Contains(err.Error(), "no server for @prod") {
		t.Fatalf("want the no-server error, got %v", err)
	}
	if strings.Contains(err.Error(), "devopsy @") {
		t.Errorf("the command in the message's words: %v", err)
	}
	var found bool
	for _, r := range report.Remedies(err) {
		for _, c := range r.Cmds {
			found = found || strings.HasPrefix(c, "devopsy @<server>")
		}
	}
	if !found {
		t.Errorf("no fix with the command: %+v", report.Remedies(err))
	}
}
