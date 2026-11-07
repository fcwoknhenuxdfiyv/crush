package opencode

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/charmbracelet/crush/internal/db"
)

// ImportOptions tunes an import run.
type ImportOptions struct {
	// Usage controls whether usage ledger rows are written.
	Usage bool
	// DryRun maps every session but writes nothing.
	DryRun bool
}

// ProjectResult reports what happened to one project's sessions.
type ProjectResult struct {
	Dir            string         `json:"dir"`
	DataDir        string         `json:"data_dir"`
	Sessions       int            `json:"sessions"`
	Imported       int            `json:"imported"`
	AlreadyPresent int            `json:"already_present"`
	Failed         int            `json:"failed"`
	Messages       int            `json:"messages"`
	Events         int            `json:"usage_events"`
	Dropped        map[string]int `json:"dropped,omitempty"`
	Warnings       []string       `json:"warnings,omitempty"`
}

// Importer copies sessions from an OpenCode source into Crush databases.
type Importer struct {
	src  *Source
	opts ImportOptions
}

// NewImporter returns an importer reading from src.
func NewImporter(src *Source, opts ImportOptions) *Importer {
	return &Importer{src: src, opts: opts}
}

// ImportProject imports the sessions of one project plan into conn, which
// must be the project's migrated Crush database. In dry-run mode conn may
// be nil or a read-only handle; it is only used to spot sessions that are
// already present.
//
// Each session is written in its own transaction, messages first and the
// session row last, so the schema triggers never touch the preserved
// timestamps. A session that fails to convert is reported and skipped
// along with its descendants; the rest of the project still imports.
func (im *Importer) ImportProject(ctx context.Context, proj *ProjectPlan, conn *sql.DB) (*ProjectResult, error) {
	res := &ProjectResult{
		Dir:      proj.Dir,
		DataDir:  proj.DataDir,
		Sessions: proj.Total,
		Dropped:  map[string]int{},
	}

	var (
		q      *db.Queries
		writer *sql.Conn
	)
	switch {
	case im.opts.DryRun:
		if conn != nil {
			q = db.New(conn)
		}
	default:
		if conn == nil {
			return nil, errors.New("import requires a database connection")
		}
		c, err := conn.Conn(ctx)
		if err != nil {
			return nil, fmt.Errorf("acquire connection: %w", err)
		}
		writer = c
		if _, err := writer.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
			writer.Close()
			return nil, fmt.Errorf("disable foreign keys: %w", err)
		}
		q = db.New(writer)
	}

	for _, root := range proj.Roots {
		if err := ctx.Err(); err != nil {
			im.release(ctx, writer)
			return res, err
		}
		im.importTree(ctx, q, writer, proj, root, root.ID, "", res)
	}

	if writer != nil {
		if violations := im.release(ctx, writer); violations > 0 {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%d rows reference missing sessions after import", violations))
		}
	}
	return res, nil
}

// release restores foreign key enforcement, checks the result and returns
// the connection to the pool. It returns the number of violations found.
func (im *Importer) release(ctx context.Context, writer *sql.Conn) int {
	if writer == nil {
		return 0
	}
	defer writer.Close()
	ctx = context.WithoutCancel(ctx)
	if _, err := writer.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return 0
	}
	rows, err := writer.QueryContext(ctx, "PRAGMA foreign_key_check(messages)")
	if err != nil {
		return 0
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	if err := rows.Err(); err != nil {
		return 0
	}
	return n
}

// importTree imports sess under the given Crush IDs and then recurses into
// its children. It returns the session's total cost including children,
// which the parent folds into its own.
func (im *Importer) importTree(ctx context.Context, q *db.Queries, writer *sql.Conn, proj *ProjectPlan, sess Session, crushID, parentCrushID string, res *ProjectResult) float64 {
	mapped, err := MapSession(ctx, im.src, sess, MapOptions{
		SessionID:       crushID,
		ParentSessionID: parentCrushID,
		Usage:           im.opts.Usage,
	})
	if err != nil {
		res.Failed++
		res.Warnings = append(res.Warnings, fmt.Sprintf("session %s: %v", sess.ID, err))
		im.skipChildren(proj, sess.ID, res)
		return 0
	}

	existing, found, err := im.existing(ctx, q, crushID)
	if err != nil {
		res.Failed++
		res.Warnings = append(res.Warnings, fmt.Sprintf("session %s: %v", sess.ID, err))
		im.skipChildren(proj, sess.ID, res)
		return 0
	}

	childCost := 0.0
	for _, child := range proj.Children[sess.ID] {
		childID := mapped.Children[child.ID]
		if childID == "" {
			childID = child.ID
		}
		childCost += im.importTree(ctx, q, writer, proj, child, childID, crushID, res)
	}

	if found {
		res.AlreadyPresent++
		return existing.Cost
	}

	mapped.Session.Cost += childCost
	for k, v := range mapped.Dropped {
		res.Dropped[k] += v
	}
	if im.opts.DryRun {
		res.Imported++
		res.Messages += len(mapped.Messages)
		res.Events += len(mapped.Events)
		return mapped.Session.Cost
	}

	if err := writeSession(ctx, writer, mapped); err != nil {
		res.Failed++
		res.Warnings = append(res.Warnings, fmt.Sprintf("session %s: %v", sess.ID, err))
		return 0
	}
	res.Imported++
	res.Messages += len(mapped.Messages)
	res.Events += len(mapped.Events)
	return mapped.Session.Cost
}

func (im *Importer) existing(ctx context.Context, q *db.Queries, id string) (db.Session, bool, error) {
	if q == nil {
		return db.Session{}, false, nil
	}
	row, err := q.GetSessionByID(ctx, id)
	switch {
	case err == nil:
		return row, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return db.Session{}, false, nil
	default:
		return db.Session{}, false, fmt.Errorf("look up existing session: %w", err)
	}
}

// skipChildren counts the descendants of a failed session as failed too,
// since importing them would leave them pointing at a missing parent.
func (im *Importer) skipChildren(proj *ProjectPlan, id string, res *ProjectResult) {
	for _, child := range proj.Children[id] {
		res.Failed++
		res.Warnings = append(res.Warnings, fmt.Sprintf("session %s: skipped because its parent failed to import", child.ID))
		im.skipChildren(proj, child.ID, res)
	}
}

// writeSession stores one mapped session atomically.
func writeSession(ctx context.Context, writer *sql.Conn, mapped *Mapped) error {
	tx, err := writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	q := db.New(tx)
	for _, msg := range mapped.Messages {
		if err := q.ImportMessage(ctx, msg); err != nil {
			tx.Rollback()
			return fmt.Errorf("write message %s: %w", msg.ID, err)
		}
	}
	if err := q.ImportSession(ctx, mapped.Session); err != nil {
		tx.Rollback()
		return fmt.Errorf("write session: %w", err)
	}
	for _, ev := range mapped.Events {
		if err := q.CreateUsageEvent(ctx, ev); err != nil {
			tx.Rollback()
			return fmt.Errorf("write usage event: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// SortedDropped returns the dropped-kind counters as "kind=count" strings
// in a stable order, for reports.
func (r *ProjectResult) SortedDropped() []string {
	keys := make([]string, 0, len(r.Dropped))
	for k := range r.Dropped {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%s=%d", k, r.Dropped[k]))
	}
	return out
}
