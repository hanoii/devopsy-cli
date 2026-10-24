package report

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Everything devopsy says goes through this package (or --say, for the
// scripts): no other code spells out the prefix or writes to stderr
// itself, so the format can change here alone. Help, which is a
// document, is the exception.
func TestNothingSaysAround(t *testing.T) {
	t.Parallel()
	said := regexp.MustCompile(`(^|[^\\a-z])["` + "`" + `]devopsy: |\\"devopsy: |Fprint(f|ln)?\((os\.Stderr|errOut)\b`)
	allowed := []struct{ file, line string }{
		// Usage, devopsy's help: printed as it is.
		{"internal/cli/cli.go", "`devopsy: docker compose for projects"},
		{"cmd/devopsy/main.go", "fmt.Fprint(os.Stderr, styleFor(os.Stderr).help(help.Text))"},
	}
	for _, dir := range []string{"../../cmd", "../../internal"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
			if strings.HasPrefix(rel, "internal/report/") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
		lines:
			for i, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "//") || !said.MatchString(line) {
					continue
				}
				for _, a := range allowed {
					if rel == a.file && strings.Contains(line, a.line) {
						continue lines
					}
				}
				t.Errorf("%s:%d says something around report: %s", rel, i+1, strings.TrimSpace(line))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
