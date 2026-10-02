package main

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// tui.go holds the root Bubble Tea model, focus handling, async commands and
// View composition. Panels live in tree.go, editor.go and grid.go; modal
// dialogs in modals.go.
//
// Rendering: Bubble Tea's standard renderer diffs output line by line and
// coalesces frames at a fixed FPS, so a keystroke only rewrites the lines
// that actually changed. Mouse capture is never enabled, so native terminal
// selection and right-click copy keep working.

type focusArea int

const (
	focusSchema focusArea = iota
	focusEditor
	focusGrid
	numFocus
)

// ---------------------------------------------------------------- styles

var (
	colAccent   = lipgloss.Color("14")  // bright cyan
	colDim      = lipgloss.Color("244") // gray
	colBorder   = lipgloss.Color("240")
	colErr      = lipgloss.Color("9")
	colOK       = lipgloss.Color("10")
	colSelectBg = lipgloss.Color("24")

	stFocusBorder = lipgloss.NewStyle().Foreground(colAccent)
	stBlurBorder  = lipgloss.NewStyle().Foreground(colBorder)
	stFocusTitle  = lipgloss.NewStyle().Foreground(colAccent).Bold(true)
	stBlurTitle   = lipgloss.NewStyle().Foreground(colDim)
	stDim         = lipgloss.NewStyle().Foreground(colDim)
	stBold        = lipgloss.NewStyle().Bold(true)
	stCursor      = lipgloss.NewStyle().Reverse(true)
	stSelect      = lipgloss.NewStyle().Background(colSelectBg).Foreground(lipgloss.Color("15"))
	stErr         = lipgloss.NewStyle().Foreground(colErr).Bold(true)
	stOK          = lipgloss.NewStyle().Foreground(colOK)
	stKeyword     = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	stString      = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	stNumber      = lipgloss.NewStyle().Foreground(lipgloss.Color("5"))
	stComment     = lipgloss.NewStyle().Foreground(colDim).Italic(true)
	stNull        = lipgloss.NewStyle().Foreground(colDim).Italic(true)
	stHeader      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	stModeInsert  = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("10")).Bold(true)
	stModeNormal  = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("12")).Bold(true)
	stModeVisual  = lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("13")).Bold(true)
)

// ---------------------------------------------------------------- messages

type execDoneMsg struct {
	sr   *ScriptResult
	edit bool // result came from an in-place edit refresh
}

type schemaMsg struct {
	tables []TableInfo
	err    error
}

type statusMsg struct {
	text string
	err  bool
}

type cellUpdatedMsg struct {
	res      *Result
	row, col int
	cells    []Cell
	err      error
}

type rowsDeletedMsg struct {
	res  *Result
	rows []int // grid row indexes (sorted ascending)
	n    int64
	err  error
}

type rowInsertedMsg struct {
	res   *Result
	rowid int64
	cells []Cell
	err   error
}

// ---------------------------------------------------------------- model

type model struct {
	db     *DB
	width  int
	height int
	focus  focusArea

	tree   *treeModel
	editor *editorModel
	grid   *gridModel
	modal  modal

	status    string
	statusErr bool
	running   bool
	cancel    context.CancelFunc
	lastSQL   string
	cmdline   bool   // vi-style ":" command line is active
	cmdText   string // text typed after ':'
}

// RunTUI starts the full-screen interface.
func RunTUI(db *DB) error {
	m := &model{
		db:     db,
		focus:  focusSchema,
		tree:   newTreeModel(),
		editor: newEditorModel(),
		grid:   newGridModel(),
	}
	m.status = fmt.Sprintf("%s  |  Tab: switch panel  F5/Ctrl+R: run all  F3/Ctrl+K: run statement  :q quit", db.Path)
	// No mouse option: terminal-native selection/copy keep working.
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithFPS(30))
	_, err := p.Run()
	return err
}

func (m *model) Init() tea.Cmd { return m.loadSchema(true) }

func (m *model) loadSchema(initial bool) tea.Cmd {
	return func() tea.Msg {
		ts, err := m.db.Schema()
		if initial {
			return schemaInitMsg{tables: ts, err: err}
		}
		return schemaMsg{tables: ts, err: err}
	}
}

type schemaInitMsg schemaMsg

func (m *model) setStatus(s string, isErr bool) {
	m.status = s
	m.statusErr = isErr
}

