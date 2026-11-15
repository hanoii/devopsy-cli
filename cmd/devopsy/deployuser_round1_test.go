package main

import (
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/setup"
)

// Bug (deploy-user review round 1): the setup's plan, the one thing the operator reads before
// confirming, says the deploy user's record "does not exist yet" and never which user it will
// record: an operator who does not know which login @<server>: reached (the case the record is
// for) confirms the wrong user into it, and the real deploy user is refused from then on.
func TestServerSetupPlanNamesTheUserItRecords(t *testing.T) {
	t.Parallel()
	s, _ := setupServer(t)
	me := currentLogin().name
	out, _ := s.run(nil, "@devopsy@one:", "--server-setup")
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, setup.ItemDeployUser+" ") {
			if !strings.Contains(l, me) {
				t.Errorf("the plan's deploy user line names no user (%s): %q\n%s", me, l, out)
			}
			return
		}
	}
	t.Errorf("no deploy user line in the plan:\n%s", out)
}
