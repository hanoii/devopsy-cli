package howto

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The docker topic's repository step leaves apt one source of Docker's, also on a server whose
// Docker came from an older edition of Docker's install guide (a docker.list signed by
// /etc/apt/keyrings/docker.gpg): two files naming download.docker.com with different keys are
// a list apt refuses whole ("Conflicting values set for option Signed-By ... The list of
// sources could not be read"), and every apt-get after it fails, security updates included.
//
// Bug (review, round 1): the step writes docker.sources beside whatever list names Docker's
// repository already, so on such a server its own apt-get update fails, the next step installs
// nothing, and apt stays broken until the admin finds the older file.
func TestDockerRepositoryStepBesideAnOlderList(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	apt := filepath.Join(dir, "apt")
	lists := filepath.Join(apt, "sources.list.d")
	if err := os.MkdirAll(lists, 0o755); err != nil {
		t.Fatal(err)
	}
	older := "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/debian trixie stable\n"
	if err := os.WriteFile(filepath.Join(lists, "docker.list"), []byte(older), 0o644); err != nil {
		t.Fatal(err)
	}
	osRelease := filepath.Join(dir, "os-release")
	if err := os.WriteFile(osRelease, []byte("ID=debian\nVERSION_CODENAME=trixie\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// apt, as it refuses two sources of Docker's: every apt-get of the step finds one at most.
	path := fakes(t, map[string]string{"apt-get": "if [ \"$(grep -ls download.docker.com " + lists + "/* | wc -l)\" -gt 1 ]; then echo 'E: Conflicting values set for option Signed-By' >&2; exit 100; fi\n"}, "apt-get", "curl")
	out := pasted(t, stepWith(t, Docker, "docker.sources"), path, "/etc/apt", apt, "/etc/os-release", osRelease)
	entries, err := os.ReadDir(lists)
	if err != nil {
		t.Fatal(err)
	}
	var docker []string
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(lists, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "download.docker.com") {
			docker = append(docker, e.Name())
		}
	}
	if len(docker) != 1 || strings.Contains(out, "E: Conflicting") {
		t.Errorf("apt has %d sources of Docker's, %v, which apt refuses when their keys differ:\n%s", len(docker), docker, out)
	}
}
