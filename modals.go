package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
)

// modals.go: modal dialogs. A modal receives all keys while open and returns
// the next modal to show (itself, another modal, or nil to close). Multi-step
// flows keep the previous modal in a closure so Cancel/Back can return to it.

type modal interface {
	update(msg tea.KeyMsg) (modal, tea.Cmd)
	view(maxW, maxH int) string
}

// box draws a bordered dialog with a title.
func box(title string, lines []string, width int) string {
	inner := width - 4
	var b strings.Builder
	t := truncate(" "+title+" ", inner)
	fill := max(0, inner+2-1-lipgloss.Width(t))
	b.WriteString(stFocusBorder.Render("╭─") + stFocusTitle.Render(t) + stFocusBorder.Render(strings.Repeat("─", fill)+"╮") + "\n")
	side := stFocusBorder.Render("│")
	for _, l := range lines {
		lw := lipgloss.Width(l)
		if lw > inner {
			l = ansi.Truncate(l, inner, "…")
			lw = lipgloss.Width(l)
		}
		b.WriteString(side + " " + l + strings.Repeat(" ", max(0, inner-lw)) + " " + side + "\n")
	}
	b.WriteString(stFocusBorder.Render("╰" + strings.Repeat("─", inner+2) + "╯"))
	return b.String()
}

func dialogWidth(maxW, want int) int {
	w := min(maxW, want)
	return max(w, 20)
}

func button(label string, active bool) string {
	if active {
		return stCursor.Render(" " + label + " ")
	}
	return stDim.Render("[" + label + "]")
}

// ---------------------------------------------------------------- text viewer

// textModal shows scrollable read-only text (DDL, help, cell values).
type textModal struct {
	title string
	lines []string
	top   int
	h     int
	pendG bool
}

func newTextModal(title string, lines []string) *textModal {
	return &textModal{title: title, lines: lines}
}

func (t *textModal) update(msg tea.KeyMsg) (modal, tea.Cmd) {
	page := max(1, t.h-1)
	key := msg.String()
	if t.pendG {
		t.pendG = false
		if key == "g" {
			t.top = 0
			return t, nil
		}
	}
	switch key {
	case "q", "enter", "esc":
		return nil, nil
	case "j", "down":
		t.top++
	case "k", "up":
		t.top--
	case "pgdown", " ", "ctrl+d":
		t.top += page
	case "pgup", "ctrl+u":
		t.top -= page
	case "g":
		t.pendG = true
	case "home":
		t.top = 0
	case "G", "end":
		t.top = len(t.lines)
	case "y":
		return t, clipboardCmd(t.title, strings.Join(t.lines, "\n"))
	}
	return t, nil
}

func (t *textModal) view(maxW, maxH int) string {
	longest := 0
	for _, l := range t.lines {
		longest = max(longest, runewidth.StringWidth(l))
	}
	w := dialogWidth(maxW, max(longest+4, 50))
	// wrap long lines to the dialog width
	var wrapped []string
	for _, l := range t.lines {
		wrapped = append(wrapped, wrapLine(sanitize(strings.ReplaceAll(l, "\t", "    ")), w-4)...)
	}
	h := min(maxH-3, len(wrapped))
	h = max(h, 1)
	t.h = h
	if t.top > len(wrapped)-h {
		t.top = len(wrapped) - h
	}
	if t.top < 0 {
		t.top = 0
	}
	var lines []string
	for i := t.top; i < t.top+h && i < len(wrapped); i++ {
		lines = append(lines, highlightSQLLine(wrapped[i]))
	}
	footer := "q/Enter/Esc close  y copy"
	if len(wrapped) > h {
		footer = fmt.Sprintf("%d-%d/%d  j/k scroll  ", t.top+1, min(len(wrapped), t.top+h), len(wrapped)) + footer
	}
	lines = append(lines, stDim.Render(footer))
	return box(t.title, lines, w)
}

// wrapLine hard-wraps s to lines of at most w display cells.
func wrapLine(s string, w int) []string {
	if w < 1 || runewidth.StringWidth(s) <= w {
		return []string{s}
	}
	var out []string
	var cur []rune
	cw := 0
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if cw+rw > w {
			out = append(out, string(cur))
			cur, cw = nil, 0
		}
		cur = append(cur, r)
		cw += rw
	}
	return append(out, string(cur))
}

