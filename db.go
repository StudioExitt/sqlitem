package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// All database access goes through this file. UI code never opens its own
// connection; it calls methods on *DB.

// MaxResultRows caps how many rows a query result keeps in memory.
const MaxResultRows = 100000

// maxLogRows caps how many before/after rows a single log entry captures.
const maxLogRows = 1000

// DB wraps a single SQLite connection plus the audit log.
type DB struct {
	conn *sql.DB
	Path string
	log  *AuditLog
}

// OpenDB opens (or creates) a database file.
func OpenDB(path, logPath string) (*DB, error) {
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// One connection only: keeps user-issued BEGIN/COMMIT, temp tables and
	// last_insert_rowid() consistent across calls.
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	conn.SetConnMaxLifetime(0)
	if _, err := conn.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		conn.Close()
		return nil, err
	}
	// Force the file to be created / validated now.
	if _, err := conn.Exec("SELECT count(*) FROM sqlite_master"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &DB{conn: conn, Path: path, log: NewAuditLog(logPath, path)}, nil
}

func (d *DB) Close() error { return d.conn.Close() }

// LogPath returns where write operations are logged.
func (d *DB) LogPath() string { return d.log.Path }

// ---------------------------------------------------------------- schema

type ColumnInfo struct {
	Name    string
	Type    string
	NotNull bool
	PK      int // position in primary key (0 = not part of PK)
	Default sql.NullString
	Hidden  bool // generated / hidden column
}

type TableInfo struct {
	Name         string
	Type         string // "table" or "view"
	Columns      []ColumnInfo
	WithoutRowID bool
	SQL          string
}

