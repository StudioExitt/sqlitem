package main

import (
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// editor.go: the SQL editor buffer, INSERT/REPLACE modes and rendering.
// NORMAL/VISUAL command handling (the vi engine) lives in vi.go.

type edMode int

const (
	modeInsert edMode = iota
	modeNormal
	modeVisual
	modeVisualLine
	modeReplace
)

func (m edMode) String() string {
	return [...]string{"INSERT", "NORMAL", "VISUAL", "V-LINE", "REPLACE"}[m]
}

type edSnap struct {
	lines    [][]rune
	row, col int
}

const (
	maxUndo    = 500
	shiftWidth = 2
)

type editorModel struct {
	lines    [][]rune
	row, col int
	mode     edMode
	vrow     int // visual anchor
	vcol     int
	undo     []edSnap
	redo     []edSnap
	snapped  bool // undo snapshot already taken for this INSERT session
	top      int
	left     int // horizontal scroll in display cells
	height   int
	reg      string
	regLine  bool
	version  int
	clsVer   int
	clsCache [][]uint8

	vi viState // NORMAL/VISUAL command state (vi.go)

	// msg is a one-shot status message for the root model (e.g. "pattern
	// not found"); msgErr marks it as an error.
	msg    string
	msgErr bool
}

func newEditorModel() *editorModel {
	return &editorModel{lines: [][]rune{{}}, mode: modeInsert, clsVer: -1, vi: newViState()}
}

func (e *editorModel) Text() string {
	parts := make([]string, len(e.lines))
	for i, l := range e.lines {
		parts[i] = string(l)
	}
	return strings.Join(parts, "\n")
}

// setText replaces the buffer (undoable) and moves the cursor to the end.
func (e *editorModel) setText(s string, undoable bool) {
	if undoable {
		e.pushUndo()
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	parts := strings.Split(s, "\n")
	e.lines = make([][]rune, len(parts))
	for i, p := range parts {
		e.lines[i] = []rune(p)
	}
	// drop a trailing empty line produced by a final newline
	if len(e.lines) > 1 && len(e.lines[len(e.lines)-1]) == 0 {
		e.lines = e.lines[:len(e.lines)-1]
	}
	e.row = len(e.lines) - 1
	e.col = len(e.lines[e.row])
	e.snapped = false
	e.changed()
}

func (e *editorModel) changed() { e.version++ }

// typing reports whether keys are inserted as text (INSERT / REPLACE).
func (e *editorModel) typing() bool { return e.mode == modeInsert || e.mode == modeReplace }

func (e *editorModel) title() string {
	t := "SQL [" + e.mode.String() + "] " + itoa(e.row+1) + ":" + itoa(e.col+1)
	if e.vi.search.active {
		return t + "  /" + string(e.vi.search.buf) + "█"
	}
	if sc := e.vi.showcmd(); sc != "" {
		t += "  " + sc
	}
	return t
}

func (e *editorModel) setMsg(s string, isErr bool) { e.msg, e.msgErr = s, isErr }

// cursorOffset returns the byte offset of the cursor within Text().
func (e *editorModel) cursorOffset() int {
	off := 0
	for i := 0; i < e.row; i++ {
		off += len(string(e.lines[i])) + 1
	}
	c := min(e.col, len(e.lines[e.row]))
	return off + len(string(e.lines[e.row][:c]))
}

// currentStatement returns the statement under the cursor.
func (e *editorModel) currentStatement() string {
	stmts := splitStatements(e.Text())
	i := statementAt(stmts, e.cursorOffset())
	if i < 0 {
		return ""
	}
	return stmts[i].Text
}

// ---------------------------------------------------------------- undo / redo

func (e *editorModel) snapshot() edSnap {
	cp := make([][]rune, len(e.lines))
	for i, l := range e.lines {
		cp[i] = append([]rune(nil), l...)
	}
	return edSnap{lines: cp, row: e.row, col: e.col}
}

// pushUndo records the state before a change; a new change clears redo.
func (e *editorModel) pushUndo() {
	e.undo = append(e.undo, e.snapshot())
	if len(e.undo) > maxUndo {
		e.undo = e.undo[len(e.undo)-maxUndo:]
	}
	e.redo = nil
}

// beforeInsertEdit takes one undo snapshot per INSERT session.
func (e *editorModel) beforeInsertEdit() {
	if !e.snapped {
		e.pushUndo()
		e.snapped = true
	}
}

// restore installs a snapshot; like vi the cursor goes to the first line
// that the undo/redo changed (or the saved position if that line changed).
func (e *editorModel) restore(s edSnap) {
	diff := -1
	for r := 0; r < max(len(e.lines), len(s.lines)); r++ {
		if r >= len(e.lines) || r >= len(s.lines) || string(e.lines[r]) != string(s.lines[r]) {
			diff = r
			break
		}
	}
	e.lines, e.row, e.col = s.lines, s.row, s.col
	if diff >= 0 && diff != s.row {
		e.row = min(diff, len(e.lines)-1)
		e.col = firstNonBlank(e.lines[e.row])
	}
	e.clampNormal()
	e.changed()
}

func (e *editorModel) doUndo() bool {
	if len(e.undo) == 0 {
		e.setMsg("already at oldest change", false)
		return false
	}
	e.redo = append(e.redo, e.snapshot())
	s := e.undo[len(e.undo)-1]
	e.undo = e.undo[:len(e.undo)-1]
	e.restore(s)
	return true
}

func (e *editorModel) doRedo() bool {
	if len(e.redo) == 0 {
		e.setMsg("already at newest change", false)
		return false
	}
	e.undo = append(e.undo, e.snapshot())
	s := e.redo[len(e.redo)-1]
	e.redo = e.redo[:len(e.redo)-1]
	e.restore(s)
	return true
}

// ---------------------------------------------------------------- cursor helpers

func (e *editorModel) lineLen() int { return len(e.lines[e.row]) }

// clampNormal keeps the cursor on a character (NORMAL/VISUAL semantics).
func (e *editorModel) clampNormal() {
	if e.row >= len(e.lines) {
		e.row = len(e.lines) - 1
	}
	if e.row < 0 {
		e.row = 0
	}
	maxc := e.lineLen() - 1
	if e.typing() {
		maxc = e.lineLen()
	}
	if e.col > maxc {
		e.col = maxc
	}
	if e.col < 0 {
		e.col = 0
	}
}

func (e *editorModel) enterInsert() {
	e.mode = modeInsert
	e.snapped = false
}

func (e *editorModel) exitVisual() {
	if e.mode == modeVisual || e.mode == modeVisualLine {
		e.mode = modeNormal
		e.clampNormal()
	}
}

// toIndex / fromIndex convert between (row, col) and an index into the
// buffer flattened with '\n' line separators.
func (e *editorModel) toIndex(r, c int) int {
	idx := 0
	for i := 0; i < r; i++ {
		idx += len(e.lines[i]) + 1
	}
	return idx + c
}

func (e *editorModel) fromIndex(idx int) (int, int) {
	if idx < 0 {
		return 0, 0
	}
	for i, l := range e.lines {
		if idx <= len(l) {
			return i, idx
		}
		idx -= len(l) + 1
	}
	last := len(e.lines) - 1
	return last, len(e.lines[last])
}

func (e *editorModel) flatRunes() []rune { return []rune(e.Text()) }

func (e *editorModel) setLinesFromRunes(rs []rune) {
	parts := strings.Split(string(rs), "\n")
	e.lines = make([][]rune, len(parts))
	for i, p := range parts {
		e.lines[i] = []rune(p)
	}
	e.changed()
}

func firstNonBlank(l []rune) int {
	for i, r := range l {
		if !unicode.IsSpace(r) {
			return i
		}
	}
	return 0
}

func leadingSpaces(l []rune) []rune {
	var out []rune
	for _, r := range l {
		if r != ' ' {
			break
		}
		out = append(out, r)
	}
	return out
}

// ---------------------------------------------------------------- editing primitives

func (e *editorModel) insertRunes(rs []rune) {
	for _, r := range rs {
		switch r {
		case '\r':
			continue
		case '\n':
			e.splitLine(false)
		default:
			if r == '\t' {
				r = ' '
			}
			if r < 0x20 {
				continue
			}
			l := e.lines[e.row]
			c := min(e.col, len(l))
			nl := make([]rune, 0, len(l)+1)
			nl = append(nl, l[:c]...)
			nl = append(nl, r)
			nl = append(nl, l[c:]...)
			e.lines[e.row] = nl
			e.col = c + 1
		}
	}
	e.changed()
}

// splitLine breaks the line at the cursor; with indent the new line copies
// the leading whitespace of the current one.
func (e *editorModel) splitLine(indent bool) {
	l := e.lines[e.row]
	c := min(e.col, len(l))
	head := append([]rune(nil), l[:c]...)
	tail := append([]rune(nil), l[c:]...)
	var pre []rune
	if indent {
		pre = leadingSpaces(head)
	}
	tail = append(pre, tail...)
	e.lines[e.row] = head
	e.lines = append(e.lines[:e.row+1], append([][]rune{tail}, e.lines[e.row+1:]...)...)
	e.row++
	e.col = len(pre)
	e.changed()
}

func (e *editorModel) backspace() {
	if e.col > 0 {
		l := e.lines[e.row]
		c := min(e.col, len(l))
		e.lines[e.row] = append(append([]rune(nil), l[:c-1]...), l[c:]...)
		e.col = c - 1
	} else if e.row > 0 {
		prev := e.lines[e.row-1]
		e.col = len(prev)
		e.lines[e.row-1] = append(append([]rune(nil), prev...), e.lines[e.row]...)
		e.lines = append(e.lines[:e.row], e.lines[e.row+1:]...)
		e.row--
	}
	e.changed()
}

func (e *editorModel) deleteForward() {
	l := e.lines[e.row]
	if e.col < len(l) {
		e.lines[e.row] = append(append([]rune(nil), l[:e.col]...), l[e.col+1:]...)
	} else if e.row+1 < len(e.lines) {
		e.lines[e.row] = append(append([]rune(nil), l...), e.lines[e.row+1]...)
		e.lines = append(e.lines[:e.row+1], e.lines[e.row+2:]...)
	}
	e.changed()
}

// shiftLine indents (dir > 0) or outdents (dir < 0) line r by shiftWidth.
func (e *editorModel) shiftLine(r, dir int) {
	l := e.lines[r]
	if dir > 0 {
		if len(l) > 0 {
			e.lines[r] = append([]rune(strings.Repeat(" ", shiftWidth)), l...)
		}
	} else {
		n := 0
		for n < shiftWidth && n < len(l) && l[n] == ' ' {
			n++
		}
		e.lines[r] = append([]rune(nil), l[n:]...)
	}
	e.changed()
}

// ---------------------------------------------------------------- visual selection

// selection returns the normalized selection (inclusive end for VISUAL).
func (e *editorModel) selection() (sr, sc, er, ec int) {
	sr, sc, er, ec = e.vrow, e.vcol, e.row, e.col
	if sr > er || (sr == er && sc > ec) {
		sr, sc, er, ec = er, ec, sr, sc
	}
	if e.mode == modeVisualLine {
		sc = 0
		ec = max(0, len(e.lines[er])-1)
	}
	return
}

func (e *editorModel) inSelection(r, c int) bool {
	if e.mode != modeVisual && e.mode != modeVisualLine {
		return false
	}
	sr, sc, er, ec := e.selection()
	if r < sr || r > er {
		return false
	}
	if e.mode == modeVisualLine {
		return true
	}
	if r == sr && c < sc {
		return false
	}
	if r == er && c > ec {
		return false
	}
	return true
}

// selectionText returns the selected text when in a VISUAL mode.
func (e *editorModel) selectionText() (string, bool) {
	if e.mode != modeVisual && e.mode != modeVisualLine {
		return "", false
	}
	return e.rangeText(e.visualRange()), true
}

// ---------------------------------------------------------------- key handling

// update handles a key; it returns text that should be copied to the system
// clipboard (from yank operations) or "".
func (e *editorModel) update(msg tea.KeyMsg) string {
	if !e.typing() {
		e.clampNormal()
		if msg.Paste && msg.Type == tea.KeyRunes {
			// bracketed paste inserts text in any mode
			e.pushUndo()
			e.mode = modeInsert
			e.insertRunes(msg.Runes)
			e.mode = modeNormal
			e.clampNormal()
			return ""
		}
	}
	if e.typing() {
		e.insertKey(msg)
		return ""
	}
	yank := e.viKey(msg)
	if !e.typing() {
		e.clampNormal()
	}
	return yank
}

// insertKey handles INSERT and REPLACE mode keys.
func (e *editorModel) insertKey(msg tea.KeyMsg) {
	e.vi.recordInsertKey(msg)
	switch msg.String() {
	case "esc":
		e.vi.finishInsert(e)
		e.mode = modeNormal
		if e.col > 0 {
			e.col--
		}
		return
	case "enter", "ctrl+j", "ctrl+m":
		e.beforeInsertEdit()
		e.splitLine(true)
	case "backspace", "ctrl+h":
		if e.mode == modeReplace {
			// REPLACE: backspace only moves left
			if e.col > 0 {
				e.col--
			}
			return
		}
		e.beforeInsertEdit()
		e.backspace()
	case "delete":
		e.beforeInsertEdit()
		e.deleteForward()
	case "ctrl+w":
		e.beforeInsertEdit()
		l := e.lines[e.row]
		c := min(e.col, len(l))
		i := c
		for i > 0 && unicode.IsSpace(l[i-1]) {
			i--
		}
		for i > 0 && !unicode.IsSpace(l[i-1]) {
			i--
		}
		e.lines[e.row] = append(append([]rune(nil), l[:i]...), l[c:]...)
		e.col = i
		e.changed()
	case "ctrl+u":
		e.beforeInsertEdit()
		l := e.lines[e.row]
		c := min(e.col, len(l))
		e.lines[e.row] = append([]rune(nil), l[c:]...)
		e.col = 0
		e.changed()
	case "ctrl+t", "ctrl+d":
		e.beforeInsertEdit()
		before := e.lineLen()
		dir := 1
		if msg.String() == "ctrl+d" {
			dir = -1
		}
		e.shiftLine(e.row, dir)
		e.col = max(0, e.col+e.lineLen()-before)
	case "left":
		if e.col > 0 {
			e.col--
		}
	case "right":
		if e.col < e.lineLen() {
			e.col++
		}
	case "up":
		if e.row > 0 {
			e.row--
			e.col = min(e.col, e.lineLen())
		}
	case "down":
		if e.row < len(e.lines)-1 {
			e.row++
			e.col = min(e.col, e.lineLen())
		}
	case "home", "ctrl+a":
		e.col = 0
	case "end", "ctrl+e":
		e.col = e.lineLen()
	case "pgup":
		e.row = max(0, e.row-max(1, e.height-1))
		e.col = min(e.col, e.lineLen())
	case "pgdown":
		e.row = min(len(e.lines)-1, e.row+max(1, e.height-1))
		e.col = min(e.col, e.lineLen())
	default:
		var rs []rune
		switch {
		case msg.Type == tea.KeyRunes && !msg.Alt:
			rs = msg.Runes
		case msg.Type == tea.KeySpace:
			rs = []rune{' '}
		default:
			return
		}
		e.beforeInsertEdit()
		if e.mode == modeReplace {
			e.overwriteRunes(rs)
		} else {
			e.insertRunes(rs)
		}
	}
}

// overwriteRunes types over existing characters (REPLACE mode).
func (e *editorModel) overwriteRunes(rs []rune) {
	for _, r := range rs {
		if r == '\n' || r == '\r' {
			e.splitLine(false)
			continue
		}
		l := e.lines[e.row]
		if e.col < len(l) {
			nl := append([]rune(nil), l...)
			nl[e.col] = r
			e.lines[e.row] = nl
			e.col++
			e.changed()
		} else {
			e.insertRunes([]rune{r})
		}
	}
}

// ---------------------------------------------------------------- view

const (
	clsPlain uint8 = iota
	clsKeyword
	clsString
	clsNumber
	clsComment
)

// classes returns per-rune syntax classes for every line (cached).
func (e *editorModel) classes() [][]uint8 {
	if e.clsVer == e.version && e.clsCache != nil {
		return e.clsCache
	}
	text := e.Text()
	out := make([][]uint8, len(e.lines))
	for i, l := range e.lines {
		out[i] = make([]uint8, len(l))
	}
	// byte offset -> (line, rune col)
	lineOf := make([]int32, len(text)+1)
	colOf := make([]int32, len(text)+1)
	{
		ln, col := 0, 0
		for bi, r := range text {
			lineOf[bi] = int32(ln)
			colOf[bi] = int32(col)
			if r == '\n' {
				ln++
				col = 0
			} else {
				col++
			}
		}
	}
	for _, t := range tokenize(text) {
		var c uint8
		switch t.kind {
		case tkWord:
			if sqlKeywords[strings.ToUpper(t.text)] {
				c = clsKeyword
			}
		case tkString, tkBlob:
			c = clsString
		case tkNumber:
			c = clsNumber
		case tkComment:
			c = clsComment
		}
		if c == clsPlain {
			continue
		}
		for bi := t.start; bi < t.end; bi++ {
			if bi < len(text) && (bi == 0 || text[bi]&0xC0 != 0x80) {
				ln, col := lineOf[bi], colOf[bi]
				if int(ln) < len(out) && int(col) < len(out[ln]) {
					out[ln][col] = c
				}
			}
		}
	}
	e.clsCache, e.clsVer = out, e.version
	return out
}

var clsStyles = map[uint8]lipgloss.Style{
	clsKeyword: stKeyword,
	clsString:  stString,
	clsNumber:  stNumber,
	clsComment: stComment,
}

var stSearch = lipgloss.NewStyle().Background(lipgloss.Color("3")).Foreground(lipgloss.Color("0"))

const (
	stIDSearch = 99
	stIDSelect = 100
	stIDCursor = 101
)

func (e *editorModel) view(w, h int, focused bool) []string {
	e.height = h
	if e.row < e.top {
		e.top = e.row
	}
	if e.row >= e.top+h {
		e.top = e.row - h + 1
	}
	gw := max(2, len(itoa(len(e.lines))))
	avail := max(1, w-gw-1)
	// horizontal scroll so the cursor is visible
	line := e.lines[e.row]
	cx := runewidth.StringWidth(string(line[:min(e.col, len(line))]))
	if cx < avail {
		e.left = 0
	} else if cx < e.left {
		e.left = cx
	}
	if cx >= e.left+avail {
		e.left = cx - avail + 1
	}
	cls := e.classes()
	out := make([]string, 0, h)
	for i := e.top; i < e.top+h; i++ {
		if i >= len(e.lines) {
			out = append(out, stDim.Render(padLeft("~", gw)))
			continue
		}
		num := padLeft(itoa(i+1), gw) + " "
		if i == e.row {
			num = stBold.Render(num)
		} else {
			num = stDim.Render(num)
		}
		out = append(out, num+e.renderLine(i, cls[i], avail, focused))
	}
	return out
}

func (e *editorModel) renderLine(i int, cls []uint8, avail int, focused bool) string {
	l := e.lines[i]
	matches := e.vi.searchMatches(l)
	var b strings.Builder
	var run strings.Builder
	curStyle := -1
	flush := func() {
		if run.Len() == 0 {
			return
		}
		s := run.String()
		switch {
		case curStyle == stIDSelect:
			b.WriteString(stSelect.Render(s))
		case curStyle == stIDCursor:
			b.WriteString(stCursor.Render(s))
		case curStyle == stIDSearch:
			b.WriteString(stSearch.Render(s))
		case curStyle > 0:
			b.WriteString(clsStyles[uint8(curStyle)].Render(s))
		default:
			b.WriteString(s)
		}
		run.Reset()
	}
	x := 0
	used := 0
	for j, r := range l {
		rw := runewidth.RuneWidth(r)
		if r < 0x20 || r == 0x7f {
			r, rw = '?', 1
		}
		if x < e.left {
			x += rw
			if x > e.left { // wide rune cut at the left edge
				for k := 0; k < x-e.left; k++ {
					run.WriteByte(' ')
				}
				used += x - e.left
			}
			continue
		}
		if used+rw > avail {
			break
		}
		st := int(cls[j])
		if matches != nil && matches[j] {
			st = stIDSearch
		}
		if e.inSelection(i, j) {
			st = stIDSelect
		}
		if focused && i == e.row && j == e.col {
			st = stIDCursor
		}
		if st != curStyle {
			flush()
			curStyle = st
		}
		run.WriteRune(r)
		x += rw
		used += rw
	}
	flush()
	if i == e.row && e.col >= len(l) && used < avail && focused {
		b.WriteString(stCursor.Render(" "))
	} else if e.mode == modeVisualLine && e.inSelection(i, 0) && used < avail && len(l) == 0 {
		b.WriteString(stSelect.Render(" "))
	}
	return b.String()
}