func statusCmd(s string, isErr bool) tea.Cmd {
	return func() tea.Msg { return statusMsg{s, isErr} }
}

func clipboardCmd(what, text string) tea.Cmd {
	return func() tea.Msg {
		how, err := copyToClipboard(text)
		if err != nil {
			return statusMsg{"copy failed: " + err.Error(), true}
		}
		return statusMsg{fmt.Sprintf("copied %s (%s)", what, how), false}
	}
}

// Update is the root event handler.
func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case schemaInitMsg:
		if msg.err != nil {
			m.setStatus("schema: "+msg.err.Error(), true)
			return m, nil
		}
		m.tree.setTables(msg.tables, true)
		return m, nil

	case schemaMsg:
		if msg.err != nil {
			m.setStatus("schema: "+msg.err.Error(), true)
			return m, nil
		}
		m.tree.setTables(msg.tables, false)
		return m, nil

	case statusMsg:
		m.setStatus(msg.text, msg.err)
		return m, nil

	case execDoneMsg:
		return m, m.handleExecDone(msg)

	case cellUpdatedMsg:
		if msg.err != nil {
			m.setStatus("update failed: "+msg.err.Error(), true)
			return m, nil
		}
		if m.grid.res == msg.res {
			m.grid.replaceRow(msg.row, msg.cells)
		}
		col := ""
		if msg.col < len(msg.res.Columns) {
			col = msg.res.Columns[msg.col]
		}
		m.setStatus(fmt.Sprintf("updated %s.%s (rowid %d) - logged to %s", msg.res.Edit.Table, col, msg.res.RowIDs[msg.row], m.db.LogPath()), false)
		return m, nil

	case rowsDeletedMsg:
		if msg.err != nil {
			m.setStatus("delete failed: "+msg.err.Error(), true)
			return m, nil
		}
		if m.grid.res == msg.res {
			m.grid.removeRows(msg.rows)
		}
		m.setStatus(fmt.Sprintf("deleted %d row(s) from %s - logged to %s", msg.n, msg.res.Edit.Table, m.db.LogPath()), false)
		return m, nil

	case rowInsertedMsg:
		if msg.err != nil {
			m.setStatus("insert failed: "+msg.err.Error(), true)
			return m, nil
		}
		if m.grid.res == msg.res {
			m.grid.appendRow(msg.rowid, msg.cells)
		}
		m.setStatus(fmt.Sprintf("inserted rowid %d into %s - logged to %s", msg.rowid, msg.res.Edit.Table, m.db.LogPath()), false)
		return m, nil

	case tea.KeyMsg:
		return m, m.handleKey(msg)
	}
	return m, nil
}

func (m *model) handleKey(msg tea.KeyMsg) tea.Cmd {
	// Fast typing (or a slow SSH link) can deliver several runes in one
	// message ("jle"); vi-style commands need them one at a time.
	if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !msg.Paste {
		var cmds []tea.Cmd
		for _, r := range msg.Runes {
			cmds = append(cmds, m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: msg.Alt}))
		}
		return tea.Batch(cmds...)
	}
	key := msg.String()
	// global keys that work everywhere
	switch key {
	case "ctrl+q":
		return tea.Quit
	case "ctrl+c":
		if m.running && m.cancel != nil {
			m.cancel()
			m.setStatus("cancelling...", false)
			return nil
		}
		return tea.Quit
	}
	if m.modal != nil {
		next, cmd := m.modal.update(msg)
		m.modal = next
		return cmd
	}
	if m.cmdline {
		return m.cmdlineKey(msg)
	}
	// ':' opens the command line wherever the key is not text input
	if key == ":" && m.colonAllowed() {
		m.cmdline = true
		m.cmdText = ""
		return nil
	}
	// panel focus cycling
	switch key {
	case "tab", "ctrl+tab":
		if !(m.focus == focusSchema && m.tree.searching) {
			m.focus = (m.focus + 1) % numFocus
			return nil
		}
	case "shift+tab", "ctrl+shift+tab":
		if !(m.focus == focusSchema && m.tree.searching) {
			m.focus = (m.focus + numFocus - 1) % numFocus
			return nil
		}
	case "f5", "ctrl+r":
		return m.runSQL(m.editor.Text(), "all")
	case "f3", "ctrl+k":
		if sel, ok := m.editor.selectionText(); ok {
			m.editor.exitVisual()
			return m.runSQL(sel, "selection")
		}
		return m.runSQL(m.editor.currentStatement(), "statement")
	case "ctrl+f":
		if m.focus == focusGrid {
			break // Ctrl+F pages down in the result grid
		}
		m.editor.setText(formatSQL(m.editor.Text()), true)
		m.focus = focusEditor
		m.setStatus("formatted", false)
		return nil
	case "f1":
		m.modal = newTextModal("Help", strings.Split(helpText, "\n"))
		return nil
	}
	switch m.focus {
	case focusSchema:
		return m.treeKey(msg)
	case focusEditor:
		return m.editorKey(msg)
	case focusGrid:
		return m.gridKey(msg)
	}
	return nil
}

