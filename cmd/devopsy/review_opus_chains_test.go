package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/remote"
)

// Bug: the chains are read as root (readChains) whatever the setup says of Docker: where the
// Docker devopsy reaches is refused as not this host's (another host's through DOCKER_HOST, a
// rootless one), the facts session and the apply still run a root container through it,
// the apply building the util image there first (build), so the host's root that container
// mounts, and the chains judged, are another machine's. stepNeeds says every root run needs
// Docker; the read before the refusals are judged is one too.
func TestReviewOpusNoChainReadThroughARefusedDocker(t *testing.T) {
	t.Parallel()
	s, host := setupServer(t)
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		t.Fatal(err)
	}
	firewallLoaded(t, host, false)
	write(t, filepath.Join(host, "docker-info.json"), `{"Name":"elsewhere","LoggingDriver":"json-file","LiveRestoreEnabled":false,"DockerRootDir":"/var/lib/docker","SecurityOptions":["name=seccomp"]}`, 0o644)
	for _, step := range []string{"facts", "apply"} {
		s.logs()
		out, _ := s.run(nil, remote.ServerSetupFlag, step)
		if !strings.Contains(out, "not this host's docker.service") && step == "apply" {
			t.Fatalf("%s: no refusal:\n%s", step, out)
		}
		if docker, _ := s.logs(); strings.Contains(docker, "chroot /host") {
			t.Errorf("%s: a root run through a Docker refused as not this host's:\n%s", step, docker)
		}
	}
}
