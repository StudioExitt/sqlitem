package main

import "strings"

// formatSQL pretty-prints a script: keywords are upper-cased, major clauses
// start on their own line and top-level select/set lists get one item per
// line. Comments and literals are preserved verbatim.
func formatSQL(src string) string {
	stmts := splitStatements(src)
	if len(stmts) == 0 {
		return src
	}
	parts := make([]string, 0, len(stmts))
	for _, s := range stmts {
		parts = append(parts, formatStmt(s.Text)+";")
	}
	return strings.Join(parts, "\n\n") + "\n"
}

var sqlKeywords = map[string]bool{}

func init() {
	for _, k := range strings.Fields(`ABORT ACTION ADD AFTER ALL ALTER ALWAYS ANALYZE AND AS ASC ATTACH
	AUTOINCREMENT BEFORE BEGIN BETWEEN BY CASCADE CASE CAST CHECK COLLATE COLUMN COMMIT CONFLICT
	CONSTRAINT CREATE CROSS CURRENT CURRENT_DATE CURRENT_TIME CURRENT_TIMESTAMP DATABASE DEFAULT
	DEFERRABLE DEFERRED DELETE DESC DETACH DISTINCT DO DROP EACH ELSE END ESCAPE EXCEPT EXCLUDE
	EXCLUSIVE EXISTS EXPLAIN FAIL FILTER FIRST FOLLOWING FOR FOREIGN FROM FULL GENERATED GLOB GROUP
	GROUPS HAVING IF IGNORE IMMEDIATE IN INDEX INDEXED INITIALLY INNER INSERT INSTEAD INTERSECT INTO
	IS ISNULL JOIN KEY LAST LEFT LIKE LIMIT MATCH MATERIALIZED NATURAL NO NOT NOTHING NOTNULL NULL
	NULLS OF OFFSET ON OR ORDER OTHERS OUTER OVER PARTITION PLAN PRAGMA PRECEDING PRIMARY QUERY
	RAISE RANGE RECURSIVE REFERENCES REGEXP REINDEX RELEASE RENAME REPLACE RESTRICT RETURNING RIGHT
	ROLLBACK ROW ROWS SAVEPOINT SELECT SET TABLE TEMP TEMPORARY THEN TIES TO TRANSACTION TRIGGER
	UNBOUNDED UNION UNIQUE UPDATE USING VACUUM VALUES VIEW VIRTUAL WHEN WHERE WINDOW WITH WITHOUT
	INTEGER TEXT REAL BLOB NUMERIC VARCHAR`) {
		sqlKeywords[k] = true
	}
}

// clause keywords that start a new line at depth 0 (multi-word handled below)
var lineBreakKw = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "GROUP": true, "ORDER": true, "HAVING": true,
	"LIMIT": true, "OFFSET": false, "UNION": true, "INTERSECT": true, "EXCEPT": true, "VALUES": true,
	"SET": true, "JOIN": true, "LEFT": true, "RIGHT": true, "INNER": true, "CROSS": true, "FULL": true,
	"NATURAL": true, "RETURNING": true, "WINDOW": true, "ON": false,
}

func formatStmt(stmt string) string {
	toks := tokenize(stmt)
	var b strings.Builder
	depth := 0
	lineStart := true
	listClause := "" // SELECT / SET / VALUES at depth 0: break after commas
	prevSig := token{}
	joinPrefix := false // inside LEFT OUTER JOIN etc.
	isDDL := classify(stmt) == KindDDL
	indent := func() string { return strings.Repeat("  ", depth) }
	newline := func() {
		s := strings.TrimRight(b.String(), " ")
		b.Reset()
		b.WriteString(s)
		b.WriteString("\n")
		lineStart = true
	}
	write := func(s string) {
		if lineStart {
			b.WriteString(indent())
			lineStart = false
		}
		b.WriteString(s)
	}
	space := func() {
		if !lineStart {
			str := b.String()
			if !strings.HasSuffix(str, " ") && !strings.HasSuffix(str, "(") && !strings.HasSuffix(str, "\n") {
				b.WriteString(" ")
			}
		}
	}
	for idx, t := range toks {
		switch t.kind {
		case tkSpace:
			continue
		case tkComment:
			space()
			write(t.text)
			if strings.HasPrefix(t.text, "--") {
				newline()
			}
			continue
		}
		up := t.upper()
		text := t.text
		if t.kind == tkWord && sqlKeywords[up] {
			// do not upper-case function-like identifiers such as replace(...)
			nextSig := nextSignificant(toks, idx)
			if !(nextSig.isPunct("(") && !lineBreakKw[up] && up != "IN" && up != "EXISTS" && up != "VALUES" && up != "AS" && up != "USING" && up != "OVER" && up != "FILTER") {
				text = up
			}
		}
		if t.kind == tkWord && !isDDL && depth == 0 && lineBreakKw[up] && !(prevSig.kind == tkWord && sqlKeywords[prevSig.upper()] && (joinPrefix || prevSig.isKw("UNION"))) && b.Len() > 0 {
			newline()
		}
		if t.kind == tkWord && depth == 0 {
			switch up {
			case "LEFT", "RIGHT", "INNER", "CROSS", "FULL", "NATURAL", "OUTER":
				joinPrefix = true
			case "JOIN":
				joinPrefix = false
			}
			switch up {
			case "SELECT", "SET", "VALUES":
				listClause = up
			case "FROM", "WHERE", "GROUP", "ORDER", "HAVING", "LIMIT", "RETURNING", "UNION", "INTERSECT", "EXCEPT":
				listClause = ""
			}
		}
		switch {
		case t.isPunct(","):
			write(",")
			if depth == 0 && listClause != "" && !isDDL {
				newline()
				b.WriteString("  ")
				lineStart = false
			} else if isDDL && depth == 1 {
				newline()
			}
		case t.isPunct("("):
			if prevSig.kind == tkWord && !sqlKeywords[prevSig.upper()] || prevSig.kind == tkQIdent && !isDDL {
				// function call: no space
			} else {
				space()
			}
			write("(")
			depth++
			if isDDL && depth == 1 && classify(stmt) == KindDDL && isCreateTable(toks) {
				newline()
			}
		case t.isPunct(")"):
			depth--
			if depth < 0 {
				depth = 0
			}
			if isDDL && depth == 0 && isCreateTable(toks) {
				newline()
			}
			write(")")
		case t.isPunct("."):
			write(".")
		default:
			if !prevSig.isPunct(".") && !prevSig.isPunct("(") {
				space()
			}
			write(text)
			if t.kind == tkWord && depth == 0 && !isDDL && (up == "SELECT" || up == "SET") {
				// first item on the following line, indented
				newline()
				b.WriteString("  ")
				lineStart = false
			}
		}
		prevSig = t
	}
	out := strings.TrimSpace(b.String())
	// collapse blank lines
	lines := strings.Split(out, "\n")
	res := lines[:0]
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		res = append(res, strings.TrimRight(l, " "))
	}
	return strings.Join(res, "\n")
}

func nextSignificant(toks []token, idx int) token {
	for j := idx + 1; j < len(toks); j++ {
		if toks[j].significant() {
			return toks[j]
		}
	}
	return token{}
}

func isCreateTable(toks []token) bool {
	sig := sigTokens(toks)
	for i, t := range sig {
		if i > 3 {
			break
		}
		if t.isKw("TABLE") {
			return sig[0].isKw("CREATE")
		}
	}
	return false
}
