package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Nothing this package renders may be wider than the terminal or taller than
// it. A single line that overflows wraps, every line after it shifts, and the
// alternate screen ends up showing torn frames -- which is what "the TUI is
// garbled" and "scrolling does nothing" both turn out to be. So every line
// goes through fitLine and every screen through fitScreen, rather than each
// view being trusted to have done its own arithmetic right.

// fitLine truncates one line to w cells, ANSI-aware so a style sequence is
// never cut in half.
func fitLine(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= w {
		return s
	}
	return ansi.Truncate(s, w, "…")
}

// fitBlock truncates every line of a block.
func fitBlock(s string, w int) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = fitLine(l, w)
	}
	return strings.Join(lines, "\n")
}

// fitScreen makes a block exactly h lines of at most w cells: the last thing
// every View does, so the frame the terminal receives always matches the
// window it is painted into.
func fitScreen(s string, w, h int) string {
	if h <= 0 || w <= 0 {
		return ""
	}
	lines := strings.Split(fitBlock(s, w), "\n")
	if len(lines) > h {
		lines = lines[:h]
	}
	for len(lines) < h {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

// window is a scroll offset over a list of rows. It is deliberately dumb:
// the caller says how many rows fit and where the cursor is, and it answers
// which slice to draw, keeping the cursor on screen.
type window struct {
	off int
}

// slice returns the visible range [from, to) for n rows in h lines, with the
// cursor kept inside it.
func (v *window) slice(n, h, cursor int) (from, to int) {
	if h <= 0 || n <= 0 {
		return 0, 0
	}
	if h >= n {
		v.off = 0
		return 0, n
	}
	// Follow the cursor: scroll only far enough to bring it back into view,
	// so the list does not jump when the selection moves by one.
	if cursor < v.off {
		v.off = cursor
	}
	if cursor >= v.off+h {
		v.off = cursor - h + 1
	}
	if v.off > n-h {
		v.off = n - h
	}
	if v.off < 0 {
		v.off = 0
	}
	return v.off, v.off + h
}

// scrollBy moves the window without moving a cursor, for the mouse wheel and
// the page keys on a screen that has no selection (the log).
func (v *window) scrollBy(delta, n, h int) {
	if h >= n {
		v.off = 0
		return
	}
	v.off += delta
	if v.off > n-h {
		v.off = n - h
	}
	if v.off < 0 {
		v.off = 0
	}
}

// atBottom reports whether the window is showing the end of the list, which
// is what "still following" means for a log.
func (v *window) atBottom(n, h int) bool { return h >= n || v.off >= n-h }

// scrollHint is the "12-31 of 40" a person needs to know there is more.
func scrollHint(from, to, n int) string {
	if n == 0 || (from == 0 && to >= n) {
		return ""
	}
	return "showing " + itoa(from+1) + "-" + itoa(to) + " of " + itoa(n)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