// runSQL executes SQL asynchronously after confirming destructive statements.
func (m *model) runSQL(src, what string) tea.Cmd {
	if m.running {
		m.setStatus("a statement is still running (Ctrl+C to cancel)", true)
		return nil
	}
	stmts := splitStatements(src)
	if len(stmts) == 0 {
		m.setStatus("nothing to execute", true)
		return nil
	}
	var destructive []string
	for _, s := range stmts {
		if isDestructive(s.Text) {
			destructive = append(destructive, s.Text)
		}
	}
	run := func() tea.Cmd {
		ctx, cancel := context.WithCancel(context.Background())
		m.running = true
		m.cancel = cancel
		m.lastSQL = src
		m.setStatus(fmt.Sprintf("running %s (%d statement(s))...", what, len(stmts)), false)
		return func() tea.Msg {
			defer cancel()
			return execDoneMsg{sr: m.db.ExecScript(ctx, src)}
		}
	}
	if len(destructive) == 0 {
		return run()
	}
	var body []string
	body = append(body, "The following statement(s) modify or remove data:", "")
	for _, s := range destructive {
		est := m.db.EstimateAffected(context.Background(), s)
		line := "  " + oneLine(s, 200)
		if est >= 0 {
			line += fmt.Sprintf("   [%d row(s)]", est)
		}
		body = append(body, line)
	}
	body = append(body, "", "Before/after rows are written to "+m.db.LogPath())
	m.modal = newConfirmModal("Confirm destructive SQL", body, "Execute", "Cancel", true,
		func() (modal, tea.Cmd) { return nil, run() },
		func() (modal, tea.Cmd) { return nil, statusCmd("cancelled", false) })
	return nil
}

func (m *model) handleExecDone(msg execDoneMsg) tea.Cmd {
	m.running = false
	m.cancel = nil
	sr := msg.sr
	var cmds []tea.Cmd
	if sr.SchemaChanged || anyWrite(sr) {
		cmds = append(cmds, m.loadSchema(false))
	}
	if last := sr.Last(); last != nil {
		m.grid.setResult(last)
	}
	if sr.Err != nil {
		prefix := ""
		if len(sr.Results) > 0 {
			prefix = fmt.Sprintf("%d statement(s) ok, then ", len(sr.Results))
		}
		m.setStatus(prefix+"error: "+sr.Err.Error()+"  in: "+oneLine(sr.ErrStmt, 60), true)
		return tea.Batch(cmds...)
	}
	m.setStatus(sr.Summary(), false)
	if last := sr.Last(); last != nil && last.HasRows {
		m.focus = focusGrid
	}
	return tea.Batch(cmds...)
}

