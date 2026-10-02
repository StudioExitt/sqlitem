package main

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// tree.go: the schema panel. Tables and views form a two level tree; the
// expanded nodes are flattened into a list and navigation moves linearly
// through it (so 'j' on a table's last column lands on the next table).

// autoExpandLimit: with fewer tables than this, everything starts expanded.
const autoExpandLimit = 10

type treeNode struct {
	table int // index into tables
	col   int // -1 for the table row itself
}

type treeModel struct {
	tables    []TableInfo
	expanded  map[string]bool
	cursor    int
	offset    int
	filter    string
	searching bool // typing a filter
	pendingG  bool
	height    int
	loaded    bool
}

type treeActKind int

const (
	treeActNone treeActKind = iota
	treeActStarter
	treeActCopy
	treeActDDL
	treeActAllDDL
	treeActRefresh
	treeActHelp
	treeActStatus
)

type treeAction struct {
	kind  treeActKind
	table string
	text  string
}

func newTreeModel() *treeModel {
	return &treeModel{expanded: map[string]bool{}}
}

// setTables installs a (re)loaded schema, preserving expansion state and the
// cursor position (by name) where possible.
func (t *treeModel) setTables(ts []TableInfo, initial bool) {
	var curTable, curCol string
	if n, ok := t.current(); ok {
		curTable = t.tables[n.table].Name
		if n.col >= 0 {
			curCol = t.tables[n.table].Columns[n.col].Name
		}
	}
	t.tables = ts
	if initial || !t.loaded {
		expandAll := len(ts) < autoExpandLimit
		for _, tb := range ts {
			t.expanded[tb.Name] = expandAll
		}
		t.loaded = true
	}
	t.cursor = 0
	if curTable != "" {
		for i, n := range t.flat() {
			tb := t.tables[n.table]
			if tb.Name == curTable && (n.col < 0 && curCol == "" || n.col >= 0 && tb.Columns[n.col].Name == curCol) {
				t.cursor = i
				break
			}
			if tb.Name == curTable && n.col < 0 {
				t.cursor = i
			}
		}
	}
	t.clamp()
}

// matches reports whether table i passes the filter, and whether the match
// came only from its columns (then it is shown expanded).
func (t *treeModel) matches(i int) (ok, byColumn bool) {
	if t.filter == "" {
		return true, false
	}
	f := strings.ToLower(t.filter)
	tb := t.tables[i]
	if strings.Contains(strings.ToLower(tb.Name), f) {
		return true, false
	}
	for _, c := range tb.Columns {
		if strings.Contains(strings.ToLower(c.Name), f) {
			return true, true
		}
	}
	return false, false
}

// flat returns the visible nodes in display order.
func (t *treeModel) flat() []treeNode {
	var out []treeNode
	for i, tb := range t.tables {
		ok, byCol := t.matches(i)
		if !ok {
			continue
		}
		out = append(out, treeNode{table: i, col: -1})
		if t.expanded[tb.Name] || byCol {
			for j := range tb.Columns {
				out = append(out, treeNode{table: i, col: j})
			}
		}
	}
	return out
}

func (t *treeModel) current() (treeNode, bool) {
	f := t.flat()
	if t.cursor < 0 || t.cursor >= len(f) {
		return treeNode{}, false
	}
	return f[t.cursor], true
}

func (t *treeModel) clamp() {
	n := len(t.flat())
	if t.cursor >= n {
		t.cursor = n - 1
	}
	if t.cursor < 0 {
		t.cursor = 0
	}
}

func (t *treeModel) title() string {
	n := 0
	for i := range t.tables {
		if ok, _ := t.matches(i); ok {
			n++
		}
	}
	if t.filter != "" {
		return "Schema (" + itoa(n) + "/" + itoa(len(t.tables)) + ")"
	}
	return "Schema (" + itoa(len(t.tables)) + ")"
}

