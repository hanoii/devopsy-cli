package report

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hanoii/devopsy-cli/internal/sources"
)

// Everything devopsy says goes through this package (or --say, for the
// scripts): no other code spells out the prefix or writes to stderr
// itself, so the format can change here alone. Help, which is a
// document, is the exception.
func TestNothingSaysAround(t *testing.T) {
	t.Parallel()
	// A command's "$ " too: report lays every one out (Fix, Hint).
	// A code anywhere, written or built (W%04d), and a coded message's line for a reader who
	// passes it on: report builds both from the registry (Code).
	said := regexp.MustCompile(`(^|[^\\a-z])["` + "`" + `]devopsy: |\\"devopsy: |Fprint(f|ln)?\((os\.Stderr|errOut)\b|["` + "`" + `']\$ |\\"\$ |\bW[0-9]{4}\b|W%0?4d|to the server's admin`)
	allowed := []struct{ file, line string }{
		// Usage, devopsy's help: printed as it is.
		{"internal/cli/cli.go", "`devopsy: docker compose for projects"},
		{"cmd/devopsy/main.go", "fmt.Fprint(os.Stderr, styleFor(os.Stderr).help(help.Text))"},
	}
	for _, f := range sources.Go(t, false) {
		if strings.HasPrefix(f.Path, "internal/report/") {
			continue
		}
	lines:
		for i, line := range strings.Split(string(f.Data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") || !said.MatchString(line) {
				continue
			}
			for _, a := range allowed {
				if f.Path == a.file && strings.Contains(line, a.line) {
					continue lines
				}
			}
			t.Errorf("%s:%d says something around report: %s", f.Path, i+1, strings.TrimSpace(line))
		}
	}
}

// A fake passwd line in a test names its user by a uid no login has:
// 61000 and up, or the running user's own through the test's helpers
// (loginLine, os.Getuid). A literal in the login range (1000 to 49999)
// is some machine's real user: GitHub's runner is uid 1001, so a fake
// user there was the runner on CI and someone else locally, and a test
// passed on one and failed on the other. The project users' range
// (door.ProjectIDMin to ProjectIDMax) and system ids stay usable.
func TestFakeUsersOwnNoLoginUID(t *testing.T) {
	t.Parallel()
	line := regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_.-]*:x:([0-9]+):[0-9]+:`)
	for _, f := range sources.Go(t, true) {
		if !strings.HasSuffix(f.Path, "_test.go") {
			continue
		}
		for i, l := range strings.Split(string(f.Data), "\n") {
			for _, m := range line.FindAllStringSubmatch(l, -1) {
				var uid int
				fmt.Sscan(m[1], &uid)
				if uid >= 1000 && uid <= 49999 {
					t.Errorf("%s:%d: a fake user with uid %d, a real login's range: use 61000 and up, or loginLine: %s", f.Path, i+1, uid, m[0])
				}
			}
		}
	}
}

// Unit tests run in parallel (AGENTS.md, Checks): every
// test calls t.Parallel() first, or says why not in its doc comment, on
// a line starting "Not parallel:" (it sets the environment, a global, or
// checks a latency the parallel tests would load). A parallel test never
// assigns a package-level variable: the others read it meanwhile. Go
// itself refuses t.Setenv in a parallel test, and -race finds a helper
// that sets a global.
func TestTestsParallel(t *testing.T) {
	t.Parallel()
	root := sources.Root(t)
	for _, dir := range sources.Dirs(t, "cmd", "internal") {
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, filepath.Join(root, dir), nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		for _, pkg := range pkgs {
			globals := map[string]bool{}
			for _, f := range pkg.Files {
				for _, decl := range f.Decls {
					if g, ok := decl.(*ast.GenDecl); ok && g.Tok == token.VAR {
						for _, s := range g.Specs {
							for _, n := range s.(*ast.ValueSpec).Names {
								globals[n.Name] = true
							}
						}
					}
				}
			}
			for file, f := range pkg.Files {
				if strings.HasSuffix(file, "_test.go") {
					checkParallel(t, fset, dir+"/"+filepath.Base(file), f, globals)
				}
			}
		}
	}
}

func checkParallel(t *testing.T, fset *token.FileSet, file string, f *ast.File, globals map[string]bool) {
	t.Helper()
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Body == nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Name.Name == "TestMain" ||
			len(fn.Type.Params.List) != 1 || len(fn.Type.Params.List[0].Names) != 1 {
			continue
		}
		where := fmt.Sprintf("%s:%d %s", file, fset.Position(fn.Pos()).Line, fn.Name.Name)
		serial := fn.Doc != nil && regexp.MustCompile(`(?m)^// Not parallel: \S`).MatchString(commentText(fn.Doc))
		parallel := false
		if len(fn.Body.List) > 0 {
			if call, ok := fn.Body.List[0].(*ast.ExprStmt); ok {
				if c, ok := call.X.(*ast.CallExpr); ok {
					if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
						if id, ok := sel.X.(*ast.Ident); ok && sel.Sel.Name == "Parallel" && id.Name == fn.Type.Params.List[0].Names[0].Name {
							parallel = true
						}
					}
				}
			}
		}
		switch {
		case parallel && serial:
			t.Errorf("%s calls t.Parallel() and says it is not parallel", where)
		case !parallel && !serial:
			t.Errorf("%s neither calls t.Parallel() first nor says why not (a \"// Not parallel: <why>\" line in its doc comment)", where)
		case parallel:
			said := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				a, ok := n.(*ast.AssignStmt)
				if !ok || a.Tok == token.DEFINE {
					return true
				}
				for _, l := range a.Lhs {
					// A package-level name, not one declared in the test.
					if id, ok := l.(*ast.Ident); ok && globals[id.Name] && !said[id.Name] && (id.Obj == nil || id.Obj.Pos() < fn.Pos() || id.Obj.Pos() > fn.End()) {
						said[id.Name] = true
						t.Errorf("%s is parallel and sets %s, a package-level variable", where, id.Name)
					}
				}
				return true
			})
		}
	}
}

// commentText is a doc comment's lines as written, "// " included.
func commentText(g *ast.CommentGroup) string {
	var b strings.Builder
	for _, c := range g.List {
		b.WriteString(c.Text + "\n")
	}
	return b.String()
}