// Schema returns all tables and views (excluding sqlite internal objects).
func (d *DB) Schema() ([]TableInfo, error) {
	rows, err := d.conn.Query(`SELECT type, name, coalesce(sql,'') FROM sqlite_master
		WHERE type IN ('table','view') AND name NOT LIKE 'sqlite_%'
		ORDER BY type DESC, name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	var out []TableInfo
	for rows.Next() {
		var t TableInfo
		if err := rows.Scan(&t.Type, &t.Name, &t.SQL); err != nil {
			rows.Close()
			return nil, err
		}
		t.WithoutRowID = t.Type == "table" && strings.Contains(strings.ToUpper(strings.Join(strings.Fields(t.SQL), " ")), "WITHOUT ROWID")
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		cols, err := d.Columns(out[i].Name)
		if err != nil {
			return nil, err
		}
		out[i].Columns = cols
	}
	return out, nil
}

// Columns returns column metadata for a table or view.
func (d *DB) Columns(table string) ([]ColumnInfo, error) {
	rows, err := d.conn.Query("SELECT name, type, \"notnull\", pk, dflt_value, hidden FROM pragma_table_xinfo(?)", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []ColumnInfo
	for rows.Next() {
		var c ColumnInfo
		var notnull, hidden int
		if err := rows.Scan(&c.Name, &c.Type, &notnull, &c.PK, &c.Default, &hidden); err != nil {
			return nil, err
		}
		c.NotNull = notnull != 0
		c.Hidden = hidden != 0
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

// tableInfo looks up one table (case-insensitive).
func (d *DB) tableInfo(name string) (*TableInfo, error) {
	var t TableInfo
	err := d.conn.QueryRow(`SELECT type, name, coalesce(sql,'') FROM sqlite_master
		WHERE type IN ('table','view') AND name = ? COLLATE NOCASE`, name).Scan(&t.Type, &t.Name, &t.SQL)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("no such table: %s", name)
		}
		return nil, err
	}
	t.WithoutRowID = t.Type == "table" && strings.Contains(strings.ToUpper(strings.Join(strings.Fields(t.SQL), " ")), "WITHOUT ROWID")
	cols, err := d.Columns(t.Name)
	if err != nil {
		return nil, err
	}
	t.Columns = cols
	return &t, nil
}

// DDL returns the CREATE statement of an object together with related
// objects (indexes, triggers) that reference it.
func (d *DB) DDL(name string) (string, error) {
	rows, err := d.conn.Query(`SELECT type, name, sql FROM sqlite_master
		WHERE sql IS NOT NULL AND (name = ?1 COLLATE NOCASE OR tbl_name = ?1 COLLATE NOCASE)
		ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'view' THEN 1 WHEN 'index' THEN 2 ELSE 3 END, name`, name)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var typ, n, s string
		if err := rows.Scan(&typ, &n, &s); err != nil {
			return "", err
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "-- %s: %s\n%s;", typ, n, s)
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("no DDL for %s", name)
	}
	return b.String(), rows.Err()
}

// AllDDL returns the complete schema.
func (d *DB) AllDDL() (string, error) {
	rows, err := d.conn.Query(`SELECT type, name, sql FROM sqlite_master WHERE sql IS NOT NULL
		ORDER BY tbl_name COLLATE NOCASE, CASE type WHEN 'table' THEN 0 WHEN 'view' THEN 1 WHEN 'index' THEN 2 ELSE 3 END, name`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var typ, n, s string
		if err := rows.Scan(&typ, &n, &s); err != nil {
			return "", err
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "-- %s: %s\n%s;", typ, n, s)
	}
	if b.Len() == 0 {
		return "-- (empty schema)", rows.Err()
	}
	return b.String(), rows.Err()
}

// ---------------------------------------------------------------- values

// Cell is one value of a result row; S is the cached display text.
type Cell struct {
	V any // nil, int64, float64, string, []byte
	S string
}

func (c Cell) IsNull() bool { return c.V == nil }

func newCell(v any) Cell {
	switch x := v.(type) {
	case nil:
		return Cell{V: nil, S: "NULL"}
	case int64:
		return Cell{V: x, S: strconv.FormatInt(x, 10)}
	case float64:
		return Cell{V: x, S: strconv.FormatFloat(x, 'g', -1, 64)}
	case string:
		return Cell{V: x, S: x}
	case []byte:
		b := append([]byte(nil), x...)
		return Cell{V: b, S: blobText(b)}
	case bool:
		if x {
			return Cell{V: int64(1), S: "1"}
		}
		return Cell{V: int64(0), S: "0"}
	case time.Time:
		s := x.Format("2006-01-02 15:04:05.999999999-07:00")
		return Cell{V: s, S: s}
	default:
		s := fmt.Sprint(x)
		return Cell{V: s, S: s}
	}
}

func blobText(b []byte) string {
	const max = 64
	h := b
	suffix := ""
	if len(h) > max {
		h = h[:max]
		suffix = "…"
	}
	return "x'" + strings.ToUpper(hex.EncodeToString(h)) + "'" + suffix
}

// SQLLiteral renders a value as a SQL literal (for display in logs/modals).
func (c Cell) SQLLiteral() string {
	switch x := c.V.(type) {
	case nil:
		return "NULL"
	case string:
		return "'" + strings.ReplaceAll(x, "'", "''") + "'"
	case []byte:
		return "x'" + strings.ToUpper(hex.EncodeToString(x)) + "'"
	default:
		return c.S
	}
}

func jsonValue(v any) any {
	switch x := v.(type) {
	case []byte:
		if utf8.Valid(x) {
			return string(x)
		}
		return "x'" + strings.ToUpper(hex.EncodeToString(x)) + "'"
	case time.Time:
		return x.Format(time.RFC3339Nano)
	}
	return v
}

// ---------------------------------------------------------------- results

// EditInfo is attached to results that can be edited in place.
type EditInfo struct {
	Table     string       // real table name
	TableCols []ColumnInfo // table columns
	ColMap    []string     // result column index -> table column ("" = read-only)
}

// Result is the outcome of executing one statement.
type Result struct {
	SQL           string
	Kind          StmtKind
	Columns       []string
	Rows          [][]Cell
	RowIDs        []int64 // parallel to Rows when Edit != nil
	Edit          *EditInfo
	ReadOnly      string // reason why the result is not editable
	HasRows       bool   // statement produced a result set
	RowsAffected  int64
	LastInsertID  int64
	Truncated     bool
	Elapsed       time.Duration
	SchemaChanged bool
	Page          *PageInfo // set when the query ends in LIMIT n [OFFSET m]
}

// PageInfo lets the grid fetch further pages of a LIMITed query.
type PageInfo struct {
	Base       string // statement without its LIMIT clause
	Limit      int64  // page size from the original LIMIT
	BaseOffset int64  // OFFSET of the original statement
	Offset     int64  // offset of Rows[0]
	Next       int64  // offset of the first row not yet loaded
	HasMore    bool   // at least one more row exists after Next
}

// Page holds rows fetched by FetchPage.
type Page struct {
	Rows    [][]Cell
	RowIDs  []int64
	HasMore bool
}

// FetchPage reads limit rows starting at offset for a paged result, using
// the same editable rewrite as the original query so rowids stay available.
// One extra row is requested to learn whether more rows exist.
func (d *DB) FetchPage(ctx context.Context, r *Result, offset, limit int64) (*Page, error) {
	if r.Page == nil {
		return nil, errors.New("result is not paged")
	}
	q := fmt.Sprintf("%s LIMIT %d OFFSET %d", r.Page.Base, limit+1, offset)
	pr, err := d.QueryEditable(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(pr.Columns) != len(r.Columns) || (r.Edit != nil) != (pr.Edit != nil) {
		return nil, errors.New("result shape changed; re-run the query")
	}
	p := &Page{Rows: pr.Rows, RowIDs: pr.RowIDs}
	if int64(len(p.Rows)) > limit {
		p.HasMore = true
		p.Rows = p.Rows[:limit]
		if p.RowIDs != nil {
			p.RowIDs = p.RowIDs[:limit]
		}
	}
	return p, nil
}

// queryPaged runs a "... LIMIT n [OFFSET m]" query, fetching one extra row
// to know whether more rows follow.
func (d *DB) queryPaged(ctx context.Context, stmt string) (*Result, error) {
	base, limit, offset, ok := parseLimit(stmt)
	if !ok || limit >= MaxResultRows {
		return d.QueryEditable(ctx, stmt)
	}
	r, err := d.QueryEditable(ctx, fmt.Sprintf("%s LIMIT %d OFFSET %d", base, limit+1, offset))
	if err != nil {
		// the rewrite is ours; fall back to exactly what the user wrote
		return d.QueryEditable(ctx, stmt)
	}
	pi := &PageInfo{Base: base, Limit: limit, BaseOffset: offset, Offset: offset}
	if int64(len(r.Rows)) > limit {
		pi.HasMore = true
		r.Rows = r.Rows[:limit]
		if r.RowIDs != nil {
			r.RowIDs = r.RowIDs[:limit]
		}
	}
	pi.Next = offset + int64(len(r.Rows))
	r.Page = pi
	return r, nil
}

// ScriptResult aggregates the execution of several statements.
type ScriptResult struct {
	Results       []*Result
	Err           error
	ErrStmt       string
	SchemaChanged bool
	Elapsed       time.Duration
}

// Last returns the last result that produced rows, or the last result.
func (s *ScriptResult) Last() *Result {
	for i := len(s.Results) - 1; i >= 0; i-- {
		if s.Results[i].HasRows {
			return s.Results[i]
		}
	}
	if len(s.Results) > 0 {
		return s.Results[len(s.Results)-1]
	}
	return nil
}

// Summary returns a short human-readable description of the run.
func (s *ScriptResult) Summary() string {
	var affected int64
	var rows int
	queries := 0
	for _, r := range s.Results {
		affected += r.RowsAffected
		if r.HasRows {
			queries++
			rows = len(r.Rows)
		}
	}
	var parts []string
	if n := len(s.Results); n != 1 {
		parts = append(parts, fmt.Sprintf("%d statements", n))
	}
	if queries > 0 {
		parts = append(parts, fmt.Sprintf("%d rows", rows))
		if l := s.Last(); l != nil && l.Truncated {
			parts[len(parts)-1] += fmt.Sprintf(" (truncated at %d)", MaxResultRows)
		}
		if l := s.Last(); l != nil && l.Page != nil && l.Page.HasMore {
			parts[len(parts)-1] += " (more available: PgDn/Ctrl+F continues, n/p ±100)"
		}
	}
	if affected > 0 || queries == 0 {
		parts = append(parts, fmt.Sprintf("%d rows affected", affected))
	}
	parts = append(parts, s.Elapsed.Round(time.Millisecond).String())
	return strings.Join(parts, ", ")
}

// ExecScript executes all statements in src sequentially, stopping at the
// first error. Queries that are single-table SELECTs are transparently made
// editable (see QueryEditable).
func (d *DB) ExecScript(ctx context.Context, src string) *ScriptResult {
	sr := &ScriptResult{}
	start := time.Now()
	for _, st := range splitStatements(src) {
		r, err := d.ExecStmt(ctx, st.Text)
		if err != nil {
			sr.Err = err
			sr.ErrStmt = st.Text
			break
		}
		sr.Results = append(sr.Results, r)
		if r.SchemaChanged {
			sr.SchemaChanged = true
		}
	}
	sr.Elapsed = time.Since(start)
	return sr
}

// ExecStmt executes a single statement.
func (d *DB) ExecStmt(ctx context.Context, stmt string) (*Result, error) {
	kind := classify(stmt)
	start := time.Now()
	var r *Result
	var err error
	switch {
	case kind == KindQuery:
		r, err = d.queryPaged(ctx, stmt)
	case kind.IsWrite() || kind == KindOther:
		r, err = d.execWrite(ctx, stmt, kind)
	}
	if err != nil {
		return nil, err
	}
	r.SQL = stmt
	r.Kind = kind
	r.Elapsed = time.Since(start)
	return r, nil
}

// QueryEditable runs a query. If it is a single-table SELECT on a rowid
// table, it is re-written to also select <table>.rowid; the synthetic column
// is hidden from the result and the rowids are kept in Result.RowIDs.
func (d *DB) QueryEditable(ctx context.Context, stmt string) (*Result, error) {
	sh := analyzeSelect(stmt)
	if !sh.IsEditable {
		r, err := d.query(ctx, stmt)
		if r != nil {
			r.ReadOnly = sh.Reason
		}
		return r, err
	}
	ti, err := d.tableInfo(sh.Table)
	if err != nil {
		// let SQLite report the real error
		r, qerr := d.query(ctx, stmt)
		if r != nil {
			r.ReadOnly = err.Error()
		}
		return r, qerr
	}
	reason := ""
	switch {
	case ti.Type == "view":
		reason = "view is read-only"
	case ti.WithoutRowID:
		reason = "WITHOUT ROWID table"
	}
	if reason != "" {
		r, err := d.query(ctx, stmt)
		if r != nil {
			r.ReadOnly = reason
		}
		return r, err
	}
	r, err := d.query(ctx, rewriteWithRowID(stmt, sh))
	if err != nil || len(r.Columns) == 0 || r.Columns[0] != RowIDColumn {
		// fall back to the original, read-only query
		r2, err2 := d.query(ctx, stmt)
		if r2 != nil {
			r2.ReadOnly = "could not resolve rowid"
		}
		return r2, err2
	}
	// strip synthetic column
	r.Columns = r.Columns[1:]
	r.RowIDs = make([]int64, len(r.Rows))
	for i, row := range r.Rows {
		if id, ok := row[0].V.(int64); ok {
			r.RowIDs[i] = id
		}
		r.Rows[i] = row[1:]
	}
	r.Edit = &EditInfo{Table: ti.Name, TableCols: ti.Columns, ColMap: mapColumns(sh, ti, len(r.Columns))}
	return r, nil
}

// mapColumns maps result columns to real table columns using the select list.
func mapColumns(sh SelectShape, ti *TableInfo, ncols int) []string {
	colByName := map[string]string{}
	var visible []string
	for _, c := range ti.Columns {
		colByName[strings.ToLower(c.Name)] = c.Name
		if !c.Hidden {
			visible = append(visible, c.Name)
		}
	}
	// generated columns and the rowid alias (INTEGER PRIMARY KEY) are not
	// editable: rows are addressed by rowid, so it must stay stable.
	generated := map[string]bool{}
	npk := 0
	for _, c := range ti.Columns {
		if c.PK > 0 {
			npk++
		}
	}
	for _, c := range ti.Columns {
		if c.Hidden || (npk == 1 && c.PK == 1 && strings.EqualFold(strings.TrimSpace(c.Type), "INTEGER")) {
			generated[strings.ToLower(c.Name)] = true
		}
	}
	qualOK := func(q string) bool {
		return q == "" || strings.EqualFold(q, sh.Table) || (sh.Alias != "" && strings.EqualFold(q, sh.Alias))
	}
	var m []string
	for _, it := range sh.Items {
		switch {
		case it.Star:
			if !qualOK(it.Qualifier) {
				return make([]string, ncols)
			}
			m = append(m, visible...)
		case it.Column != "" && qualOK(it.Qualifier):
			lc := strings.ToLower(it.Column)
			if name, ok := colByName[lc]; ok && !generated[lc] {
				m = append(m, name)
			} else {
				m = append(m, "") // rowid alias, generated col, string literal...
			}
		default:
			m = append(m, "")
		}
	}
	if len(m) != ncols {
		return make([]string, ncols)
	}
	return m
}

func (d *DB) query(ctx context.Context, stmt string) (*Result, error) {
	rows, err := d.conn.QueryContext(ctx, stmt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	r := &Result{Columns: cols, HasRows: true}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if len(r.Rows) >= MaxResultRows {
			r.Truncated = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make([]Cell, len(cols))
		for i, v := range vals {
			row[i] = newCell(v)
		}
		r.Rows = append(r.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return r, nil
}

// execWrite runs a non-query statement, capturing before/after rows for the
// audit log where the statement shape allows it.
func (d *DB) execWrite(ctx context.Context, stmt string, kind StmtKind) (*Result, error) {
	entry := LogEntry{Op: "exec", SQL: stmt}
	r := &Result{}
	var targetIDs []int64
	var wt WriteTarget
	captured := false
	switch kind {
	case KindUpdate, KindDelete:
		entry.Op = kind.String()
		if t, ok := parseWriteTarget(stmt); ok {
			wt = t
			entry.Table = t.Table
			q := "SELECT rowid AS " + RowIDColumn + ", * FROM " + quoteIdent(t.Table)
			if t.Where != "" {
				q += " WHERE " + t.Where
			}
			if before, ids, err := d.snapshot(ctx, q); err == nil {
				entry.Before = before
				targetIDs = ids
				captured = true
			} else {
				entry.Note = "before snapshot unavailable: " + err.Error()
			}
		} else {
			entry.Note = "statement shape not supported for before/after capture"
		}
	case KindInsert:
		entry.Op = "insert"
		if t, ok := insertTarget(stmt); ok {
			entry.Table = t
		}
	case KindDDL:
		entry.Op = "ddl"
		r.SchemaChanged = true
	}

	if hasReturning(stmt) {
		qr, err := d.query(ctx, stmt)
		if err != nil {
			return nil, err
		}
		r.Columns, r.Rows, r.HasRows, r.Truncated = qr.Columns, qr.Rows, true, qr.Truncated
		r.RowsAffected = int64(len(qr.Rows))
		r.ReadOnly = "RETURNING result"
		_ = d.conn.QueryRowContext(ctx, "SELECT changes(), last_insert_rowid()").Scan(&r.RowsAffected, &r.LastInsertID)
	} else {
		res, err := d.conn.ExecContext(ctx, stmt)
		if err != nil {
			return nil, err
		}
		r.RowsAffected, _ = res.RowsAffected()
		r.LastInsertID, _ = res.LastInsertId()
	}
	if kind == KindOther {
		// PRAGMA x=y, BEGIN, VACUUM ...: only log statements that may write.
		sig := sigTokens(tokenize(stmt))
		if len(sig) > 0 {
			switch sig[0].upper() {
			case "BEGIN", "COMMIT", "END", "ROLLBACK", "SAVEPOINT", "RELEASE", "ANALYZE", "EXPLAIN":
				return r, nil
			}
		}
	}
	entry.RowsAffected = r.RowsAffected

	// after snapshots
	switch {
	case kind == KindUpdate && captured && len(targetIDs) > 0:
		q := "SELECT rowid AS " + RowIDColumn + ", * FROM " + quoteIdent(wt.Table) + " WHERE rowid IN (" + joinInts(targetIDs) + ")"
		if after, _, err := d.snapshot(ctx, q); err == nil {
			entry.After = after
		}
	case kind == KindInsert && entry.Table != "" && r.RowsAffected > 0 && r.RowsAffected <= maxLogRows:
		lo := r.LastInsertID - r.RowsAffected + 1
		q := fmt.Sprintf("SELECT rowid AS %s, * FROM %s WHERE rowid BETWEEN %d AND %d", RowIDColumn, quoteIdent(entry.Table), lo, r.LastInsertID)
		if after, _, err := d.snapshot(ctx, q); err == nil {
			entry.After = after
		}
	}
	if kind == KindDDL || kind == KindOther {
		sig := sigTokens(tokenize(stmt))
		if len(sig) > 0 && (sig[0].isKw("VACUUM") || sig[0].isKw("ATTACH") || sig[0].isKw("DETACH")) {
			r.SchemaChanged = true
		}
		if kind == KindOther && len(sig) > 0 && sig[0].isKw("PRAGMA") {
			r.SchemaChanged = true
		}
	}
	if err := d.log.Write(entry); err != nil {
		return r, fmt.Errorf("statement executed but audit log failed: %w", err)
	}
	return r, nil
}

func joinInts(ids []int64) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(s, ",")
}

// snapshot runs a "SELECT rowid AS __sqlitem_rowid__, * ..." query and
// returns JSON-friendly rows keyed by column name plus the rowids.
func (d *DB) snapshot(ctx context.Context, q string) ([]map[string]any, []int64, error) {
	r, err := d.query(ctx, q+" LIMIT "+strconv.Itoa(maxLogRows+1))
	if err != nil {
		return nil, nil, err
	}
	if len(r.Rows) > maxLogRows {
		return nil, nil, fmt.Errorf("more than %d rows", maxLogRows)
	}
	var out []map[string]any
	var ids []int64
	for _, row := range r.Rows {
		m := map[string]any{}
		for i, c := range r.Columns {
			if c == RowIDColumn {
				if id, ok := row[i].V.(int64); ok {
					ids = append(ids, id)
					m["rowid"] = id
				}
				continue
			}
			m[c] = jsonValue(row[i].V)
		}
		out = append(out, m)
	}
	return out, ids, nil
}

// EstimateAffected returns how many rows an UPDATE/DELETE would touch, or -1.
func (d *DB) EstimateAffected(ctx context.Context, stmt string) int64 {
	wt, ok := parseWriteTarget(stmt)
	if !ok {
		return -1
	}
	q := "SELECT count(*) FROM " + quoteIdent(wt.Table)
	if wt.Where != "" {
		q += " WHERE " + wt.Where
	}
	var n int64
	if err := d.conn.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return -1
	}
	return n
}

// ---------------------------------------------------------------- row edits

// rowSnapshot returns the full row (all table columns) as a map.
func (d *DB) rowSnapshot(ctx context.Context, table string, rowid int64) (map[string]any, error) {
	rows, _, err := d.snapshot(ctx, fmt.Sprintf("SELECT rowid AS %s, * FROM %s WHERE rowid = %d", RowIDColumn, quoteIdent(table), rowid))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("row %d no longer exists in %s", rowid, table)
	}
	return rows[0], nil
}

// UpdateCell sets one column of one row (identified by rowid). value nil
// stores NULL. Returns the value as stored by SQLite (after affinity).
func (d *DB) UpdateCell(ctx context.Context, table string, rowid int64, column string, value any) (Cell, error) {
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		return Cell{}, err
	}
	defer tx.Rollback()
	before, err := d.rowSnapshotTx(ctx, tx, table, rowid)
	if err != nil {
		return Cell{}, err
	}
	stmt := fmt.Sprintf("UPDATE %s SET %s = ? WHERE rowid = ?", quoteIdent(table), quoteIdent(column))
	res, err := tx.ExecContext(ctx, stmt, value, rowid)
	if err != nil {
		return Cell{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Cell{}, fmt.Errorf("expected 1 row to change, got %d", n)
	}
	after, err := d.rowSnapshotTx(ctx, tx, table, rowid)
	if err != nil {
		return Cell{}, err
	}
	var stored any
	if err := tx.QueryRowContext(ctx, fmt.Sprintf("SELECT %s FROM %s WHERE rowid = ?", quoteIdent(column), quoteIdent(table)), rowid).Scan(&stored); err != nil {
		return Cell{}, err
	}
	if err := tx.Commit(); err != nil {
		return Cell{}, err
	}
	err = d.log.Write(LogEntry{Op: "update", Table: table, RowID: &rowid, SQL: stmt, Args: []any{jsonValue(value), rowid},
		Before: []map[string]any{before}, After: []map[string]any{after}, RowsAffected: 1})
	return newCell(stored), err
}

// DeleteRows deletes rows by rowid.
func (d *DB) DeleteRows(ctx context.Context, table string, rowids []int64) (int64, error) {
	if len(rowids) == 0 {
		return 0, nil
	}
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var before []map[string]any
	for _, id := range rowids {
		m, err := d.rowSnapshotTx(ctx, tx, table, id)
		if err != nil {
			return 0, err
		}
		before = append(before, m)
	}
	stmt := fmt.Sprintf("DELETE FROM %s WHERE rowid IN (%s)", quoteIdent(table), joinInts(rowids))
	res, err := tx.ExecContext(ctx, stmt)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, d.log.Write(LogEntry{Op: "delete", Table: table, SQL: stmt, Before: before, RowsAffected: n})
}

// InsertRow inserts a row. values maps column name -> value (nil = NULL);
// columns not present use their DEFAULT. Returns the new rowid.
func (d *DB) InsertRow(ctx context.Context, table string, values map[string]any) (int64, error) {
	names := make([]string, 0, len(values))
	for k := range values {
		names = append(names, k)
	}
	sort.Strings(names)
	var stmt string
	args := make([]any, 0, len(names))
	if len(names) == 0 {
		stmt = fmt.Sprintf("INSERT INTO %s DEFAULT VALUES", quoteIdent(table))
	} else {
		q := make([]string, len(names))
		ph := make([]string, len(names))
		for i, n := range names {
			q[i] = quoteIdent(n)
			ph[i] = "?"
			args = append(args, values[n])
		}
		stmt = fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", quoteIdent(table), strings.Join(q, ", "), strings.Join(ph, ", "))
	}
	tx, err := d.conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, stmt, args...)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	after, err := d.rowSnapshotTx(ctx, tx, table, id)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	jargs := make([]any, len(args))
	for i, a := range args {
		jargs[i] = jsonValue(a)
	}
	return id, d.log.Write(LogEntry{Op: "insert", Table: table, RowID: &id, SQL: stmt, Args: jargs,
		After: []map[string]any{after}, RowsAffected: 1})
}

// FetchRow re-reads selected columns of one row (used to refresh the grid).
func (d *DB) FetchRow(ctx context.Context, table string, rowid int64, cols []string) ([]Cell, error) {
	if len(cols) == 0 {
		return nil, nil
	}
	sel := make([]string, len(cols))
	for i, c := range cols {
		if c == "" {
			sel[i] = "NULL"
		} else {
			sel[i] = quoteIdent(c)
		}
	}
	r, err := d.query(ctx, fmt.Sprintf("SELECT %s FROM %s WHERE rowid = %d", strings.Join(sel, ", "), quoteIdent(table), rowid))
	if err != nil {
		return nil, err
	}
	if len(r.Rows) == 0 {
		return nil, fmt.Errorf("row %d not found", rowid)
	}
	return r.Rows[0], nil
}

func (d *DB) rowSnapshotTx(ctx context.Context, tx *sql.Tx, table string, rowid int64) (map[string]any, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf("SELECT * FROM %s WHERE rowid = ?", quoteIdent(table)), rowid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("row %d no longer exists in %s", rowid, table)
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	m := map[string]any{"rowid": rowid}
	for i, c := range cols {
		m[c] = jsonValue(vals[i])
	}
	return m, nil
}

// fileExists is used by main to report whether a new DB will be created.
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
