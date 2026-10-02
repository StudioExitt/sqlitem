package main

import (
	"strings"
	"unicode"
)

// This file contains a small, dependency-free SQL lexer and the light-weight
// structural analysis sqlitem needs: statement splitting, statement
// classification, single-table SELECT detection (for in-place editing) and
// UPDATE/DELETE target extraction (for confirmation and before/after logging).

type tokKind int

const (
	tkSpace tokKind = iota
	tkComment
	tkWord   // identifier or keyword
	tkQIdent // "quoted", `quoted`, [quoted]
	tkString // 'string'
	tkNumber
	tkBlob  // x'ABCD'
	tkParam // ? ?1 :a @a $a
	tkPunct
)

type token struct {
	kind  tokKind
	text  string
	start int // byte offset in source
	end   int
}

func (t token) upper() string { return strings.ToUpper(t.text) }

// isKw reports whether the token is the given (upper case) keyword.
func (t token) isKw(kw string) bool { return t.kind == tkWord && strings.EqualFold(t.text, kw) }

func (t token) isPunct(p string) bool { return t.kind == tkPunct && t.text == p }

// significant reports whether the token is neither whitespace nor a comment.
func (t token) significant() bool { return t.kind != tkSpace && t.kind != tkComment }

func isIdentStart(r rune) bool { return r == '_' || unicode.IsLetter(r) || r >= 0x80 }
func isIdentPart(r rune) bool  { return isIdentStart(r) || unicode.IsDigit(r) || r == '$' }

// tokenize splits src into tokens. It never fails: unterminated strings or
// comments simply run to the end of the input.
func tokenize(src string) []token {
	var toks []token
	rs := []rune(src)
	// byte offsets for each rune index
	offs := make([]int, len(rs)+1)
	{
		o := 0
		for i, r := range rs {
			offs[i] = o
			o += len(string(r))
		}
		offs[len(rs)] = o
	}
	n := len(rs)
	i := 0
	emit := func(k tokKind, from, to int) {
		toks = append(toks, token{kind: k, text: string(rs[from:to]), start: offs[from], end: offs[to]})
	}
	for i < n {
		r := rs[i]
		start := i
		switch {
		case unicode.IsSpace(r):
			for i < n && unicode.IsSpace(rs[i]) {
				i++
			}
			emit(tkSpace, start, i)
		case r == '-' && i+1 < n && rs[i+1] == '-':
			for i < n && rs[i] != '\n' {
				i++
			}
			emit(tkComment, start, i)
		case r == '/' && i+1 < n && rs[i+1] == '*':
			i += 2
			for i < n && !(rs[i] == '*' && i+1 < n && rs[i+1] == '/') {
				i++
			}
			if i < n {
				i += 2
			}
			emit(tkComment, start, i)
		case (r == 'x' || r == 'X') && i+1 < n && rs[i+1] == '\'':
			i += 2
			for i < n && rs[i] != '\'' {
				i++
			}
			if i < n {
				i++
			}
			emit(tkBlob, start, i)
		case r == '\'':
			i = scanQuoted(rs, i, '\'')
			emit(tkString, start, i)
		case r == '"' || r == '`':
			i = scanQuoted(rs, i, r)
			emit(tkQIdent, start, i)
		case r == '[':
			for i < n && rs[i] != ']' {
				i++
			}
			if i < n {
				i++
			}
			emit(tkQIdent, start, i)
		case unicode.IsDigit(r) || (r == '.' && i+1 < n && unicode.IsDigit(rs[i+1])):
			for i < n && (unicode.IsDigit(rs[i]) || rs[i] == '.' || rs[i] == '_' ||
				unicode.IsLetter(rs[i]) || ((rs[i] == '+' || rs[i] == '-') && (rs[i-1] == 'e' || rs[i-1] == 'E'))) {
				i++
			}
			emit(tkNumber, start, i)
		case r == '?' || ((r == ':' || r == '@' || r == '$') && i+1 < n && isIdentStart(rs[i+1])):
			i++
			for i < n && (isIdentPart(rs[i]) || unicode.IsDigit(rs[i])) {
				i++
			}
			emit(tkParam, start, i)
		case isIdentStart(r):
			for i < n && isIdentPart(rs[i]) {
				i++
			}
			emit(tkWord, start, i)
		default:
			i++
			if i < n {
				two := string(rs[start : i+1])
				switch two {
				case "<=", ">=", "<>", "!=", "==", "||", "<<", ">>", "->":
					i++
					if two == "->" && i < n && rs[i] == '>' {
						i++
					}
				}
			}
			emit(tkPunct, start, i)
		}
	}
	return toks
}