func anyWrite(sr *ScriptResult) bool {
	for _, r := range sr.Results {
		if r.Kind.IsWrite() || r.Kind == KindOther {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- view

const minWidth, minHeight = 40, 10

func (m *model) View() string {
	if m.width == 0 || m.height == 0 {
		return "loading..."
	}
	if m.width < minWidth || m.height < minHeight {
		return fmt.Sprintf("terminal too small (%dx%d), need at least %dx%d", m.width, m.height, minWidth, minHeight)
	}
	h := m.height - 1 // status bar
	leftW := m.width / 4
	if leftW < 22 {
		leftW = 22
	}
	if leftW > 42 {
		leftW = 42
	}
	rightW := m.width - leftW
	edH := h * 35 / 100
	if edH < 5 {
		edH = 5
	}
	gridH := h - edH

	left := renderPanel(m.tree.title(), m.tree.view(leftW-2, h-2), leftW, h, m.focus == focusSchema)
	top := renderPanel(m.editor.title(), m.editor.view(rightW-2, edH-2, m.focus == focusEditor), rightW, edH, m.focus == focusEditor)
	bottom := renderPanel(m.grid.title(), m.grid.view(rightW-2, gridH-2, m.focus == focusGrid), rightW, gridH, m.focus == focusGrid)
	right := append(top, bottom...)

	lines := make([]string, 0, m.height)
	for i := 0; i < h; i++ {
		lines = append(lines, left[i]+right[i])
	}
	lines = append(lines, m.statusLine())
	if m.modal != nil {
		lines = overlay(lines, m.modal.view(m.width-4, m.height-2), m.width)
	}
	return strings.Join(lines, "\n")
}

// colonAllowed reports whether ':' should open the command line rather than
// being typed (editor INSERT mode, schema filter input).
func (m *model) colonAllowed() bool {
	switch m.focus {
	case focusSchema:
		return !m.tree.searching
	case focusEditor:
		return m.editor.mode == modeNormal && m.editor.pending == ""
	case focusGrid:
		return m.grid.pending == ""
	}
	return false
}

func (m *model) cmdlineKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		m.cmdline = false
	case "enter":
		m.cmdline = false
		return m.runCommand(strings.TrimSpace(m.cmdText))
	case "backspace":
		if r := []rune(m.cmdText); len(r) > 0 {
			m.cmdText = string(r[:len(r)-1])
		} else {
			m.cmdline = false // backspace on empty ':' cancels, like vi
		}
	default:
		if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
			m.cmdText += string(msg.Runes)
		}
	}
	return nil
}

// runCommand executes a ':' command.
func (m *model) runCommand(c string) tea.Cmd {
	switch c {
	case "":
		return nil
	case "q", "q!", "qa", "qa!", "quit", "wq", "x":
		return tea.Quit
	}
	m.setStatus("unknown command: :"+c+"  (available: :q)", true)
	return nil
}

func (m *model) statusLine() string {
	if m.cmdline {
		return truncate(":"+sanitize(m.cmdText)+"█", m.width)
	}
	hint := m.helpHint()
	hw := strWidth(hint)
	s := sanitize(m.status)
	if m.running {
		s = "⏳ " + s
	}
	s = padRight(truncate(s, m.width-hw-1), m.width-hw)
	if m.statusErr {
		s = stErr.Render(s)
	}
	return s + stFocusTitle.Render(hint)
}

// helpHint is the right-aligned help reminder in the status bar. In the
// editor's INSERT mode '?' is typed as text, so F1 is shown instead.
func (m *model) helpHint() string {
	if m.focus == focusEditor && m.editor.mode == modeInsert {
		return " F1: help "
	}
	return " ?: help "
}

// renderPanel draws a bordered box w x h with the title in the top border.
// Focused panels get a bright border/title and a leading "*".
func renderPanel(title string, content []string, w, h int, focused bool) []string {
	bs, ts := stBlurBorder, stBlurTitle
	mark := "  "
	if focused {
		bs, ts = stFocusBorder, stFocusTitle
		mark = "* "
	}
	inner := w - 2
	t := truncate(mark+title, inner-2)
	topFill := inner - 1 - lipgloss.Width(t) - 1
	if topFill < 0 {
		topFill = 0
	}
	out := make([]string, 0, h)
	out = append(out, bs.Render("╭─")+ts.Render(t)+bs.Render(" "+strings.Repeat("─", topFill)+"╮"))
	side := bs.Render("│")
	for i := 0; i < h-2; i++ {
		line := ""
		if i < len(content) {
			line = content[i]
		}
		lw := lipgloss.Width(line)
		if lw > inner {
			line = ansi.Truncate(line, inner, "")
			lw = lipgloss.Width(line)
		}
		out = append(out, side+line+strings.Repeat(" ", inner-lw)+side)
	}
	out = append(out, bs.Render("╰"+strings.Repeat("─", inner)+"╯"))
	return out
}

// overlay draws box (a multi-line string) centered over base lines.
func overlay(base []string, box string, width int) []string {
	bl := strings.Split(box, "\n")
	bw := 0
	for _, l := range bl {
		if w := lipgloss.Width(l); w > bw {
			bw = w
		}
	}
	y0 := (len(base) - len(bl)) / 2
	if y0 < 0 {
		y0 = 0
	}
	x0 := (width - bw) / 2
	if x0 < 0 {
		x0 = 0
	}
	out := append([]string(nil), base...)
	for i, l := range bl {
		y := y0 + i
		if y >= len(out) {
			break
		}
		b := out[y]
		left := ansi.Truncate(b, x0, "")
		if lw := lipgloss.Width(left); lw < x0 {
			left += strings.Repeat(" ", x0-lw)
		}
		l += strings.Repeat(" ", bw-lipgloss.Width(l))
		right := ansi.TruncateLeft(b, x0+bw, "")
		out[y] = left + "\x1b[0m" + l + "\x1b[0m" + right
	}
	return out
}

