package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

// fixture is a scratch OpenCode database built with the same driver the
// importer reads through.
type fixture struct {
	t    *testing.T
	conn *sql.DB
	path string
}

const v2Schema = `
CREATE TABLE project (
	id text PRIMARY KEY, worktree text NOT NULL, vcs text, name text,
	time_created integer NOT NULL, time_updated integer NOT NULL, sandboxes text NOT NULL DEFAULT '[]'
);
CREATE TABLE session_v2 (
	id text PRIMARY KEY, project_id text NOT NULL, workspace_id text, parent_id text,
	fork_session_id text, fork_boundary text, slug text NOT NULL, directory text NOT NULL,
	path text, title text, version text NOT NULL, share_url text, metadata text,
	cost real DEFAULT 0 NOT NULL, tokens_input integer DEFAULT 0 NOT NULL,
	tokens_output integer DEFAULT 0 NOT NULL, tokens_reasoning integer DEFAULT 0 NOT NULL,
	tokens_cache_read integer DEFAULT 0 NOT NULL, tokens_cache_write integer DEFAULT 0 NOT NULL,
	revert text, permission text, agent text, model text,
	time_created integer NOT NULL, time_updated integer NOT NULL, time_compacting integer, time_archived integer
);
CREATE TABLE session_message (
	id text PRIMARY KEY, session_id text NOT NULL, type text NOT NULL, seq integer NOT NULL,
	time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL
);
CREATE UNIQUE INDEX session_message_session_seq_idx ON session_message (session_id, seq);
CREATE TABLE session_part (
	message_id text NOT NULL, session_id text NOT NULL, ordinal integer NOT NULL, type text NOT NULL,
	call_id text, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL,
	PRIMARY KEY (message_id, ordinal)
);
CREATE TABLE blob (
	hash text PRIMARY KEY, mime text NOT NULL, bytes integer NOT NULL, width integer, height integer,
	data blob NOT NULL, time_created integer NOT NULL
);
CREATE TABLE todo (
	session_id text NOT NULL, content text NOT NULL, status text NOT NULL, priority text NOT NULL,
	position integer NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL,
	PRIMARY KEY (session_id, position)
);
`

// v1Schema mirrors the legacy layout before the cost columns were added so
// the reader's column probing is exercised.
const v1Schema = `
CREATE TABLE project (
	id text PRIMARY KEY, worktree text NOT NULL, vcs text, name text,
	time_created integer NOT NULL, time_updated integer NOT NULL, sandboxes text NOT NULL DEFAULT '[]'
);
CREATE TABLE session (
	id text PRIMARY KEY, project_id text NOT NULL, parent_id text, slug text NOT NULL,
	directory text NOT NULL, title text NOT NULL, version text NOT NULL, share_url text,
	revert text, permission text, time_created integer NOT NULL, time_updated integer NOT NULL,
	time_compacting integer, time_archived integer
);
CREATE TABLE message (
	id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL,
	time_updated integer NOT NULL, data text NOT NULL
);
CREATE TABLE part (
	id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL,
	time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL
);
CREATE TABLE todo (
	session_id text NOT NULL, content text NOT NULL, status text NOT NULL, priority text NOT NULL,
	position integer NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL,
	PRIMARY KEY (session_id, position)
);
`

func newFixture(t *testing.T, schema string) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	conn, err := db.OpenFile(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	_, err = conn.ExecContext(t.Context(), schema)
	require.NoError(t, err)
	return &fixture{t: t, conn: conn, path: path}
}

func (f *fixture) exec(query string, args ...any) {
	f.t.Helper()
	_, err := f.conn.ExecContext(context.Background(), query, args...)
	require.NoError(f.t, err)
}

func (f *fixture) project(id, worktree string) {
	f.exec(`INSERT INTO project (id, worktree, name, time_created, time_updated) VALUES (?, ?, ?, 1, 1)`,
		id, worktree, filepath.Base(worktree))
}

// sessionV2 inserts a session_v2 row. created and updated are Unix
// milliseconds.
func (f *fixture) sessionV2(id, projectID, parentID, dir, title string, cost float64, created, updated int64) {
	var parent any
	if parentID != "" {
		parent = parentID
	}
	f.exec(`INSERT INTO session_v2 (id, project_id, parent_id, slug, directory, title, version, cost, time_created, time_updated)
		VALUES (?, ?, ?, ?, ?, ?, '1.0.0', ?, ?, ?)`, id, projectID, parent, "slug-"+id, dir, nullableString(title), cost, created, updated)
}