// scanQuoted scans a quoted run starting at rs[i] (the opening quote), where a
// doubled quote is an escaped quote. Returns the index after the closing quote.
func scanQuoted(rs []rune, i int, q rune) int {
	n := len(rs)
	i++
	for i < n {
		if rs[i] == q {
			if i+1 < n && rs[i+1] == q {
				i += 2
				continue
			}
			return i + 1
		}
		i++
	}
	return n
}

// sigTokens returns only significant tokens (no whitespace or comments).
func sigTokens(toks []token) []token {
	out := make([]token, 0, len(toks))
	for _, t := range toks {
		if t.significant() {
			out = append(out, t)
		}
	}
	return out
}

// Stmt is one statement of a script with its byte span in the source.
type Stmt struct {
	Text  string // trimmed statement text without the trailing ';'
	Start int    // byte offset of the statement's first byte (incl. leading space)
	End   int    // byte offset just after the terminating ';' (or end of input)
}

// splitStatements splits a script on top-level semicolons. Semicolons inside
// a CREATE TRIGGER ... BEGIN ... END body do not terminate the statement.
// Statements consisting only of whitespace/comments are dropped.
func splitStatements(src string) []Stmt {
	toks := tokenize(src)
	var out []Stmt
	segStart := 0
	var sig []token // significant tokens of current segment
	depth := 0      // BEGIN/CASE ... END nesting inside a trigger
	flush := func(end, next int) {
		if len(sig) > 0 {
			text := strings.TrimSpace(src[sig[0].start:sig[len(sig)-1].end])
			// keep leading comments out of Text but inside span
			out = append(out, Stmt{Text: text, Start: segStart, End: next})
		}
		_ = end
		segStart = next
		sig = sig[:0]
		depth = 0
	}
	for _, t := range toks {
		if !t.significant() {
			continue
		}
		if t.isPunct(";") && depth == 0 {
			flush(t.start, t.end)
			continue
		}
		sig = append(sig, t)
		if isTriggerDef(sig) {
			if t.isKw("BEGIN") || t.isKw("CASE") {
				depth++
			} else if t.isKw("END") && depth > 0 {
				depth--
			}
		}
	}
	flush(len(src), len(src))
	return out
}

// isTriggerDef reports whether the significant tokens begin a CREATE TRIGGER.
func isTriggerDef(sig []token) bool {
	if len(sig) < 2 || !sig[0].isKw("CREATE") {
		return false
	}
	for _, t := range sig[1:min(len(sig), 4)] {
		if t.isKw("TRIGGER") {
			return true
		}
		if !(t.isKw("TEMP") || t.isKw("TEMPORARY")) {
			return false
		}
	}
	return false
}

// statementAt returns the index of the statement containing byte offset off.
// When off sits between statements it picks the preceding one (or the first).
func statementAt(stmts []Stmt, off int) int {
	if len(stmts) == 0 {
		return -1
	}
	for i, s := range stmts {
		if off >= s.Start && off <= s.End {
			return i
		}
	}
	best := 0
	for i, s := range stmts {
		if s.Start <= off {
			best = i
		}
	}
	return best
}

// stmtComplete reports whether src ends with a terminated statement (used by
// the line REPL to know when to execute).
func stmtComplete(src string) bool {
	toks := sigTokens(tokenize(src))
	if len(toks) == 0 {
		return false
	}
	last := toks[len(toks)-1]
	if !last.isPunct(";") {
		return false
	}
	// make sure the last ';' actually terminated a statement (not inside a trigger body)
	stmts := splitStatements(src)
	if len(stmts) == 0 {
		return true
	}
	return stmts[len(stmts)-1].End == last.end
}

