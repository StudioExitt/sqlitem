package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

// vi.go: the NORMAL / VISUAL command engine of the SQL editor.
//
// Keys are collected as tokens until they form a complete command:
//
//	[count] operator [count] (motion | text-object | operator)   e.g. 2d3w, ciw, >>
//	[count] motion                                                e.g. 5j, f(, gg
//	[count] command                                               e.g. 3x, p, J, .
//
// Ex commands (":s/a/b/g", ":12", ":noh") are dispatched from the root
// model's ':' command line to exCommand.

const (
	pIncomplete = iota
	pDone
	pInvalid
)

type viCmd struct {
	count    int
	hasCount bool
	op       string // d c y > < gu gU g~
	opLine   bool   // doubled operator: dd, cc, yy, >>, gUU ...
	motion   string
	arg      rune
	obj      string // text object: iw, a(, i" ...
	cmd      string // simple command: x p J . ...
}

type viState struct {
	pend     []string
	pendKeys []tea.KeyMsg

	// dot repeat
	lastChange []tea.KeyMsg
	recording  bool
	rec        []tea.KeyMsg
	replaying  bool

	// count for i/a/I/A ("3ifoo<Esc>")
	insCount  int
	insKeys   []tea.KeyMsg
	repeating bool

	want         int  // desired column for vertical motions
	wantEOL      bool // after "$": stick to end of line
	lastVertical bool // previous command was a vertical motion
	findCmd      string
	findCh       rune
	marks        map[rune][2]int
	jump         [2]int
	hasJump      bool
	search       struct {
		active bool
		buf    []rune
	}
	lastSearch string
	searchDir  int
	hl         bool
}

func newViState() viState { return viState{marks: map[rune][2]int{}, searchDir: 1} }

func (v *viState) idle() bool { return len(v.pend) == 0 && !v.search.active }

func (v *viState) showcmd() string { return strings.Join(v.pend, "") }

// recordInsertKey captures keys typed in INSERT/REPLACE mode for '.' and
// for count-repeated inserts.
func (v *viState) recordInsertKey(msg tea.KeyMsg) {
	if v.repeating {
		return
	}
	v.insKeys = append(v.insKeys, msg)
	if v.recording && !v.replaying {
		v.rec = append(v.rec, msg)
	}
}

// finishInsert runs when INSERT/REPLACE ends with Esc.
func (v *viState) finishInsert(e *editorModel) {
	if v.insCount > 1 && len(v.insKeys) > 0 {
		typed := v.insKeys[:len(v.insKeys)-1] // without the Esc
		v.repeating = true
		for k := 1; k < v.insCount; k++ {
			for _, key := range typed {
				e.insertKey(key)
			}
		}
		v.repeating = false
	}
	if v.recording && !v.replaying {
		v.lastChange = v.rec
	}
	v.recording = false
	v.rec = nil
	v.insCount = 0
	v.insKeys = nil
}

func keyToken(msg tea.KeyMsg) string {
	switch msg.Type {
	case tea.KeyRunes:
		if len(msg.Runes) == 1 && !msg.Alt {
			return string(msg.Runes)
		}
	case tea.KeySpace:
		return " "
	}
	return "<" + msg.String() + ">"
}

// ---------------------------------------------------------------- parser

var singleMotions = map[string]bool{
	"h": true, "j": true, "k": true, "l": true, "w": true, "W": true, "b": true, "B": true,
	"e": true, "E": true, "0": true, "^": true, "$": true, "G": true, "H": true, "M": true,
	"L": true, ";": true, ",": true, "%": true, "{": true, "}": true, "n": true, "N": true,
	"*": true, "#": true, "-": true, "+": true, "_": true, " ": true,
	"<left>": true, "<right>": true, "<up>": true, "<down>": true, "<home>": true, "<end>": true,
	"<enter>": true, "<backspace>": true, "<pgup>": true, "<pgdown>": true, "<ctrl+d>": true,
	"<ctrl+u>": true, "<ctrl+b>": true,
}

var normalCmds = map[string]bool{
	"x": true, "X": true, "D": true, "C": true, "s": true, "S": true, "p": true, "P": true,
	"J": true, "u": true, "U": true, ".": true, "~": true, "i": true, "a": true, "I": true,
	"A": true, "o": true, "O": true, "v": true, "V": true, "R": true, "Y": true, "/": true,
	"<delete>": true, "<ctrl+e>": true, "<ctrl+y>": true, "<esc>": true,
}

var visualCmds = map[string]bool{
	"x": true, "X": true, "D": true, "C": true, "s": true, "S": true, "R": true, "Y": true,
	"J": true, "u": true, "U": true, "~": true, "p": true, "P": true, "o": true, "O": true,
	"v": true, "V": true, "/": true, "<delete>": true, "<esc>": true,
}

var textObjects = map[string]bool{
	"w": true, "W": true, "p": true, `"`: true, "'": true, "`": true, "(": true, ")": true,
	"b": true, "[": true, "]": true, "{": true, "}": true, "B": true, "<": true, ">": true,
}

func isSingleRune(s string) bool { return utf8.RuneCountInString(s) == 1 }

