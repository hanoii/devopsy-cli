package layout

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// treeCopy is g with every row of it and of the grids in its cells changed
// by f, told whether its grid is the root or draws a box, g left as it is.
func treeCopy(g *Grid, f func(r *Row, rootOrBox bool)) *Grid {
	return gridCopy(g, f, true, false)
}

func gridCopy(g *Grid, f func(*Row, bool), root, inBox bool) *Grid {
	c := *g
	c.Rows = make([]Row, len(g.Rows))
	box := g.Border == Box && !inBox
	for i, r := range g.Rows {
		r.Cells = append([]Cell(nil), r.Cells...)
		for k := range r.Cells {
			if r.Cells[k].Grid != nil {
				r.Cells[k].Grid = gridCopy(r.Cells[k].Grid, f, false, inBox || box)
			}
		}
		f(&r, root || box)
		c.Rows[i] = r
	}
	return &c
}

// guidesOnBlanks says where guided, a grid's lines with its guides, differs
// from plain, the same grid's with none and each row's indent as wide as
// its guides, other than by a guide in a blank column, "" when nowhere.
func guidesOnBlanks(guided, plain []string) string {
	if len(guided) != len(plain) {
		return fmt.Sprintf("%d lines with guides, %d without", len(guided), len(plain))
	}
	for i := range guided {
		g, p := []rune(guided[i]), []rune(plain[i])
		if len(g) < len(p) {
			return fmt.Sprintf("line %d shorter with guides", i)
		}
		for x := range g {
			if x < len(p) && g[x] == p[x] {
				continue
			}
			// Past the line's end without guides, a guide's spaces before it.
			if (x < len(p) && p[x] != ' ') || !strings.ContainsRune("├└│─ ", g[x]) || x < len(p) && g[x] == ' ' {
				return fmt.Sprintf("line %d col %d: %q with guides, %q without", i, x, guided[i], plain[i])
			}
		}
	}
	return ""
}

// Random grids and reports, their rows at random levels of a tree, some
// continuing their node: the guides only fill columns that are blank
// without them, the grid laid out otherwise as with each row's indent as
// wide as its guides, which the other random tests hold to the contract.
// The trees are the root's and the boxes': a grid in a cell (a box's too) is
// measured as it draws, its guides included, which widen it where one
// runs past every line's end; and an underline is as wide as its row's
// lines, a grid's guides in them counted, so none is drawn.
func TestTreeGuidesAtRandom(t *testing.T) {
	t.Parallel()
	roots := map[string]func(seed int64) *Grid{
		"grid":   func(seed int64) *Grid { g, _ := randomGrid(seed, randomAll); return g },
		"report": func(seed int64) *Grid { g, _ := reportReport(seed, seed%2 == 0); return g },
		"max":    func(seed int64) *Grid { g, _ := maxWidthReport(seed, seed%2 == 0); return g },
	}
	for name, root := range roots {
		for seed := range int64(300) {
			rnd := rand.New(rand.NewSource(seed))
			tree := treeCopy(root(seed), func(r *Row, inTree bool) {
				r.Underline = false
				if inTree && rnd.Float64() < 0.6 {
					r.Level = rnd.Intn(4)
					r.Continues = rnd.Float64() < 0.3
				}
			})
			plain := treeCopy(tree, func(r *Row, _ bool) {
				r.Indent += guideWidth * r.Level
				r.Level, r.Continues = 0, false
			})
			for _, room := range []int{0, 120, 60, 30, rnd.Intn(60)} {
				tree.Width, plain.Width = room, room
				if msg := guidesOnBlanks(tree.Lines(), plain.Lines()); msg != "" {
					t.Fatalf("%s seed %d room %d: %s\n%s", name, seed, room, msg, tree.String())
				}
			}
		}
	}
}
