package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/crush/internal/db"
)

// Source is a read-only handle on an OpenCode database.
type Source struct {
	conn   *sql.DB
	path   string
	gen    Generation
	tables map[string]bool
	cols   map[string]map[string]bool
}

// Open opens the OpenCode database at path read-only and works out which
// storage layout it uses. It fails when the file holds no recognisable
// OpenCode tables.
func Open(ctx context.Context, path string) (*Source, error) {
	conn, err := db.ConnectReadOnly(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	src := &Source{conn: conn, path: path, cols: map[string]map[string]bool{}}
	if err := src.detect(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return src, nil
}

// Close releases the database handle.
func (s *Source) Close() error {
	return s.conn.Close()
}

// Path is the database file this source reads.
func (s *Source) Path() string { return s.path }

// Generation reports the storage layout detected at Open.
func (s *Source) Generation() Generation { return s.gen }

func (s *Source) detect(ctx context.Context) error {
	rows, err := s.conn.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table'`)
	if err != nil {
		return fmt.Errorf("list tables in %s: %w", s.path, err)
	}
	defer rows.Close()
	s.tables = map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		s.tables[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	switch {
	case s.tables["session_v2"] && s.tables["session_message"]:
		s.gen = GenerationV2
	case s.tables["session"] && s.tables["message"] && s.tables["part"]:
		s.gen = GenerationV1
	default:
		names := make([]string, 0, len(s.tables))
		for name := range s.tables {
			names = append(names, name)
		}
		sort.Strings(names)
		return fmt.Errorf("%s does not look like an OpenCode database (tables: %s)", s.path, strings.Join(names, ", "))
	}
	return nil
}

// hasColumn reports whether table has a column named col. Older databases
// may predate columns that newer OpenCode releases added.
func (s *Source) hasColumn(ctx context.Context, table, col string) (bool, error) {
	cols, ok := s.cols[table]
	if !ok {
		rows, err := s.conn.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%q)", table))
		if err != nil {
			return false, fmt.Errorf("inspect %s: %w", table, err)
		}
		defer rows.Close()
		cols = map[string]bool{}
		for rows.Next() {
			var (
				cid        int
				name, typ  string
				notNull    int
				defaultVal sql.NullString
				pk         int
			)
			if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultVal, &pk); err != nil {
				return false, err
			}
			cols[name] = true
		}
		if err := rows.Err(); err != nil {
			return false, err
		}
		s.cols[table] = cols
	}
	return cols[col], nil
}

// Sessions returns every session in the database, oldest first.
func (s *Source) Sessions(ctx context.Context) ([]Session, error) {
	table := "session_v2"
	titleExpr := "COALESCE(title, '')"
	if s.gen == GenerationV1 {
		table = "session"
		titleExpr = "title"
	}
	costExpr := "0"
	if ok, err := s.hasColumn(ctx, table, "cost"); err != nil {
		return nil, err
	} else if ok {
		costExpr = "cost"
	}

	query := fmt.Sprintf(`SELECT id, project_id, COALESCE(parent_id, ''), slug, directory, %s, %s, time_created, time_updated
		FROM %q ORDER BY time_created ASC, id ASC`, titleExpr, costExpr, table)
	rows, err := s.conn.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var sess Session
		if err := rows.Scan(&sess.ID, &sess.ProjectID, &sess.ParentID, &sess.Slug, &sess.Directory,
			&sess.Title, &sess.Cost, &sess.Created, &sess.Updated); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

// Projects returns the project table keyed by ID. Databases without the
// table yield an empty map.
func (s *Source) Projects(ctx context.Context) (map[string]Project, error) {
	projects := map[string]Project{}
	if !s.tables["project"] {
		return projects, nil
	}
	rows, err := s.conn.QueryContext(ctx, `SELECT id, worktree, COALESCE(name, '') FROM project`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Worktree, &p.Name); err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		projects[p.ID] = p
	}
	return projects, rows.Err()
}

// Messages returns the messages of one session in conversation order with
// their out-of-line content attached. Only one session is loaded at a time
// so that very large histories stream through bounded memory.
func (s *Source) Messages(ctx context.Context, sessionID string) ([]Message, error) {
	if s.gen == GenerationV1 {
		return s.messagesV1(ctx, sessionID)
	}
	return s.messagesV2(ctx, sessionID)
}

func (s *Source) messagesV2(ctx context.Context, sessionID string) ([]Message, error) {
	rows, err := s.conn.QueryContext(ctx, `SELECT id, type, seq, time_created, time_updated, data
		FROM session_message WHERE session_id = ? ORDER BY seq ASC, id ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list messages for %s: %w", sessionID, err)
	}
	defer rows.Close()

	var messages []Message
	index := map[string]int{}
	for rows.Next() {
		var (
			msg  Message
			data []byte
		)
		if err := rows.Scan(&msg.ID, &msg.Type, &msg.Seq, &msg.Created, &msg.Updated, &data); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		msg.Data = json.RawMessage(data)
		index[msg.ID] = len(messages)
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !s.tables["session_part"] || len(messages) == 0 {
		return messages, nil
	}

	parts, err := s.conn.QueryContext(ctx, `SELECT message_id, ordinal, time_created, time_updated, data
		FROM session_part WHERE session_id = ? ORDER BY message_id ASC, ordinal ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list parts for %s: %w", sessionID, err)
	}
	defer parts.Close()
	for parts.Next() {
		var (
			messageID string
			part      RawPart
			data      []byte
		)
		if err := parts.Scan(&messageID, &part.Ordinal, &part.Created, &part.Updated, &data); err != nil {
			return nil, fmt.Errorf("scan part: %w", err)
		}
		part.Data = json.RawMessage(data)
		if i, ok := index[messageID]; ok {
			messages[i].Parts = append(messages[i].Parts, part)
		}
	}
	return messages, parts.Err()
}

// v1Envelope is the subset of the legacy message document the reader
// needs to classify a row.
type v1Envelope struct {
	Role string `json:"role"`
}

func (s *Source) messagesV1(ctx context.Context, sessionID string) ([]Message, error) {
	rows, err := s.conn.QueryContext(ctx, `SELECT id, time_created, time_updated, data
		FROM message WHERE session_id = ? ORDER BY time_created ASC, id ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list messages for %s: %w", sessionID, err)
	}
	defer rows.Close()

	var messages []Message
	index := map[string]int{}
	for rows.Next() {
		var (
			msg  Message
			data []byte
		)
		if err := rows.Scan(&msg.ID, &msg.Created, &msg.Updated, &data); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		msg.Data = json.RawMessage(data)
		var env v1Envelope
		if err := json.Unmarshal(data, &env); err == nil {
			msg.Type = env.Role
		}
		msg.Seq = int64(len(messages))
		index[msg.ID] = len(messages)
		messages = append(messages, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(messages) == 0 {
		return messages, nil
	}

	parts, err := s.conn.QueryContext(ctx, `SELECT id, message_id, time_created, time_updated, data
		FROM part WHERE session_id = ? ORDER BY message_id ASC, id ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list parts for %s: %w", sessionID, err)
	}
	defer parts.Close()
	for parts.Next() {
		var (
			messageID string
			part      RawPart
			data      []byte
		)
		if err := parts.Scan(&part.ID, &messageID, &part.Created, &part.Updated, &data); err != nil {
			return nil, fmt.Errorf("scan part: %w", err)
		}
		part.Data = json.RawMessage(data)
		if i, ok := index[messageID]; ok {
			part.Ordinal = int64(len(messages[i].Parts))
			messages[i].Parts = append(messages[i].Parts, part)
		}
	}
	return messages, parts.Err()
}

// Todos returns the todo list OpenCode kept for a session, in display
// order. Databases without the table yield nil.
func (s *Source) Todos(ctx context.Context, sessionID string) ([]Todo, error) {
	if !s.tables["todo"] {
		return nil, nil
	}
	rows, err := s.conn.QueryContext(ctx, `SELECT content, status, priority, position
		FROM todo WHERE session_id = ? ORDER BY position ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list todos for %s: %w", sessionID, err)
	}
	defer rows.Close()
	var todos []Todo
	for rows.Next() {
		var todo Todo
		if err := rows.Scan(&todo.Content, &todo.Status, &todo.Priority, &todo.Position); err != nil {
			return nil, fmt.Errorf("scan todo: %w", err)
		}
		todos = append(todos, todo)
	}
	return todos, rows.Err()
}

// Blob looks up a stored attachment by its hex SHA-256. It returns nil
// when the database has no blob table or no such blob.
func (s *Source) Blob(ctx context.Context, hash string) (*Blob, error) {
	if !s.tables["blob"] {
		return nil, nil
	}
	blob := &Blob{Hash: hash}
	err := s.conn.QueryRowContext(ctx, `SELECT mime, data FROM blob WHERE hash = ?`, hash).Scan(&blob.Mime, &blob.Data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load blob %s: %w", hash, err)
	}
	return blob, nil
}
