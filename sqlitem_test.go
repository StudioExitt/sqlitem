package main

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestSplitStatements(t *testing.T) {
	src := `SELECT 1; -- c;
SELECT 'a;b';
CREATE TRIGGER tr AFTER INSERT ON t BEGIN
  UPDATE t SET x = CASE WHEN 1 THEN 2 END;
  SELECT 1;
END;
;;
SELECT "x;y" FROM t`
	st := splitStatements(src)
	if len(st) != 4 {
		for _, s := range st {
			t.Logf("%q", s.Text)
		}
		t.Fatalf("want 4 statements, got %d", len(st))
	}
	if !strings.HasPrefix(st[2].Text, "CREATE TRIGGER") || !strings.HasSuffix(st[2].Text, "END") {
		t.Errorf("trigger not kept whole: %q", st[2].Text)
	}
	if st[3].Text != `SELECT "x;y" FROM t` {
		t.Errorf("last: %q", st[3].Text)
	}
}

func TestStatementAt(t *testing.T) {
	src := "SELECT 1;\nSELECT 2;\n\nSELECT 3;"
	st := splitStatements(src)
	cases := map[int]string{0: "SELECT 1", 9: "SELECT 1", 10: "SELECT 2", 19: "SELECT 2", 21: "SELECT 3", len(src): "SELECT 3"}
	for off, want := range cases {
		if got := st[statementAt(st, off)].Text; got != want {
			t.Errorf("offset %d: got %q want %q", off, got, want)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]StmtKind{
		"select 1":                             KindQuery,
		"WITH x AS (SELECT 1) SELECT * FROM x": KindQuery,
		"WITH x AS (SELECT 1) DELETE FROM t":   KindDelete,
		"pragma table_info(t)":                 KindQuery,
		"pragma foreign_keys = 1":              KindOther,
		"insert into t values(1)":              KindInsert,
		"update t set a=1":                     KindUpdate,
		"delete from t":                        KindDelete,
		"create table t(a)":                    KindDDL,
		"  -- hi\n drop table t":               KindDDL,
	}
	for s, want := range cases {
		if got := classify(s); got != want {
			t.Errorf("%q: got %v want %v", s, got, want)
		}
	}
	if !isDestructive("DROP TABLE x") || !isDestructive("update t set a=1") || isDestructive("insert into t values(1)") {
		t.Error("isDestructive")
	}
}

func TestAnalyzeSelect(t *testing.T) {
	ok := []string{
		"SELECT * FROM users",
		"select id, name as n from users u where id > 3 order by name limit 10",
		"SELECT u.* FROM main.users AS u WHERE id IN (SELECT 1)",
		`SELECT "name", upper(name) FROM "users"`,
	}
	for _, s := range ok {
		if sh := analyzeSelect(s); !sh.IsEditable {
			t.Errorf("%q should be editable: %s", s, sh.Reason)
		}
	}
	bad := map[string]string{
		"SELECT * FROM a JOIN b ON a.id=b.id":   "JOIN",
		"SELECT * FROM a, b":                    "JOIN",
		"SELECT count(*) FROM a":                "aggregate",
		"SELECT x, count(*) FROM a GROUP BY x":  "aggregate",
		"SELECT * FROM (SELECT 1)":              "subquery",
		"SELECT DISTINCT a FROM t":              "DISTINCT",
		"SELECT 1":                              "FROM",
		"SELECT a FROM t UNION SELECT b FROM u": "compound",
	}
	for s, want := range bad {
		sh := analyzeSelect(s)
		if sh.IsEditable || !strings.Contains(sh.Reason, want) {
			t.Errorf("%q: editable=%v reason=%q (want %q)", s, sh.IsEditable, sh.Reason, want)
		}
	}
	sh := analyzeSelect("SELECT * FROM t u")
	if sh.Alias != "u" || rewriteWithRowID("SELECT * FROM t u", sh) != `SELECT "u".rowid AS __sqlitem_rowid__, * FROM t u` {
		t.Errorf("rewrite: %q", rewriteWithRowID("SELECT * FROM t u", sh))
	}
}

func TestParseWriteTarget(t *testing.T) {
	wt, ok := parseWriteTarget(`UPDATE "my t" SET a = 1, b = (SELECT 2) WHERE id = 3 AND x IN (1,2)`)
	if !ok || wt.Table != "my t" || wt.Where != "id = 3 AND x IN (1,2)" {
		t.Errorf("update: %+v %v", wt, ok)
	}
	wt, ok = parseWriteTarget("DELETE FROM t")
	if !ok || wt.Table != "t" || wt.Where != "" {
		t.Errorf("delete: %+v %v", wt, ok)
	}
	wt, ok = parseWriteTarget("DELETE FROM t WHERE a = 1 RETURNING *")
	if !ok || wt.Where != "a = 1" {
		t.Errorf("returning: %+v %v", wt, ok)
	}
	if _, ok := parseWriteTarget("UPDATE t SET a = b.x FROM b WHERE t.id = b.id"); ok {
		t.Error("UPDATE FROM should be unsupported")
	}
}

func TestFormatSQL(t *testing.T) {
	got := formatSQL("select a, b from t where a = 'x, y' and b in (1,2) order by a;")
	want := "SELECT\n  a,\n  b\nFROM t\nWHERE a = 'x, y' AND b IN (1, 2)\nORDER BY a;\n"
	if got != want {
		t.Errorf("format:\n%s\nwant:\n%s", got, want)
	}
	// formatting must not change the statement semantics/count
	src := "create table t(a int primary key, b text); insert into t values(1,'a;b'); select count(*) from t left join u on t.a=u.a"
	if n := len(splitStatements(formatSQL(src))); n != 3 {
		t.Errorf("formatted statement count %d", n)
	}
}

func openTestDB(t *testing.T) (*DB, string) {
	t.Helper()
	dir := t.TempDir()
	logp := filepath.Join(dir, "audit.jsonl")
	db, err := OpenDB(filepath.Join(dir, "test.db"), logp)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sr := db.ExecScript(context.Background(), `
		CREATE TABLE users (id INTEGER PRIMARY KEY, name TEXT NOT NULL, age INT, note TEXT);
		CREATE TABLE nopk (a TEXT, b TEXT);
		CREATE INDEX users_name ON users(name);
		CREATE VIEW adults AS SELECT * FROM users WHERE age >= 18;
		INSERT INTO users (name, age) VALUES ('kim', 30), ('lee', 12), ('park', 45);
		INSERT INTO nopk VALUES ('x','1'), ('x','1');`)
	if sr.Err != nil {
		t.Fatal(sr.Err)
	}
	return db, logp
}

func readLog(t *testing.T, p string) []LogEntry {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []LogEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e LogEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func TestQueryEditableAndEdits(t *testing.T) {
	db, logp := openTestDB(t)
	ctx := context.Background()

	r, err := db.QueryEditable(ctx, "SELECT name, age + 1 AS next, id FROM users ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if r.Edit == nil {
		t.Fatalf("expected editable, reason %q", r.ReadOnly)
	}
	if strings.Join(r.Columns, ",") != "name,next,id" {
		t.Errorf("synthetic column not hidden: %v", r.Columns)
	}
	if strings.Join(r.Edit.ColMap, ",") != "name,," { // id is the rowid alias -> read-only
		t.Errorf("colmap %v", r.Edit.ColMap)
	}
	if len(r.RowIDs) != 3 || r.RowIDs[0] != 1 {
		t.Errorf("rowids %v", r.RowIDs)
	}

	// views / joins / aggregates are read-only with a reason
	for q, want := range map[string]string{
		"SELECT * FROM adults":                         "view",
		"SELECT count(*) FROM users":                   "aggregate",
		"SELECT * FROM users a JOIN users b USING(id)": "JOIN",
	} {
		r, err := db.QueryEditable(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if r.Edit != nil || !strings.Contains(r.ReadOnly, want) {
			t.Errorf("%q: edit=%v reason=%q", q, r.Edit != nil, r.ReadOnly)
		}
	}

	// rowid-based edit on a table without primary key touches exactly one row
	r, _ = db.QueryEditable(ctx, "SELECT * FROM nopk")
	if r.Edit == nil {
		t.Fatal("nopk should be editable")
	}
	if _, err := db.UpdateCell(ctx, "nopk", r.RowIDs[1], "b", "2"); err != nil {
		t.Fatal(err)
	}
	r, _ = db.QueryEditable(ctx, "SELECT b FROM nopk ORDER BY rowid")
	if r.Rows[0][0].S != "1" || r.Rows[1][0].S != "2" {
		t.Errorf("rowid update hit wrong rows: %v", r.Rows)
	}
	// NULL
	if c, err := db.UpdateCell(ctx, "users", 1, "age", nil); err != nil || !c.IsNull() {
		t.Errorf("set null: %v %v", c, err)
	}
	// affinity: text "42" stored as integer in INT column
	if c, err := db.UpdateCell(ctx, "users", 1, "age", "42"); err != nil || c.V != int64(42) {
		t.Errorf("affinity: %#v %v", c.V, err)
	}
	id, err := db.InsertRow(ctx, "users", map[string]any{"name": "choi", "age": nil})
	if err != nil || id != 4 {
		t.Fatalf("insert: %d %v", id, err)
	}
	n, err := db.DeleteRows(ctx, "users", []int64{2, 4})
	if err != nil || n != 2 {
		t.Fatalf("delete: %d %v", n, err)
	}

	entries := readLog(t, logp)
	var ops []string
	for _, e := range entries {
		ops = append(ops, e.Op)
	}
	// setup script: 5 ddl + 2 inserts, then update x3, insert, delete
	if got := strings.Join(ops, ","); got != "ddl,ddl,ddl,ddl,insert,insert,update,update,update,insert,delete" {
		t.Fatalf("ops %s", got)
	}
	up := entries[6]
	if up.Before[0]["b"] != "1" || up.After[0]["b"] != "2" || *up.RowID != 2 {
		t.Errorf("update before/after: %+v", up)
	}
	del := entries[10]
	if len(del.Before) != 2 || del.After != nil {
		t.Errorf("delete log: %+v", del)
	}
	if len(entries[4].After) != 3 {
		t.Errorf("insert via SQL should capture 3 after rows: %+v", entries[4])
	}
}

func TestExecScriptWriteCapture(t *testing.T) {
	db, logp := openTestDB(t)
	ctx := context.Background()
	if n := db.EstimateAffected(ctx, "UPDATE users SET age = age + 1 WHERE age > 20"); n != 2 {
		t.Errorf("estimate %d", n)
	}
	sr := db.ExecScript(ctx, "UPDATE users SET age = age + 1 WHERE age > 20; DELETE FROM users WHERE name = 'lee'; SELECT * FROM users")
	if sr.Err != nil {
		t.Fatal(sr.Err)
	}
	if last := sr.Last(); last == nil || len(last.Rows) != 2 || last.Edit == nil {
		t.Fatalf("last result: %+v", last)
	}
	entries := readLog(t, logp)
	up := entries[len(entries)-2]
	if up.Op != "update" || len(up.Before) != 2 || len(up.After) != 2 {
		t.Fatalf("update capture: %+v", up)
	}
	if up.Before[0]["age"].(float64)+1 != up.After[0]["age"].(float64) {
		t.Errorf("before/after values: %v %v", up.Before[0], up.After[0])
	}
	del := entries[len(entries)-1]
	if del.Op != "delete" || len(del.Before) != 1 || del.Before[0]["name"] != "lee" {
		t.Errorf("delete capture: %+v", del)
	}
	ddl, err := db.DDL("users")
	if err != nil || !strings.Contains(ddl, "CREATE INDEX users_name") {
		t.Errorf("DDL should include index: %q %v", ddl, err)
	}
}

func TestConsoleRefusesDestructiveWithoutConfirm(t *testing.T) {
	db, _ := openTestDB(t)
	var out strings.Builder
	c := NewConsole(db, true, false)
	c.out = &out
	c.confirm = func(string) bool { return false }
	if err := c.RunScript("DELETE FROM users"); err == nil {
		t.Fatal("expected cancellation")
	}
	r, _ := db.QueryEditable(context.Background(), "SELECT * FROM users")
	if len(r.Rows) != 3 {
		t.Fatal("rows were deleted without confirmation")
	}
	c.readOnly = true
	if err := c.RunScript("INSERT INTO users(name) VALUES('x')"); err == nil {
		t.Fatal("-query must refuse writes")
	}
	c.readOnly = false
	out.Reset()
	if err := c.REPL(strings.NewReader("SELECT name FROM users\n WHERE id = 1;\n.tables\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "| kim  |") || !strings.Contains(out.String(), "adults (view)") {
		t.Errorf("console output:\n%s", out.String())
	}
}

// ---------------------------------------------------------------- TUI model smoke test

func keys(m *model, ks ...string) {
	for _, k := range ks {
		var msg tea.KeyMsg
		switch k {
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "f5":
			msg = tea.KeyMsg{Type: tea.KeyF5}
		case "f3":
			msg = tea.KeyMsg{Type: tea.KeyF3}
		case "ctrl+n":
			msg = tea.KeyMsg{Type: tea.KeyCtrlN}
		case "backspace":
			msg = tea.KeyMsg{Type: tea.KeyBackspace}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		runCmd(m, m.handleKey(msg))
	}
}

// runCmd executes a command synchronously and feeds resulting messages back.
func runCmd(m *model, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	msg := cmd()
	switch msg := msg.(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			runCmd(m, c)
		}
	case tea.QuitMsg:
	default:
		_, next := m.Update(msg)
		runCmd(m, next)
	}
}

func TestTUIFlow(t *testing.T) {
	db, _ := openTestDB(t)
	m := &model{db: db, focus: focusEditor, tree: newTreeModel(), editor: newEditorModel(), grid: newGridModel()}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	runCmd(m, m.Init())
	if len(m.tree.flat()) != 5+3+5 { // <10 tables: all expanded
		t.Errorf("tree nodes %d", len(m.tree.flat()))
	}
	// schema: move to "users" and press Enter for the starter query
	m.focus = focusSchema
	keys(m, "G")
	for i := 0; i < 10; i++ {
		if n, _ := m.tree.current(); m.tree.tables[n.table].Name == "users" && n.col < 0 {
			break
		}
		keys(m, "k")
	}
	keys(m, "enter")
	if m.focus != focusEditor || m.editor.Text() != "SELECT * FROM users LIMIT 100;" {
		t.Fatalf("starter: focus=%v text=%q", m.focus, m.editor.Text())
	}
	keys(m, "f5")
	if m.grid.res == nil || len(m.grid.res.Rows) != 3 || m.focus != focusGrid {
		t.Fatalf("run: %+v status=%q", m.grid.res, m.status)
	}
	// edit cell (row 1, col "name"), type, confirm
	keys(m, "l", "e")
	if _, ok := m.modal.(*cellEditModal); !ok {
		t.Fatalf("expected edit modal, status=%q", m.status)
	}
	keys(m, "backspace", "backspace", "backspace", "h", "a", "n", "enter")
	cm, ok := m.modal.(*confirmModal)
	if !ok {
		t.Fatal("expected confirm modal")
	}
	// Back returns to the edit modal with the typed value intact
	keys(m, "n")
	if em, ok := m.modal.(*cellEditModal); !ok || em.input.String() != "han" {
		t.Fatalf("cancel should return to edit modal: %#v", m.modal)
	}
	keys(m, "enter")
	_ = cm
	keys(m, "y")
	if m.modal != nil || m.grid.res.Rows[0][1].S != "han" {
		t.Fatalf("update not applied: modal=%v cell=%q status=%q", m.modal, m.grid.res.Rows[0][1].S, m.status)
	}
	// delete with double confirmation; Back from second returns to first
	keys(m, "j", "d")
	if _, ok := m.modal.(*confirmModal); !ok {
		t.Fatal("delete confirm 1")
	}
	keys(m, "y", "n")
	if c, ok := m.modal.(*confirmModal); !ok || c.title != "Delete rows" {
		t.Fatal("back to first confirm")
	}
	keys(m, "y", "y")
	if len(m.grid.res.Rows) != 2 {
		t.Fatalf("rows after delete %d (%s)", len(m.grid.res.Rows), m.status)
	}
	// destructive SQL from the editor needs confirmation
	m.focus = focusEditor
	m.editor.setText("DELETE FROM users", true)
	keys(m, "f5")
	if _, ok := m.modal.(*confirmModal); !ok {
		t.Fatal("DELETE without confirmation modal")
	}
	keys(m, "n")
	r, _ := db.QueryEditable(context.Background(), "SELECT * FROM users")
	if len(r.Rows) != 2 {
		t.Fatal("cancelled DELETE still ran")
	}
	// read-only result: edit keys ignored with a reason in the status bar
	m.editor.setText("SELECT count(*) FROM users;", true)
	keys(m, "f5", "e")
	if m.modal != nil || !strings.Contains(m.status, "aggregate") {
		t.Errorf("read-only status: %q", m.status)
	}
	// zero rows: motions are no-ops, 'i' still opens the insert modal
	m.focus = focusEditor
	m.editor.setText("SELECT * FROM users WHERE 0;", true)
	keys(m, "f5", "j", "l", "G", "i")
	if _, ok := m.modal.(*insertModal); !ok {
		t.Fatalf("insert modal on empty result, status=%q", m.status)
	}
	keys(m, "tab", "z", "z", "enter", "y")
	if len(m.grid.res.Rows) != 1 || m.grid.res.Rows[0][1].S != "zz" {
		t.Fatalf("insert: %v %q", m.grid.res.Rows, m.status)
	}
	// DDL via editor refreshes the tree
	m.focus = focusEditor
	m.editor.setText("CREATE TABLE extra(x);", true)
	keys(m, "f5")
	found := false
	for _, tb := range m.tree.tables {
		found = found || tb.Name == "extra"
	}
	if !found {
		t.Error("schema tree not refreshed after CREATE")
	}
	// the view renders without panicking and has the focus marker
	v := m.View()
	if !strings.Contains(v, "* SQL") {
		t.Error("focused panel marker missing")
	}
	// editor: vi undo
	m.editor.setText("abc", false)
	m.editor.mode = modeNormal
	keys(m, "x", "u")
	if m.editor.Text() != "abc" {
		t.Errorf("undo: %q", m.editor.Text())
	}
}

func TestColonQuit(t *testing.T) {
	db, _ := openTestDB(t)
	m := &model{db: db, focus: focusSchema, tree: newTreeModel(), editor: newEditorModel(), grid: newGridModel()}
	isQuit := func(cmd tea.Cmd) bool {
		if cmd == nil {
			return false
		}
		_, ok := cmd().(tea.QuitMsg)
		return ok
	}
	send := func(k string) tea.Cmd {
		if k == "enter" {
			return m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
		}
		return m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
	}
	// editor INSERT mode: ':' is typed text, not a command
	m.focus = focusEditor
	send(":")
	if m.cmdline || m.editor.Text() != ":" {
		t.Fatalf("':' in INSERT mode should be text, got cmdline=%v text=%q", m.cmdline, m.editor.Text())
	}
	// unknown command reports an error and does not quit
	m.focus = focusSchema
	send(":")
	send("x")
	send("y")
	if isQuit(send("enter")) || !m.statusErr {
		t.Fatal("unknown command should not quit")
	}
	send(":")
	send("q")
	if !isQuit(send("enter")) {
		t.Fatal(":q should quit")
	}
}

func TestGridKeys(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		db.ExecStmt(ctx, "INSERT INTO users(name, age) VALUES ('u', 1)")
	}
	m := &model{db: db, focus: focusEditor, tree: newTreeModel(), editor: newEditorModel(), grid: newGridModel()}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.editor.setText("SELECT * FROM users;", false)
	keys(m, "f5")
	g := m.grid
	ctrl := func(t tea.KeyType) { runCmd(m, m.handleKey(tea.KeyMsg{Type: t})) }
	ctrl(tea.KeyCtrlF)
	if g.row != 20 || m.editor.Text() != "SELECT * FROM users;" {
		t.Fatalf("Ctrl+F: row=%d editor=%q", g.row, m.editor.Text())
	}
	ctrl(tea.KeyCtrlB)
	if g.row != 0 {
		t.Fatalf("Ctrl+B: row=%d", g.row)
	}
	keys(m, "G")
	if g.row != 52 {
		t.Fatalf("G: row=%d", g.row)
	}
	keys(m, "g", "g")
	if g.row != 0 {
		t.Fatalf("gg: row=%d", g.row)
	}
	// VISUAL: e/u are ignored, Esc leaves VISUAL
	keys(m, "l", "v", "j", "e")
	if m.modal != nil || !g.visual {
		t.Fatal("e must be ignored in VISUAL")
	}
	keys(m, "u")
	if m.modal != nil {
		t.Fatal("u must be ignored in VISUAL")
	}
	keys(m, "esc")
	if g.visual {
		t.Fatal("Esc should leave VISUAL")
	}
	keys(m, "u")
	if _, ok := m.modal.(*cellEditModal); !ok {
		t.Fatal("u should open the cell editor")
	}
	keys(m, "esc")
	// d on a VISUAL block deletes its rows (after double confirmation)
	keys(m, "v", "j", "d")
	c, ok := m.modal.(*confirmModal)
	if !ok || !strings.Contains(c.body[0], "Delete 2 row") {
		t.Fatalf("visual d: %#v", m.modal)
	}
}

func TestParseLimit(t *testing.T) {
	cases := []struct {
		in       string
		base     string
		lim, off int64
		ok       bool
	}{
		{"SELECT * FROM t LIMIT 100", "SELECT * FROM t", 100, 0, true},
		{"select * from t where a in (select b from u limit 3) order by a limit 10 offset 20", "select * from t where a in (select b from u limit 3) order by a", 10, 20, true},
		{"SELECT * FROM t LIMIT 5, 10", "SELECT * FROM t", 10, 5, true},
		{"SELECT * FROM t LIMIT ?", "", 0, 0, false},
		{"SELECT * FROM t LIMIT 10 + 1", "", 0, 0, false},
		{"SELECT * FROM t", "", 0, 0, false},
		{"UPDATE t SET a = 1 LIMIT 3", "", 0, 0, false},
	}
	for _, c := range cases {
		base, lim, off, ok := parseLimit(c.in)
		if ok != c.ok || base != c.base || lim != c.lim || off != c.off {
			t.Errorf("%q: got %q %d %d %v", c.in, base, lim, off, ok)
		}
	}
}

func TestGridPaging(t *testing.T) {
	db, _ := openTestDB(t)
	ctx := context.Background()
	db.ExecStmt(ctx, "WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < 247) INSERT INTO users(name, age) SELECT 'u'||i, i FROM n")
	// 3 + 247 = 250 rows
	m := &model{db: db, focus: focusEditor, tree: newTreeModel(), editor: newEditorModel(), grid: newGridModel()}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.editor.setText("SELECT * FROM users LIMIT 100;", false)
	keys(m, "f5")
	g := m.grid
	if len(g.res.Rows) != 100 || !g.hasMore() || !strings.Contains(g.title(), "▼ more") {
		t.Fatalf("first page: rows=%d more=%v title=%q", len(g.res.Rows), g.hasMore(), g.title())
	}
	if !strings.Contains(strings.Join(g.view(100, 30, true), "\n"), "▼ more rows") {
		// indicator shows once the last loaded row is on screen
		keys(m, "G")
		if !strings.Contains(strings.Join(g.view(100, 30, true), "\n"), "▼ more rows") {
			t.Error("more-rows indicator missing")
		}
	}
	keys(m, "g", "g")
	// Ctrl+F through the first page keeps going into the second
	for i := 0; i < 5; i++ {
		runCmd(m, m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlF}))
	}
	if g.row != 100 || len(g.res.Rows) != 200 {
		t.Fatalf("continuous paging: row=%d rows=%d status=%q", g.row, len(g.res.Rows), m.status)
	}
	// j at the end of loaded rows also continues
	keys(m, "G")
	keys(m, "j")
	if len(g.res.Rows) != 250 || g.row != 200 || g.hasMore() {
		t.Fatalf("j paging: row=%d rows=%d more=%v", g.row, len(g.res.Rows), g.hasMore())
	}
	// rowids follow the appended rows, so editing still works on page 3
	if g.res.Edit == nil || len(g.res.RowIDs) != 250 || g.res.RowIDs[249] != 250 {
		t.Fatalf("rowids after paging: %d", len(g.res.RowIDs))
	}
	// n / p: within the loaded rows they just move the cursor
	keys(m, "p")
	if g.row != 100 {
		t.Fatalf("p within loaded rows: row=%d", g.row)
	}
	// fresh query, then n fetches the next 100-row window
	keys(m, "f5")
	keys(m, "n")
	if g.rowBase() != 100 || g.row != 0 || len(g.res.Rows) != 100 {
		t.Fatalf("n: base=%d row=%d rows=%d", g.rowBase(), g.row, len(g.res.Rows))
	}
	if !strings.Contains(g.title(), "rows 101-200 ▲ ▼ more") {
		t.Errorf("title: %q", g.title())
	}
	keys(m, "n", "n")
	if g.rowBase() != 200 || len(g.res.Rows) != 50 || g.hasMore() || m.status != "no more rows" {
		t.Fatalf("last page: base=%d rows=%d more=%v status=%q", g.rowBase(), len(g.res.Rows), g.hasMore(), m.status)
	}
	keys(m, "p", "p", "p")
	if g.rowBase() != 0 || g.row != 0 {
		t.Fatalf("p back to start: base=%d row=%d", g.rowBase(), g.row)
	}
}
