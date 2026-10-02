package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Console implements the TTY-less modes: line REPL (-console / -ascii) and
// one-shot execution (-exec / -query).
type Console struct {
	db        *DB
	out       io.Writer
	errOut    io.Writer
	ascii     bool
	assumeYes bool
	readOnly  bool // -query: refuse statements that write
	confirm   func(prompt string) bool
}

const consoleMaxColWidth = 60

func NewConsole(db *DB, ascii, assumeYes bool) *Console {
	c := &Console{db: db, out: os.Stdout, errOut: os.Stderr, ascii: ascii, assumeYes: assumeYes}
	c.confirm = c.ttyConfirm
	return c
}

// ttyConfirm asks y/N on the controlling terminal. Without a terminal it
// refuses, so destructive statements never run unconfirmed in scripts
// (use -yes to opt in explicitly).
func (c *Console) ttyConfirm(prompt string) bool {
	if c.assumeYes {
		return true
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintln(c.errOut, "refusing destructive statement without a terminal for confirmation (use -yes)")
		return false
	}
	defer tty.Close()
	fmt.Fprintf(tty, "%s [y/N]: ", prompt)
	line, _ := bufio.NewReader(tty).ReadString('\n')
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}

// RunScript executes src (one or more statements) and prints results.
func (c *Console) RunScript(src string) error {
	ctx := context.Background()
	stmts := splitStatements(src)
	if len(stmts) == 0 {
		return nil
	}
	for _, st := range stmts {
		kind := classify(st.Text)
		if c.readOnly && kind != KindQuery {
			return fmt.Errorf("-query only runs read-only statements (got %s); use -exec", kind)
		}
		if isDestructive(st.Text) {
			n := c.db.EstimateAffected(ctx, st.Text)
			prompt := "Execute destructive statement: " + oneLine(st.Text, 100)
			if n >= 0 {
				prompt += fmt.Sprintf(" (%d rows)", n)
			}
			if !c.confirm(prompt) {
				return errors.New("cancelled: " + oneLine(st.Text, 60))
			}
		}
		r, err := c.db.ExecStmt(ctx, st.Text)
		if err != nil {
			return err
		}
		c.printResult(r)
	}
	return nil
}

func (c *Console) printResult(r *Result) {
	if r.HasRows {
		if len(r.Columns) > 0 {
			fmt.Fprint(c.out, RenderTable(r, c.ascii, consoleMaxColWidth))
		}
		suffix := ""
		if r.Truncated {
			suffix = fmt.Sprintf(" (truncated at %d)", MaxResultRows)
		}
		fmt.Fprintf(c.out, "(%d rows%s)\n", len(r.Rows), suffix)
		return
	}
	switch r.Kind {
	case KindInsert, KindUpdate, KindDelete:
		fmt.Fprintf(c.out, "OK, %d rows affected\n", r.RowsAffected)
	default:
		fmt.Fprintln(c.out, "OK")
	}
}

// REPL reads statements line by line until EOF or .quit.
func (c *Console) REPL(in io.Reader) error {
	interactive := false
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		interactive = true
	}
	if interactive {
		fmt.Fprintf(c.out, "sqlitem console - %s\nEnter SQL terminated by ';'. Type .help for commands.\n", c.db.Path)
	}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	var buf strings.Builder
	failed := false
	prompt := func() {
		if !interactive {
			return
		}
		if buf.Len() == 0 {
			fmt.Fprint(c.out, "sqlitem> ")
		} else {
			fmt.Fprint(c.out, "   ...> ")
		}
	}
	prompt()
	for sc.Scan() {
		line := sc.Text()
		if buf.Len() == 0 && strings.HasPrefix(strings.TrimSpace(line), ".") {
			quit, err := c.dotCommand(strings.TrimSpace(line))
			if err != nil {
				fmt.Fprintln(c.errOut, "Error:", err)
				failed = true
			}
			if quit {
				return nil
			}
			prompt()
			continue
		}
		buf.WriteString(line)
		buf.WriteString("\n")
		if stmtComplete(buf.String()) {
			if err := c.RunScript(buf.String()); err != nil {
				fmt.Fprintln(c.errOut, "Error:", err)
				failed = true
			}
			buf.Reset()
		}
		prompt()
	}
	if err := sc.Err(); err != nil {
		return err
	}
	// run an unterminated trailing statement
	if strings.TrimSpace(buf.String()) != "" {
		if err := c.RunScript(buf.String()); err != nil {
			fmt.Fprintln(c.errOut, "Error:", err)
			failed = true
		}
	}
	if interactive {
		fmt.Fprintln(c.out)
	}
	if failed && !interactive {
		return errors.New("one or more statements failed")
	}
	return nil
}

func (c *Console) dotCommand(line string) (quit bool, err error) {
	f := strings.Fields(line)
	switch f[0] {
	case ".quit", ".exit", ".q":
		return true, nil
	case ".help":
		fmt.Fprint(c.out, `.tables            list tables and views
.schema [name]     show DDL (all objects, or one table with its indexes/triggers)
.columns <table>   show columns of a table
.log               show audit log path
.quit              exit
`)
	case ".tables":
		ts, err := c.db.Schema()
		if err != nil {
			return false, err
		}
		for _, t := range ts {
			kind := ""
			if t.Type == "view" {
				kind = " (view)"
			}
			fmt.Fprintf(c.out, "%s%s\n", t.Name, kind)
		}
	case ".schema":
		var s string
		if len(f) > 1 {
			s, err = c.db.DDL(f[1])
		} else {
			s, err = c.db.AllDDL()
		}
		if err != nil {
			return false, err
		}
		fmt.Fprintln(c.out, s)
	case ".columns":
		if len(f) < 2 {
			return false, errors.New("usage: .columns <table>")
		}
		cols, err := c.db.Columns(f[1])
		if err != nil {
			return false, err
		}
		if len(cols) == 0 {
			return false, fmt.Errorf("no such table: %s", f[1])
		}
		for _, col := range cols {
			fmt.Fprintf(c.out, "%-24s %-12s %s\n", col.Name, col.Type, columnFlags(col, c.ascii))
		}
	case ".log":
		fmt.Fprintln(c.out, c.db.LogPath())
	default:
		return false, fmt.Errorf("unknown command %s (try .help)", f[0])
	}
	return false, nil
}

// columnFlags renders PK / NN markers.
func columnFlags(c ColumnInfo, ascii bool) string {
	var parts []string
	if c.PK > 0 {
		parts = append(parts, "PK")
	}
	if c.NotNull {
		parts = append(parts, "NN")
	}
	if c.Hidden {
		parts = append(parts, "GEN")
	}
	return strings.Join(parts, " ")
}

// oneLine collapses whitespace and truncates for prompts/status lines.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if max > 0 && len([]rune(s)) > max {
		return string([]rune(s)[:max-3]) + "..."
	}
	return s
}