// StmtKind classifies statements.
type StmtKind int

const (
	KindQuery  StmtKind = iota // returns rows
	KindInsert                 // INSERT / REPLACE
	KindUpdate
	KindDelete
	KindDDL   // CREATE / DROP / ALTER
	KindOther // BEGIN, COMMIT, VACUUM, PRAGMA x=y, ATTACH ...
)

func (k StmtKind) String() string {
	return [...]string{"query", "insert", "update", "delete", "ddl", "other"}[k]
}

// IsWrite reports whether the statement modifies data or schema.
func (k StmtKind) IsWrite() bool {
	return k == KindInsert || k == KindUpdate || k == KindDelete || k == KindDDL
}

// classify determines the kind of a single statement.
func classify(stmt string) StmtKind {
	sig := sigTokens(tokenize(stmt))
	if len(sig) == 0 {
		return KindOther
	}
	first := sig[0].upper()
	if first == "WITH" {
		// find the first top-level verb after the CTE list
		depth := 0
		for _, t := range sig[1:] {
			switch {
			case t.isPunct("("):
				depth++
			case t.isPunct(")"):
				depth--
			case depth == 0 && t.kind == tkWord:
				switch t.upper() {
				case "SELECT", "VALUES":
					return KindQuery
				case "INSERT", "REPLACE":
					return KindInsert
				case "UPDATE":
					return KindUpdate
				case "DELETE":
					return KindDelete
				}
			}
		}
		return KindQuery
	}
	switch first {
	case "SELECT", "VALUES", "EXPLAIN":
		return KindQuery
	case "PRAGMA":
		for _, t := range sig {
			if t.isPunct("=") {
				return KindOther
			}
		}
		return KindQuery
	case "INSERT", "REPLACE":
		return KindInsert
	case "UPDATE":
		return KindUpdate
	case "DELETE":
		return KindDelete
	case "CREATE", "DROP", "ALTER":
		return KindDDL
	}
	return KindOther
}

// hasReturning reports whether a write statement has a top-level RETURNING clause.
func hasReturning(stmt string) bool {
	depth := 0
	for _, t := range sigTokens(tokenize(stmt)) {
		switch {
		case t.isPunct("("):
			depth++
		case t.isPunct(")"):
			depth--
		case depth == 0 && t.isKw("RETURNING"):
			return true
		}
	}
	return false
}

// isDestructive reports whether a statement requires explicit confirmation.
func isDestructive(stmt string) bool {
	switch classify(stmt) {
	case KindUpdate, KindDelete:
		return true
	case KindDDL:
		sig := sigTokens(tokenize(stmt))
		return len(sig) > 0 && sig[0].isKw("DROP")
	}
	return false
}

// unquoteIdent strips SQL identifier quoting.
func unquoteIdent(s string) string {
	if len(s) >= 2 {
		switch {
		case s[0] == '"' && s[len(s)-1] == '"':
			return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
		case s[0] == '`' && s[len(s)-1] == '`':
			return strings.ReplaceAll(s[1:len(s)-1], "``", "`")
		case s[0] == '[' && s[len(s)-1] == ']':
			return s[1 : len(s)-1]
		}
	}
	return s
}

// quoteIdent always double-quotes an identifier.
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func isIdentTok(t token) bool { return t.kind == tkWord || t.kind == tkQIdent || t.kind == tkString }

// WriteTarget describes the table and filter of an UPDATE or DELETE.
type WriteTarget struct {
	Kind  StmtKind
	Table string // unquoted table name
	Where string // WHERE expression text ("" means all rows)
}

