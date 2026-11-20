package cli

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/report"
)

// Compose's words never carry a secret of several lines (a key in PEM, a JSON
// credential), on either path. BUG (review 2, fable): MissingVars splits compose's stderr
// into lines and missingOf masks each line by itself, where Secrets.Mask replaces a value
// whole: a value of several lines matches no single line, so its first line is said on the
// "required variable" line (compose interpolates a :? message) and every line of it when
// compose fails for another reason.
func TestComposeWordsMaskSecretsOfSeveralLines(t *testing.T) {
	t.Parallel()
	key := "first-line-of-the-key\nsecond-line-of-the-key"
	for _, c := range []struct{ name, said, vars string }{
		{"missing", "error while interpolating services.app.environment.TOKEN: required variable TOKEN is missing a value: sign it with " + key, `{"TOKEN":{"Required":true}}`},
		{"unreadable", "services.app.configs: invalid content " + key, `{}`},
	} {
		root := project(t, "shop", map[string]string{
			"compose.yaml": minimalCompose,
			"config.yaml":  "project: shop\nsecrets: [KEY]\n",
			"bin/docker*":  fakeCompose(c.said, c.vars),
		})
		p, err := loadProject(root, []string{"PATH=" + filepath.Join(root, ProjectDirName, "bin") + ":/usr/bin:/bin", "KEY=" + key})
		if err != nil {
			t.Fatal(err)
		}
		var failure *report.Failure
		if err := missingVars(p); !errors.As(err, &failure) {
			t.Fatalf("%s: %v, want a failure", c.name, err)
		}
		said := failure.Msg + "\n" + strings.Join(failure.Details, "\n")
		if strings.Contains(said, "of-the-key") {
			t.Errorf("%s: a line of the secret said:\n%s", c.name, said)
		}
		// The follow-up's lines, what --check-images --hint prints for the operator.
		if m, err := missingOf(p); m != nil {
			if lines := strings.Join(m.Lines(), "\n"); strings.Contains(lines, "of-the-key") {
				t.Errorf("%s: a line of the secret in the check's lines:\n%s", c.name, lines)
			}
		} else if err != nil && strings.Contains(err.Error(), "of-the-key") {
			t.Errorf("%s: a line of the secret in the check's error: %v", c.name, err)
		}
	}
}
