package remote

import (
	"strings"
	"testing"
)

// Review round 1 of refactor/fixes, bug: a reload whose owners step
// fails offers --log as a detail in prose, "devopsy @vm1:prod --log shows
// it", where the same reload's failed step offers it as a hint
// (sayRemedies(report.Hint("", ... --log))): one command, two layouts,
// and a command offered outside report.Remedy (ReloadScript).
func TestReviewReloadOwnersLogIsAHint(t *testing.T) {
	t.Parallel()
	s := ReloadScript(&Target{Name: "prod", Address: "vm1:prod", Host: "vm1", Path: "app/prod"}, "app-prod")
	if strings.Contains(s, `--log shows it"`) {
		t.Errorf("the owners' failure names --log in a detail's prose:\n%s", s)
	}
}
