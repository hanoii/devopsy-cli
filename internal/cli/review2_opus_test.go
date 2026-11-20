package cli

import (
	"path/filepath"
	"slices"
	"testing"
)

// Bug (review 2): a project's edit that removes REQUIRED where the environment's own
// .env holds REQUIRED= leaves it missing as before, the nearer empty line winning; round
// 1's fix moved REQUIRED from SetAgain to SetThem, but varsFix words SetThem at the edited
// level too, so the fix still offers `--vars-project set REQUIRED`, which the nearer
// empty line wins over again. No key at the edited level brings it back: the fix must
// name the file that empties it (the environment's), never the edited level.
func TestReview2ExplainNeverSetsBelowANearerEmptyLine(t *testing.T) {
	t.Parallel()
	said := "error while interpolating services.app.environment.REQUIRED: required variable REQUIRED is missing a value"
	root := project(t, "shop", map[string]string{
		"compose.yaml": minimalCompose,
		"config.yaml":  "project: shop\n",
		"bin/docker*":  fakeCompose(said, `{"REQUIRED":{"Required":true}}`),
		".env":         "REQUIRED=\n",
		"project.env":  "",
	})
	m, err := CheckMissing(root, []string{"PATH=" + filepath.Join(root, ProjectDirName, "bin") + ":/usr/bin:/bin"}, &Before{File: BeforeFiles["project"], Content: []byte("REQUIRED='1'\n")})
	if err != nil || m == nil || m.Before == nil {
		t.Fatalf("%+v %v", m, err)
	}
	e := m.Explain([]string{"REQUIRED"})
	if slices.Contains(e.SetThem, "REQUIRED") || slices.Contains(e.SetAgain, "REQUIRED") {
		t.Errorf("the fix sets REQUIRED at the edited level, below the environment's empty line that wins: %+v", e)
	}
}