// parseWriteTarget extracts the target table and WHERE clause of a simple
// UPDATE/DELETE. ok is false for forms it does not understand (CTEs, UPDATE
// ... FROM, ORDER BY/LIMIT, schema-qualified names etc.).
func parseWriteTarget(stmt string) (wt WriteTarget, ok bool) {
	sig := sigTokens(tokenize(stmt))
	i := 0
	next := func() (token, bool) {
		if i >= len(sig) {
			return token{}, false
		}
		t := sig[i]
		i++
		return t, true
	}
	t, _ := next()
	switch {
	case t.isKw("UPDATE"):
		wt.Kind = KindUpdate
		if i < len(sig) && sig[i].isKw("OR") {
			i += 2
		}
	case t.isKw("DELETE"):
		wt.Kind = KindDelete
		if f, ok := next(); !ok || !f.isKw("FROM") {
			return wt, false
		}
	default:
		return wt, false
	}
	name, ok2 := next()
	if !ok2 || !isIdentTok(name) {
		return wt, false
	}
	if i < len(sig) && sig[i].isPunct(".") {
		// schema.table
		if i+1 >= len(sig) {
			return wt, false
		}
		schema := unquoteIdent(name.text)
		if !strings.EqualFold(schema, "main") {
			return wt, false
		}
		i++
		name = sig[i]
		i++
	}
	wt.Table = unquoteIdent(name.text)
	// find top-level WHERE and make sure no unsupported clause follows
	depth := 0
	whereStart := -1
	for j := i; j < len(sig); j++ {
		tk := sig[j]
		switch {
		case tk.isPunct("("):
			depth++
		case tk.isPunct(")"):
			depth--
		case depth == 0 && tk.kind == tkWord:
			switch tk.upper() {
			case "FROM":
				if wt.Kind == KindUpdate {
					return wt, false
				}
			case "WHERE":
				if whereStart < 0 {
					whereStart = j
				}
			case "ORDER", "LIMIT", "RETURNING":
				if whereStart >= 0 {
					wt.Where = strings.TrimSpace(stmt[sig[whereStart+1].start:sig[j-1].end])
				}
				if tk.upper() == "RETURNING" {
					return wt, true
				}
				return wt, false
			}
		}
	}
	if whereStart >= 0 {
		if whereStart+1 >= len(sig) {
			return wt, false
		}
		end := sig[len(sig)-1].end
		wt.Where = strings.TrimSpace(stmt[sig[whereStart+1].start:end])
	}
	return wt, true
}

// insertTarget extracts the table of an INSERT/REPLACE statement.
func insertTarget(stmt string) (string, bool) {
	sig := sigTokens(tokenize(stmt))
	for i := 0; i+1 < len(sig); i++ {
		if sig[i].isKw("INTO") {
			name := sig[i+1]
			if !isIdentTok(name) {
				return "", false
			}
			if i+3 < len(sig) && sig[i+2].isPunct(".") {
				if !strings.EqualFold(unquoteIdent(name.text), "main") {
					return "", false
				}
				name = sig[i+3]
			}
			return unquoteIdent(name.text), true
		}
		if sig[i].isPunct("(") {
			return "", false
		}
	}
	return "", false
}

// SelectItem is one top-level element of a SELECT list.
type SelectItem struct {
	Star      bool   // * or qualifier.*
	Qualifier string // table/alias qualifier (unquoted) if any
	Column    string // bare column name if the expression is a plain column ref
	Expr      string // original expression text
}

// SelectShape is the result of analysing a SELECT for in-place editability.
type SelectShape struct {
	Table      string // unquoted table name
	Alias      string // unquoted alias ("" if none)
	Items      []SelectItem
	selectEnd  int // byte offset just after the SELECT keyword
	Reason     string
	IsEditable bool
}

var aggregateFuncs = map[string]bool{
	"COUNT": true, "SUM": true, "AVG": true, "MIN": true, "MAX": true,
	"TOTAL": true, "GROUP_CONCAT": true, "STRING_AGG": true, "JSON_GROUP_ARRAY": true,
	"JSON_GROUP_OBJECT": true,
}