func parseVi(toks []string, visual bool) (viCmd, int) {
	var c viCmd
	i := 0
	readCount := func() (int, bool) {
		n, has := 0, false
		for i < len(toks) && len(toks[i]) == 1 && toks[i][0] >= '0' && toks[i][0] <= '9' {
			if toks[i] == "0" && !has {
				break // a leading 0 is the motion
			}
			n = n*10 + int(toks[i][0]-'0')
			has = true
			i++
		}
		return n, has
	}
	n1, h1 := readCount()
	setCount := func(n2 int, h2 bool) {
		c.count = 1
		if h1 {
			c.count = n1
		}
		if h2 {
			c.count *= n2
		}
		c.hasCount = h1 || h2
	}
	setCount(0, false)
	if i >= len(toks) {
		return c, pIncomplete
	}
	t := toks[i]
	i++
	// operator?
	op := ""
	switch t {
	case "d", "c", "y", ">", "<":
		op = t
	case "g":
		if i >= len(toks) {
			return c, pIncomplete
		}
		switch toks[i] {
		case "u", "U", "~":
			op = "g" + toks[i]
			i++
		}
	}
	if op != "" {
		c.op = op
		if visual {
			return c, done(i, toks)
		}
		n2, h2 := readCount()
		setCount(n2, h2)
		if i >= len(toks) {
			return c, pIncomplete
		}
		t2 := toks[i]
		i++
		short := op[len(op)-1:] // gu -> u
		switch {
		case t2 == op || (len(op) == 2 && t2 == short):
			c.opLine = true
			return c, done(i, toks)
		case len(op) == 2 && t2 == "g":
			if i >= len(toks) {
				return c, pIncomplete
			}
			if toks[i] == short {
				c.opLine = true
				return c, done(i+1, toks)
			}
		case t2 == "i" || t2 == "a":
			if i >= len(toks) {
				return c, pIncomplete
			}
			if !textObjects[toks[i]] {
				return c, pInvalid
			}
			c.obj = t2 + toks[i]
			return c, done(i+1, toks)
		}
		return parseMotion(c, toks, i-1)
	}
	if visual {
		switch {
		case visualCmds[t]:
			c.cmd = t
			return c, done(i, toks)
		case t == "r":
			return argCmd(c, toks, i, "r")
		case t == "i" || t == "a":
			if i >= len(toks) {
				return c, pIncomplete
			}
			if !textObjects[toks[i]] {
				return c, pInvalid
			}
			c.obj = t + toks[i]
			return c, done(i+1, toks)
		case t == "g" && i < len(toks) && toks[i] == "J":
			c.cmd = "gJ"
			return c, done(i+1, toks)
		}
		return parseMotion(c, toks, i-1)
	}
	switch {
	case normalCmds[t]:
		c.cmd = t
		return c, done(i, toks)
	case t == "r" || t == "m":
		return argCmd(c, toks, i, t)
	case t == "z":
		if i >= len(toks) {
			return c, pIncomplete
		}
		switch toks[i] {
		case "z", ".":
			c.cmd = "zz"
		case "t", "<enter>":
			c.cmd = "zt"
		case "b", "-":
			c.cmd = "zb"
		default:
			return c, pInvalid
		}
		return c, done(i+1, toks)
	case t == "g" && i < len(toks) && toks[i] == "J":
		c.cmd = "gJ"
		return c, done(i+1, toks)
	}
	return parseMotion(c, toks, i-1)
}

func done(i int, toks []string) int {
	if i == len(toks) {
		return pDone
	}
	return pInvalid
}

func argCmd(c viCmd, toks []string, i int, cmd string) (viCmd, int) {
	if i >= len(toks) {
		return c, pIncomplete
	}
	if !isSingleRune(toks[i]) {
		return c, pInvalid
	}
	c.cmd = cmd
	c.arg, _ = utf8.DecodeRuneInString(toks[i])
	return c, done(i+1, toks)
}

func parseMotion(c viCmd, toks []string, i int) (viCmd, int) {
	t := toks[i]
	switch {
	case singleMotions[t]:
		c.motion = t
		return c, done(i+1, toks)
	case t == "g":
		if i+1 >= len(toks) {
			return c, pIncomplete
		}
		switch toks[i+1] {
		case "g", "e", "E", "_":
			c.motion = "g" + toks[i+1]
			return c, done(i+2, toks)
		}
		return c, pInvalid
	case t == "f" || t == "F" || t == "t" || t == "T" || t == "'" || t == "`":
		if i+1 >= len(toks) {
			return c, pIncomplete
		}
		if !isSingleRune(toks[i+1]) {
			return c, pInvalid
		}
		c.motion = t
		c.arg, _ = utf8.DecodeRuneInString(toks[i+1])
		return c, done(i+2, toks)
	}
	return c, pInvalid
}

// ---------------------------------------------------------------- dispatch

// viKey handles one key in NORMAL/VISUAL mode and returns yanked text.
func (e *editorModel) viKey(msg tea.KeyMsg) string {
	v := &e.vi
	if v.search.active {
		e.searchKey(msg)
		return ""
	}
	v.pend = append(v.pend, keyToken(msg))
	v.pendKeys = append(v.pendKeys, msg)
	visual := e.mode == modeVisual || e.mode == modeVisualLine
	c, st := parseVi(v.pend, visual)
	if st == pIncomplete {
		return ""
	}
	keys := v.pendKeys
	v.pend, v.pendKeys = nil, nil
	if st == pInvalid {
		return ""
	}
	return e.execVi(c, keys, visual)
}

func (e *editorModel) execVi(c viCmd, keys []tea.KeyMsg, visual bool) string {
	v := &e.vi
	var yank string
	change := false
	switch {
	case visual && (c.op != "" || (c.cmd != "" && c.cmd != "/")):
		yank = e.visualOp(c)
	case visual && c.obj != "":
		e.visualObject(c.obj)
	case c.op != "":
		rg, ok := e.opRange(c)
		if !ok {
			return ""
		}
		yank, change = e.applyOp(c.op, rg)
	case c.cmd != "":
		yank, change = e.simpleCmd(c)
	case c.motion != "":
		e.moveTo(c)
		v.lastVertical = verticalMotions[c.motion]
		return ""
	}
	v.lastVertical = false
	v.wantEOL = false
	if !v.replaying {
		switch {
		case e.typing():
			v.recording = change
			v.rec = append([]tea.KeyMsg(nil), keys...)
		case change:
			v.lastChange = keys
		}
	}
	if e.typing() {
		v.insKeys = nil
		v.insCount = 1
		switch c.cmd {
		case "i", "a", "I", "A":
			v.insCount = c.count
		}
	}
	return yank
}

// ---------------------------------------------------------------- ranges

// edRange is either a set of whole lines (r0..r1) or a flat character
// range [a, b) over the buffer joined with '\n'.
type edRange struct {
	line   bool
	r0, r1 int
	a, b   int
}

func (e *editorModel) rangeText(rg edRange) string {
	if rg.line {
		parts := make([]string, 0, rg.r1-rg.r0+1)
		for r := rg.r0; r <= rg.r1; r++ {
			parts = append(parts, string(e.lines[r]))
		}
		return strings.Join(parts, "\n")
	}
	rs := e.flatRunes()
	a, b := max(0, rg.a), min(len(rs), rg.b)
	if a >= b {
		return ""
	}
	return string(rs[a:b])
}

func (e *editorModel) visualRange() edRange {
	sr, sc, er, ec := e.selection()
	if e.mode == modeVisualLine {
		return edRange{line: true, r0: sr, r1: er}
	}
	end := e.toIndex(er, ec) + 1
	if len(e.lines[er]) == 0 {
		end = e.toIndex(er, 0)
	}
	return edRange{a: e.toIndex(sr, sc), b: end}
}

func (e *editorModel) toLines(rg edRange) edRange {
	if rg.line {
		return rg
	}
	r0, _ := e.fromIndex(rg.a)
	r1, _ := e.fromIndex(max(rg.a, rg.b-1))
	return edRange{line: true, r0: r0, r1: r1}
}