func (t *treeModel) update(msg tea.KeyMsg) treeAction {
	key := msg.String()
	if t.searching {
		switch key {
		case "esc":
			t.searching = false
			t.filter = ""
			t.clamp()
		case "enter":
			t.searching = false
		case "backspace":
			if r := []rune(t.filter); len(r) > 0 {
				t.filter = string(r[:len(r)-1])
			}
			t.cursor = 0
		default:
			if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
				t.filter += string(msg.Runes)
				t.cursor = 0
			}
		}
		t.clamp()
		return treeAction{}
	}
	if t.pendingG {
		t.pendingG = false
		if key == "g" {
			t.cursor = 0
			return treeAction{}
		}
	}
	flat := t.flat()
	cur, ok := t.current()
	switch key {
	case "j", "down":
		if t.cursor < len(flat)-1 {
			t.cursor++
		}
	case "k", "up":
		if t.cursor > 0 {
			t.cursor--
		}
	case "pgdown", "ctrl+d":
		t.cursor = min(len(flat)-1, t.cursor+max(1, t.height-1))
	case "pgup", "ctrl+u":
		t.cursor = max(0, t.cursor-max(1, t.height-1))
	case "g":
		t.pendingG = true
	case "home":
		t.cursor = 0
	case "G", "end":
		t.cursor = len(flat) - 1
	case "l", "right":
		if !ok {
			break
		}
		if cur.col < 0 {
			name := t.tables[cur.table].Name
			if _, byCol := t.matches(cur.table); !t.expanded[name] && !byCol {
				t.expanded[name] = true
			} else if len(t.tables[cur.table].Columns) > 0 {
				t.cursor++
			}
		}
	case "h", "left":
		if !ok {
			break
		}
		if cur.col >= 0 {
			// jump to parent table
			for i := t.cursor; i >= 0; i-- {
				if flat[i].col < 0 {
					t.cursor = i
					break
				}
			}
		} else {
			t.expanded[t.tables[cur.table].Name] = false
		}
	case " ":
		if ok && cur.col < 0 {
			name := t.tables[cur.table].Name
			t.expanded[name] = !t.expanded[name]
		}
	case "enter":
		if ok {
			return treeAction{kind: treeActStarter, table: t.tables[cur.table].Name}
		}
	case "y":
		if ok {
			name := t.tables[cur.table].Name
			if cur.col >= 0 {
				name = t.tables[cur.table].Columns[cur.col].Name
			}
			return treeAction{kind: treeActCopy, text: name}
		}
	case "v":
		if ok {
			return treeAction{kind: treeActDDL, table: t.tables[cur.table].Name}
		}
	case "V":
		return treeAction{kind: treeActAllDDL}
	case "/":
		t.searching = true
		t.filter = ""
		t.cursor = 0
	case "esc":
		if t.filter != "" {
			t.filter = ""
			t.clamp()
			return treeAction{kind: treeActStatus, text: "filter cleared"}
		}
	case "r":
		return treeAction{kind: treeActRefresh}
	case "?":
		return treeAction{kind: treeActHelp}
	}
	t.clamp()
	return treeAction{}
}

func (t *treeModel) view(w, h int) []string {
	listH := h
	if t.searching || t.filter != "" {
		listH = h - 1
	}
	t.height = listH
	flat := t.flat()
	if t.cursor < t.offset {
		t.offset = t.cursor
	}
	if t.cursor >= t.offset+listH {
		t.offset = t.cursor - listH + 1
	}
	if t.offset > max(0, len(flat)-listH) {
		t.offset = max(0, len(flat)-listH)
	}
	lines := make([]string, 0, h)
	if len(t.tables) == 0 {
		lines = append(lines, stDim.Render(truncate(" (no tables)", w)))
	}
	for i := t.offset; i < len(flat) && len(lines) < listH; i++ {
		n := flat[i]
		tb := t.tables[n.table]
		var text string
		var styled string
		if n.col < 0 {
			marker := "▸ "
			if _, byCol := t.matches(n.table); t.expanded[tb.Name] || byCol {
				marker = "▾ "
			}
			suffix := ""
			if tb.Type == "view" {
				suffix = " (view)"
			}
			label := marker + sanitize(tb.Name)
			if i == t.cursor {
				styled = stCursor.Render(padRight(label+suffix, w))
			} else {
				label = truncate(label, w)
				styled = stBold.Render(label) + stDim.Render(truncate(suffix, w-strWidth(label)))
			}
		} else {
			c := tb.Columns[n.col]
			conn := "├ "
			if n.col == len(tb.Columns)-1 {
				conn = "└ "
			}
			name := "  " + conn + sanitize(c.Name)
			meta := strings.TrimSpace(strings.ToLower(c.Type) + " " + columnFlags(c, false))
			nw := strWidth(name)
			mw := strWidth(meta)
			if nw+1+mw > w {
				mw = max(0, w-nw-1)
				meta = truncate(meta, mw)
				mw = strWidth(meta)
			}
			gap := max(1, w-nw-mw)
			text = padRight(name+strings.Repeat(" ", gap)+meta, w)
			if i == t.cursor {
				styled = stCursor.Render(text)
			} else if nw+gap+mw <= w {
				styled = name + strings.Repeat(" ", gap) + stDim.Render(meta)
			} else {
				styled = truncate(name, w)
			}
		}
		lines = append(lines, styled)
	}
	for len(lines) < listH {
		lines = append(lines, "")
	}
	if t.searching || t.filter != "" {
		s := "/" + t.filter
		if t.searching {
			s += "█"
		}
		lines = append(lines, stFocusTitle.Render(truncate(s, w)))
	}
	return lines
}
