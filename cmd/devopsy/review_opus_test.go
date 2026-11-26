package main

import (
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// projectUserInfo is a project user's --server-info facts as serverFacts gives them since
// askDoorClaims: its docker the door, no claims directory, its claims the door's answer
// (Name the owner, as parseUIDClaims sets it).
func projectUserInfo(paths []string, owners ...string) remote.ServerInfo {
	info := releasedAt(paths)
	info.ProjectUser, info.UIDsDir, info.Docker.Door = "devopsy-shop", "", true
	for i, o := range owners {
		info.UIDs = append(info.UIDs, remote.UIDClaim{Owner: o, UID: uint32(100 + i), Name: o})
	}
	return info
}

// Bug (review, opus): a project user's report offers the removal of an environment's uid claim
// left (an environment removed by hand from its tree) as it does to the admin, through
// --shell-host and an rm of the claim in a claims directory it cannot read, the path relative
// since UIDsDir is empty for it ("rm env:shop:staging" in its own shell). The commit stops
// this for a project claim left (projectEntries) but not for an environment's (leftEntries,
// goneEntry with claimRemoval), which before the commit never had claims to judge.
func TestReviewOpusProjectUserOfferedNoEnvClaimRemoval(t *testing.T) {
	t.Parallel()
	info := projectUserInfo([]string{"shop/prod"}, "project:shop", "env:shop:prod", "env:shop:staging")
	for _, e := range serverInfoReport(info, "vm1", "devopsy-shop@vm1:", "0.53.0", time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)) {
		if cmds := fixText(e.Remedies); strings.Contains(cmds, "$ rm ") {
			t.Errorf("a project user is offered a claim's removal in the deploy user's claims directory: %v\n%s", e.Lines, cmds)
		}
	}
}

// The door's answer failing is said as the claims unread, with no removal offered: its claim
// has no Name, and a project user's UIDsDir is empty, so the two match as the directory's.
func TestReviewOpusProjectUserClaimsUnread(t *testing.T) {
	t.Parallel()
	info := projectUserInfo([]string{"shop/prod"})
	info.UIDs = []remote.UIDClaim{{Bad: "the door gave none: exit status 2"}}
	found := false
	for _, e := range serverInfoReport(info, "vm1", "devopsy-shop@vm1:", "0.53.0", time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)) {
		if strings.Contains(strings.Join(e.Lines, " "), "the uid claims could not be read") {
			found = true
			if len(e.Remedies) > 0 {
				t.Errorf("claims unread offer a remedy: %s", fixText(e.Remedies))
			}
		}
	}
	if !found {
		t.Error("the door's failed answer is not said as the claims unread")
	}
}