// opRange computes what an operator applies to.
func (e *editorModel) opRange(c viCmd) (edRange, bool) {
	last := len(e.lines) - 1
	if c.opLine {
		return edRange{line: true, r0: e.row, r1: min(last, e.row+c.count-1)}, true
	}
	if c.obj != "" {
		return e.textObject(c.obj)
	}
	m := c.motion
	if c.op == "c" && (m == "w" || m == "W") && e.lineLen() > 0 && !unicode.IsSpace(e.lines[e.row][e.col]) {
		m = map[string]string{"w": "e", "W": "E"}[m] // cw behaves like ce
	}
	sr, sc := e.row, e.col
	tr, tc, lw, incl, ok := e.evalMotion(m, c.arg, c.count, c.hasCount, true)
	if !ok {
		return edRange{}, false
	}
	if lw {
		return edRange{line: true, r0: min(sr, tr), r1: max(sr, tr)}, true
	}
	a, b := e.toIndex(sr, sc), e.toIndex(tr, tc)
	if a > b {
		a, b = b, a
	}
	if incl {
		b++
	}
	ar, ac := e.fromIndex(a)
	br, bc := e.fromIndex(b)
	if !incl && br > ar {
		switch {
		case (m == "w" || m == "W") && bc <= firstNonBlank(e.lines[br]):
			// "dw" on the last word of a line stops at the end of that line
			b = e.toIndex(br-1, len(e.lines[br-1]))
		case bc == 0 && ac <= firstNonBlank(e.lines[ar]):
			// exclusive motion ending in column 0 from the line start: linewise
			return edRange{line: true, r0: ar, r1: br - 1}, true
		case bc == 0:
			b = e.toIndex(br-1, len(e.lines[br-1]))
		}
	}
	return edRange{a: a, b: min(b, len(e.flatRunes()))}, true
}

// applyOp runs an operator on a range. Returns yanked text and whether
// the buffer changed.
func (e *editorModel) applyOp(op string, rg edRange) (string, bool) {
	text := e.rangeText(rg)
	switch op {
	case "y":
		e.reg, e.regLine = text, rg.line
		if rg.line {
			e.row = rg.r0
			if n := rg.r1 - rg.r0 + 1; n > 2 {
				e.setMsg(fmt.Sprintf("%d lines yanked", n), false)
			}
		} else {
			e.row, e.col = e.fromIndex(rg.a)
		}
		return text, false
	case "d":
		e.pushUndo()
		e.reg, e.regLine = text, rg.line
		e.deleteRange(rg)
	case "c":
		e.pushUndo()
		e.reg, e.regLine = text, rg.line
		if rg.line {
			ind := leadingSpaces(e.lines[rg.r0])
			e.lines = append(e.lines[:rg.r0], append([][]rune{ind}, e.lines[rg.r1+1:]...)...)
			e.row, e.col = rg.r0, len(ind)
			e.changed()
		} else {
			e.deleteRange(rg)
		}
		e.enterInsert()
		e.snapped = true
	case ">", "<":
		e.pushUndo()
		lr := e.toLines(rg)
		dir := 1
		if op == "<" {
			dir = -1
		}
		for r := lr.r0; r <= lr.r1; r++ {
			e.shiftLine(r, dir)
		}
		e.row = lr.r0
		e.col = firstNonBlank(e.lines[e.row])
	case "gu", "gU", "g~":
		e.pushUndo()
		conv := map[string]func(rune) rune{"gu": unicode.ToLower, "gU": unicode.ToUpper, "g~": toggleCase}[op]
		if rg.line {
			for r := rg.r0; r <= rg.r1; r++ {
				e.lines[r] = mapRunes(e.lines[r], conv)
			}
			e.row = rg.r0
		} else {
			rs := e.flatRunes()
			copy(rs[rg.a:rg.b], mapRunes(rs[rg.a:rg.b], conv))
			e.setLinesFromRunes(rs)
			e.row, e.col = e.fromIndex(rg.a)
		}
		e.changed()
	default:
		return "", false
	}
	return "", true
}

func (e *editorModel) deleteRange(rg edRange) {
	if rg.line {
		e.lines = append(e.lines[:rg.r0], e.lines[rg.r1+1:]...)
		if len(e.lines) == 0 {
			e.lines = [][]rune{{}}
		}
		e.row = min(rg.r0, len(e.lines)-1)
		e.col = firstNonBlank(e.lines[e.row])
		e.changed()
		return
	}
	rs := e.flatRunes()
	a, b := max(0, rg.a), min(len(rs), rg.b)
	if a >= b {
		e.row, e.col = e.fromIndex(a)
		return
	}
	e.setLinesFromRunes(append(append([]rune(nil), rs[:a]...), rs[b:]...))
	e.row, e.col = e.fromIndex(a)
}

func toggleCase(r rune) rune {
	if unicode.IsUpper(r) {
		return unicode.ToLower(r)
	}
	return unicode.ToUpper(r)
}

func mapRunes(rs []rune, f func(rune) rune) []rune {
	out := make([]rune, len(rs))
	for i, r := range rs {
		out[i] = f(r)
	}
	return out
}

// ---------------------------------------------------------------- simple commands

