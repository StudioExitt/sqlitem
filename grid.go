package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// grid.go: the result grid. All coordinates (row, col) are 0-based indexes
// into res.Rows / res.Columns. The leading '#' column is render-only and
// never takes part in coordinates. With zero rows every motion is a
// structural no-op; only 'i' (insert) stays meaningful.

const (
	gridPage        = 20
	gridMaxColWidth = 40
	gridMinColWidth = 3
)

type gridModel struct {
	res      *Result
	widths   []int // cached per result
	row, col int
	top      int
	left     int // first visible column
	visual   bool
	vrow     int
	vcol     int
	pending  string
	height   int
}

func newGridModel() *gridModel { return &gridModel{} }

func (g *gridModel) setResult(r *Result) {
	g.res = r
	g.row, g.col, g.top, g.left = 0, 0, 0, 0
	g.visual = false
	g.pending = ""
	g.widths = nil
	if r == nil {
		return
	}
	g.widths = make([]int, len(r.Columns))
	for i, c := range r.Columns {
		g.widths[i] = strWidth(sanitize(c))
	}
	for _, row := range r.Rows {
		for i := range g.widths {
			if g.widths[i] >= gridMaxColWidth {
				continue
			}
			if w := strWidth(sanitize(row[i].S)); w > g.widths[i] {
				g.widths[i] = w
			}
		}
	}
	for i := range g.widths {
		g.widths[i] = max(gridMinColWidth, min(gridMaxColWidth, g.widths[i]))
	}
}

// widen grows cached widths for a changed row.
func (g *gridModel) widen(cells []Cell) {
	for i := range g.widths {
		if i < len(cells) {
			if w := strWidth(sanitize(cells[i].S)); w > g.widths[i] {
				g.widths[i] = min(gridMaxColWidth, w)
			}
		}
	}
}

func (g *gridModel) nrows() int {
	if g.res == nil {
		return 0
	}
	return len(g.res.Rows)
}

func (g *gridModel) ncols() int {
	if g.res == nil {
		return 0
	}
	return len(g.res.Columns)
}

func (g *gridModel) replaceRow(i int, cells []Cell) {
	if i < 0 || i >= g.nrows() || len(cells) != g.ncols() {
		return
	}
	g.res.Rows[i] = cells
	g.widen(cells)
}

func (g *gridModel) removeRows(idx []int) {
	if g.res == nil {
		return
	}
	del := map[int]bool{}
	for _, i := range idx {
		del[i] = true
	}
	rows := g.res.Rows[:0]
	var ids []int64
	for i, r := range g.res.Rows {
		if del[i] {
			continue
		}
		rows = append(rows, r)
		if g.res.RowIDs != nil {
			ids = append(ids, g.res.RowIDs[i])
		}
	}
	g.res.Rows = rows
	if g.res.RowIDs != nil {
		g.res.RowIDs = ids
	}
	g.visual = false
	if g.row >= len(rows) {
		g.row = max(0, len(rows)-1)
	}
}

func (g *gridModel) appendRow(rowid int64, cells []Cell) {
	if g.res == nil || len(cells) != g.ncols() {
		return
	}
	g.res.Rows = append(g.res.Rows, cells)
	g.res.RowIDs = append(g.res.RowIDs, rowid)
	g.widen(cells)
	g.row = len(g.res.Rows) - 1
}

func (g *gridModel) title() string {
	if g.res == nil {
		return "Result"
	}
	r := g.res
	if !r.HasRows {
		return "Result"
	}
	pos := ""
	if len(r.Rows) > 0 {
		pos = fmt.Sprintf(" %d/%d", g.row+1, len(r.Rows))
		if r.Truncated {
			pos += "+"
		}
	} else {
		pos = " 0 rows"
	}
	tag := ""
	switch {
	case r.Edit != nil:
		tag = " [" + r.Edit.Table + ": editable]"
	case r.ReadOnly != "":
		tag = " [read-only: " + r.ReadOnly + "]"
	}
	vis := ""
	if g.visual {
		vis = " VISUAL"
	}
	return "Result" + pos + vis + tag
}

