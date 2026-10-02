package main

import (
	"strings"

	"github.com/mattn/go-runewidth"
)

// sanitize makes a cell value printable on one line.
func sanitize(s string) string {
	if !strings.ContainsAny(s, "\n\r\t\x00\x1b") {
		return s
	}
	r := strings.NewReplacer("\r\n", "↵", "\n", "↵", "\r", "↵", "\t", " ", "\x00", "?", "\x1b", "?")
	return r.Replace(s)
}

// sanitizeASCII is like sanitize but only uses ASCII replacements.
func sanitizeASCII(s string) string {
	if !strings.ContainsAny(s, "\n\r\t\x00\x1b") {
		return s
	}
	r := strings.NewReplacer("\r\n", "\\n", "\n", "\\n", "\r", "\\r", "\t", " ", "\x00", "?", "\x1b", "?")
	return r.Replace(s)
}

func strWidth(s string) int { return runewidth.StringWidth(s) }

// truncate cuts s to at most w display cells, adding an ellipsis if cut.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= w {
		return s
	}
	if w == 1 {
		return "…"
	}
	return runewidth.Truncate(s, w, "…")
}

// truncateASCII cuts with a "~" marker instead of a unicode ellipsis.
func truncateASCII(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if runewidth.StringWidth(s) <= w {
		return s
	}
	return runewidth.Truncate(s, w, "~")
}

// padRight pads s with spaces to exactly w display cells (truncating if longer).
func padRight(s string, w int) string {
	s = truncate(s, w)
	if d := w - runewidth.StringWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

func padLeft(s string, w int) string {
	s = truncate(s, w)
	if d := w - runewidth.StringWidth(s); d > 0 {
		return strings.Repeat(" ", d) + s
	}
	return s
}

// isNumeric reports whether a cell should be right-aligned.
func isNumeric(c Cell) bool {
	switch c.V.(type) {
	case int64, float64:
		return true
	}
	return false
}

// RenderTable renders a result as a text table. ascii selects +-| borders
// and ASCII-only escapes; otherwise box-drawing characters are used.
func RenderTable(r *Result, ascii bool, maxColWidth int) string {
	if r == nil || len(r.Columns) == 0 {
		return ""
	}
	clean := sanitize
	cut := truncate
	if ascii {
		clean = sanitizeASCII
		cut = truncateASCII
	}
	ncol := len(r.Columns) + 1
	headers := append([]string{"#"}, r.Columns...)
	widths := make([]int, ncol)
	for i, h := range headers {
		widths[i] = strWidth(clean(h))
	}
	cells := make([][]string, len(r.Rows))
	for ri, row := range r.Rows {
		line := make([]string, ncol)
		line[0] = itoa(ri + 1)
		for ci, c := range row {
			line[ci+1] = cut(clean(c.S), maxColWidth)
		}
		for i, s := range line {
			if w := strWidth(s); w > widths[i] {
				widths[i] = w
			}
		}
		cells[ri] = line
	}
	for i := range widths {
		if maxColWidth > 0 && widths[i] > maxColWidth {
			widths[i] = maxColWidth
		}
	}
	var h, v, tl, tm, tr, ml, mm, mr, bl, bm, br string
	if ascii {
		h, v = "-", "|"
		tl, tm, tr, ml, mm, mr, bl, bm, br = "+", "+", "+", "+", "+", "+", "+", "+", "+"
	} else {
		h, v = "─", "│"
		tl, tm, tr, ml, mm, mr, bl, bm, br = "┌", "┬", "┐", "├", "┼", "┤", "└", "┴", "┘"
	}
	sep := func(l, m, rr string) string {
		var b strings.Builder
		b.WriteString(l)
		for i, w := range widths {
			b.WriteString(strings.Repeat(h, w+2))
			if i < len(widths)-1 {
				b.WriteString(m)
			}
		}
		b.WriteString(rr)
		b.WriteString("\n")
		return b.String()
	}
	pad := func(s string, w int, right bool) string {
		if d := w - strWidth(s); d > 0 {
			if right {
				return strings.Repeat(" ", d) + s
			}
			return s + strings.Repeat(" ", d)
		}
		return s
	}
	var b strings.Builder
	b.WriteString(sep(tl, tm, tr))
	b.WriteString(v)
	for i, hd := range headers {
		b.WriteString(" " + pad(cut(clean(hd), widths[i]), widths[i], false) + " " + v)
	}
	b.WriteString("\n")
	b.WriteString(sep(ml, mm, mr))
	for ri, line := range cells {
		b.WriteString(v)
		for i, s := range line {
			right := i == 0 || (i > 0 && isNumeric(r.Rows[ri][i-1]))
			b.WriteString(" " + pad(s, widths[i], right) + " " + v)
		}
		b.WriteString("\n")
	}
	b.WriteString(sep(bl, bm, br))
	return b.String()
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	p := len(buf)
	for i > 0 {
		p--
		buf[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		buf[p] = '-'
	}
	return string(buf[p:])
}