func (e *editorModel) simpleCmd(c viCmd) (string, bool) {
	v := &e.vi
	n := c.count
	l := e.lines[e.row]
	opOn := func(op, motion string, line bool) (string, bool) {
		rg, ok := e.opRange(viCmd{op: op, motion: motion, opLine: line, count: n, hasCount: c.hasCount})
		if !ok {
			return "", false
		}
		return e.applyOp(op, rg)
	}
	switch c.cmd {
	case "x", "<delete>":
		if len(l) == 0 {
			return "", false
		}
		end := min(len(l), e.col+n)
		return e.applyOp("d", edRange{a: e.toIndex(e.row, e.col), b: e.toIndex(e.row, end)})
	case "X":
		if e.col == 0 {
			return "", false
		}
		start := max(0, e.col-n)
		return e.applyOp("d", edRange{a: e.toIndex(e.row, start), b: e.toIndex(e.row, e.col)})
	case "D":
		return opOn("d", "$", false)
	case "C":
		return opOn("c", "$", false)
	case "s":
		return opOn("c", "l", false)
	case "S":
		return opOn("c", "", true)
	case "Y":
		return opOn("y", "", true)
	case "p", "P":
		return "", e.put(c.cmd == "p", n)
	case "J", "gJ":
		return "", e.join(max(2, n), c.cmd == "J")
	case "u":
		for k := 0; k < n && e.doUndo(); k++ {
		}
	case "U":
		for k := 0; k < n && e.doRedo(); k++ {
		}
	case ".":
		if len(v.lastChange) == 0 {
			return "", false
		}
		keys := v.lastChange
		v.replaying = true
		for k := 0; k < n; k++ {
			for _, key := range keys {
				e.update(key)
			}
		}
		v.replaying = false
	case "~":
		if len(l) == 0 {
			return "", false
		}
		e.pushUndo()
		end := min(len(l), e.col+n)
		nl := append([]rune(nil), l...)
		copy(nl[e.col:end], mapRunes(nl[e.col:end], toggleCase))
		e.lines[e.row] = nl
		e.col = min(len(l)-1, end)
		e.changed()
		return "", true
	case "r":
		if e.col+n > len(l) {
			return "", false
		}
		e.pushUndo()
		nl := append([]rune(nil), l...)
		for k := e.col; k < e.col+n; k++ {
			nl[k] = c.arg
		}
		e.lines[e.row] = nl
		e.col += n - 1
		e.changed()
		return "", true
	case "i":
		e.enterInsert()
		return "", true
	case "a":
		if len(l) > 0 {
			e.col++
		}
		e.enterInsert()
		return "", true
	case "I":
		e.col = firstNonBlank(l)
		e.enterInsert()
		return "", true
	case "A":
		e.col = len(l)
		e.enterInsert()
		return "", true
	case "o", "O":
		e.pushUndo()
		ind := leadingSpaces(l)
		at := e.row + 1
		if c.cmd == "O" {
			at = e.row
		}
		e.lines = append(e.lines[:at], append([][]rune{append([]rune(nil), ind...)}, e.lines[at:]...)...)
		e.row, e.col = at, len(ind)
		e.changed()
		e.enterInsert()
		e.snapped = true
		return "", true
	case "R":
		e.enterInsert()
		e.mode = modeReplace
		return "", true
	case "v":
		e.mode = modeVisual
		e.vrow, e.vcol = e.row, e.col
	case "V":
		e.mode = modeVisualLine
		e.vrow, e.vcol = e.row, e.col
	case "m":
		v.marks[c.arg] = [2]int{e.row, e.col}
	case "zz":
		e.top = max(0, e.row-e.viewHeight()/2)
	case "zt":
		e.top = e.row
	case "zb":
		e.top = max(0, e.row-e.viewHeight()+1)
	case "<ctrl+e>":
		e.top = min(len(e.lines)-1, e.top+n)
		e.row = max(e.row, e.top)
	case "<ctrl+y>":
		e.top = max(0, e.top-n)
		e.row = min(e.row, e.top+e.viewHeight()-1)
	case "/":
		v.search.active = true
		v.search.buf = nil
	}
	return "", false
}

func (e *editorModel) viewHeight() int {
	if e.height > 0 {
		return e.height
	}
	return len(e.lines)
}

// put pastes the register n times after/before the cursor.
func (e *editorModel) put(after bool, n int) bool {
	if e.reg == "" && !e.regLine {
		return false
	}
	e.pushUndo()
	if e.regLine {
		var nl [][]rune
		for k := 0; k < n; k++ {
			for _, p := range strings.Split(e.reg, "\n") {
				nl = append(nl, []rune(p))
			}
		}
		at := e.row
		if after {
			at++
		}
		e.lines = append(e.lines[:at], append(nl, e.lines[at:]...)...)
		e.row = at
		e.col = firstNonBlank(e.lines[at])
		e.changed()
		return true
	}
	text := []rune(strings.Repeat(e.reg, n))
	col := e.col
	if after && e.lineLen() > 0 {
		col++
	}
	idx := e.toIndex(e.row, col)
	rs := e.flatRunes()
	rs = append(rs[:idx], append(text, rs[idx:]...)...)
	e.setLinesFromRunes(rs)
	e.row, e.col = e.fromIndex(idx + len(text) - 1)
	return true
}

// join joins n lines starting at the cursor line.
func (e *editorModel) join(n int, spaces bool) bool {
	if e.row+1 >= len(e.lines) {
		return false
	}
	e.pushUndo()
	for k := 1; k < n && e.row+1 < len(e.lines); k++ {
		cur := e.lines[e.row]
		nxt := e.lines[e.row+1]
		var joined []rune
		if spaces {
			cur = []rune(strings.TrimRight(string(cur), " "))
			nxt = []rune(strings.TrimLeft(string(nxt), " "))
			joined = append([]rune(nil), cur...)
			e.col = len(cur)
			if len(nxt) > 0 && len(cur) > 0 && nxt[0] != ')' {
				joined = append(joined, ' ')
			}
		} else {
			joined = append([]rune(nil), cur...)
			e.col = len(cur)
		}
		e.lines[e.row] = append(joined, nxt...)
		e.lines = append(e.lines[:e.row+1], e.lines[e.row+2:]...)
	}
	e.changed()
	return true
}

// ---------------------------------------------------------------- visual mode

func (e *editorModel) visualOp(c viCmd) string {
	rg := e.visualRange()
	op := c.op
	switch c.cmd {
	case "<esc>":
		e.mode = modeNormal
		return ""
	case "v", "V":
		target := map[string]edMode{"v": modeVisual, "V": modeVisualLine}[c.cmd]
		if e.mode == target {
			e.mode = modeNormal
		} else {
			e.mode = target
		}
		return ""
	case "o", "O":
		e.row, e.col, e.vrow, e.vcol = e.vrow, e.vcol, e.row, e.col
		return ""
	case "x", "<delete>":
		op = "d"
	case "X", "D":
		op, rg = "d", e.toLines(rg)
	case "s":
		op = "c"
	case "S", "R", "C":
		op, rg = "c", e.toLines(rg)
	case "Y":
		op, rg = "y", e.toLines(rg)
	case "u":
		op = "gu"
	case "U":
		op = "gU"
	case "~":
		op = "g~"
	case "J", "gJ":
		lr := e.toLines(rg)
		e.mode = modeNormal
		e.row = lr.r0
		e.join(max(2, lr.r1-lr.r0+1), c.cmd == "J")
		return ""
	case "r":
		e.mode = modeNormal
		e.pushUndo()
		if rg.line {
			for r := rg.r0; r <= rg.r1; r++ {
				e.lines[r] = mapRunes(e.lines[r], func(rune) rune { return c.arg })
			}
			e.row, e.col = rg.r0, 0
			e.changed()
			return ""
		}
		rs := e.flatRunes()
		for k := rg.a; k < rg.b && k < len(rs); k++ {
			if rs[k] != '\n' {
				rs[k] = c.arg
			}
		}
		e.setLinesFromRunes(rs)
		e.row, e.col = e.fromIndex(rg.a)
		return ""
	case "p", "P":
		saved, savedLine := e.reg, e.regLine
		e.mode = modeNormal
		e.applyOp("d", rg)
		e.reg, e.regLine = saved, savedLine
		n := len(e.undo)
		if savedLine && !rg.line {
			e.splitLine(false)
			e.row--
			e.put(true, 1)
		} else {
			e.put(false, 1)
		}
		if len(e.undo) > n {
			e.undo = e.undo[:n] // delete + put is a single undo step
		}
		return ""
	}
	e.mode = modeNormal
	text, _ := e.applyOp(op, rg)
	return text
}

