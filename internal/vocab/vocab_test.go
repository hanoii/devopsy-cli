package vocab

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/sources"
)

// retired are the words devopsy no longer says, each with what to say instead.
var retired = []struct{ words, use string }{
	{"locally", "a form of vocab.Operator, vocab.NoTarget, or into the local environment"},
	{"your machine", "a form of vocab.Operator"},
	{"local one", "a form of vocab.Operator"},
	{"local devopsy", "a form of vocab.Operator"},
}

// docs are the markdown files people read, from the repository's root: their words are written
// out, so only the retired ones are checked there.
var docs = []string{"README.md", "docs/*.md", "internal/skill/*.md"}

// kept are the places that keep a word, by file and a substring of the line the word is on
// ("" for the whole file).
var kept = []struct{ file, line string }{
	// Until the rename reaches them.
	{"docs/capabilities.md", ""},
	{"docs/config.md", ""},
	{"docs/data.md", ""},
	{"docs/images.md", ""},
	{"docs/local.md", ""},
	{"docs/project-users.md", ""},
	{"docs/releases.md", ""},
	{"docs/servers.md", ""},
	{"docs/syncs.md", ""},
	{"internal/skill/SKILL.md", ""},
	{"README.md", ""},
}

// Every form is one literal fit for a script's double quotes and holds no retired word; no Go
// text outside this package writes a form's words, and none says a retired word, nor does a
// doc: a term reworded is an edit to vocab.go, and the test finds what a rename missed.
func TestVocabulary(t *testing.T) {
	t.Parallel()
	forms := formsOf(t)
	var gone []*regexp.Regexp
	for _, r := range retired {
		gone = append(gone, phrase(r.words))
	}
	for name, f := range forms {
		if strings.ContainsAny(f, "\"$`\\") {
			t.Errorf("vocab.go: %s holds a character a script's double quotes would read: %q", name, f)
		}
		for i, re := range gone {
			if re.MatchString(f) {
				t.Errorf("vocab.go: %s says %q, retired", name, retired[i].words)
			}
		}
	}
	var said []*regexp.Regexp
	var saidName []string
	for name, f := range forms {
		if strings.HasPrefix(name, "Operator.") {
			said, saidName = append(said, phrase(f)), append(saidName, name)
		}
	}

	for _, f := range sources.Go(t, false) {
		if strings.HasPrefix(f.Path, "internal/vocab/") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f.Path, f.Data, 0)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(f.Data), "\n")
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			line := func(at int) int {
				l := fset.Position(lit.Pos()).Line
				if strings.HasPrefix(lit.Value, "`") {
					l += strings.Count(text[:at], "\n")
				}
				return l
			}
			for i, re := range gone {
				for _, m := range re.FindAllStringIndex(text, -1) {
					if l := line(m[0]); !isKept(f.Path, lines[l-1]) {
						t.Errorf("%s:%d says %q: use %s", f.Path, l, retired[i].words, retired[i].use)
					}
				}
			}
			for i, re := range said {
				for _, m := range re.FindAllStringIndex(text, -1) {
					if l := line(m[0]); !isKept(f.Path, lines[l-1]) {
						t.Errorf("%s:%d writes vocab.%s's words: splice the form", f.Path, l, saidName[i])
					}
				}
			}
			return true
		})
	}

	root := sources.Root(t)
	for _, pattern := range docs {
		paths, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil || len(paths) == 0 {
			t.Fatalf("no docs at %s: %v", pattern, err)
		}
		for _, p := range paths {
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			lines := strings.Split(string(data), "\n")
			for i, re := range gone {
				for _, m := range re.FindAllIndex(data, -1) {
					if l := strings.Count(string(data[:m[0]]), "\n") + 1; !isKept(rel, lines[l-1]) {
						t.Errorf("%s:%d says %q: use %s", rel, l, retired[i].words, retired[i].use)
					}
				}
			}
		}
	}
}

// formsOf is every form vocab.go declares, by its name (Operator.On, NoTarget), each asserted
// to be one literal: a form built from another would make its pieces match everywhere.
func formsOf(t *testing.T) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "vocab.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	forms := map[string]string{}
	add := func(name string, e ast.Expr) {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Errorf("vocab.go: %s is not one string literal", name)
			return
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			t.Fatal(err)
		}
		forms[name] = s
	}
	for _, decl := range f.Decls {
		g, ok := decl.(*ast.GenDecl)
		if !ok || (g.Tok != token.VAR && g.Tok != token.CONST) {
			continue
		}
		for _, spec := range g.Specs {
			v := spec.(*ast.ValueSpec)
			for i, id := range v.Names {
				if i >= len(v.Values) {
					continue
				}
				if c, ok := v.Values[i].(*ast.CompositeLit); ok {
					for _, el := range c.Elts {
						kv := el.(*ast.KeyValueExpr)
						add(id.Name+"."+kv.Key.(*ast.Ident).Name, kv.Value)
					}
					continue
				}
				add(id.Name, v.Values[i])
			}
		}
	}
	if len(forms) == 0 {
		t.Fatal("vocab.go declares no forms")
	}
	return forms
}

// phrase matches words whole, in any case, with any whitespace between them (help and docs
// are wrapped by hand); a word ending in a hyphenated name (local devopsy-template-traefik)
// is not matched.
func phrase(words string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\b` + strings.Join(strings.Fields(regexp.QuoteMeta(words)), `\s+`) + `\b(?:[^-]|$)`)
}

// isKept says whether a word on line of file is in kept.
func isKept(file, line string) bool {
	for _, k := range kept {
		if k.file == file && strings.Contains(line, k.line) {
			return true
		}
	}
	return false
}
