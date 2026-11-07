package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/projects"
	"github.com/stretchr/testify/require"
)

// openCodeFixtureSchema is the subset of the OpenCode v2 schema the
// importer reads.
const openCodeFixtureSchema = `
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
`

// writeOpenCodeFixture builds an OpenCode database holding one session in
// projectDir and one in a directory that no longer exists. It returns the
// database path.
func writeOpenCodeFixture(t *testing.T, projectDir, missingDir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	conn, err := db.OpenFile(t.Context(), path)
	require.NoError(t, err)
	defer conn.Close()
	ctx := t.Context()

	_, err = conn.ExecContext(ctx, openCodeFixtureSchema)
	require.NoError(t, err)

	exec := func(query string, args ...any) {
		t.Helper()
		_, err := conn.ExecContext(ctx, query, args...)
		require.NoError(t, err)
	}
	exec(`INSERT INTO project (id, worktree, name, time_created, time_updated) VALUES ('prj_1', ?, 'proj', 1, 1)`, projectDir)
	exec(`INSERT INTO project (id, worktree, name, time_created, time_updated) VALUES ('prj_2', ?, 'gone', 1, 1)`, missingDir)
	exec(`INSERT INTO session_v2 (id, project_id, slug, directory, title, version, cost, time_created, time_updated)
		VALUES ('ses_keep', 'prj_1', 'keep', ?, 'Fix the build', '1.0.0', 0.5, 1700000000000, 1700000010000)`, projectDir)
	exec(`INSERT INTO session_v2 (id, project_id, slug, directory, title, version, cost, time_created, time_updated)
		VALUES ('ses_gone', 'prj_2', 'gone', ?, 'Old work', '1.0.0', 0.1, 1700000000000, 1700000010000)`, missingDir)

	message := func(id, sessionID, typ string, seq, created int64, data map[string]any) {
		t.Helper()
		raw, err := json.Marshal(data)
		require.NoError(t, err)
		exec(`INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, id, sessionID, typ, seq, created, created, string(raw))
	}
	for _, sessionID := range []string{"ses_keep", "ses_gone"} {
		message("msg_"+sessionID+"_1", sessionID, "user", 1, 1700000000000, map[string]any{
			"text": "Please fix the build",
			"time": map[string]any{"created": 1700000000000},
		})
		message("msg_"+sessionID+"_2", sessionID, "assistant", 2, 1700000005000, map[string]any{
			"agent":   "build",
			"model":   map[string]any{"providerID": "anthropic", "id": "claude-sonnet-4"},
			"content": []map[string]any{{"type": "text", "text": "Fixed."}},
			"finish":  "stop",
			"cost":    0.5,
			"tokens":  map[string]any{"input": 10, "output": 5, "reasoning": 0, "cache": map[string]any{"read": 0, "write": 0}},
			"time":    map[string]any{"created": 1700000005000, "completed": 1700000006000},
		})
	}
	return path
}

func setupImportOpenCodeEnv(t *testing.T) (projectDir, sourcePath string) {
	t.Helper()
	t.Setenv("CRUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("CRUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("CRUSH_DISABLE_PROVIDER_AUTO_UPDATE", "1")

	projectDir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	gitInit := exec.CommandContext(t.Context(), "git", "init", "-q")
	gitInit.Dir = projectDir
	require.NoError(t, gitInit.Run())
	t.Chdir(projectDir)

	missingDir := filepath.Join(projectDir, "vanished")
	return projectDir, writeOpenCodeFixture(t, projectDir, missingDir)
}

// runImportOpenCodeCmd runs the command with the given flags and returns
// the decoded JSON report.
func runImportOpenCodeCmd(t *testing.T, flags map[string]string) (importOpenCodeOutput, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	importOpenCodeCmd.SetOut(&out)
	importOpenCodeCmd.SetErr(&errOut)
	importOpenCodeCmd.SetContext(t.Context())
	require.NoError(t, importOpenCodeCmd.Flags().Set("json", "true"))
	for name, value := range flags {
		require.NoError(t, importOpenCodeCmd.Flags().Set(name, value))
	}
	t.Cleanup(func() {
		importOpenCodeCmd.Flags().Set("json", "false")
		importOpenCodeCmd.Flags().Set("source", "")
		importOpenCodeCmd.Flags().Set("dry-run", "false")
		importOpenCodeCmd.Flags().Set("current", "false")
		importOpenCodeCmd.Flags().Set("no-usage", "false")
		importOpenCodeCmd.Flags().Set("project", "")
	})

	runErr := importOpenCodeCmd.RunE(importOpenCodeCmd, nil)
	require.NoError(t, runErr, errOut.String())

	var report importOpenCodeOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &report), out.String())
	return report, errOut.String()
}

func TestImportOpenCodeDryRunWritesNothing(t *testing.T) {
	projectDir, source := setupImportOpenCodeEnv(t)

	report, _ := runImportOpenCodeCmd(t, map[string]string{"source": source, "dry-run": "true"})

	require.True(t, report.DryRun)
	require.Equal(t, "v2", report.Generation)
	require.Len(t, report.Projects, 1)
	require.Equal(t, projectDir, report.Projects[0].Dir)
	require.Equal(t, filepath.Join(projectDir, ".crush"), report.Projects[0].DataDir)
	require.Equal(t, 1, report.Projects[0].Imported)
	require.Equal(t, 2, report.Projects[0].Messages)
	require.Equal(t, 1, report.Projects[0].Events)
	require.Len(t, report.Skipped, 1)
	require.Equal(t, filepath.Join(projectDir, "vanished"), report.Skipped[0].Dir)
	require.Equal(t, "directory does not exist", report.Skipped[0].Reason)
	require.Equal(t, 1, report.Total.Skipped)
	require.Equal(t, 1, report.Total.Imported)

	require.NoDirExists(t, filepath.Join(projectDir, ".crush"))
	list, err := projects.Load()
	require.NoError(t, err)
	require.Empty(t, list.Projects)
}

func TestImportOpenCodeImportsAndIsIdempotent(t *testing.T) {
	projectDir, source := setupImportOpenCodeEnv(t)

	report, _ := runImportOpenCodeCmd(t, map[string]string{"source": source})
	require.False(t, report.DryRun)
	require.Len(t, report.Projects, 1)
	require.Equal(t, 1, report.Projects[0].Imported)
	require.Equal(t, 0, report.Projects[0].AlreadyPresent)
	require.Equal(t, 0, report.Projects[0].Failed)
	require.Empty(t, report.Errors)
	require.FileExists(t, filepath.Join(projectDir, ".crush", "crush.db"))

	list, err := projects.Load()
	require.NoError(t, err)
	require.Len(t, list.Projects, 1)
	require.Equal(t, projectDir, list.Projects[0].Path)
	require.Equal(t, filepath.Join(projectDir, ".crush"), list.Projects[0].DataDir)

	conn, err := db.Connect(t.Context(), filepath.Join(projectDir, ".crush"))
	require.NoError(t, err)
	q := db.New(conn)
	sess, err := q.GetSessionByID(t.Context(), "ses_keep")
	require.NoError(t, err)
	require.Equal(t, "Fix the build", sess.Title)
	require.Equal(t, int64(2), sess.MessageCount)
	require.InDelta(t, 0.5, sess.Cost, 1e-9)
	msgs, err := q.ListMessagesBySession(t.Context(), "ses_keep")
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.NoError(t, db.Release(filepath.Join(projectDir, ".crush")))

	again, _ := runImportOpenCodeCmd(t, map[string]string{"source": source})
	require.Len(t, again.Projects, 1)
	require.Equal(t, 0, again.Projects[0].Imported)
	require.Equal(t, 1, again.Projects[0].AlreadyPresent)
	require.Equal(t, 1, again.Total.AlreadyPresent)
}

func TestImportOpenCodeCurrentFilter(t *testing.T) {
	projectDir, source := setupImportOpenCodeEnv(t)

	elsewhere := t.TempDir()
	t.Chdir(elsewhere)
	report, _ := runImportOpenCodeCmd(t, map[string]string{"source": source, "dry-run": "true", "current": "true"})
	require.Empty(t, report.Projects)
	require.Empty(t, report.Skipped, "filtered-out directories are not reported as skipped")

	t.Chdir(projectDir)
	report, _ = runImportOpenCodeCmd(t, map[string]string{"source": source, "dry-run": "true", "current": "true"})
	require.Len(t, report.Projects, 1)
	require.Equal(t, projectDir, report.Projects[0].Dir)
}

func TestImportOpenCodeNoUsage(t *testing.T) {
	_, source := setupImportOpenCodeEnv(t)

	report, _ := runImportOpenCodeCmd(t, map[string]string{"source": source, "dry-run": "true", "no-usage": "true"})
	require.Len(t, report.Projects, 1)
	require.Equal(t, 0, report.Projects[0].Events)
	require.Equal(t, 0, report.Total.Events)
}

func TestImportOpenCodeRejectsUnrelatedDatabase(t *testing.T) {
	setupImportOpenCodeEnv(t)

	path := filepath.Join(t.TempDir(), "other.db")
	conn, err := db.OpenFile(t.Context(), path)
	require.NoError(t, err)
	_, err = conn.ExecContext(t.Context(), `CREATE TABLE notes (id integer primary key)`)
	require.NoError(t, err)
	require.NoError(t, conn.Close())

	importOpenCodeCmd.SetContext(t.Context())
	importOpenCodeCmd.SetOut(&bytes.Buffer{})
	importOpenCodeCmd.SetErr(&bytes.Buffer{})
	require.NoError(t, importOpenCodeCmd.Flags().Set("source", path))
	t.Cleanup(func() { importOpenCodeCmd.Flags().Set("source", "") })

	err = importOpenCodeCmd.RunE(importOpenCodeCmd, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not look like an OpenCode database")
}

func TestResolveOpenCodeSource(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "opencode.db")

	_, err := resolveOpenCodeSource(dir)
	require.Error(t, err, "directory without a database is rejected")

	require.NoError(t, os.WriteFile(dbPath, nil, 0o644))
	got, err := resolveOpenCodeSource(dir)
	require.NoError(t, err)
	require.Equal(t, dbPath, got)

	got, err = resolveOpenCodeSource(dbPath)
	require.NoError(t, err)
	require.Equal(t, dbPath, got)

	_, err = resolveOpenCodeSource(filepath.Join(dir, "missing.db"))
	require.Error(t, err)

	t.Setenv("XDG_DATA_HOME", dir)
	_, err = resolveOpenCodeSource("")
	require.Error(t, err)
	require.Contains(t, err.Error(), "pass --source")

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "opencode"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "opencode", "opencode.db"), nil, 0o644))
	got, err = resolveOpenCodeSource("")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "opencode", "opencode.db"), got)
}

func TestShortenHome(t *testing.T) {
	t.Parallel()
	home := filepath.Join(string(filepath.Separator), "home", "me")
	require.Equal(t, "~", shortenHome(home, home))
	require.Equal(t, filepath.Join("~", "src", "app"), shortenHome(filepath.Join(home, "src", "app"), home))
	require.Equal(t, filepath.Join(string(filepath.Separator), "home", "meow"), shortenHome(filepath.Join(string(filepath.Separator), "home", "meow"), home))
	require.Equal(t, "/tmp/x", shortenHome("/tmp/x", ""))
}
