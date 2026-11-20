// Package store persists the message log, memory notes and the agent tree in SQLite
// (pure Go driver, FTS5 for keyword recall).
package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode"

	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS messages(
  id INTEGER PRIMARY KEY, conv TEXT, sender TEXT, recipient TEXT, kind TEXT, content TEXT, ts INTEGER);
CREATE INDEX IF NOT EXISTS messages_conv ON messages(conv, id);
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(content, content='messages', content_rowid='id');
CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
  INSERT INTO messages_fts(rowid, content) VALUES (new.id, new.content); END;

CREATE TABLE IF NOT EXISTS notes(
  id INTEGER PRIMARY KEY, kind TEXT, subject TEXT, content TEXT, ts INTEGER, active INTEGER DEFAULT 1);
CREATE VIRTUAL TABLE IF NOT EXISTS notes_fts USING fts5(content, content='notes', content_rowid='id');
CREATE TRIGGER IF NOT EXISTS notes_ai AFTER INSERT ON notes BEGIN
  INSERT INTO notes_fts(rowid, content) VALUES (new.id, new.content); END;

CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE IF NOT EXISTS agents(
  id TEXT PRIMARY KEY, conv TEXT, parent TEXT, role TEXT, model TEXT, task TEXT,
  status TEXT, result TEXT, tokens INTEGER DEFAULT 0, created INTEGER, updated INTEGER);
`

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

type Msg struct {
	ID                int64
	Conv              string
	Sender, Recipient string
	Kind              string // user | reply | task | message | report
	Content           string
	TS                time.Time
}

func (s *Store) AddMessage(conv, sender, recipient, kind, content string) {
	s.db.Exec(`INSERT INTO messages(conv,sender,recipient,kind,content,ts) VALUES(?,?,?,?,?,?)`,
		conv, sender, recipient, kind, content, time.Now().Unix())
}

// Dialog returns the last n user<->chat messages of a conversation, oldest first.
func (s *Store) Dialog(conv string, n int) []Msg {
	rows, err := s.db.Query(`SELECT id,conv,sender,recipient,kind,content,ts FROM messages
		WHERE conv=? AND kind IN ('user','reply') ORDER BY id DESC LIMIT ?`, conv, n)
	if err != nil {
		return nil
	}
	out := scanMsgs(rows)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// DialogSince returns user<->chat messages with id > after.
func (s *Store) DialogSince(conv string, after int64) []Msg {
	rows, err := s.db.Query(`SELECT id,conv,sender,recipient,kind,content,ts FROM messages
		WHERE conv=? AND kind IN ('user','reply') AND id>? ORDER BY id`, conv, after)
	if err != nil {
		return nil
	}
	return scanMsgs(rows)
}

func (s *Store) LastMessageID() int64 {
	var id sql.NullInt64
	s.db.QueryRow(`SELECT MAX(id) FROM messages`).Scan(&id)
	return id.Int64
}

func (s *Store) SearchMessages(query string, limit int) []Msg {
	q := ftsQuery(query)
	if q == "" {
		return nil
	}
	rows, err := s.db.Query(`SELECT m.id,m.conv,m.sender,m.recipient,m.kind,m.content,m.ts
		FROM messages_fts f JOIN messages m ON m.id=f.rowid
		WHERE messages_fts MATCH ? ORDER BY bm25(messages_fts) LIMIT ?`, q, limit)
	if err != nil {
		return nil
	}
	return scanMsgs(rows)
}

func scanMsgs(rows *sql.Rows) []Msg {
	defer rows.Close()
	var out []Msg
	for rows.Next() {
		var m Msg
		var ts int64
		if rows.Scan(&m.ID, &m.Conv, &m.Sender, &m.Recipient, &m.Kind, &m.Content, &ts) == nil {
			m.TS = time.Unix(ts, 0)
			out = append(out, m)
		}
	}
	return out
}

type Note struct {
	ID      int64
	Kind    string // fact | summary | lesson | note
	Subject string // user | role name | project
	Content string
	TS      time.Time
}

func (s *Store) AddNote(kind, subject, content string) int64 {
	r, err := s.db.Exec(`INSERT INTO notes(kind,subject,content,ts) VALUES(?,?,?,?)`,
		kind, subject, content, time.Now().Unix())
	if err != nil {
		return 0
	}
	id, _ := r.LastInsertId()
	return id
}

func (s *Store) ForgetNote(id int64) bool {
	r, err := s.db.Exec(`UPDATE notes SET active=0 WHERE id=?`, id)
	if err != nil {
		return false
	}
	n, _ := r.RowsAffected()
	return n > 0
}

// Notes lists active notes of a kind (and subject, if non-empty), newest first.
func (s *Store) Notes(kind, subject string, limit int) []Note {
	q := `SELECT id,kind,subject,content,ts FROM notes WHERE active=1 AND kind=?`
	args := []any{kind}
	if subject != "" {
		q += ` AND subject=?`
		args = append(args, subject)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil
	}
	return scanNotes(rows)
}

func (s *Store) SearchNotes(query string, limit int) []Note {
	q := ftsQuery(query)
	if q == "" {
		return nil
	}
	rows, err := s.db.Query(`SELECT n.id,n.kind,n.subject,n.content,n.ts
		FROM notes_fts f JOIN notes n ON n.id=f.rowid
		WHERE notes_fts MATCH ? AND n.active=1 ORDER BY bm25(notes_fts) LIMIT ?`, q, limit)
	if err != nil {
		return nil
	}
	return scanNotes(rows)
}

func scanNotes(rows *sql.Rows) []Note {
	defer rows.Close()
	var out []Note
	for rows.Next() {
		var n Note
		var ts int64
		if rows.Scan(&n.ID, &n.Kind, &n.Subject, &n.Content, &ts) == nil {
			n.TS = time.Unix(ts, 0)
			out = append(out, n)
		}
	}
	return out
}

func (s *Store) UpsertAgent(id, conv, parent, role, model, task, status string) {
	now := time.Now().Unix()
	s.db.Exec(`INSERT INTO agents(id,conv,parent,role,model,task,status,created,updated) VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET status=excluded.status, updated=excluded.updated`,
		id, conv, parent, role, model, task, status, now, now)
}

func (s *Store) FinishAgent(id, status, result string, tokens int64) {
	s.db.Exec(`UPDATE agents SET status=?, result=?, tokens=?, updated=? WHERE id=?`,
		status, result, tokens, time.Now().Unix(), id)
}

// ftsQuery turns free text into an OR query of quoted terms, safe for FTS5 syntax.
func ftsQuery(text string) string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
	var terms []string
	seen := map[string]bool{}
	for _, w := range words {
		if len([]rune(w)) < 2 || seen[w] {
			continue
		}
		seen[w] = true
		terms = append(terms, `"`+w+`"`)
	}
	return strings.Join(terms, " OR ")
}

// Setting / SetSetting persist small per-conversation preferences (e.g. /think).
func (s *Store) Setting(key string) string {
	var v string
	s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v
}

func (s *Store) SetSetting(key, value string) {
	s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
}
