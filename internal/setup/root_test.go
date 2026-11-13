package setup

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// --server-setup names the release root in the deploy user's config as text: appended where
// the config has no releases:, put first in its releases: block where it has one, the rest
// (comments, order, other keys) as it was, a path YAML would read otherwise quoted; a config
// whose result would not read as releases: root added, or whose releases: names a root, is
// refused and left alone.
func TestSetReleaseRoot(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		have, root, want, err string
	}{
		"no file":        {"", "/srv", "releases:\n  root: /srv\n", ""},
		"no releases":    {"# mine\nfirewall:\n  metadata: block\n", "/srv", "# mine\nfirewall:\n  metadata: block\nreleases:\n  root: /srv\n", ""},
		"no newline":     {"snapshots:\n  root: /backups", "/srv", "snapshots:\n  root: /backups\nreleases:\n  root: /srv\n", ""},
		"releases block": {"releases:\n    # how many\n    keep: 3 # mine\nsnapshots:\n  root: /b\n", "/srv", "releases:\n    # how many\n    root: /srv\n    keep: 3 # mine\nsnapshots:\n  root: /b\n", ""},
		"a path quoted":  {"", "/mnt/a #b", "releases:\n  root: '/mnt/a #b'\n", ""},
		"root named":     {"releases:\n  root: /home/devopsy\n", "/srv", "", "names a root already"},
		"flow":           {"releases: {keep: 3}\n", "/srv", "", "no block mapping"},
		"empty releases": {"releases:\n", "/srv", "", "no block mapping"},
		"no mapping":     {"- a\n", "/srv", "", "no YAML mapping"},
		"no YAML":        {"releases: [\n", "/srv", "", "no YAML devopsy reads"},
		// A second document: lines appended land in it, which the loader never reads.
		"two documents": {"keep: 1\n---\nother: 2\n", "/srv", "", "would not be read"},
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
