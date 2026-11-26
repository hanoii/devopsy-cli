package main

import (
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// projectUsersOwn is info as a project user's own --server-info reads it (serverFacts): its
// docker the door's, no claims directory of its own, its claims the door's answer
// (parseUIDClaims names each by its owner).
func projectUsersOwn(info remote.ServerInfo, user string) remote.ServerInfo {
	info.ProjectUser, info.UIDsDir, info.DoorProjects = user, "", nil
	info.Docker.Door = true
	for i := range info.UIDs {
		info.UIDs[i].Name = info.UIDs[i].Owner
	}
	return info
}

// A project user is offered no removal of a claim: the claims directory is the deploy user's,
// closed to it, whatever the claim is of.
//
// Bug (review, fails): an environment's claim left (its directory removed by hand, a --destroy
// cut before its free) is still offered to a project user with an rm, and of a relative path,
// "rm env:shop:staging", its directory unknown to that report (UIDsDir empty): leftEntries'
// claimRemoval, which only the project claim's site in projectEntries was kept from.
func TestProjectUserIsOfferedNoClaimRemoval(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for name, info := range map[string]remote.ServerInfo{
		"an environment beside it": releasedAt([]string{"shop/prod"}, "project:shop", "env:shop:prod", "env:shop:staging"),
		"no environment left":      releasedAt(nil, "project:shop", "env:shop:staging"),
	} {
		said := false
		for _, e := range serverInfoReport(projectUsersOwn(info, "devopsy-shop"), "vm1", "devopsy-shop@vm1:", "0.53.0", now) {
			said = said || e.Group == "shop" && strings.Contains(strings.Join(e.Lines, " "), "uid claim 10")
			for _, r := range e.Remedies {
				for _, c := range r.Cmds {
					if strings.HasPrefix(c, "rm ") {
						t.Errorf("%s: a project user is offered %q under %q", name, c, strings.Join(e.Lines, " "))
					}
				}
			}
		}
		// The claim itself is still said: every claim is said somewhere.
		if !said {
			t.Errorf("%s: env:shop:staging's claim left is not said", name)
		}
	}
}

// What the door did not answer (askDoorClaims' one claim that says it) is a warning of the
// claims not read, with no removal offered, and no uid shown as if none were claimed.
func TestProjectUserClaimsNotAnswered(t *testing.T) {
	t.Parallel()
	info := projectUsersOwn(releasedAt([]string{"shop/prod"}), "devopsy-shop")
	info.UIDs = []remote.UIDClaim{{Bad: "the door gave none: exit status 2"}}
	warned := false
	for _, e := range serverInfoReport(info, "vm1", "devopsy-shop@vm1:", "0.53.0", time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)) {
		if e.Key != "uids" {
			continue
		}
		warned = e.Level == remote.StatusWarning && strings.Contains(e.Lines[0], "the door gave none")
		if len(e.Remedies) > 0 {
			t.Errorf("claims the door did not answer offer %+v", e.Remedies)
		}
	}
	if !warned {
		t.Error("claims the door did not answer are no warning")
	}
}
