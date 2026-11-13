package setup

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// --server-setup names the release root in the deploy user's config as text, the rest
// (comments, order, other keys, line ends) as it was, a path YAML would read otherwise quoted:
// appended as AppendDoorConfig appends door: where it has no releases:, else put first in its
// releases: block at the block's indent. A releases: with a root: already, empty or not, or one
// that is no block (flow, a scalar) is refused and left alone, its lines the admin's.
func TestSetReleaseRoot(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		have, root, want, err string
	}{
		"no file":           {"", "/srv", "releases:\n  root: /srv\n", ""},
		"no releases":       {"# mine\nfirewall:\n  metadata: block\n", "/srv", "# mine\nfirewall:\n  metadata: block\nreleases:\n  root: /srv\n", ""},
		"crlf":              {"snapshots:\r\n  mirror: false\r\n", "/srv", "snapshots:\r\n  mirror: false\r\nreleases:\r\n  root: /srv\r\n", ""},
		"a path quoted":     {"", "/mnt/a #b", "releases:\n  root: '/mnt/a #b'\n", ""},
		"a releases:":       {"releases:\n  keep: 3\n", "/srv", "releases:\n  root: /srv\n  keep: 3\n", ""},
		"its own indent":    {"# mine\nreleases: # the admin's\n\n    # kept\n    keep: 3\nother: 1\n", "/srv", "# mine\nreleases: # the admin's\n    root: /srv\n\n    # kept\n    keep: 3\nother: 1\n", ""},
		"a block, crlf":     {"releases:\r\n  keep: 3\r\n", "/mnt/a #b", "releases:\r\n  root: '/mnt/a #b'\r\n  keep: 3\r\n", ""},
		"releases: empty":   {"releases:\nother: 1\n", "/srv", "releases:\n  root: /srv\nother: 1\n", ""},
		"empty, at the end": {"releases:", "/srv", "releases:\n  root: /srv\n", ""},
		"root empty":        {"releases:\n  root:\n", "/srv", "", "it has a releases: root: already"},
		"root named":        {"releases:\n  keep: 3\n  root: /home/devopsy\n", "/srv", "", "it has a releases: root: already"},
		"flow":              {"releases: {keep: 3}\n", "/srv", "", "its releases: is no block"},
		"a null":            {"releases: ~\n", "/srv", "", "would not be read"},
		"two documents":     {"keep: 1\n---\nother: 2\n", "/srv", "", "would not be read"},
	} {
		got, err := SetReleaseRoot([]byte(c.have), c.root)
		switch {
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: %q, %v, want an error %q", name, got, err, c.err)
		case c.err == "" && (err != nil || string(got) != c.want):
			t.Errorf("%s: %q, %v, want:\n%s", name, got, err, c.want)
		case c.err == "":
			var back struct {
				Releases struct {
					Root string `yaml:"root"`
				} `yaml:"releases"`
			}
			if err := yaml.Unmarshal(got, &back); err != nil || back.Releases.Root != c.root {
				t.Errorf("%s: read back %q, %v", name, back.Releases.Root, err)
			}
		}
	}
}