func (f *fixture) sessionV1(id, projectID, parentID, dir, title string, created, updated int64) {
	var parent any
	if parentID != "" {
		parent = parentID
	}
	f.exec(`INSERT INTO session (id, project_id, parent_id, slug, directory, title, version, time_created, time_updated)
		VALUES (?, ?, ?, ?, ?, ?, '0.9.0', ?, ?)`, id, projectID, parent, "slug-"+id, dir, title, created, updated)
}

// messageV2 inserts a session_message row with data encoded from v.
func (f *fixture) messageV2(id, sessionID, typ string, seq, created int64, v any) {
	f.exec(`INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, id, sessionID, typ, seq, created, created, mustJSON(f.t, v))
}

func (f *fixture) partV2(messageID, sessionID string, ordinal int64, typ string, v any) {
	f.exec(`INSERT INTO session_part (message_id, session_id, ordinal, type, call_id, time_created, time_updated, data)
		VALUES (?, ?, ?, ?, NULL, 1, 1, ?)`, messageID, sessionID, ordinal, typ, mustJSON(f.t, v))
}

func (f *fixture) messageV1(id, sessionID string, created int64, v any) {
	f.exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`,
		id, sessionID, created, created, mustJSON(f.t, v))
}

func (f *fixture) partV1(id, messageID, sessionID string, created int64, v any) {
	f.exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)`,
		id, messageID, sessionID, created, created, mustJSON(f.t, v))
}

func (f *fixture) blob(hash, mime string, data []byte) {
	f.exec(`INSERT INTO blob (hash, mime, bytes, data, time_created) VALUES (?, ?, ?, ?, 1)`, hash, mime, len(data), data)
}

func (f *fixture) todo(sessionID, content, status string, position int64) {
	f.exec(`INSERT INTO todo (session_id, content, status, priority, position, time_created, time_updated)
		VALUES (?, ?, ?, 'medium', ?, 1, 1)`, sessionID, content, status, position)
}

func (f *fixture) open() *Source {
	f.t.Helper()
	src, err := Open(f.t.Context(), f.path)
	require.NoError(f.t, err)
	f.t.Cleanup(func() { src.Close() })
	return src
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	if s, ok := v.(string); ok {
		return s
	}
	data, err := json.Marshal(v)
	require.NoError(t, err)
	return string(data)
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// obj is shorthand for building JSON documents in tests.
type obj = map[string]any

// seedV2Conversation writes a representative v2 session into f: a user
// prompt with a blob attachment, an assistant turn with reasoning, text and
// three tool calls (one failed), the sub-agent child session one of them
// spawned, a compaction and the bookkeeping messages Crush drops. It
// returns the root session ID.
func seedV2Conversation(f *fixture, dir string) string {
	const (
		root  = "ses_root"
		child = "ses_child"
	)
	f.project("prj_1", dir)
	f.sessionV2(root, "prj_1", "", dir, "Fix the flaky test", 1.5, 1_700_000_000_000, 1_700_000_900_000)
	f.sessionV2(child, "prj_1", root, dir, "Explore the repo", 0.25, 1_700_000_100_000, 1_700_000_200_000)
	f.blob("ab12", "image/png", []byte("PNGDATA"))
	f.todo(root, "Reproduce failure", "completed", 0)
	f.todo(root, "Fix it", "in_progress", 1)
	f.todo(root, "Old idea", "cancelled", 2)

	f.messageV2("msg_1", root, "user", 1, 1_700_000_000_000, obj{
		"text": "Please fix the flaky test",
		"files": []obj{{
			"mime": "image/png", "name": "shot.png", "uri": "media:ab12",
			"source": obj{"type": "inline"},
		}},
		"time": obj{"created": 1_700_000_000_000},
	})
	f.messageV2("msg_2", root, "agent-switched", 2, 1_700_000_000_000, obj{
		"time": obj{"created": 1_700_000_000_000}, "agent": "build", "previous": "plan",
	})
	f.messageV2("msg_3", root, "assistant", 3, 1_700_000_000_000, obj{
		"agent": "build",
		"model": obj{"providerID": "github-copilot", "id": "claude-opus-4.8", "variant": "max"},
		"content": []obj{
			{"type": "reasoning", "text": "Need to look at the test first.", "time": obj{"created": 1_700_000_001_000, "completed": 1_700_000_003_000}},
			{"type": "text", "text": "Let me look."},
			{
				"type": "tool", "id": "call_read", "name": "read",
				"state": obj{
					"status":  "completed",
					"input":   obj{"filePath": "/repo/x_test.go", "offset": 10, "limit": 5},
					"content": []obj{{"type": "text", "text": "line 10\nline 11"}},
				},
				"time": obj{"created": 1_700_000_004_000, "completed": 1_700_000_005_000},
			},
			{
				"type": "tool", "id": "call_bash", "name": "bash",
				"state": obj{
					"status": "error",
					"input":  obj{"command": "go test ./...", "description": "Run tests", "timeout": 60},
					"error":  obj{"type": "tool.execution", "message": "exit status 1"},
				},
				"time": obj{"created": 1_700_000_005_000, "completed": 1_700_000_006_000},
			},
			{
				"type": "tool", "id": "call_task", "name": "task",
				"state": obj{
					"status":   "completed",
					"input":    obj{"description": "Explore", "prompt": "Find the test", "subagent_type": "explore"},
					"content":  []obj{{"type": "text", "text": "Found it in x_test.go"}},
					"metadata": obj{"sessionId": child, "parentSessionId": root},
				},
				"time": obj{"created": 1_700_000_006_000, "completed": 1_700_000_007_000},
			},
		},
		"finish": "tool-calls",
		"cost":   1.25,
		"tokens": obj{"input": 100, "output": 50, "reasoning": 10, "cache": obj{"read": 1000, "write": 200}},
		"time":   obj{"created": 1_700_000_000_000, "completed": 1_700_000_007_000},
	})
	f.messageV2("msg_4", root, "assistant", 4, 1_700_000_010_000, obj{
		"agent":   "build",
		"model":   obj{"providerID": "github-copilot", "id": "claude-opus-4.8"},
		"content": []obj{{"type": "text", "text": "Done, the test passes now."}},
		"finish":  "stop",
		"cost":    0.25,
		"tokens":  obj{"input": 20, "output": 30, "reasoning": 0, "cache": obj{"read": 1500, "write": 0}},
		"time":    obj{"created": 1_700_000_010_000, "completed": 1_700_000_012_000},
	})
	f.messageV2("msg_5", root, "idle", 5, 1_700_000_012_000, obj{
		"time": obj{"created": 1_700_000_012_000}, "outcome": "succeeded",
	})
	f.messageV2("msg_6", root, "compaction", 6, 1_700_000_020_000, obj{
		"status": "completed", "reason": "auto", "summary": "## Summary\nFixed the test.",
		"time": obj{"created": 1_700_000_020_000, "completed": 1_700_000_021_000},
	})
	f.messageV2("msg_7", root, "synthetic", 7, 1_700_000_022_000, obj{
		"text": "Continue if you have next steps.", "time": obj{"created": 1_700_000_022_000},
	})
	f.messageV2("msg_8", root, "assistant", 8, 1_700_000_030_000, obj{
		"agent":   "build",
		"model":   obj{"providerID": "github-copilot", "id": "claude-opus-4.8"},
		"content": []obj{},
		"cost":    0,
		"tokens":  obj{"input": 0, "output": 0, "reasoning": 0, "cache": obj{"read": 0, "write": 0}},
		"error":   obj{"type": "aborted", "message": "Aborted"},
		"time":    obj{"created": 1_700_000_030_000, "completed": 1_700_000_031_000},
	})

	f.messageV2("msg_c1", child, "user", 1, 1_700_000_100_000, obj{
		"text": "Find the test", "time": obj{"created": 1_700_000_100_000},
	})
	f.messageV2("msg_c2", child, "assistant", 2, 1_700_000_101_000, obj{
		"agent":   "explore",
		"model":   obj{"providerID": "github-copilot", "id": "claude-haiku-4.5"},
		"content": []obj{{"type": "text", "text": "Found it in x_test.go"}},
		"finish":  "stop",
		"cost":    0.25,
		"tokens":  obj{"input": 10, "output": 5, "reasoning": 0, "cache": obj{"read": 0, "write": 0}},
		"time":    obj{"created": 1_700_000_101_000, "completed": 1_700_000_102_000},
	})
	return root
}

// seedV1Conversation writes a representative legacy session: a user prompt,
// an assistant turn with reasoning, a tool call and two steps, a synthetic
// continuation, a compaction summary, and an aborted turn.
func seedV1Conversation(f *fixture, dir string) string {
	const root = "ses_legacy"
	f.project("prj_1", dir)
	f.sessionV1(root, "prj_1", "", dir, "Legacy chat", 1_600_000_000_000, 1_600_000_900_000)
	f.todo(root, "Ship it", "pending", 0)

	f.messageV1("msg_l1", root, 1_600_000_000_000, obj{
		"role": "user", "time": obj{"created": 1_600_000_000_000},
		"agent": "build", "model": obj{"providerID": "openai", "modelID": "gpt-5"},
	})
	f.partV1("prt_1", "msg_l1", root, 1_600_000_000_000, obj{"type": "text", "text": `"quoted prompt"`})

	f.messageV1("msg_l2", root, 1_600_000_001_000, obj{
		"role": "assistant", "parentID": "msg_l1", "agent": "build",
		"modelID": "gpt-5", "providerID": "openai",
		"cost":   0.5,
		"tokens": obj{"total": 130, "input": 100, "output": 20, "reasoning": 10, "cache": obj{"read": 0, "write": 0}},
		"time":   obj{"created": 1_600_000_001_000, "completed": 1_600_000_009_000},
		"finish": "stop",
	})
	f.partV1("prt_2a", "msg_l2", root, 1_600_000_001_000, obj{"type": "step-start", "snapshot": "abc"})
	f.partV1("prt_2b", "msg_l2", root, 1_600_000_002_000, obj{
		"type": "reasoning", "text": "Thinking about it.", "time": obj{"start": 1_600_000_002_000, "end": 1_600_000_003_000},
	})
	f.partV1("prt_2c", "msg_l2", root, 1_600_000_003_000, obj{
		"type": "tool", "tool": "bash", "callID": "call_ls",
		"state": obj{
			"status": "completed",
			"input":  obj{"command": "ls", "description": "List files"},
			"output": "a.go\nb.go\n",
			"time":   obj{"start": 1_600_000_003_000, "end": 1_600_000_004_000},
		},
	})
	f.partV1("prt_2d", "msg_l2", root, 1_600_000_004_000, obj{
		"type": "step-finish", "reason": "tool-calls",
		"tokens": obj{"total": 60, "input": 50, "output": 5, "reasoning": 5, "cache": obj{"read": 0, "write": 0}},
		"cost":   0.2,
	})
	f.partV1("prt_2e", "msg_l2", root, 1_600_000_005_000, obj{"type": "step-start", "snapshot": "def"})
	f.partV1("prt_2f", "msg_l2", root, 1_600_000_006_000, obj{"type": "text", "text": "Two files."})
	f.partV1("prt_2g", "msg_l2", root, 1_600_000_007_000, obj{
		"type": "step-finish", "reason": "stop",
		"tokens": obj{"total": 70, "input": 50, "output": 15, "reasoning": 5, "cache": obj{"read": 0, "write": 0}},
		"cost":   0.3,
	})

	f.messageV1("msg_l3", root, 1_600_000_010_000, obj{
		"role": "user", "time": obj{"created": 1_600_000_010_000}, "agent": "build",
	})
	f.partV1("prt_3", "msg_l3", root, 1_600_000_010_000, obj{"type": "text", "text": "Continue", "synthetic": true})

	f.messageV1("msg_l4", root, 1_600_000_011_000, obj{
		"role": "assistant", "parentID": "msg_l3", "agent": "compaction", "summary": true,
		"modelID": "gpt-5", "providerID": "openai",
		"cost":   0,
		"tokens": obj{"total": 40, "input": 30, "output": 10, "reasoning": 0, "cache": obj{"read": 0, "write": 0}},
		"time":   obj{"created": 1_600_000_011_000, "completed": 1_600_000_012_000},
		"finish": "stop",
	})
	f.partV1("prt_4", "msg_l4", root, 1_600_000_011_000, obj{"type": "text", "text": "Summary of the chat."})

	f.messageV1("msg_l5", root, 1_600_000_013_000, obj{
		"role": "assistant", "parentID": "msg_l3", "agent": "build",
		"modelID": "gpt-5", "providerID": "openai",
		"cost":   0,
		"tokens": obj{"total": 0, "input": 0, "output": 0, "reasoning": 0, "cache": obj{"read": 0, "write": 0}},
		"time":   obj{"created": 1_600_000_013_000, "completed": 1_600_000_014_000},
		"error":  obj{"name": "MessageAbortedError", "data": obj{"message": "Aborted"}},
	})
	return root
}
