package report

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// rootDoc is the agents' root file, from the repository's root.
const rootDoc = "AGENTS.md"

// repoRoot is the repository's root, from this package's directory.
const repoRoot = "../.."

// externalNames are names the rules cite that are other tools' (ssh's
// and sshd's options) and appear nowhere in the Go source.
var externalNames = []string{"ClientAliveInterval", "IdentityAgent", "LocalForward"}

// projectFiles are file names the rules cite that are a project's (or
// a server's), not the repository's.
var projectFiles = []string{"config.yaml", "config.local.yaml", "devopsy-compose.yaml"}

// rootRules are the places in the root a package's doc comment may name
// for its rules, in the words it uses, with the heading that must exist.
var rootRules = map[string]string{
	"the Areas index in AGENTS.md":        "## Areas",
	"the Checks in AGENTS.md":             "## Checks",
	"the Output conventions in AGENTS.md": "### Output",
}

// Every file path and every Go name the agents' rules cite in code
// spans exists, and every package's doc comment names the rules that
// hold for it (a line "Rules: internals/<area>.md."), which exist: a rename or a removal that leaves the rules
// behind fails here. The histories are left out: they tell of names
// that went on purpose. What is ambiguous (a flag, a variable, a label,
// a shell word, a value, a project's or a server's path) is left out
// too: a check that misses a name costs less than one that cries wolf.
func TestAgentsNamesWhatExists(t *testing.T) {
	t.Parallel()
	docs := []string{rootDoc}
	areas, _ := filepath.Glob(filepath.Join(repoRoot, "internals", "*.md"))
	for _, a := range areas {
		if !strings.HasSuffix(a, ".history.md") {
			docs = append(docs, strings.TrimPrefix(filepath.ToSlash(a), repoRoot+"/"))
		}
	}
	if len(docs) < 2 {
		t.Fatal("no areas found")
	}
	files := repoFiles(t)
	src := goNames(t)
	for _, doc := range docs {
		data, err := os.ReadFile(filepath.Join(repoRoot, doc))
		if err != nil {
			t.Fatal(err)
		}
		for _, span := range codeSpans(string(data)) {
			if p, ok := repoPath(span); ok {
				if !files.has(p) {
					t.Errorf("%s names %s, no file or directory of the repository", doc, span)
				}
				continue
			}
			if ok, checked := src.has(span); checked && !ok {
				t.Errorf("%s names %s, declared nowhere in the Go source", doc, span)
			}
		}
	}

	root, err := os.ReadFile(filepath.Join(repoRoot, rootDoc))
	if err != nil {
		t.Fatal(err)
	}
	for dir, doc := range src.docs {
		m := regexp.MustCompile(`(?m)^Rules: (.*)\.$`).FindStringSubmatch(doc)
		if m == nil {
			t.Errorf("%s's package doc comment names no rules (a line \"Rules: internals/<area>.md.\")", dir)
			continue
		}
		if heading, ok := rootRules[m[1]]; ok {
			if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(heading) + `$`).Match(root) {
				t.Errorf("%s's rules are in %q, but %s has no %q", dir, m[1], rootDoc, heading)
			}
			continue
		}
		for _, p := range strings.Split(m[1], ", ") {
			if !regexp.MustCompile(`^internals/[a-z0-9-]+\.md$`).MatchString(p) || !files[p] {
				t.Errorf("%s's package doc comment names rules in %s, no area", dir, p)
			}
		}
	}
}

// codeSpans are a markdown file's inline code spans, outside fenced
// blocks.
func codeSpans(md string) []string {
	var spans []string
	fenced := false
	for _, line := range strings.Split(md, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		for _, m := range regexp.MustCompile("`([^`]+)`").FindAllStringSubmatch(line, -1) {
			spans = append(spans, m[1])
		}
	}
	return spans
}

var (
	// A path under one of the repository's top directories.
	topPath = regexp.MustCompile(`^(cmd|internal|internals|e2e|scripts|\.github)(/[A-Za-z0-9_.-]+)*/?$`)
	// A file name, or a relative path to one, by its extension.
	extPath = regexp.MustCompile(`^(\.?[A-Za-z0-9_-][A-Za-z0-9_.-]*/)*\.?[A-Za-z0-9_-][A-Za-z0-9_.-]*\.(go|md|yml|yaml|sh)$`)
)

// repoPath says whether a code span is a path of the repository's, and
// gives it cleaned. A path of a project's (`.devopsy/...`), a server's
// (absolute, `$base`, `~`), with a placeholder or a pattern is none.
func repoPath(span string) (string, bool) {
	if strings.HasPrefix(span, ".devopsy/") || strings.HasPrefix(span, ".agents/") || strings.HasPrefix(span, ".claude/") || slices.Contains(projectFiles, span) {
		return "", false
	}
	if topPath.MatchString(span) || extPath.MatchString(span) || strings.HasPrefix(span, "_") && extPath.MatchString("x"+span) {
		return strings.TrimSuffix(span, "/"), true
	}
	return "", false
}

// files are the repository's files and directories, slash separated
// from its root.
type files map[string]bool