func (e *editorModel) visualObject(obj string) {
	rg, ok := e.textObject(obj)
	if !ok {
		return
	}
	if rg.line {
		e.mode = modeVisualLine
		e.vrow, e.vcol = rg.r0, 0
		e.row, e.col = rg.r1, 0
		return
	}
	if rg.b <= rg.a {
		return
	}
	e.vrow, e.vcol = e.fromIndex(rg.a)
	e.row, e.col = e.fromIndex(rg.b - 1)
}

// ---------------------------------------------------------------- text objects

func (e *editorModel) textObject(obj string) (edRange, bool) {
	around := obj[0] == 'a'
	switch o := obj[1:]; o {
	case "w", "W":
		l := e.lines[e.row]
		if len(l) == 0 {
			return edRange{}, false
		}
		big := o == "W"
		cls := func(r rune) int { return wordClass(r, big) }
		c := min(e.col, len(l)-1)
		k := cls(l[c])
		s, t := c, c
		for s > 0 && cls(l[s-1]) == k {
			s--
		}
		for t+1 < len(l) && cls(l[t+1]) == k {
			t++
		}
		if around && k != 0 {
			if t+1 < len(l) && cls(l[t+1]) == 0 {
				for t+1 < len(l) && cls(l[t+1]) == 0 {
					t++
				}
			} else {
				for s > 0 && cls(l[s-1]) == 0 {
					s--
				}
			}
		}
		return edRange{a: e.toIndex(e.row, s), b: e.toIndex(e.row, t+1)}, true
	case `"`, "'", "`":
		q := []rune(o)[0]
		l := e.lines[e.row]
		var qs []int
		for i, r := range l {
			if r == q {
				qs = append(qs, i)
			}
		}
		for k := 0; k+1 < len(qs); k += 2 {
			p0, p1 := qs[k], qs[k+1]
			if e.col <= p1 {
				if around {
					end := p1 + 1
					for end < len(l) && l[end] == ' ' {
						end++
					}
					return edRange{a: e.toIndex(e.row, p0), b: e.toIndex(e.row, end)}, true
				}
				return edRange{a: e.toIndex(e.row, p0+1), b: e.toIndex(e.row, p1)}, true
			}
		}
		return edRange{}, false
	case "(", ")", "b", "[", "]", "{", "}", "B", "<", ">":
		pair := bracketPairs[o]
		open, close := pair[0], pair[1]
		rs := e.flatRunes()
		i := e.toIndex(e.row, e.col)
		if i >= len(rs) {
			i = len(rs) - 1
		}
		o0, depth := -1, 0
		for j := i; j >= 0; j-- {
			switch {
			case rs[j] == close && j != i:
				depth++
			case rs[j] == open:
				if depth == 0 {
					o0 = j
				} else {
					depth--
				}
			}
			if o0 >= 0 {
				break
			}
		}
		if o0 < 0 {
			return edRange{}, false
		}
		c0 := matchForward(rs, o0, open, close)
		if c0 < 0 {
			return edRange{}, false
		}
		if around {
			return edRange{a: o0, b: c0 + 1}, true
		}
		return edRange{a: o0 + 1, b: c0}, true
	case "p":
		blank := func(r int) bool { return strings.TrimSpace(string(e.lines[r])) == "" }
		last := len(e.lines) - 1
		b := blank(e.row)
		r0, r1 := e.row, e.row
		for r0 > 0 && blank(r0-1) == b {
			r0--
		}
		for r1 < last && blank(r1+1) == b {
			r1++
		}
		if around {
			for r1 < last && blank(r1+1) != b {
				r1++
			}
		}
		return edRange{line: true, r0: r0, r1: r1}, true
	}
	return edRange{}, false
}

var bracketPairs = map[string][2]rune{
	"(": {'(', ')'}, ")": {'(', ')'}, "b": {'(', ')'}, "[": {'[', ']'}, "]": {'[', ']'},
	"{": {'{', '}'}, "}": {'{', '}'}, "B": {'{', '}'}, "<": {'<', '>'}, ">": {'<', '>'},
}

// matchForward finds the bracket closing rs[at] (an opening bracket).
func matchForward(rs []rune, at int, open, close rune) int {
	depth := 0
	for j := at + 1; j < len(rs); j++ {
		switch rs[j] {
		case open:
			depth++
		case close:
			if depth == 0 {
				return j
			}
			depth--
		}
	}
	return -1
}

func matchBackward(rs []rune, at int, open, close rune) int {
	depth := 0
	for j := at - 1; j >= 0; j-- {
		switch rs[j] {
		case close:
			depth++
		case open:
			if depth == 0 {
				return j
			}
			depth--
		}
	}
	return -1
}

// ---------------------------------------------------------------- motions

func wordClass(r rune, big bool) int {
	switch {
	case unicode.IsSpace(r):
		return 0
	case big:
		return 1
	case r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
		return 1
	default:
		return 2
	}
}

func wordFwd(rs []rune, i int, big bool) int {
	n := len(rs)
	if i >= n {
		return n
	}
	if k := wordClass(rs[i], big); k != 0 {
		for i < n && wordClass(rs[i], big) == k {
			i++
		}
	}
	for i < n && wordClass(rs[i], big) == 0 {
		if rs[i] == '\n' && i+1 < n && rs[i+1] == '\n' {
			return i + 1 // an empty line counts as a word
		}
		i++
	}
	return i
}

func wordBack(rs []rune, i int, big bool) int {
	i--
	for i > 0 && wordClass(rs[i], big) == 0 {
		i--
	}
	if i <= 0 {
		return 0
	}
	k := wordClass(rs[i], big)
	for i > 0 && wordClass(rs[i-1], big) == k {
		i--
	}
	return i
}

func wordEnd(rs []rune, i int, big bool) int {
	i++
	for i < len(rs) && wordClass(rs[i], big) == 0 {
		i++
	}
	if i >= len(rs) {
		return len(rs) - 1
	}
	k := wordClass(rs[i], big)
	for i+1 < len(rs) && wordClass(rs[i+1], big) == k {
		i++
	}
	return i
}

func wordEndBack(rs []rune, i int, big bool) int {
	if i >= len(rs) {
		i = len(rs) - 1
	}
	if k := wordClass(rs[i], big); k != 0 {
		for i > 0 && wordClass(rs[i], big) == k {
			i--
		}
	}
	for i > 0 && wordClass(rs[i], big) == 0 {
		i--
	}
	return max(0, i)
}