// analyzeSelect decides whether stmt is a simple single-table SELECT whose
// rows can be edited by rowid. When not editable, Reason says why.
func analyzeSelect(stmt string) SelectShape {
	var sh SelectShape
	sig := sigTokens(tokenize(stmt))
	if len(sig) == 0 {
		sh.Reason = "empty statement"
		return sh
	}
	if sig[0].isKw("WITH") {
		sh.Reason = "CTE (WITH) query"
		return sh
	}
	if !sig[0].isKw("SELECT") {
		sh.Reason = "not a SELECT"
		return sh
	}
	sh.selectEnd = sig[0].end
	i := 1
	if i < len(sig) && (sig[i].isKw("DISTINCT")) {
		sh.Reason = "DISTINCT query"
		return sh
	}
	if i < len(sig) && sig[i].isKw("ALL") {
		i++
		sh.selectEnd = sig[i-1].end
	}
	// collect select items up to top-level FROM
	depth := 0
	itemStart := i
	fromIdx := -1
	var items [][]token
	for j := i; j < len(sig); j++ {
		t := sig[j]
		switch {
		case t.isPunct("("):
			depth++
		case t.isPunct(")"):
			depth--
		case depth == 0 && t.isPunct(","):
			items = append(items, sig[itemStart:j])
			itemStart = j + 1
		case depth == 0 && t.isKw("FROM"):
			items = append(items, sig[itemStart:j])
			fromIdx = j
		}
		if fromIdx >= 0 {
			break
		}
	}
	if fromIdx < 0 {
		sh.Reason = "no FROM clause"
		return sh
	}
	// FROM <table> [[AS] alias] then only WHERE/ORDER/LIMIT/OFFSET
	j := fromIdx + 1
	if j >= len(sig) {
		sh.Reason = "incomplete FROM"
		return sh
	}
	if sig[j].isPunct("(") {
		sh.Reason = "subquery in FROM"
		return sh
	}
	if !isIdentTok(sig[j]) {
		sh.Reason = "unsupported FROM"
		return sh
	}
	name := sig[j]
	j++
	if j+1 < len(sig) && sig[j].isPunct(".") {
		if !strings.EqualFold(unquoteIdent(name.text), "main") {
			sh.Reason = "attached/temp schema"
			return sh
		}
		name = sig[j+1]
		j += 2
	}
	if j < len(sig) && sig[j].isPunct("(") {
		sh.Reason = "table-valued function"
		return sh
	}
	sh.Table = unquoteIdent(name.text)
	if j < len(sig) && sig[j].isKw("AS") {
		j++
		if j < len(sig) {
			sh.Alias = unquoteIdent(sig[j].text)
			j++
		}
	} else if j < len(sig) && isIdentTok(sig[j]) && !isClauseKw(sig[j]) {
		sh.Alias = unquoteIdent(sig[j].text)
		j++
	}
	depth = 0
	for ; j < len(sig); j++ {
		t := sig[j]
		switch {
		case t.isPunct("("):
			depth++
			if depth == 1 && j+1 < len(sig) && sig[j+1].isKw("SELECT") {
				// subqueries in WHERE are fine for editing
			}
		case t.isPunct(")"):
			depth--
		case depth == 0 && t.isPunct(","):
			sh.Reason = "multiple tables (JOIN)"
			return sh
		case depth == 0 && t.kind == tkWord:
			switch t.upper() {
			case "JOIN", "NATURAL", "CROSS", "INNER", "LEFT", "RIGHT", "FULL", "OUTER":
				sh.Reason = "JOIN query"
				return sh
			case "GROUP", "HAVING":
				sh.Reason = "aggregate (GROUP BY)"
				return sh
			case "UNION", "INTERSECT", "EXCEPT":
				sh.Reason = "compound SELECT"
				return sh
			case "WINDOW":
				sh.Reason = "window query"
				return sh
			}
		}
	}
	for _, it := range items {
		si, agg := parseSelectItem(stmt, it)
		if agg {
			sh.Reason = "aggregate function"
			return sh
		}
		sh.Items = append(sh.Items, si)
	}
	sh.IsEditable = true
	return sh
}

func isClauseKw(t token) bool {
	if t.kind != tkWord {
		return false
	}
	switch t.upper() {
	case "WHERE", "ORDER", "LIMIT", "OFFSET", "GROUP", "HAVING", "JOIN", "NATURAL", "CROSS",
		"INNER", "LEFT", "RIGHT", "FULL", "OUTER", "UNION", "INTERSECT", "EXCEPT", "WINDOW", "INDEXED", "NOT":
		return true
	}
	return false
}

