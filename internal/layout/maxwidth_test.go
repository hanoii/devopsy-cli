package layout

import (
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// maxWidthReport is a report at random (reportReport) whose boxes have a
// MaxWidth at times, at their room or past it, and whose text cells are at
// times one word no room holds, in any column: what reportGen does not make.
func maxWidthReport(seed int64, flat bool) (*Grid, *reportGen) {
	root, gen := reportReport(seed, flat)
	rnd := rand.New(rand.NewSource(seed + 7919))
	n := 0
	var walk func(g *Grid)
	walk = func(g *Grid) {
		for i := range g.Rows {
			for k := range g.Rows[i].Cells {
				c := &g.Rows[i].Cells[k]
				if c.Grid != nil {
					walk(c.Grid)
					continue
				}
				if c.Text == "" || rnd.Float64() >= 0.12 {
					continue
				}
				n++
				w := "L" + strconv.Itoa(n) + strings.Repeat("y", 15+rnd.Intn(90))
				for _, old := range strings.Fields(c.Text) {
					for j, x := range gen.words {
						if x == old {
							gen.words[j] = w
						}
					}
				}
				gen.desc = append(gen.desc, fmt.Sprintf("%q now %q", c.Text, w))
				c.Text = w
			}
		}
	}
	walk(root)
	for i, b := range reportBoxes(root) {
		switch rnd.Intn(4) {
		case 0:
		case 1:
			b.MaxWidth = 100
		default:
			b.MaxWidth = 100 + rnd.Intn(60)
		}
		gen.desc = append(gen.desc, fmt.Sprintf("box %d MaxWidth=%d", i, b.MaxWidth))
	}
	return root, gen
}

// sided says whether a box's line ends in its side or a corner.
func sided(l string) bool {
	for _, end := range []string{"│", "┤", "╮", "╯"} {
		if strings.HasSuffix(l, end) {
			return true
		}
	}
	return false
}

// maxWidthBreak says what in root's boxes, drawn in room, is not as
// MaxWidth says: every word shown, a second render alike, each line of a
// box as wide as the box and ending in its side but one that runs past it,
// which a box with no MaxWidth has none of, which fits no box of that
// MaxWidth, and beside which the box is as wide as its MaxWidth; the lines
// joining once those are cut at the side.
func maxWidthBreak(root *Grid, gen *reportGen, room int) string {
	root.Width = room
	lines := root.Lines()
	all := strings.Join(lines, "\n")
	for _, w := range gen.words {
		if !strings.Contains(all, w) {
			return "word " + w + " missing"
		}
	}
	if again := strings.Join(root.Lines(), "\n"); again != all {
		return "second render differs"
	}
	if msg := settledBreak(root); msg != "" {
		return msg
	}
	root.Lines()
	boxes := reportBoxes(root)
	most := map[*Widths]int{}
	for _, b := range boxes {
		w := b.shapeOf().w
		if b.MaxWidth > 0 && (most[w] == 0 || b.MaxWidth < most[w]) {
			most[w] = b.MaxWidth
		}
	}
	for i, b := range boxes {
		bl := b.render(b.fit)
		if len(bl) == 0 {
			continue
		}
		wide, m := Width(bl[0]), most[b.shapeOf().w]
		past := 0
		cut := make([]string, len(bl))
		for j, l := range bl {
			n := Width(l)
			cut[j] = l
			if n == wide && sided(l) {
				continue
			}
			past++
			cut[j] = string([]rune(Pad(l, wide))[:wide-1]) + boxSide
			switch {
			case sided(l):
				return fmt.Sprintf("box %d line %d: %d wide with its side, the box %d", i, j, n, wide)
			case n < wide-1:
				return fmt.Sprintf("box %d line %d: %d wide, the box %d", i, j, n, wide)
			case m == 0:
				return fmt.Sprintf("box %d line %d runs past the side with no MaxWidth", i, j)
			case n+boxPadding+sideWidth <= m:
				return fmt.Sprintf("box %d line %d runs past the side, %d wide, within its MaxWidth, %d", i, j, n, m)
			}
		}
		// A line runs past only a box at its MaxWidth, or past it already for
		// what it holds otherwise.
		if past > 0 && wide < m {
			return fmt.Sprintf("box %d: %d lines run past its side, the box %d wide, its MaxWidth %d", i, past, wide, m)
		}
		if msg := boxJoined(cut); msg != "" {
			return fmt.Sprintf("box %d: %s", i, msg)
		}
		if msg := boxesSquare(cut); msg != "" {
			return fmt.Sprintf("box %d: %s", i, msg)
		}
	}
	return ""
}

// Reports in boxes with a MaxWidth and words no room holds, at random,
// flat and with grids in their cells: a line runs past its box's side only
// where the box, at its MaxWidth, cannot hold it, and the rest of the box
// is whole.
func TestBoxesWithAMaxWidthAtRandom(t *testing.T) {
	t.Parallel()
	for _, flat := range []bool{true, false} {
		for seed := range 150 {
			root, gen := maxWidthReport(int64(seed), flat)
			for _, room := range []int{100, 60, 30} {
				if msg := maxWidthBreak(root, gen, room); msg != "" {
					root.Width = room
					out := root.String()
					if len(out) > 6000 {
						out = "(long)"
					}
					t.Fatalf("flat %v, seed %d, a room of %d: %s\n%s\n%s", flat, seed, room, msg, strings.Join(gen.desc, "\n"), out)
				}
			}
		}
	}
}

// A box past its MaxWidth for a column before its last keeps its sides: no
// line runs past one unless the box is as wide as its MaxWidth.
func TestBoxMaxWidthWithAWideEarlierColumn(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("an-image-name-", 4)
	for name, g := range map[string]*Grid{
		"the first column": {Border: Box, Width: 40, MaxWidth: 40, Rows: []Row{
			{Cells: Cells("NAME", "SIZE")},
			{Cells: Cells(long, "12 MB")},
		}},
		"a middle column": {Border: Box, Width: 40, MaxWidth: 40, Rows: []Row{
			{Cells: Cells("NAME", "IMAGE", "SIZE")},
			{Cells: Cells("app", long, "12 MB")},
		}},
		"a minimum": {Border: Box, Width: 40, MaxWidth: 40, Widths: NewWidths().Min(0, 50), Rows: []Row{
			{Cells: Cells("NAME", "SIZE")},
			{Cells: Cells("app", "12 MB")},
		}},
	} {
		lines := g.Lines()
		wide := Width(lines[0])
		for i, l := range lines {
			if wide > g.MaxWidth && (!sided(l) || Width(l) != wide) {
				t.Errorf("%s: the box is %d wide, past its MaxWidth of %d, and line %d has no side at it:\n%s", name, wide, g.MaxWidth, i, strings.Join(lines, "\n"))
				break
			}
		}
	}
}

// A grid before a box's last column keeps its width when the box cannot fit
// its room anyway, and prepare settles.
func TestBoxGridBeforeItsLastColumnSettles(t *testing.T) {
	t.Parallel()
	prose := "some prose of several words"
	heading := strings.Repeat("a heading ", 9) + "kept whole"
	sub := func() *Grid {
		return &Grid{Rows: []Row{{Cells: []Cell{{Text: prose, Fit: WrapWords}}}}}
	}
	for name, g := range map[string]*Grid{
		// The heading alone is a column wider than the room.
		"a heading wider than the room": {Border: Box, Width: len(heading) + 3, Rows: []Row{
			{Cells: Cells(heading)},
			{Cells: Cells("NAME", "NOTE", "SIZE")},
			{Cells: []Cell{{Text: "app"}, {Grid: sub()}}},
		}},
		"a last column at MaxWidth": {Border: Box, Width: 50, MaxWidth: 60, Rows: []Row{
			{Cells: Cells("NAME", "NOTE", strings.Repeat("an-image-name-", 4))},
			{Cells: []Cell{{Text: "app"}, {Grid: sub()}}},
		}},
	} {
		lines := g.Lines()
		if msg := settledBreak(g); msg != "" {
			t.Errorf("%s: %s:\n%s", name, msg, strings.Join(lines, "\n"))
		}
		whole := false
		for _, l := range lines {
			whole = whole || strings.Contains(l, "of several")
		}
		if !whole {
			t.Errorf("%s: the grid's prose is cut to a word a line, the box no narrower for it:\n%s", name, strings.Join(lines, "\n"))
		}
	}
}

// A grid in a right-aligned last column, one of its lines past the box's
// MaxWidth: only that line runs past the side, every other ends at it.
func TestBoxRightAlignedGridPastMaxWidthKeepsItsSide(t *testing.T) {
	t.Parallel()
	sub := &Grid{Rows: []Row{
		{Cells: Cells("a", strings.Repeat("an-image-name-", 4))},
		{Cells: Cells("abc", "d")},
	}}
	g := &Grid{Border: Box, Width: 40, MaxWidth: 40, Widths: NewWidths().Right(1), Rows: []Row{
		{Cells: Cells("k", "v")},
		{Cells: []Cell{{Text: "key"}, {Grid: sub}}},
	}}
	lines := g.Lines()
	for i, l := range lines {
		if sided(l) && Width(l) != Width(lines[0]) {
			t.Errorf("line %d has its side at %d, the box's is at %d:\n%s", i, Width(l), Width(lines[0]), strings.Join(lines, "\n"))
		}
	}
}

// Prose runs past no side: beside a column wider than the room, it wraps in
// the column a box at its MaxWidth leaves it.
func TestBoxProseBesideAWideColumnWrapsInsideMaxWidth(t *testing.T) {
	t.Parallel()
	g := &Grid{Border: Box, Width: 40, MaxWidth: 100, Rows: []Row{
		{Cells: Cells("NAME", "IMAGE", "NOTE")},
		{Cells: []Cell{{Text: "app"}, {Text: strings.Repeat("an-image-name-", 4)}, {Text: "some prose of several words, that wraps wherever it has the room to", Fit: WrapWords}}},
	}}
	lines := g.Lines()
	for i, l := range lines {
		if !sided(l) || Width(l) != Width(lines[0]) {
			t.Errorf("line %d has no side at the box's, its prose not wrapped in its column:\n%s", i, strings.Join(lines, "\n"))
			break
		}
	}
}