var verticalMotions = map[string]bool{
	"j": true, "k": true, "<up>": true, "<down>": true, "<pgup>": true, "<pgdown>": true,
	"<ctrl+d>": true, "<ctrl+u>": true, "<ctrl+b>": true,
}

var jumpMotions = map[string]bool{
	"G": true, "gg": true, "H": true, "M": true, "L": true, "%": true, "{": true, "}": true,
	"n": true, "N": true, "*": true, "#": true, "'": true, "`": true,
}

// moveTo moves the cursor by a motion (NORMAL and VISUAL mode).
func (e *editorModel) moveTo(c viCmd) {
	v := &e.vi
	from := [2]int{e.row, e.col}
	tr, tc, _, _, ok := e.evalMotion(c.motion, c.arg, c.count, c.hasCount, false)
	if !ok {
		return
	}
	if jumpMotions[c.motion] {
		v.jump, v.hasJump = from, true
	}
	if verticalMotions[c.motion] {
		if !v.lastVertical {
			v.want = e.col
			if v.wantEOL {
				v.want = 1 << 30
			}
		}
		e.row = tr
		e.col = min(v.want, max(0, len(e.lines[tr])-1))
		return
	}
	e.row, e.col = tr, tc
	e.clampNormal()
	v.wantEOL = c.motion == "$" || c.motion == "<end>"
}

// evalMotion returns the target of a motion: position, whether it is
// linewise and whether the target character is included in operator ranges.
func (e *editorModel) evalMotion(m string, arg rune, count int, hasCount, forOp bool) (tr, tc int, lw, incl, ok bool) {
	v := &e.vi
	r, c := e.row, e.col
	last := len(e.lines) - 1
	lineLen := func(row int) int { return len(e.lines[row]) }
	fnb := func(row int) int { return firstNonBlank(e.lines[row]) }
	half := max(1, e.viewHeight()/2)
	switch m {
	case "h", "<left>", "<backspace>":
		if c == 0 {
			return 0, 0, false, false, false
		}
		return r, max(0, c-count), false, false, true
	case "l", "<right>", " ":
		maxc := lineLen(r) - 1
		if forOp {
			maxc = lineLen(r)
		}
		if c >= maxc {
			return 0, 0, false, false, false
		}
		return r, min(c+count, maxc), false, false, true
	case "j", "<down>":
		if r == last {
			return 0, 0, false, false, false
		}
		return min(last, r+count), c, true, false, true
	case "k", "<up>":
		if r == 0 {
			return 0, 0, false, false, false
		}
		return max(0, r-count), c, true, false, true
	case "<pgdown>", "<ctrl+d>":
		step := half
		if m == "<pgdown>" {
			step = max(1, e.viewHeight()-1)
		}
		return min(last, r+step*count), c, true, false, true
	case "<pgup>", "<ctrl+u>", "<ctrl+b>":
		step := half
		if m != "<ctrl+u>" {
			step = max(1, e.viewHeight()-1)
		}
		return max(0, r-step*count), c, true, false, true
	case "+", "<enter>":
		if r == last {
			return 0, 0, false, false, false
		}
		tr = min(last, r+count)
		return tr, fnb(tr), true, false, true
	case "-":
		if r == 0 {
			return 0, 0, false, false, false
		}
		tr = max(0, r-count)
		return tr, fnb(tr), true, false, true
	case "_":
		tr = min(last, r+count-1)
		return tr, fnb(tr), true, false, true
	case "0", "<home>":
		return r, 0, false, false, true
	case "^":
		return r, fnb(r), false, false, true
	case "$", "<end>":
		tr = min(last, r+count-1)
		if lineLen(tr) == 0 {
			return tr, 0, false, false, true
		}
		return tr, lineLen(tr) - 1, false, true, true
	case "g_":
		tr = min(last, r+count-1)
		t := []rune(strings.TrimRight(string(e.lines[tr]), " "))
		return tr, max(0, len(t)-1), false, len(t) > 0, true
	case "w", "W", "b", "B", "e", "E", "ge", "gE":
		rs := e.flatRunes()
		if len(rs) == 0 {
			return 0, 0, false, false, false
		}
		i := e.toIndex(r, c)
		big := m == "W" || m == "B" || m == "E" || m == "gE"
		for k := 0; k < count; k++ {
			switch m {
			case "w", "W":
				i = wordFwd(rs, i, big)
			case "b", "B":
				i = wordBack(rs, i, big)
			case "e", "E":
				i = wordEnd(rs, i, big)
			default:
				i = wordEndBack(rs, i, big)
			}
		}
		if (m == "w" || m == "W") && i >= len(rs) && !forOp {
			i = len(rs) - 1
		}
		tr, tc = e.fromIndex(i)
		incl = m == "e" || m == "E" || m == "ge" || m == "gE"
		return tr, tc, false, incl, true
	case "G":
		tr = last
		if hasCount {
			tr = max(0, min(last, count-1))
		}
		return tr, fnb(tr), true, false, true
	case "gg":
		tr = 0
		if hasCount {
			tr = max(0, min(last, count-1))
		}
		return tr, fnb(tr), true, false, true
	case "H", "M", "L":
		h := e.viewHeight()
		bottom := min(last, e.top+h-1)
		switch m {
		case "H":
			tr = min(bottom, e.top+count-1)
		case "L":
			tr = max(e.top, bottom-count+1)
		default:
			tr = e.top + (bottom-e.top)/2
		}
		return tr, fnb(tr), true, false, true
	case "f", "F", "t", "T":
		v.findCmd, v.findCh = m, arg
		col, found := e.findChar(m, arg, count, false)
		if !found {
			return 0, 0, false, false, false
		}
		return r, col, false, m == "f" || m == "t", true
	case ";", ",":
		if v.findCmd == "" {
			return 0, 0, false, false, false
		}
		cmd := v.findCmd
		if m == "," {
			cmd = map[string]string{"f": "F", "F": "f", "t": "T", "T": "t"}[cmd]
		}
		col, found := e.findChar(cmd, v.findCh, count, true)
		if !found {
			return 0, 0, false, false, false
		}
		return r, col, false, cmd == "f" || cmd == "t", true
	case "%":
		rs := e.flatRunes()
		l := e.lines[r]
		for j := c; j < len(l); j++ {
			idx := e.toIndex(r, j)
			var to int
			switch l[j] {
			case '(':
				to = matchForward(rs, idx, '(', ')')
			case '[':
				to = matchForward(rs, idx, '[', ']')
			case '{':
				to = matchForward(rs, idx, '{', '}')
			case ')':
				to = matchBackward(rs, idx, '(', ')')
			case ']':
				to = matchBackward(rs, idx, '[', ']')
			case '}':
				to = matchBackward(rs, idx, '{', '}')
			default:
				continue
			}
			if to < 0 {
				return 0, 0, false, false, false
			}
			tr, tc = e.fromIndex(to)
			return tr, tc, false, true, true
		}
		return 0, 0, false, false, false
	case "}", "{":
		blank := func(row int) bool { return strings.TrimSpace(string(e.lines[row])) == "" }
		tr = r
		for k := 0; k < count; k++ {
			if m == "}" {
				for tr < last && blank(tr) {
					tr++
				}
				for tr < last && !blank(tr) {
					tr++
				}
			} else {
				for tr > 0 && blank(tr) {
					tr--
				}
				for tr > 0 && !blank(tr) {
					tr--
				}
			}
		}
		if m == "}" && tr == last && !blank(tr) {
			return tr, lineLen(tr), false, false, true // end of buffer
		}
		return tr, 0, false, false, true
	case "n", "N", "*", "#":
		if m == "*" || m == "#" {
			w := e.wordUnderCursor()
			if w == "" {
				return 0, 0, false, false, false
			}
			v.lastSearch, v.hl = w, true
			v.searchDir = 1
			if m == "#" {
				v.searchDir = -1
			}
		}
		if v.lastSearch == "" {
			e.setMsg("no previous search pattern", true)
			return 0, 0, false, false, false
		}
		dir := v.searchDir
		if m == "N" {
			dir = -dir
		}
		from := e.toIndex(r, c)
		if m == "*" || m == "#" {
			from = e.toIndex(r, e.wordStart())
		}
		idx, found := e.findMatch(v.lastSearch, from, dir, count)
		if !found {
			return 0, 0, false, false, false
		}
		v.hl = true
		tr, tc = e.fromIndex(idx)
		return tr, tc, false, false, true
	case "'", "`":
		var p [2]int
		switch {
		case arg == '\'' || arg == '`':
			if !v.hasJump {
				return 0, 0, false, false, false
			}
			p = v.jump
		default:
			mk, okm := v.marks[arg]
			if !okm {
				e.setMsg("mark not set: "+string(arg), true)
				return 0, 0, false, false, false
			}
			p = mk
		}
		tr = min(last, p[0])
		if m == "'" {
			return tr, fnb(tr), true, false, true
		}
		return tr, min(p[1], max(0, lineLen(tr)-1)), false, false, true
	}
	return 0, 0, false, false, false
}