func repoFiles(t *testing.T) files {
	t.Helper()
	f := files{}
	err := filepath.WalkDir(repoRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(filepath.ToSlash(p), repoRoot+"/")
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".worktrees") {
			return filepath.SkipDir
		}
		f[rel] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// has says whether p is a file or directory from the root, or the end
// of a file's path (`schema.go` after `internal/remote/config.go`,
// `images/releases.go`); `_linux.go` is the end of a file's name.
func (f files) has(p string) bool {
	if f[p] {
		return true
	}
	for name := range f {
		if strings.HasSuffix(name, "/"+p) || strings.HasPrefix(p, "_") && strings.HasSuffix(path.Base(name), p) {
			return true
		}
	}
	return false
}

// names are the Go source's names: what each package declares at its
// top level, the members (methods and fields) it declares, its types,
// and every identifier and every word of a string anywhere (a script's
// functions live in strings).
type names struct {
	decls   map[string]map[string]bool // package name: top-level names
	members map[string]map[string]bool // package name: methods and fields
	types   map[string][]string        // type name: packages declaring it
	any     map[string]bool
	docs    map[string]string // package directory: its doc comment
}

func goNames(t *testing.T) names {
	t.Helper()
	n := names{decls: map[string]map[string]bool{}, members: map[string]map[string]bool{}, types: map[string][]string{}, any: map[string]bool{}, docs: map[string]string{}}
	word := regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	for _, dir := range goDirs(t) {
		n.docs[dir] = ""
		fset := token.NewFileSet()
		pkgs, err := parser.ParseDir(fset, filepath.Join(repoRoot, dir), nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		for name, pkg := range pkgs {
			name = strings.TrimSuffix(name, "_test")
			if n.decls[name] == nil {
				n.decls[name], n.members[name] = map[string]bool{}, map[string]bool{}
			}
			for file, f := range pkg.Files {
				if f.Doc != nil && !strings.HasSuffix(file, "_test.go") {
					n.docs[dir] += f.Doc.Text()
				}
				for _, decl := range f.Decls {
					switch d := decl.(type) {
					case *ast.FuncDecl:
						if d.Recv != nil {
							n.members[name][d.Name.Name] = true
						} else {
							n.decls[name][d.Name.Name] = true
						}
					case *ast.GenDecl:
						for _, s := range d.Specs {
							switch s := s.(type) {
							case *ast.TypeSpec:
								n.decls[name][s.Name.Name] = true
								n.types[s.Name.Name] = append(n.types[s.Name.Name], name)
							case *ast.ValueSpec:
								for _, id := range s.Names {
									n.decls[name][id.Name] = true
								}
							}
						}
					}
				}
				ast.Inspect(f, func(node ast.Node) bool {
					switch x := node.(type) {
					case *ast.Ident:
						n.any[x.Name] = true
					case *ast.Field:
						for _, id := range x.Names {
							n.members[name][id.Name] = true
						}
					case *ast.BasicLit:
						if x.Kind == token.STRING {
							s, err := strconv.Unquote(x.Value)
							if err != nil {
								s = x.Value
							}
							for _, w := range word.FindAllString(s, -1) {
								n.any[w] = true
							}
						}
					}
					return true
				})
			}
		}
	}
	for _, e := range externalNames {
		n.any[e] = true
	}
	return n
}

// goDirs are the directories of the repository's Go packages, slash
// separated from its root.
func goDirs(t *testing.T) []string {
	t.Helper()
	var dirs []string
	for _, top := range []string{"cmd", "internal", "e2e"} {
		err := filepath.WalkDir(filepath.Join(repoRoot, top), func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return err
			}
			if gos, _ := filepath.Glob(filepath.Join(p, "*.go")); len(gos) > 0 {
				dirs = append(dirs, strings.TrimPrefix(filepath.ToSlash(p), repoRoot+"/"))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(dirs) < 2 {
		t.Fatal("no Go packages found")
	}
	return dirs
}

var (
	qualified = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)\.([A-Za-z_][A-Za-z0-9_]*)(?:\.([A-Za-z_][A-Za-z0-9_]*))?(?:\(.*\))?$`)
	// camelCase, or CamelCase of two humps at least (one hump is a word).
	camel = regexp.MustCompile(`^(?:[a-z][a-z0-9]*|[A-Z]+[a-z0-9]+)(?:[A-Z]+[a-z0-9]*)+(?:\(.*\))?$`)
)

// has says whether a code span is a Go name the source has; checked is
// false when the span looks like no Go name of the repository's.
func (n names) has(span string) (ok, checked bool) {
	if m := qualified.FindStringSubmatch(span); m != nil {
		if _, ours := n.decls[m[1]]; ours && m[1] != "main" {
			return n.decls[m[1]][m[2]] && (m[3] == "" || n.members[m[1]][m[3]]), true
		}
		if pkgs := n.types[m[1]]; len(pkgs) > 0 && m[3] == "" {
			for _, p := range pkgs {
				if n.members[p][m[2]] {
					return true, true
				}
			}
			return false, true
		}
		return false, false
	}
	if camel.MatchString(span) {
		name, _, _ := strings.Cut(span, "(")
		return n.any[name], true
	}
	return false, false
}
