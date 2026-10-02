package main

import (
	"flag"
	"fmt"
	"os"

	"golang.org/x/term"
)

var version = "0.1.0"

func usage() {
	fmt.Fprintf(os.Stderr, `sqlitem %s - SQLite TUI

Usage:
  sqlitem [flags] <file.db>
  sqlitem -db <file.db> [flags]

Modes (default: full-screen TUI):
  -console          line based REPL (no TTY needed; reads stdin)
  -exec  "<sql>"    execute SQL and exit
  -query "<sql>"    run a read-only query and exit
  -ascii            ASCII tables (implies -console unless -exec/-query given)

Flags:
`, version)
	flag.PrintDefaults()
}

func main() {
	var (
		dbFlag    = flag.String("db", "", "path to SQLite database file (created if missing)")
		console   = flag.Bool("console", false, "line based REPL, no TTY required")
		execSQL   = flag.String("exec", "", "execute SQL statements and exit")
		querySQL  = flag.String("query", "", "execute a read-only query and exit")
		ascii     = flag.Bool("ascii", false, "render ASCII tables (for terminals without Unicode)")
		assumeYes = flag.Bool("yes", false, "non-interactive modes: run UPDATE/DELETE/DROP without confirmation")
		logPath   = flag.String("log", "", "audit log path (default: <file.db>.sqlitem.log.jsonl)")
		showVer   = flag.Bool("version", false, "print version and exit")
	)
	flag.Usage = usage
	flag.Parse()
	if *showVer {
		fmt.Println("sqlitem", version)
		return
	}
	path := *dbFlag
	if path == "" && flag.NArg() > 0 {
		path = flag.Arg(0)
	}
	if path == "" {
		usage()
		os.Exit(2)
	}
	lp := *logPath
	if lp == "" {
		lp = defaultLogPath(path)
	}
	db, err := OpenDB(path, lp)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sqlitem:", err)
		os.Exit(1)
	}
	defer db.Close()

	exit := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			db.Close()
			os.Exit(1)
		}
	}

	switch {
	case *execSQL != "" || *querySQL != "":
		c := NewConsole(db, *ascii, *assumeYes)
		if *execSQL != "" {
			exit(c.RunScript(*execSQL))
		}
		if *querySQL != "" {
			c.readOnly = true
			exit(c.RunScript(*querySQL))
		}
	case *console || *ascii:
		c := NewConsole(db, *ascii, *assumeYes)
		exit(c.REPL(os.Stdin))
	default:
		if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
			// no TTY: fall back to the console so pipes keep working
			c := NewConsole(db, *ascii, *assumeYes)
			exit(c.REPL(os.Stdin))
			return
		}
		exit(RunTUI(db))
	}
}