// highlightSQLLine applies light syntax colouring to a single line.
func highlightSQLLine(s string) string {
	if !strings.ContainsAny(s, "'-") && !hasKeyword(s) {
		return s
	}
	var b strings.Builder
	for _, t := range tokenize(s) {
		switch {
		case t.kind == tkWord && sqlKeywords[strings.ToUpper(t.text)]:
			b.WriteString(stKeyword.Render(t.text))
		case t.kind == tkString:
			b.WriteString(stString.Render(t.text))
		case t.kind == tkComment:
			b.WriteString(stComment.Render(t.text))
		default:
			b.WriteString(t.text)
		}
	}
	return b.String()
}

func hasKeyword(s string) bool {
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return !isIdentPart(r) }) {
		if sqlKeywords[strings.ToUpper(f)] {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- confirm

type confirmModal struct {
	title   string
	body    []string
	yes, no string
	selYes  bool
	onYes   func() (modal, tea.Cmd)
	onNo    func() (modal, tea.Cmd)
	top     int
}

// newConfirmModal builds a yes/no dialog. When safeDefault is true the
// cancel button is preselected so a stray Enter does nothing destructive.
func newConfirmModal(title string, body []string, yes, no string, safeDefault bool,
	onYes, onNo func() (modal, tea.Cmd)) *confirmModal {
	return &confirmModal{title: title, body: body, yes: yes, no: no, selYes: !safeDefault, onYes: onYes, onNo: onNo}
}

func (c *confirmModal) update(msg tea.KeyMsg) (modal, tea.Cmd) {
	switch msg.String() {
	case "left", "right", "h", "l", "tab", "shift+tab":
		c.selYes = !c.selYes
	case "y", "Y":
		return c.onYes()
	case "n", "N", "esc", "q":
		return c.onNo()
	case "enter", " ":
		if c.selYes {
			return c.onYes()
		}
		return c.onNo()
	case "j", "down":
		c.top++
	case "k", "up":
		c.top = max(0, c.top-1)
	}
	return c, nil
}

func (c *confirmModal) view(maxW, maxH int) string {
	longest := 0
	for _, l := range c.body {
		longest = max(longest, runewidth.StringWidth(l))
	}
	w := dialogWidth(maxW, max(50, longest+4))
	room := max(1, maxH-5)
	body := c.body
	if len(body) > room {
		c.top = min(c.top, len(body)-room)
		body = body[c.top : c.top+room]
	}
	lines := make([]string, 0, len(body)+3)
	for _, l := range body {
		lines = append(lines, sanitize(l))
	}
	lines = append(lines, "")
	lines = append(lines, button(c.yes, c.selYes)+"  "+button(c.no, !c.selYes)+stDim.Render("   y/n, ←/→ + Enter"))
	return box(c.title, lines, w)
}

// ---------------------------------------------------------------- line input

// lineInput is a minimal single-line text field.
type lineInput struct {
	text []rune
	pos  int
	off  int
}

func newLineInput(s string) lineInput {
	r := []rune(s)
	return lineInput{text: r, pos: len(r)}
}

func (li *lineInput) String() string { return string(li.text) }

// handle returns true if the key changed the text.
func (li *lineInput) handle(msg tea.KeyMsg) (changed, handled bool) {
	switch msg.String() {
	case "left", "ctrl+b":
		li.pos = max(0, li.pos-1)
	case "right", "ctrl+f":
		li.pos = min(len(li.text), li.pos+1)
	case "home", "ctrl+a":
		li.pos = 0
	case "end", "ctrl+e":
		li.pos = len(li.text)
	case "backspace", "ctrl+h":
		if li.pos > 0 {
			li.text = append(li.text[:li.pos-1], li.text[li.pos:]...)
			li.pos--
			return true, true
		}
	case "delete", "ctrl+d":
		if li.pos < len(li.text) {
			li.text = append(li.text[:li.pos], li.text[li.pos+1:]...)
			return true, true
		}
	case "ctrl+u":
		li.text = li.text[li.pos:]
		li.pos = 0
		return true, true
	case "ctrl+k":
		li.text = li.text[:li.pos]
		return true, true
	default:
		if (msg.Type == tea.KeyRunes && !msg.Alt) || msg.Type == tea.KeySpace {
			rs := msg.Runes
			if msg.Type == tea.KeySpace {
				rs = []rune{' '}
			}
			nt := make([]rune, 0, len(li.text)+len(rs))
			nt = append(nt, li.text[:li.pos]...)
			for _, r := range rs {
				if r == '\r' {
					continue
				}
				nt = append(nt, r)
			}
			added := len(nt) - li.pos
			nt = append(nt, li.text[li.pos:]...)
			li.text = nt
			li.pos += added
			return true, true
		}
		return false, false
	}
	return false, true
}

// view renders the field w cells wide; cursor shown when focused.
func (li *lineInput) view(w int, focused bool, dim bool) string {
	disp := make([]rune, len(li.text))
	for i, r := range li.text {
		switch {
		case r == '\n':
			disp[i] = '↵'
		case r == '\t':
			disp[i] = ' '
		case r < 0x20:
			disp[i] = '?'
		default:
			disp[i] = r
		}
	}
	// keep cursor in view (rune based, approximate for wide chars)
	if li.pos < li.off {
		li.off = li.pos
	}
	for runewidth.StringWidth(string(disp[li.off:li.pos])) >= w && li.off < li.pos {
		li.off++
	}
	base := lipgloss.NewStyle().Underline(true)
	if dim {
		base = stNull
	}
	var b strings.Builder
	var run strings.Builder
	flush := func() {
		if run.Len() > 0 {
			b.WriteString(base.Render(run.String()))
			run.Reset()
		}
	}
	used := 0
	for i := li.off; i < len(disp); i++ {
		rw := runewidth.RuneWidth(disp[i])
		if used+rw > w {
			break
		}
		if focused && i == li.pos {
			flush()
			b.WriteString(stCursor.Render(string(disp[i])))
		} else {
			run.WriteRune(disp[i])
		}
		used += rw
	}
	if focused && li.pos >= len(disp) && used < w {
		flush()
		b.WriteString(stCursor.Render(" "))
		used++
	}
	run.WriteString(strings.Repeat(" ", max(0, w-used)))
	flush()
	return b.String()
}

// ---------------------------------------------------------------- cell edit

type cellEditModal struct {
	table    string
	col      ColumnInfo
	rowid    int64
	orig     Cell
	input    lineInput
	setNull  bool
	focus    int // 0 input, 1 null checkbox, 2 OK, 3 Cancel
	onSubmit func(value any, isNull bool) (modal, tea.Cmd)
}

func newCellEditModal(table string, col ColumnInfo, rowid int64, orig Cell) *cellEditModal {
	init := orig.S
	if orig.IsNull() {
		init = ""
	}
	return &cellEditModal{table: table, col: col, rowid: rowid, orig: orig,
		input: newLineInput(init), setNull: orig.IsNull()}
}

func (c *cellEditModal) submit() (modal, tea.Cmd) {
	if c.setNull {
		if c.col.NotNull {
			// let SQLite enforce it, but warn early for a better message
			return c, statusCmd(fmt.Sprintf("column %s is NOT NULL", c.col.Name), true)
		}
		return c.onSubmit(nil, true)
	}
	return c.onSubmit(c.input.String(), false)
}

func (c *cellEditModal) update(msg tea.KeyMsg) (modal, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return nil, statusCmd("edit cancelled", false)
	case "tab", "down":
		c.focus = (c.focus + 1) % 4
		return c, nil
	case "shift+tab", "up":
		c.focus = (c.focus + 3) % 4
		return c, nil
	case "ctrl+n":
		c.setNull = !c.setNull
		return c, nil
	case "enter":
		if c.focus == 3 {
			return nil, statusCmd("edit cancelled", false)
		}
		return c.submit()
	}
	switch c.focus {
	case 0:
		if changed, _ := c.input.handle(msg); changed {
			c.setNull = false
		}
	case 1:
		if msg.String() == " " || msg.String() == "x" {
			c.setNull = !c.setNull
		}
	case 2, 3:
		switch msg.String() {
		case "left", "right", "h", "l":
			c.focus = 5 - c.focus
		}
	}
	return c, nil
}

func (c *cellEditModal) view(maxW, maxH int) string {
	w := dialogWidth(maxW, 70)
	inner := w - 4
	check := "[ ]"
	if c.setNull {
		check = "[x]"
	}
	nullLine := check + " Set NULL"
	if c.focus == 1 {
		nullLine = stCursor.Render(nullLine)
	}
	typ := c.col.Type
	if typ == "" {
		typ = "any"
	}
	flags := columnFlags(c.col, false)
	lines := []string{
		stDim.Render(truncate(fmt.Sprintf("%s.%s  (%s %s)  rowid=%d", c.table, c.col.Name, strings.ToLower(typ), flags, c.rowid), inner)),
		stDim.Render("current: ") + truncate(sanitize(c.orig.SQLLiteral()), inner-9),
		"",
		c.input.view(inner, c.focus == 0, c.setNull),
		"",
		nullLine + stDim.Render("   (Ctrl+N toggles)"),
		"",
		button("OK", c.focus == 2) + "  " + button("Cancel", c.focus == 3) + stDim.Render("   Enter apply, Esc cancel, Tab move"),
	}
	return box("Edit cell", lines, w)
}

// ---------------------------------------------------------------- insert row

type insertModal struct {
	table    string
	cols     []ColumnInfo
	inputs   []lineInput
	state    []int // 0 DEFAULT, 1 value, 2 NULL
	focus    int   // field index; len(cols) = OK, len(cols)+1 = Cancel
	top      int
	onSubmit func(values map[string]any) (modal, tea.Cmd)
}

func newInsertModal(table string, cols []ColumnInfo) *insertModal {
	var vis []ColumnInfo
	for _, c := range cols {
		if !c.Hidden {
			vis = append(vis, c)
		}
	}
	m := &insertModal{table: table, cols: vis, inputs: make([]lineInput, len(vis)), state: make([]int, len(vis))}
	return m
}

func (im *insertModal) values() map[string]any {
	v := map[string]any{}
	for i, c := range im.cols {
		switch im.state[i] {
		case 1:
			v[c.Name] = im.inputs[i].String()
		case 2:
			v[c.Name] = nil
		}
	}
	return v
}

func (im *insertModal) update(msg tea.KeyMsg) (modal, tea.Cmd) {
	n := len(im.cols)
	switch msg.String() {
	case "esc":
		return nil, statusCmd("insert cancelled", false)
	case "tab", "down":
		im.focus = (im.focus + 1) % (n + 2)
		return im, nil
	case "shift+tab", "up":
		im.focus = (im.focus + n + 1) % (n + 2)
		return im, nil
	case "ctrl+n":
		if im.focus < n {
			if im.state[im.focus] == 2 {
				im.state[im.focus] = 0
			} else {
				im.state[im.focus] = 2
			}
		}
		return im, nil
	case "ctrl+d":
		if im.focus < n {
			im.state[im.focus] = 0
			im.inputs[im.focus] = newLineInput("")
		}
		return im, nil
	case "enter":
		if im.focus == n+1 {
			return nil, statusCmd("insert cancelled", false)
		}
		return im.onSubmit(im.values())
	}
	if im.focus < n {
		if changed, _ := im.inputs[im.focus].handle(msg); changed {
			im.state[im.focus] = 1
		}
	} else {
		switch msg.String() {
		case "left", "right", "h", "l":
			if im.focus == n {
				im.focus = n + 1
			} else {
				im.focus = n
			}
		}
	}
	return im, nil
}

func (im *insertModal) view(maxW, maxH int) string {
	w := dialogWidth(maxW, 80)
	inner := w - 4
	nameW := 4
	for _, c := range im.cols {
		nameW = max(nameW, runewidth.StringWidth(c.Name))
	}
	nameW = min(nameW, inner/3)
	room := max(1, maxH-8)
	if im.focus < len(im.cols) {
		if im.focus < im.top {
			im.top = im.focus
		}
		if im.focus >= im.top+room {
			im.top = im.focus - room + 1
		}
	}
	lines := []string{stDim.Render(truncate("Type to set a value. Ctrl+N: NULL, Ctrl+D: DEFAULT. Untouched fields use DEFAULT.", inner)), ""}
	for i := im.top; i < len(im.cols) && i < im.top+room; i++ {
		c := im.cols[i]
		label := padRight(c.Name, nameW)
		if im.focus == i {
			label = stFocusTitle.Render(label)
		}
		meta := strings.TrimSpace(strings.ToLower(c.Type) + " " + columnFlags(c, false))
		fieldW := max(5, inner-nameW-2-min(18, runewidth.StringWidth(meta))-1)
		var field string
		switch im.state[i] {
		case 0:
			ph := "DEFAULT"
			if c.Default.Valid {
				ph = "DEFAULT " + c.Default.String
			}
			if im.focus == i {
				field = stCursor.Render(" ") + stNull.Render(padRight(ph, fieldW-1))
			} else {
				field = stNull.Render(padRight(ph, fieldW))
			}
		case 2:
			field = stNull.Render(padRight("NULL", fieldW))
			if im.focus == i {
				field = stCursor.Render("N") + stNull.Render(padRight("ULL", fieldW-1))
			}
		default:
			field = im.inputs[i].view(fieldW, im.focus == i, false)
		}
		lines = append(lines, label+"  "+field+" "+stDim.Render(truncate(meta, 18)))
	}
	if len(im.cols) > room {
		lines = append(lines, stDim.Render(fmt.Sprintf("(%d columns, scroll with Tab/↑/↓)", len(im.cols))))
	}
	n := len(im.cols)
	lines = append(lines, "", button("Insert", im.focus == n)+"  "+button("Cancel", im.focus == n+1)+stDim.Render("   Enter submit, Esc cancel"))
	return box("Insert into "+im.table, lines, w)
}
