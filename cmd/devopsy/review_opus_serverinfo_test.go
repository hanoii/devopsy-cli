package main

import (
	"strings"
	"testing"
	"time"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// Bug: --server-info warns twice of one missing tar. serverTools (--server-info's own list)
// and setupTools (the setup's tools item, toolsRefusal) both hold tar and flock, and the
// server's facts carry both: info.Missing and the setup's tools refusal, each worded and
// warned on its own. One list of the tools every release needs, judged once, says it once.
func TestReviewServerInfoSaysAMissingToolOnce(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	info := remote.ServerInfo{Version: "0.53.0", Protocol: remote.Protocol, Root: "/home/devopsy", NTP: "yes", Missing: []string{"tar"}}
	info.Setup = setUp("")
	if r := toolsRefusalOf([]string{"tar"}); r != nil {
		info.Setup.Refusals = []setup.Refusal{*r}
	}
	_, warnings := serverInfoSays(info, "vm1", "vm1:", "0.53.0", now)
	var tar []string
	for _, w := range warnings {
		if !w.note && strings.Contains(w.msg, "tar") {
			tar = append(tar, w.msg)
		}
	}
	if len(tar) != 1 {
		t.Errorf("a missing tar warned %d times: %q", len(tar), tar)
	}
}

// toolsRefusalOf is toolsRefusal's refusal as a server missing tools has it: the words the
// setup's facts carry from the server.
func toolsRefusalOf(missing []string) *setup.Refusal {
	return &setup.Refusal{Item: "tools", Message: "no " + strings.Join(missing, " or ") + " in PATH, which every release needs",
		Fix: "install them with the system's packages (tar, util-linux), or with devopsy-server's setup.sh"}
}