// selRect returns the normalized visual block (or the cursor cell).
func (g *gridModel) selRect() (r0, c0, r1, c1 int) {
	if !g.visual {
		return g.row, g.col, g.row, g.col
	}
	r0, r1 = min(g.vrow, g.row), max(g.vrow, g.row)
	c0, c1 = min(g.vcol, g.col), max(g.vcol, g.col)
	return
}

func (g *gridModel) selected(r, c int) bool {
	if !g.visual {
		return false
	}
	r0, c0, r1, c1 := g.selRect()
	return r >= r0 && r <= r1 && c >= c0 && c <= c1
}

// selectionTSV returns the selected block (or current cell) as TSV.
func (g *gridModel) selectionTSV() string {
	r0, c0, r1, c1 := g.selRect()
	var b strings.Builder
	if g.visual {
		for c := c0; c <= c1; c++ {
			if c > c0 {
				b.WriteByte('\t')
			}
			b.WriteString(g.res.Columns[c])
		}
		b.WriteByte('\n')
	}
	for r := r0; r <= r1; r++ {
		for c := c0; c <= c1; c++ {
			if c > c0 {
				b.WriteByte('\t')
			}
			cell := g.res.Rows[r][c]
			if cell.IsNull() {
				if g.visual {
					b.WriteString("NULL")
				}
				continue
			}
			s := cell.S
			if g.visual {
				s = strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s)
			}
			b.WriteString(s)
		}
		if r < r1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// move applies a navigation key; returns false if the key is not a motion.
func (g *gridModel) move(key string) bool {
	n, nc := g.nrows(), g.ncols()
	isMotion := true
	if n == 0 {
		// zero rows: every motion is a no-op
		switch key {
		case "h", "j", "k", "l", "left", "right", "up", "down", "pgup", "pgdown", "ctrl+f", "ctrl+b", "ctrl+d", "ctrl+u",
			"home", "end", "G", "0", "$", "^":
			return true
		}
		return false
	}
	switch key {
	case "j", "down":
		g.row = min(n-1, g.row+1)
	case "k", "up":
		g.row = max(0, g.row-1)
	case "h", "left":
		g.col = max(0, g.col-1)
	case "l", "right":
		g.col = min(nc-1, g.col+1)
	case "ctrl+f", "pgdown", "ctrl+d":
		g.row = min(n-1, g.row+gridPage)
	case "ctrl+b", "pgup", "ctrl+u":
		g.row = max(0, g.row-gridPage)
	case "home":
		g.row = 0
	case "G", "end":
		g.row = n - 1
	case "0", "^":
		g.col = 0
	case "$":
		g.col = nc - 1
	default:
		isMotion = false
	}
	return isMotion
}

// ---------------------------------------------------------------- rendering

func (g *gridModel) view(w, h int, focused bool) []string {
	g.height = h
	out := make([]string, 0, h)
	if g.res == nil {
		out = append(out, stDim.Render(truncate(" Run a query with F5 / Ctrl+R (whole editor) or F3 / Ctrl+K (current statement).", w)))
		return out
	}
	r := g.res
	if !r.HasRows {
		msg := fmt.Sprintf(" OK - %d row(s) affected", r.RowsAffected)
		if r.Kind == KindDDL {
			msg = " OK - schema updated"
		} else if r.Kind == KindOther {
			msg = " OK"
		}
		out = append(out, stOK.Render(truncate(msg, w)))
		out = append(out, stDim.Render(truncate(" "+oneLine(r.SQL, 200), w)))
		return out
	}
	n := len(r.Rows)
	numW := max(1, len(itoa(n)))
	// each row is followed by a horizontal rule, so a row takes two lines
	bodyH := (h - 2 + 1) / 2
	if bodyH < 1 {
		bodyH = 1
	}
	if g.row < g.top {
		g.top = g.row
	}
	if g.row >= g.top+bodyH {
		g.top = g.row - bodyH + 1
	}
	// horizontal: make the cursor column visible
	avail := w - (numW + 1)
	if g.col < g.left {
		g.left = g.col
	}
	for g.left < g.col {
		used := 0
		for c := g.left; c <= g.col; c++ {
			used += g.widths[c] + 3
		}
		if used <= avail {
			break
		}
		g.left++
	}
	// visible columns
	var cols []int
	used := 0
	for c := g.left; c < len(r.Columns); c++ {
		if used >= avail {
			break
		}
		cols = append(cols, c)
		used += g.widths[c] + 3
	}
	colW := func(c int, idx int) int {
		wd := g.widths[c]
		if idx == len(cols)-1 {
			// last visible column may be partial
			rest := avail
			for _, cc := range cols[:idx] {
				rest -= g.widths[cc] + 3
			}
			wd = min(wd, rest-3)
		}
		return max(0, wd)
	}
	sep := stDim.Render("│")
	// header
	var hb strings.Builder
	hb.WriteString(stDim.Render(padLeft("#", numW)))
	hb.WriteString(sep)
	for idx, c := range cols {
		cw := colW(c, idx)
		name := " " + padRight(sanitize(r.Columns[c]), cw) + " "
		readonly := r.Edit != nil && r.Edit.ColMap[c] == ""
		switch {
		case readonly:
			hb.WriteString(stDim.Render(name))
		default:
			hb.WriteString(stHeader.Render(name))
		}
		if idx < len(cols)-1 || cw == g.widths[c] {
			hb.WriteString(sep)
		}
	}
	out = append(out, hb.String())
	// separator line
	var sb strings.Builder
	sb.WriteString(strings.Repeat("─", numW) + "┼")
	for idx, c := range cols {
		cw := colW(c, idx)
		sb.WriteString(strings.Repeat("─", cw+2))
		if idx < len(cols)-1 || cw == g.widths[c] {
			sb.WriteString("┼")
		}
	}
	ruleText := sb.String()
	bottomText := strings.ReplaceAll(ruleText, "┼", "┴")
	if strings.HasSuffix(ruleText, "┼") { // right edge of a fully visible last column
		ruleText = strings.TrimSuffix(ruleText, "┼") + "┤"
		bottomText = strings.TrimSuffix(bottomText, "┴") + "┘"
	}
	rule := stDim.Render(truncate(ruleText, w))
	out = append(out, rule)
	if n == 0 {
		msg := " (0 rows)"
		if r.Edit != nil {
			msg += "  press i to insert a row"
		}
		out = append(out, stDim.Render(truncate(msg, w)))
		return out
	}
	for ri := g.top; ri < n && ri < g.top+bodyH; ri++ {
		if ri > g.top {
			out = append(out, rule)
		}
		var b strings.Builder
		num := padLeft(itoa(ri+1), numW)
		if ri == g.row {
			b.WriteString(stBold.Render(num))
		} else {
			b.WriteString(stDim.Render(num))
		}
		b.WriteString(sep)
		for idx, c := range cols {
			cw := colW(c, idx)
			cell := r.Rows[ri][c]
			text := sanitize(cell.S)
			var s string
			if isNumeric(cell) {
				s = " " + padLeft(text, cw) + " "
			} else {
				s = " " + padRight(text, cw) + " "
			}
			switch {
			case ri == g.row && c == g.col && focused:
				s = stCursor.Render(s)
			case g.selected(ri, c):
				s = stSelect.Render(s)
			case ri == g.row && c == g.col:
				s = stBold.Underline(true).Render(s)
			case cell.IsNull():
				s = stNull.Render(s)
			}
			b.WriteString(s)
			if idx < len(cols)-1 || cw == g.widths[c] {
				b.WriteString(sep)
			}
		}
		out = append(out, b.String())
	}
	// close the table with a bottom rule when there is room
	if len(out) < h {
		out = append(out, stDim.Render(truncate(bottomText, w)))
	}
	return out
}

// ---------------------------------------------------------------- key handling (root model)

func (m *model) gridKey(msg tea.KeyMsg) tea.Cmd {
	g := m.grid
	key := msg.String()
	if p := g.pending; p != "" {
		g.pending = ""
		switch p + key {
		case "gg":
			if g.nrows() > 0 {
				g.row = 0
			}
			return nil
		}
		return nil
	}
	if g.move(key) {
		return nil
	}
	switch key {
	case "g":
		g.pending = "g"
	case "?":
		m.modal = newTextModal("Help", strings.Split(helpText, "\n"))
	case "esc":
		if g.visual {
			g.visual = false
		}
	case "v", "ctrl+v":
		if g.nrows() == 0 {
			return nil
		}
		if g.visual {
			g.visual = false
		} else {
			g.visual = true
			g.vrow, g.vcol = g.row, g.col
		}
	case "y":
		if g.nrows() == 0 {
			return nil
		}
		text := g.selectionTSV()
		what := "cell"
		if g.visual {
			r0, c0, r1, c1 := g.selRect()
			what = fmt.Sprintf("%dx%d block", r1-r0+1, c1-c0+1)
			g.visual = false
		}
		return clipboardCmd(what, text)
	case "Y":
		if g.nrows() == 0 {
			return nil
		}
		g.visual = false
		cells := g.res.Rows[g.row]
		parts := make([]string, len(cells))
		for i, c := range cells {
			parts[i] = c.S
		}
		return clipboardCmd("row", strings.Join(parts, "\t"))
	case "enter":
		if g.nrows() == 0 {
			return nil
		}
		cell := g.res.Rows[g.row][g.col]
		title := fmt.Sprintf("%s (row %d)", g.res.Columns[g.col], g.row+1)
		body := strings.Split(cell.S, "\n")
		if cell.IsNull() {
			body = []string{"NULL"}
		}
		m.modal = newTextModal(title, body)
	case "e", "u":
		if g.visual {
			m.setStatus("cell edit is not available in VISUAL mode (Esc to leave)", true)
			return nil
		}
		return m.editCellFlow()
	case "i":
		return m.insertRowFlow()
	case "d":
		// current row, or every row covered by the VISUAL block
		return m.deleteRowsFlow()
	}
	return nil
}

// editableReason returns "" if the current result can be edited.
func (m *model) editableReason() string {
	r := m.grid.res
	switch {
	case r == nil:
		return "no result"
	case !r.HasRows:
		return "not a query result"
	case r.Edit == nil:
		if r.ReadOnly != "" {
			return "read-only result: " + r.ReadOnly
		}
		return "read-only result"
	}
	return ""
}

func (m *model) editCellFlow() tea.Cmd {
	if why := m.editableReason(); why != "" {
		m.setStatus(why+" - editing needs a single-table SELECT", true)
		return nil
	}
	g := m.grid
	if g.nrows() == 0 {
		return nil
	}
	r := g.res
	col := r.Edit.ColMap[g.col]
	if col == "" {
		m.setStatus(fmt.Sprintf("column %q is read-only (computed expression, generated column or rowid alias of %s)", r.Columns[g.col], r.Edit.Table), true)
		return nil
	}
	orig := r.Rows[g.row][g.col]
	if _, isBlob := orig.V.([]byte); isBlob {
		m.setStatus("BLOB values cannot be edited in the grid", true)
		return nil
	}
	rowIdx, colIdx := g.row, g.col
	rowid := r.RowIDs[rowIdx]
	var ci ColumnInfo
	for _, c := range r.Edit.TableCols {
		if c.Name == col {
			ci = c
		}
	}
	em := newCellEditModal(r.Edit.Table, ci, rowid, orig)
	em.onSubmit = func(value any, isNull bool) (modal, tea.Cmd) {
		newDisp := "NULL"
		if !isNull {
			newDisp = "'" + strings.ReplaceAll(value.(string), "'", "''") + "'"
		}
		body := []string{
			fmt.Sprintf("Table : %s", r.Edit.Table),
			fmt.Sprintf("Row   : rowid = %d", rowid),
			fmt.Sprintf("Column: %s", col),
			"",
			"Before: " + oneLine(orig.SQLLiteral(), 200),
			"After : " + oneLine(newDisp, 200),
		}
		if !isNull && orig.SQLLiteral() == newDisp {
			body = append(body, "", "(value unchanged)")
		}
		cm := newConfirmModal("Apply change?", body, "Apply", "Back", true,
			func() (modal, tea.Cmd) {
				return nil, func() tea.Msg {
					var v any
					if !isNull {
						v = value
					}
					ctx := context.Background()
					stored, err := m.db.UpdateCell(ctx, r.Edit.Table, rowid, col, v)
					if err != nil {
						return cellUpdatedMsg{res: r, err: err}
					}
					old := r.Rows[rowIdx]
					cells := append([]Cell(nil), old...)
					cells[colIdx] = stored
					// re-read the row so triggers/affinity changes show up;
					// computed columns keep their old values
					if fresh, err := m.db.FetchRow(ctx, r.Edit.Table, rowid, r.Edit.ColMap); err == nil {
						for i, c := range r.Edit.ColMap {
							if c != "" {
								cells[i] = fresh[i]
							}
						}
					}
					return cellUpdatedMsg{res: r, row: rowIdx, col: colIdx, cells: cells}
				}
			},
			// Cancel returns to the edit modal (kept in this closure).
			func() (modal, tea.Cmd) { return em, nil })
		return cm, nil
	}
	m.modal = em
	return nil
}

func (m *model) deleteRowsFlow() tea.Cmd {
	if why := m.editableReason(); why != "" {
		m.setStatus(why+" - deleting needs a single-table SELECT", true)
		return nil
	}
	g := m.grid
	if g.nrows() == 0 {
		return nil
	}
	r := g.res
	r0, _, r1, _ := g.selRect()
	var idx []int
	var ids []int64
	for i := r0; i <= r1; i++ {
		idx = append(idx, i)
		ids = append(ids, r.RowIDs[i])
	}
	sort.Ints(idx)
	preview := []string{fmt.Sprintf("Delete %d row(s) from %s?", len(ids), r.Edit.Table), ""}
	for k, i := range idx {
		if k >= 8 {
			preview = append(preview, fmt.Sprintf("  ... and %d more", len(idx)-k))
			break
		}
		var vals []string
		for c, cell := range r.Rows[i] {
			if c >= 6 {
				vals = append(vals, "...")
				break
			}
			vals = append(vals, r.Columns[c]+"="+oneLine(cell.SQLLiteral(), 20))
		}
		preview = append(preview, fmt.Sprintf("  rowid %d: %s", r.RowIDs[i], strings.Join(vals, ", ")))
	}
	var first *confirmModal
	first = newConfirmModal("Delete rows", preview, "Delete", "Cancel", true,
		func() (modal, tea.Cmd) {
			second := newConfirmModal("Really delete?", []string{
				fmt.Sprintf("This permanently deletes %d row(s) from %s.", len(ids), r.Edit.Table),
				"The deleted rows are written to the audit log.",
			}, "Yes, delete", "Back", true,
				func() (modal, tea.Cmd) {
					return nil, func() tea.Msg {
						n, err := m.db.DeleteRows(context.Background(), r.Edit.Table, ids)
						return rowsDeletedMsg{res: r, rows: idx, n: n, err: err}
					}
				},
				// Cancel goes back to the first confirmation
				func() (modal, tea.Cmd) { return first, nil })
			return second, nil
		},
		func() (modal, tea.Cmd) { return nil, statusCmd("delete cancelled", false) })
	m.modal = first
	return nil
}

func (m *model) insertRowFlow() tea.Cmd {
	if why := m.editableReason(); why != "" {
		m.setStatus(why+" - inserting needs a single-table SELECT", true)
		return nil
	}
	r := m.grid.res
	im := newInsertModal(r.Edit.Table, r.Edit.TableCols)
	im.onSubmit = func(values map[string]any) (modal, tea.Cmd) {
		body := []string{"INSERT INTO " + r.Edit.Table, ""}
		for _, c := range r.Edit.TableCols {
			if c.Hidden {
				continue
			}
			v, ok := values[c.Name]
			switch {
			case !ok:
				body = append(body, fmt.Sprintf("  %-20s DEFAULT", c.Name))
			case v == nil:
				body = append(body, fmt.Sprintf("  %-20s NULL", c.Name))
			default:
				body = append(body, fmt.Sprintf("  %-20s %s", c.Name, oneLine("'"+strings.ReplaceAll(v.(string), "'", "''")+"'", 60)))
			}
		}
		cm := newConfirmModal("Insert row?", body, "Insert", "Back", false,
			func() (modal, tea.Cmd) {
				return nil, func() tea.Msg {
					ctx := context.Background()
					id, err := m.db.InsertRow(ctx, r.Edit.Table, values)
					if err != nil {
						return rowInsertedMsg{res: r, err: err}
					}
					cells, err := m.db.FetchRow(ctx, r.Edit.Table, id, r.Edit.ColMap)
					return rowInsertedMsg{res: r, rowid: id, cells: cells, err: err}
				}
			},
			func() (modal, tea.Cmd) { return im, nil })
		return cm, nil
	}
	m.modal = im
	return nil
}
