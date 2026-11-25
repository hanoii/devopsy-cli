// Package layout lays out every column devopsy prints: the reports
// (--status, --server-info and the steps that print as they do), the
// lists (--snapshots, --releases, --ssh-keys, --project-users), help and
// --debug. One place measures a column, wraps prose and draws borders,
// so a fix to one reaches every other.
//
// It never picks a color or quotes a word: callers paint and quote their
// cells (report.Copyable) before they reach it, and it measures them as
// they show.
//
// Rules: the Output conventions in AGENTS.md.
package layout

import (
	"strings"

	"github.com/rivo/uniseg"
)

// Width is how many columns s takes on a terminal: grapheme clusters as
// uniseg measures them (a wide character two, a combining mark none),
// ANSI escape sequences (CSI, OSC) none.
func Width(s string) int {
	if !strings.Contains(s, "\x1b") {
		return uniseg.StringWidth(s)
	}
	return uniseg.StringWidth(Strip(s))
}

// Strip is s without its ANSI escape sequences: a CSI (ESC [ ... a
// final byte from @ to ~), an OSC (ESC ] ... BEL or ESC \), or any other
// ESC with the byte after it.
func Strip(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != 0x1b {
			b.WriteByte(s[i])
			continue
		}
		if i+1 >= len(s) {
			break
		}
		switch s[i+1] {
		case '[':
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = j
		case ']':
			j := i + 2
			for j < len(s) {
				if s[j] == 0x07 {
					break
				}
				if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
					j++
					break
				}
				j++
			}
			i = j
		default:
			i++
		}
	}
	return b.String()
}

// Pad is s with spaces after it to n columns (Width); s as it is when
// it is as wide already.
func Pad(s string, n int) string {
	return s + strings.Repeat(" ", max(0, n-Width(s)))
}

// padLeft is s with spaces before it to n columns.
func padLeft(s string, n int) string {
	return strings.Repeat(" ", max(0, n-Width(s))) + s
}

// Wrap cuts text at words into parts at most room columns wide: the
// first keeps the text's leading spaces, the others start bare (the
// caller hangs them); a word wider than the room stays whole on a part
// of its own. It splits at single spaces, so a run of spaces inside the
// text stays as it is unless a part ends in it, and every part is
// trimmed on its right. Text it wraps is plain: color comes after.
func Wrap(text string, room int) []string {
	body := strings.TrimLeft(text, " ")
	lead := text[:len(text)-len(body)]
	var parts []string
	cur := ""
	for _, w := range strings.Split(body, " ") {
		switch {
		case cur == "":
			// The first word, or the next after a break (a space at the
			// break goes with it).
			cur = w
		case Width(cur)+1+Width(w) <= room:
			cur += " " + w
		default:
			parts = append(parts, strings.TrimRight(cur, " "))
			cur = w
		}
	}
	parts = append(parts, strings.TrimRight(cur, " "))
	parts[0] = lead + parts[0]
	return parts
}