// parseSelectItem analyses a single select-list item. agg is true when the
// item contains a top-level aggregate call (making the query an aggregate).
func parseSelectItem(src string, toks []token) (si SelectItem, agg bool) {
	if len(toks) == 0 {
		return si, false
	}
	si.Expr = src[toks[0].start:toks[len(toks)-1].end]
	for k, t := range toks {
		if t.kind == tkWord && aggregateFuncs[t.upper()] && k+1 < len(toks) && toks[k+1].isPunct("(") {
			// window functions (OVER) are not aggregates of the whole result
			hasOver := false
			for _, u := range toks[k:] {
				if u.isKw("OVER") {
					hasOver = true
				}
			}
			if !hasOver {
				return si, true
			}
		}
	}
	// strip alias: expr [AS] alias
	body := toks
	if len(body) >= 3 && body[len(body)-2].isKw("AS") {
		body = body[:len(body)-2]
	} else if len(body) == 2 && isIdentTok(body[0]) && isIdentTok(body[1]) {
		body = body[:1]
	} else if len(body) == 4 && body[1].isPunct(".") && isIdentTok(body[3]) {
		body = body[:3]
	}
	switch {
	case len(body) == 1 && body[0].isPunct("*"):
		si.Star = true
	case len(body) == 3 && body[1].isPunct(".") && body[2].isPunct("*"):
		si.Star = true
		si.Qualifier = unquoteIdent(body[0].text)
	case len(body) == 1 && (body[0].kind == tkWord || body[0].kind == tkQIdent):
		si.Column = unquoteIdent(body[0].text)
	case len(body) == 3 && body[1].isPunct(".") && isIdentTok(body[0]) && (body[2].kind == tkWord || body[2].kind == tkQIdent):
		si.Qualifier = unquoteIdent(body[0].text)
		si.Column = unquoteIdent(body[2].text)
	}
	return si, false
}

// RowIDColumn is the synthetic column name prepended for editable queries.
const RowIDColumn = "__sqlitem_rowid__"

// rewriteWithRowID inserts "<qual>.rowid AS __sqlitem_rowid__," right after
// the SELECT keyword.
func rewriteWithRowID(stmt string, sh SelectShape) string {
	qual := sh.Table
	if sh.Alias != "" {
		qual = sh.Alias
	}
	return stmt[:sh.selectEnd] + " " + quoteIdent(qual) + ".rowid AS " + RowIDColumn + "," + stmt[sh.selectEnd:]
}

// parseLimit recognises a trailing top-level "LIMIT n [OFFSET m]" or
// "LIMIT m, n" with integer literals on a query. It returns the statement
// without that clause so pages can be fetched with other offsets.
func parseLimit(stmt string) (base string, limit, offset int64, ok bool) {
	sig := sigTokens(tokenize(stmt))
	if len(sig) == 0 || !(sig[0].isKw("SELECT") || sig[0].isKw("WITH") || sig[0].isKw("VALUES")) {
		return "", 0, 0, false
	}
	depth := 0
	at := -1
	for i, t := range sig {
		switch {
		case t.isPunct("("):
			depth++
		case t.isPunct(")"):
			depth--
		case depth == 0 && t.isKw("LIMIT"):
			at = i
		}
	}
	if at < 0 {
		return "", 0, 0, false
	}
	num := func(t token) (int64, bool) {
		if t.kind != tkNumber {
			return 0, false
		}
		var n int64
		for _, c := range t.text {
			if c < '0' || c > '9' {
				return 0, false
			}
			n = n*10 + int64(c-'0')
		}
		return n, true
	}
	rest := sig[at+1:]
	switch {
	case len(rest) == 1:
		limit, ok = num(rest[0])
	case len(rest) == 3 && rest[1].isKw("OFFSET"):
		var ok2 bool
		limit, ok = num(rest[0])
		offset, ok2 = num(rest[2])
		ok = ok && ok2
	case len(rest) == 3 && rest[1].isPunct(","):
		var ok2 bool
		offset, ok = num(rest[0])
		limit, ok2 = num(rest[2])
		ok = ok && ok2
	}
	if !ok || limit <= 0 {
		return "", 0, 0, false
	}
	return strings.TrimSpace(stmt[:sig[at].start]), limit, offset, true
}