// ---------------------------------------------------------------- panel key routing

func (m *model) treeKey(msg tea.KeyMsg) tea.Cmd {
	act := m.tree.update(msg)
	switch act.kind {
	case treeActStarter:
		q := "SELECT * FROM " + sqlIdent(act.table) + " LIMIT 100;"
		m.editor.setText(q, true)
		m.editor.mode = modeInsert
		m.focus = focusEditor
		m.setStatus("starter query for "+act.table+" (F5 to run)", false)
	case treeActCopy:
		return clipboardCmd(fmt.Sprintf("%q", act.text), act.text)
	case treeActDDL:
		ddl, err := m.db.DDL(act.table)
		if err != nil {
			m.setStatus(err.Error(), true)
			return nil
		}
		m.modal = newTextModal("DDL: "+act.table, strings.Split(ddl, "\n"))
	case treeActAllDDL:
		ddl, err := m.db.AllDDL()
		if err != nil {
			m.setStatus(err.Error(), true)
			return nil
		}
		m.modal = newTextModal("Schema DDL", strings.Split(ddl, "\n"))
	case treeActRefresh:
		m.setStatus("schema reloaded", false)
		return m.loadSchema(false)
	case treeActHelp:
		m.modal = newTextModal("Help", strings.Split(helpText, "\n"))
	case treeActStatus:
		m.setStatus(act.text, false)
	}
	return nil
}

func (m *model) editorKey(msg tea.KeyMsg) tea.Cmd {
	if m.editor.mode != modeInsert && msg.String() == "?" && m.editor.pending == "" {
		m.modal = newTextModal("Help", strings.Split(helpText, "\n"))
		return nil
	}
	if yank := m.editor.update(msg); yank != "" {
		return clipboardCmd("selection", yank)
	}
	return nil
}

// sqlIdent quotes an identifier only when necessary.
func sqlIdent(name string) string {
	if name == "" {
		return `""`
	}
	for i, r := range name {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return quoteIdent(name)
		}
	}
	if sqlKeywords[strings.ToUpper(name)] {
		return quoteIdent(name)
	}
	return name
}

const helpText = `GLOBAL
  Tab / Shift+Tab        next / previous panel  (Ctrl+Tab / Ctrl+Shift+Tab if the terminal sends them)
  F5, Ctrl+R             execute the whole editor
  F3, Ctrl+K             execute the statement under the cursor (or the VISUAL selection)
  Ctrl+F                 format SQL (in the result grid: page down)
  Ctrl+C                 cancel a running query, otherwise quit
  Ctrl+Q, :q             quit (':' works outside INSERT mode / filter input)
  F1, ?                  this help

SCHEMA
  j/k, Down/Up           move           l/Right   expand / enter first column
  h/Left                 column -> table, table -> collapse
  gg, Home / G, End      top / bottom   Enter     starter SELECT in the editor
  y                      copy table/column name
  v / V                  DDL of table (with indexes, triggers) / whole schema
  /                      filter tables & columns (Enter keep, Esc clear)
  r                      reload schema

SQL EDITOR (vi-like; starts in INSERT)
  Esc                    INSERT -> NORMAL
  i a I A o O            enter INSERT
  h j k l w b e 0 ^ $    motions        gg / G    first / last line
  x dd D C yy p P        edit / yank / put
  u                      undo
  v / V                  VISUAL char / line;  y d c on the selection

RESULT GRID
  h j k l, arrows        move one cell
  Ctrl+F / Ctrl+B        20 rows down / up  (also PgDn / PgUp)
  gg, Home / G, End      first / last row      0 / $  first / last column
  v                      start / end VISUAL block selection
  y                      copy cell (VISUAL: copy block as TSV)
  e, u                   edit cell (single-table SELECT only; not in VISUAL)
  i                      insert row
  d                      delete row (VISUAL: selected rows), double confirmation
  Esc                    leave VISUAL
  Enter                  view full cell value (q, Enter, Esc close)

All writes are logged as JSON lines with before/after rows.`
