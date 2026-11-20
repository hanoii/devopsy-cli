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

// Bug (review 2): a variable an include's env_file sets is set for compose, which reads
// that file, but MissingVars looks it up in devopsy's environment alone, so once another
// variable fails the files, it is listed with it: "compose requires INC_NAME and INC_SET,
// which are not set" (e2e/release_test.go now asserts that line), and the --vars
// follow-up's fix sets INC_SET in shared/.env, overriding the include's value. Seen with
// the real compose too (config -q passes once INC_NAME alone is set).
func TestReview2MissingVarsIncludeEnvFileSet(t *testing.T) {
	t.Parallel()
	said := "error while interpolating services.side.environment.[]: required variable INC_NAME is missing a value: the name"
	root := project(t, "inclvars", map[string]string{
		"compose.yaml": "include:\n  - path: sub/inc.yaml\n    env_file: sub/inc.env\nservices:\n  app:\n    image: busybox\n",
		"sub/inc.yaml": "services:\n  side:\n    image: busybox\n    environment: [\"NAME=${INC_NAME:?the name}\", \"SET=${INC_SET:?}\"]\n",
		"sub/inc.env":  "INC_SET=1\n",
		"config.yaml":  "project: inclvars\n",
		"bin/docker*":  fakeCompose(said, `{"INC_NAME":{"Required":true},"INC_SET":{"Required":true}}`),
	})
	p, err := loadProject(root, []string{"PATH=" + filepath.Join(root, ProjectDirName, "bin") + ":/usr/bin:/bin"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := missingOf(p)
	if err != nil || m == nil {
		t.Fatalf("%+v %v", m, err)
	}
	if !slices.Equal(m.Names, []string{"INC_NAME"}) {
		t.Errorf("missing %q, want INC_NAME alone: the include's env_file sets INC_SET", m.Names)
	}
}