// findChar implements f/F/t/T on the current line.
func (e *editorModel) findChar(cmd string, ch rune, count int, repeat bool) (int, bool) {
	l := e.lines[e.row]
	j := e.col
	switch cmd {
	case "f", "t":
		if cmd == "t" && repeat && j+1 < len(l) && l[j+1] == ch {
			j++
		}
		for k := 0; k < count; k++ {
			j++
			for j < len(l) && l[j] != ch {
				j++
			}
			if j >= len(l) {
				return 0, false
			}
		}
		if cmd == "t" {
			j--
		}
	default:
		if cmd == "T" && repeat && j-1 >= 0 && l[j-1] == ch {
			j--
		}
		for k := 0; k < count; k++ {
			j--
			for j >= 0 && l[j] != ch {
				j--
			}
			if j < 0 {
				return 0, false
			}
		}
		if cmd == "T" {
			j++
		}
	}
	return j, true
}

func (e *editorModel) wordStart() int {
	l := e.lines[e.row]
	c := min(e.col, len(l)-1)
	for c > 0 && wordClass(l[c-1], false) == 1 {
		c--
	}
	return max(0, c)
}

func (e *editorModel) wordUnderCursor() string {
	l := e.lines[e.row]
	if len(l) == 0 {
		return ""
	}
	c := min(e.col, len(l)-1)
	for c < len(l) && wordClass(l[c], false) != 1 {
		c++ // like vim: use the next keyword on the line
	}
	if c >= len(l) {
		return ""
	}
	s, t := c, c
	for s > 0 && wordClass(l[s-1], false) == 1 {
		s--
	}
	for t+1 < len(l) && wordClass(l[t+1], false) == 1 {
		t++
	}
	return string(l[s : t+1])
}

// ---------------------------------------------------------------- search

func (e *editorModel) searchKey(msg tea.KeyMsg) {
	v := &e.vi
	switch msg.String() {
	case "esc":
		v.search.active = false
	case "enter":
		v.search.active = false
		if len(v.search.buf) > 0 {
			v.lastSearch = string(v.search.buf)
		}
		v.searchDir = 1
		if v.lastSearch == "" {
			return
		}
		v.hl = true
		e.moveTo(viCmd{motion: "n", count: 1})
	case "backspace":
		if len(v.search.buf) == 0 {
			v.search.active = false
			return
		}
		v.search.buf = v.search.buf[:len(v.search.buf)-1]
	default:
		if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
			if msg.Type == tea.KeySpace {
				v.search.buf = append(v.search.buf, ' ')
			} else {
				v.search.buf = append(v.search.buf, msg.Runes...)
			}
		}
	}
}

// smartcase: a pattern without capitals matches case-insensitively.
func foldFor(pattern string) func([]rune) []rune {
	for _, r := range pattern {
		if unicode.IsUpper(r) {
			return func(rs []rune) []rune { return rs }
		}
	}
	return func(rs []rune) []rune { return mapRunes(rs, unicode.ToLower) }
}

func runesAt(hay []rune, i int, needle []rune) bool {
	if i < 0 || i+len(needle) > len(hay) {
		return false
	}
	for k, r := range needle {
		if hay[i+k] != r {
			return false
		}
	}
	return true
}

// findMatch searches count times from flat index from in direction dir,
// wrapping around the buffer.
func (e *editorModel) findMatch(pattern string, from, dir, count int) (int, bool) {
	fold := foldFor(pattern)
	hay := fold(e.flatRunes())
	needle := fold([]rune(pattern))
	n := len(hay)
	if n == 0 || len(needle) == 0 {
		return 0, false
	}
	pos := from
	wrapped := false
	for k := 0; k < count; k++ {
		found := false
		for step := 1; step <= n; step++ {
			i := ((pos+dir*step)%n + n) % n
			if runesAt(hay, i, needle) {
				if (dir > 0 && i <= pos) || (dir < 0 && i >= pos) {
					wrapped = true
				}
				pos = i
				found = true
				break
			}
		}
		if !found {
			e.setMsg("pattern not found: "+pattern, true)
			return 0, false
		}
	}
	if wrapped {
		if dir > 0 {
			e.setMsg("search hit BOTTOM, continuing at TOP", false)
		} else {
			e.setMsg("search hit TOP, continuing at BOTTOM", false)
		}
	}
	return pos, true
}

