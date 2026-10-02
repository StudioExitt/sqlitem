package main

import (
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
)

// editor.go: a small vi-like multi-line SQL editor. It starts in INSERT
// mode; Esc switches to NORMAL. VISUAL (charwise) and VISUAL LINE selections
// are highlighted. Every modification is undoable with 'u'.

type edMode int

const (
	modeInsert edMode = iota
	modeNormal
	modeVisual
	modeVisualLine
)

func (m edMode) String() string {
	return [...]string{"INSERT", "NORMAL", "VISUAL", "V-LINE"}[m]
}

type edSnap struct {
	lines    [][]rune
	row, col int
}

const maxUndo = 500

type editorModel struct {
	lines    [][]rune
	row, col int
	mode     edMode
	vrow     int // visual anchor
	vcol     int
	undo     []edSnap
	snapped  bool // undo snapshot already taken for this INSERT session
	pending  string
	top      int
	left     int // horizontal scroll in display cells
	height   int
	reg      string
	regLine  bool
	version  int
	clsVer   int
	clsCache [][]uint8
}

func newEditorModel() *editorModel {
	return &editorModel{lines: [][]rune{{}}, mode: modeInsert, clsVer: -1}
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

func (e *editorModel) title() string {
	return "SQL [" + e.mode.String() + "] " + itoa(e.row+1) + ":" + itoa(e.col+1)
}

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

// ---------------------------------------------------------------- undo

func (e *editorModel) snapshot() edSnap {
	cp := make([][]rune, len(e.lines))
	for i, l := range e.lines {
		cp[i] = append([]rune(nil), l...)
	}
	return edSnap{lines: cp, row: e.row, col: e.col}
}

func (e *editorModel) pushUndo() {
	e.undo = append(e.undo, e.snapshot())
	if len(e.undo) > maxUndo {
		e.undo = e.undo[len(e.undo)-maxUndo:]
	}
}

// beforeInsertEdit takes one undo snapshot per INSERT session.
func (e *editorModel) beforeInsertEdit() {
	if !e.snapped {
		e.pushUndo()
		e.snapped = true
	}
}

func (e *editorModel) doUndo() bool {
	if len(e.undo) == 0 {
		return false
	}
	s := e.undo[len(e.undo)-1]
	e.undo = e.undo[:len(e.undo)-1]
	e.lines, e.row, e.col = s.lines, s.row, s.col
	e.clampNormal()
	e.changed()
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
	if e.mode == modeInsert {
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

// flat position helpers for word motions
func (e *editorModel) toIndex(r, c int) int {
	idx := 0
	for i := 0; i < r; i++ {
		idx += len(e.lines[i]) + 1
	}
	return idx + c
}

func (e *editorModel) fromIndex(idx int) (int, int) {
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

func charClass(r rune) int {
	switch {
	case unicode.IsSpace(r):
		return 0
	case r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
		return 1
	default:
		return 2
	}
}

func (e *editorModel) wordForward() {
	rs := e.flatRunes()
	i := e.toIndex(e.row, e.col)
	if i >= len(rs) {
		return
	}
	cls := charClass(rs[i])
	for i < len(rs) && cls != 0 && charClass(rs[i]) == cls {
		i++
	}
	for i < len(rs) && charClass(rs[i]) == 0 {
		if rs[i] == '\n' && i+1 < len(rs) && rs[i+1] == '\n' {
			i++
			break // stop on empty line
		}
		i++
	}
	if i >= len(rs) {
		i = max(0, len(rs)-1)
	}
	e.row, e.col = e.fromIndex(i)
}

func (e *editorModel) wordBackward() {
	rs := e.flatRunes()
	i := e.toIndex(e.row, e.col) - 1
	for i > 0 && charClass(rs[i]) == 0 {
		i--
	}
	if i < 0 {
		i = 0
	}
	if i < len(rs) {
		cls := charClass(rs[i])
		for i > 0 && charClass(rs[i-1]) == cls && cls != 0 {
			i--
		}
	}
	e.row, e.col = e.fromIndex(i)
}

func (e *editorModel) wordEnd() {
	rs := e.flatRunes()
	i := e.toIndex(e.row, e.col) + 1
	for i < len(rs) && charClass(rs[i]) == 0 {
		i++
	}
	if i >= len(rs) {
		return
	}
	cls := charClass(rs[i])
	for i+1 < len(rs) && charClass(rs[i+1]) == cls {
		i++
	}
	e.row, e.col = e.fromIndex(i)
}

func firstNonBlank(l []rune) int {
	for i, r := range l {
		if !unicode.IsSpace(r) {
			return i
		}
	}
	return 0
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
		for _, r := range head {
			if r != ' ' {
				break
			}
			pre = append(pre, r)
		}
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

func (e *editorModel) deleteLines(from, to int) {
	var sb []string
	for i := from; i <= to; i++ {
		sb = append(sb, string(e.lines[i]))
	}
	e.reg, e.regLine = strings.Join(sb, "\n"), true
	e.lines = append(e.lines[:from], e.lines[to+1:]...)
	if len(e.lines) == 0 {
		e.lines = [][]rune{{}}
	}
	e.row = min(from, len(e.lines)-1)
	e.col = firstNonBlank(e.lines[e.row])
	e.changed()
}

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
	sr, sc, er, ec := e.selection()
	a := e.toIndex(sr, sc)
	b := e.toIndex(er, ec) + 1
	rs := e.flatRunes()
	if b > len(rs) {
		b = len(rs)
	}
	if a > b {
		a = b
	}
	return string(rs[a:b]), true
}

func (e *editorModel) deleteSelection() string {
	text, _ := e.selectionText()
	sr, sc, er, ec := e.selection()
	if e.mode == modeVisualLine {
		e.deleteLines(sr, er)
		return text
	}
	rs := e.flatRunes()
	a := e.toIndex(sr, sc)
	b := min(e.toIndex(er, ec)+1, len(rs))
	nrs := append(append([]rune(nil), rs[:a]...), rs[b:]...)
	e.setLinesFromRunes(nrs)
	e.row, e.col = e.fromIndex(a)
	e.reg, e.regLine = text, false
	return text
}

func (e *editorModel) setLinesFromRunes(rs []rune) {
	parts := strings.Split(string(rs), "\n")
	e.lines = make([][]rune, len(parts))
	for i, p := range parts {
		e.lines[i] = []rune(p)
	}
	e.changed()
}

func (e *editorModel) put(after bool) {
	if e.reg == "" && !e.regLine {
		return
	}
	e.pushUndo()
	if e.regLine {
		var nl [][]rune
		for _, p := range strings.Split(e.reg, "\n") {
			nl = append(nl, []rune(p))
		}
		at := e.row
		if after {
			at++
		}
		e.lines = append(e.lines[:at], append(nl, e.lines[at:]...)...)
		e.row = at
		e.col = firstNonBlank(e.lines[at])
		e.changed()
		return
	}
	if after && e.lineLen() > 0 {
		e.col++
	}
	start := e.toIndex(e.row, e.col)
	e.insertRunes([]rune(e.reg))
	n := len([]rune(e.reg))
	e.row, e.col = e.fromIndex(start + n - 1)
}

// ---------------------------------------------------------------- key handling

// update handles a key; it returns text that should be copied to the system
// clipboard (from yank operations) or "".
func (e *editorModel) update(msg tea.KeyMsg) string {
	defer func() {
		if e.mode != modeInsert {
			e.clampNormal()
		}
	}()
	if e.mode != modeInsert {
		e.clampNormal()
		if msg.Paste && msg.Type == tea.KeyRunes {
			// bracketed paste inserts text in any mode
			e.pushUndo()
			e.mode = modeInsert
			e.insertRunes(msg.Runes)
			e.mode = modeNormal
			return ""
		}
	}
	switch e.mode {
	case modeInsert:
		e.insertKey(msg)
		return ""
	case modeNormal:
		return e.normalKey(msg)
	default:
		return e.visualKey(msg)
	}
}

func (e *editorModel) insertKey(msg tea.KeyMsg) {
	switch msg.String() {
	case "esc":
		e.mode = modeNormal
		if e.col > 0 {
			e.col--
		}
		return
	case "enter", "ctrl+j", "ctrl+m":
		e.beforeInsertEdit()
		e.splitLine(true)
	case "backspace", "ctrl+h":
		e.beforeInsertEdit()
		e.backspace()
	case "delete", "ctrl+d":
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
		if msg.Type == tea.KeyRunes && !msg.Alt {
			e.beforeInsertEdit()
			e.insertRunes(msg.Runes)
		} else if msg.Type == tea.KeySpace {
			e.beforeInsertEdit()
			e.insertRunes([]rune{' '})
		}
	}
}

// motion applies a cursor motion shared by NORMAL and VISUAL modes.
func (e *editorModel) motion(key string) bool {
	switch key {
	case "h", "left", "backspace":
		if e.col > 0 {
			e.col--
		}
	case "l", "right", " ":
		if e.col < e.lineLen()-1 {
			e.col++
		}
	case "j", "down", "enter":
		if e.row < len(e.lines)-1 {
			e.row++
		}
	case "k", "up":
		if e.row > 0 {
			e.row--
		}
	case "w":
		e.wordForward()
	case "b":
		e.wordBackward()
	case "e":
		e.wordEnd()
	case "0", "home":
		e.col = 0
	case "^":
		e.col = firstNonBlank(e.lines[e.row])
	case "$", "end":
		e.col = max(0, e.lineLen()-1)
	case "G":
		e.row = len(e.lines) - 1
		e.col = firstNonBlank(e.lines[e.row])
	case "pgdown", "ctrl+d":
		e.row = min(len(e.lines)-1, e.row+max(1, e.height/2))
	case "pgup", "ctrl+u":
		e.row = max(0, e.row-max(1, e.height/2))
	default:
		return false
	}
	return true
}

func (e *editorModel) normalKey(msg tea.KeyMsg) string {
	key := msg.String()
	if p := e.pending; p != "" {
		e.pending = ""
		switch p + key {
		case "gg":
			e.row, e.col = 0, 0
		case "dd":
			e.pushUndo()
			e.deleteLines(e.row, e.row)
		case "yy":
			e.reg, e.regLine = string(e.lines[e.row]), true
			return e.reg
		case "cc":
			e.pushUndo()
			ind := firstNonBlank(e.lines[e.row])
			e.reg, e.regLine = string(e.lines[e.row]), true
			e.lines[e.row] = append([]rune(nil), e.lines[e.row][:ind]...)
			e.col = ind
			e.changed()
			e.enterInsert()
			e.snapped = true
		}
		return ""
	}
	if e.motion(key) {
		return ""
	}
	switch key {
	case "g", "d", "y", "c":
		e.pending = key
	case "i":
		e.enterInsert()
	case "a":
		e.enterInsert()
		if e.lineLen() > 0 {
			e.col++
		}
	case "I":
		e.enterInsert()
		e.col = firstNonBlank(e.lines[e.row])
	case "A":
		e.enterInsert()
		e.col = e.lineLen()
	case "o", "O":
		e.pushUndo()
		ind := []rune{}
		for _, r := range e.lines[e.row] {
			if r != ' ' {
				break
			}
			ind = append(ind, ' ')
		}
		at := e.row + 1
		if key == "O" {
			at = e.row
		}
		e.lines = append(e.lines[:at], append([][]rune{ind}, e.lines[at:]...)...)
		e.row, e.col = at, len(ind)
		e.changed()
		e.enterInsert()
		e.snapped = true
	case "x", "delete":
		if e.lineLen() > 0 {
			e.pushUndo()
			e.reg, e.regLine = string(e.lines[e.row][e.col]), false
			e.deleteForward()
		}
	case "X":
		if e.col > 0 {
			e.pushUndo()
			e.backspace()
		}
	case "D", "C":
		e.pushUndo()
		l := e.lines[e.row]
		e.reg, e.regLine = string(l[min(e.col, len(l)):]), false
		e.lines[e.row] = append([]rune(nil), l[:min(e.col, len(l))]...)
		e.changed()
		if key == "C" {
			e.enterInsert()
			e.snapped = true
		}
	case "s":
		e.pushUndo()
		if e.lineLen() > 0 {
			e.deleteForward()
		}
		e.enterInsert()
		e.snapped = true
	case "J":
		if e.row+1 < len(e.lines) {
			e.pushUndo()
			l := strings.TrimRight(string(e.lines[e.row]), " ")
			n := strings.TrimLeft(string(e.lines[e.row+1]), " ")
			joined := l
			if n != "" {
				joined += " " + n
			}
			e.lines[e.row] = []rune(joined)
			e.lines = append(e.lines[:e.row+1], e.lines[e.row+2:]...)
			e.col = len([]rune(l))
			e.changed()
		}
	case "p":
		e.put(true)
	case "P":
		e.put(false)
	case "u":
		e.doUndo()
	case "v":
		e.mode = modeVisual
		e.vrow, e.vcol = e.row, e.col
	case "V":
		e.mode = modeVisualLine
		e.vrow, e.vcol = e.row, e.col
	}
	return ""
}

func (e *editorModel) visualKey(msg tea.KeyMsg) string {
	key := msg.String()
	if e.pending == "g" {
		e.pending = ""
		if key == "g" {
			e.row, e.col = 0, 0
		}
		return ""
	}
	if e.motion(key) {
		return ""
	}
	switch key {
	case "g":
		e.pending = "g"
	case "esc":
		e.mode = modeNormal
	case "v":
		if e.mode == modeVisual {
			e.mode = modeNormal
		} else {
			e.mode = modeVisual
		}
	case "V":
		if e.mode == modeVisualLine {
			e.mode = modeNormal
		} else {
			e.mode = modeVisualLine
		}
	case "o":
		e.row, e.col, e.vrow, e.vcol = e.vrow, e.vcol, e.row, e.col
	case "y":
		text, _ := e.selectionText()
		e.reg, e.regLine = text, e.mode == modeVisualLine
		sr, sc, _, _ := e.selection()
		e.row, e.col = sr, sc
		e.mode = modeNormal
		return text
	case "d", "x", "delete":
		e.pushUndo()
		e.deleteSelection()
		e.mode = modeNormal
	case "c", "s":
		e.pushUndo()
		line := e.mode == modeVisualLine
		sr, _, _, _ := e.selection()
		e.deleteSelection()
		if line {
			e.lines = append(e.lines[:sr], append([][]rune{{}}, e.lines[sr:]...)...)
			e.row, e.col = sr, 0
			e.changed()
		}
		e.enterInsert()
		e.snapped = true
	}
	return ""
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
	// byte offset -> (line, rune col)
	out := make([][]uint8, len(e.lines))
	for i, l := range e.lines {
		out[i] = make([]uint8, len(l))
	}
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

const (
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
	gw := len(itoa(len(e.lines)))
	if gw < 2 {
		gw = 2
	}
	avail := w - gw - 1
	if avail < 1 {
		avail = 1
	}
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
