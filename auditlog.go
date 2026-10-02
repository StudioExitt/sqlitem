package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// LogEntry is one JSON line in the audit log.
type LogEntry struct {
	Time         string           `json:"time"`
	DB           string           `json:"db"`
	Op           string           `json:"op"` // update, delete, insert, ddl, exec
	Table        string           `json:"table,omitempty"`
	RowID        *int64           `json:"rowid,omitempty"`
	SQL          string           `json:"sql"`
	Args         []any            `json:"args,omitempty"`
	RowsAffected int64            `json:"rows_affected"`
	Before       []map[string]any `json:"before"`
	After        []map[string]any `json:"after"`
	Note         string           `json:"note,omitempty"`
}

// AuditLog appends JSON lines describing every write operation.
type AuditLog struct {
	Path   string
	dbPath string
	mu     sync.Mutex
}

// defaultLogPath returns "<db>.sqlitem.log.jsonl" next to the database, or a
// file under the user's home directory if that location is not writable.
func defaultLogPath(dbPath string) string {
	p := dbPath + ".sqlitem.log.jsonl"
	_, statErr := os.Stat(p)
	if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Close()
		if os.IsNotExist(statErr) {
			os.Remove(p) // only probing writability; created on first write
		}
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	dir := filepath.Join(home, ".sqlitem")
	_ = os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, filepath.Base(dbPath)+".log.jsonl")
}

func NewAuditLog(path, dbPath string) *AuditLog {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		abs = dbPath
	}
	return &AuditLog{Path: path, dbPath: abs}
}

// Write appends one entry. The file is opened per write so that external
// log rotation and concurrent sqlitem instances behave sanely.
func (l *AuditLog) Write(e LogEntry) error {
	if l == nil || l.Path == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Path == "" {
		return nil
	}
	e.Time = time.Now().Format(time.RFC3339Nano)
	e.DB = l.dbPath
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}