// searchMatches marks the runes of l that match the highlighted search.
func (v *viState) searchMatches(l []rune) []bool {
	if !v.hl || v.lastSearch == "" || len(l) == 0 {
		return nil
	}
	fold := foldFor(v.lastSearch)
	hay := fold(l)
	needle := fold([]rune(v.lastSearch))
	var marks []bool
	for i := 0; i+len(needle) <= len(hay); i++ {
		if runesAt(hay, i, needle) {
			if marks == nil {
				marks = make([]bool, len(l))
			}
			for k := range needle {
				marks[i+k] = true
			}
		}
	}
	return marks
}

// ---------------------------------------------------------------- ex commands

// exCommand runs an editor ':' command. handled is false for commands the
// editor does not know.
func (e *editorModel) exCommand(cmd string) (handled bool) {
	v := &e.vi
	last := len(e.lines) - 1
	switch {
	case cmd == "$":
		e.row = last
		e.col = firstNonBlank(e.lines[e.row])
		return true
	case isDigits(cmd):
		n, _ := strconv.Atoi(cmd)
		e.row = max(0, min(last, n-1))
		e.col = firstNonBlank(e.lines[e.row])
		return true
	case cmd == "noh" || cmd == "nohlsearch":
		v.hl = false
		return true
	case cmd == "u" || cmd == "undo":
		e.doUndo()
		return true
	case cmd == "red" || cmd == "redo":
		e.doRedo()
		return true
	}
	// [range]s/pattern/replacement/[flags]
	r0, r1 := e.row, e.row
	rest := cmd
	switch {
	case strings.HasPrefix(rest, "%"):
		r0, r1, rest = 0, last, rest[1:]
	default:
		if i := strings.IndexFunc(rest, func(r rune) bool { return !(r >= '0' && r <= '9' || r == ',' || r == '.' || r == '$') }); i > 0 {
			a, b, ok := parseExRange(rest[:i], e.row, last)
			if !ok {
				e.setMsg("invalid range: "+rest[:i], true)
				return true
			}
			r0, r1, rest = a, b, rest[i:]
		}
	}
	if strings.HasPrefix(rest, "s") && len(rest) > 1 {
		e.substitute(rest[1:], r0, r1)
		return true
	}
	if rest == "d" {
		e.pushUndo()
		e.reg, e.regLine = e.rangeText(edRange{line: true, r0: r0, r1: r1}), true
		e.deleteRange(edRange{line: true, r0: r0, r1: r1})
		return true
	}
	return false
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func parseExRange(s string, cur, last int) (int, int, bool) {
	addr := func(a string) (int, bool) {
		switch a {
		case ".":
			return cur, true
		case "$":
			return last, true
		}
		n, err := strconv.Atoi(a)
		if err != nil {
			return 0, false
		}
		return max(0, min(last, n-1)), true
	}
	parts := strings.SplitN(s, ",", 2)
	a, ok := addr(parts[0])
	if !ok {
		return 0, 0, false
	}
	b := a
	if len(parts) == 2 {
		if b, ok = addr(parts[1]); !ok {
			return 0, 0, false
		}
	}
	if a > b {
		a, b = b, a
	}
	return a, b, true
}

// substitute implements :s/pat/rep/[gi] over lines r0..r1. The pattern is a
// Go regular expression; \1..\9 and & in the replacement refer to groups.
func (e *editorModel) substitute(spec string, r0, r1 int) {
	if spec == "" {
		return
	}
	delim, size := utf8.DecodeRuneInString(spec)
	parts := splitEscaped(spec[size:], delim)
	if len(parts) < 2 {
		e.setMsg("usage: :s/pattern/replacement/[g][i]", true)
		return
	}
	pat, rep, flags := parts[0], parts[1], ""
	if len(parts) > 2 {
		flags = parts[2]
	}
	if pat == "" {
		pat = regexp.QuoteMeta(e.vi.lastSearch)
	}
	if strings.Contains(flags, "i") {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		e.setMsg("bad pattern: "+err.Error(), true)
		return
	}
	repl := vimReplacement(rep)
	global := strings.Contains(flags, "g")
	changedLines, subs, lastRow := 0, 0, r0
	newLines := make([][]rune, len(e.lines))
	copy(newLines, e.lines)
	for r := r0; r <= r1; r++ {
		s := string(e.lines[r])
		var out string
		if global {
			n := len(re.FindAllStringIndex(s, -1))
			if n == 0 {
				continue
			}
			subs += n
			out = re.ReplaceAllString(s, repl)
		} else {
			loc := re.FindStringSubmatchIndex(s)
			if loc == nil {
				continue
			}
			subs++
			out = s[:loc[0]] + string(re.ExpandString(nil, repl, s, loc)) + s[loc[1]:]
		}
		newLines[r] = []rune(out)
		changedLines++
		lastRow = r
	}
	if subs == 0 {
		e.setMsg("pattern not found: "+parts[0], true)
		return
	}
	e.pushUndo()
	// substitutions may contain newlines: re-split the buffer
	parts2 := make([]string, len(newLines))
	for i, l := range newLines {
		parts2[i] = string(l)
	}
	e.setLinesFromRunes([]rune(strings.Join(parts2, "\n")))
	e.row = min(lastRow, len(e.lines)-1)
	e.col = firstNonBlank(e.lines[e.row])
	e.setMsg(fmt.Sprintf("%d substitution(s) on %d line(s)", subs, changedLines), false)
}

// splitEscaped splits s on delim, honouring backslash-escaped delimiters.
func splitEscaped(s string, delim rune) []string {
	var parts []string
	var cur strings.Builder
	esc := false
	for _, r := range s {
		switch {
		case esc:
			if r != delim {
				cur.WriteRune('\\')
			}
			cur.WriteRune(r)
			esc = false
		case r == '\\':
			esc = true
		case r == delim:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if esc {
		cur.WriteRune('\\')
	}
	return append(parts, cur.String())
}

// vimReplacement converts vi replacement syntax (\1, &, \&) to Go's.
func vimReplacement(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case r == '\\' && i+1 < len(rs):
			n := rs[i+1]
			i++
			switch {
			case n >= '0' && n <= '9':
				b.WriteString("${" + string(n) + "}")
			case n == 'n':
				b.WriteRune('\n')
			case n == 't':
				b.WriteRune('\t')
			case n == '$':
				b.WriteString("$$")
			default:
				b.WriteRune(n)
			}
		case r == '&':
			b.WriteString("${0}")
		case r == '$':
			b.WriteString("$$")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
