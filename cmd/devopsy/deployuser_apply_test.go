package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
	"github.com/hanoii/devopsy-cli/internal/setup"
)

// An apply refused as another deploy user's server changes nothing of the login's, its
// config included: a volume chosen (--root) is never named as its releases: root, as no
// server.yaml of its is written, since a setup as another user would make that user's release
// root and config.
//
// BUG (review round 3): applySetup names the root chosen in the login's config.yaml
// (nameReleaseRoot) before it reads the facts under its lock and before its root run, so a
// login refused there (another docker member set up, which the first session could not see in
// a home of 0750, or one set up meanwhile) is left with releases: root naming the volume: its
// next releases go there, and its next --server-setup reads the root as named already.
func TestServerSetupRefusedNamesNoRoot(t *testing.T) {
	t.Parallel()
	s, host, vol, config := rootChoice(t)
	me := currentLogin().name
	write(t, filepath.Join(host, "etc", "group"), "docker:x:990:"+me+",ops\n", 0o644)
	write(t, filepath.Join(host, "etc", "passwd"), me+":x:1000:1000::/home/"+me+":/bin/sh\nops:x:1001:1001::/home/ops:/bin/sh\n", 0o644)
	write(t, filepath.Join(host, "home", "ops", ".config", "devopsy", setup.RecordFile), "{}\n", 0o600)
	before := readString(config)
	out, code := s.run(nil, remote.ServerSetupFlag, "apply", "--root", vol)
	if code == 0 || !strings.Contains(out, "ops was set up too on this server") {
		t.Fatalf("the apply was not refused as another deploy user's (%d):\n%s", code, out)
	}
	if got := readString(config); got != before {
		t.Errorf("a refused apply named the root in the login's config:\n%s\n%s", got, out)
	}
}
