package complete

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// BUG (review round 2): --status takes --json locally as on a target
// (cli.Flags gives it [--json] for both scopes), but only @<target>
// --status completes it: a local --status completes nothing, where a
// local --env completes --show.
func TestCompleteLocalStatusJSON(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "app")
	write(t, filepath.Join(root, ".devopsy", "compose.yaml"), "services:\n  web:\n    image: busybox\n", 0o644)
	r := Complete(root, []string{"--status", ""}, os.Environ())
	if got := values(r); !slices.Contains(got, "--json") {
		t.Errorf("devopsy --status <tab>: %q, no --json", got)
	}
}
