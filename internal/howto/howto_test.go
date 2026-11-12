package howto

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// docsPage is the generated page, from this package's directory.
const docsPage = "../../docs/server-setup-howto.md"

// placeholder is a word to replace in a command (<topic>, <your server>), never a redirection
// or a heredoc: a command is copied as it is, a value to choose being a variable set on the
// step's first line.
var placeholder = regexp.MustCompile(`<[A-Za-z][A-Za-z0-9 _-]*>`)

// Every topic has what a reader needs (a summary, a title, why, steps, a check), a command to
// copy holds no placeholder, and the names are unique.
func TestTopics(t *testing.T) {
	t.Parallel()
	seen := map[Name]bool{}
	for _, tp := range Topics {
		if tp.Name == "" || seen[tp.Name] {
			t.Errorf("a topic named %q, empty or twice", tp.Name)
		}
		seen[tp.Name] = true
		if tp.Summary == "" || tp.Title == "" || tp.Why == "" || len(tp.Steps) == 0 || tp.Check.Text == "" {
			t.Errorf("%s: a summary, a title, why, steps and a check, each: %+v", tp.Name, tp)
		}
		for i, s := range append(slices.Clone(tp.Steps), tp.Check) {
			if s.Text == "" {
				t.Errorf("%s: step %d says nothing", tp.Name, i+1)
			}
			for _, c := range s.Cmds {
				if placeholder.MatchString(c) || strings.HasPrefix(c, "$ ") || strings.TrimSpace(c) != c || c == "" {
					t.Errorf("%s: step %d's command cannot be copied as it is: %q", tp.Name, i+1, c)
				}
			}
		}
	}
}

// Every constant of the package's topics names a topic, and every topic is a constant: a fix
// names a topic by its constant (Command), so one that does not exist fails here, not in a
// reader's terminal.
func TestConstantsAreTopics(t *testing.T) {
	t.Parallel()
	f, err := parser.ParseFile(token.NewFileSet(), "topics.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var consts []string
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, spec := range g.Specs {
			for _, v := range spec.(*ast.ValueSpec).Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok {
					t.Fatalf("a topic's constant that is no string: %#v", v)
				}
				name, _ := strconv.Unquote(lit.Value)
				consts = append(consts, name)
			}
		}
	}
	for _, c := range consts {
		if _, ok := Find(c); !ok {
			t.Errorf("the constant %q names no topic", c)
		}
	}
	for _, n := range Names() {
		if !slices.Contains(consts, n) {
			t.Errorf("the topic %q is no constant of topics.go", n)
		}
	}
}

// No code but this package spells the flag: the flags' registry takes Flag, and every fix offers a
// topic through Command, whose name the test above checks.
func TestFlagSpelledOnce(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{"../../cmd", "../../internal"} {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
			if strings.HasPrefix(rel, "internal/howto/") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(data), "\n") {
				if strings.Contains(line, Flag) && !strings.HasPrefix(strings.TrimSpace(line), "//") {
					t.Errorf("%s:%d spells %s: name it by howto.Flag or howto.Command", rel, i+1, Flag)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// docs/server-setup-howto.md is what the flag prints with --all, whole: the page and the
// binary never say a topic two ways, and a change to a topic changes the page in the same
// commit (go run ./cmd/devopsy --server-setup-howto --all > docs/server-setup-howto.md).
func TestDocsPage(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(docsPage)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != All() {
		t.Errorf("%s is not what %s %s prints", docsPage, Flag, AllFlag)
	}
}

// Every topic together: one title, a table of contents linking each topic's section, named
// as the topic is so a link to the page's #<name> holds, then each topic's title and body.
func TestAll(t *testing.T) {
	t.Parallel()
	all := All()
	if !strings.HasPrefix(all, "# Server setup how-to\n\n") || strings.Count(all, "\n# ") != 0 {
		t.Errorf("not one title:\n%s", all)
	}
	for _, tp := range Topics {
		n := string(tp.Name)
		if !strings.Contains(all, "\n- ["+n+"](#"+n+"): "+tp.Summary+"\n") {
			t.Errorf("%s is not in the table of contents", n)
		}
		if !strings.Contains(all, "\n## "+n+"\n\n"+tp.Title+"\n\n"+tp.Body()) {
			t.Errorf("%s has no section with its title and body", n)
		}
	}
}

// A topic as the flag prints it, Markdown that reads plainly: its title a heading, why, the
// steps numbered, each step's commands as they are in a fenced block under it, the check
// last; its words within 80 columns. Its opening says where and as whom the steps run, who
// the deploy user is, and where a value to change is set.
func TestText(t *testing.T) {
	t.Parallel()
	for _, words := range []string{"as root on the server", "Debian or Ubuntu with systemd", "The deploy user is the Unix user devopsy logs in as", "first line sets the values to change", "user=devopsy"} {
		if !strings.Contains(asRoot, words) {
			t.Errorf("the steps' opening does not say %q: %s", words, asRoot)
		}
	}
	for _, tp := range Topics {
		text := tp.Text()
		if !strings.HasPrefix(text, "# "+tp.Title+"\n\n") || !strings.Contains(text, wrap(asRoot, "", "")) || !strings.Contains(text, "\nTo check it: ") {
			t.Errorf("%s:\n%s", tp.Name, text)
		}
		var cmds []string
		for _, s := range append(slices.Clone(tp.Steps), tp.Check) {
			cmds = append(cmds, s.Cmds...)
		}
		for _, c := range cmds {
			if !strings.Contains(text, "\n"+stepIndent+c+"\n") && !strings.Contains(text, "\n"+c+"\n") {
				t.Errorf("%s: %q not on a line of its own", tp.Name, c)
			}
		}
		if n := strings.Count(text, fence); n%2 != 0 || n != 2*blocks(tp) {
			t.Errorf("%s: %d fences for %d blocks of commands", tp.Name, n, blocks(tp))
		}
		for i := range tp.Steps {
			if !strings.Contains(text, "\n"+strconv.Itoa(i+1)+". ") {
				t.Errorf("%s: no step %d", tp.Name, i+1)
			}
		}
		in := false
		for _, l := range strings.Split(text, "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), fence) {
				in = !in
			} else if !in && len(l) > 80 {
				t.Errorf("%s: %q", tp.Name, l)
			}
		}
	}
}

// The list names every topic with its summary, aligned, a Markdown list, and the command
// that prints one, and all.
// blocks counts a topic's steps that have commands, the check among them.
func blocks(tp Topic) int {
	n := 0
	for _, s := range append(slices.Clone(tp.Steps), tp.Check) {
		if len(s.Cmds) > 0 {
			n++
		}
	}
	return n
}

func TestList(t *testing.T) {
	t.Parallel()
	list := List()
	for _, tp := range Topics {
		if !regexp.MustCompile("\n- `" + regexp.QuoteMeta(string(tp.Name)) + "` +" + regexp.QuoteMeta(tp.Summary) + `\n`).MatchString(list) {
			t.Errorf("%s missing:\n%s", tp.Name, list)
		}
	}
	for _, l := range strings.Split(list, "\n") {
		if len(l) > 80 {
			t.Errorf("over 80 columns: %q", l)
		}
	}
	if !strings.Contains(list, "devopsy "+Flag) || !strings.Contains(list, AllFlag) {
		t.Errorf("the list does not say how to print a topic:\n%s", list)
	}
	if got := Command(Processes); got != "devopsy --server-setup-howto processes" {
		t.Errorf("Command: %q", got)
	}
}
