package opencode

import (
	"path/filepath"
	"testing"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/stretchr/testify/require"
)

func TestOpenDetectsGeneration(t *testing.T) {
	t.Parallel()

	t.Run("v2", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, v2Schema)
		src := f.open()
		require.Equal(t, GenerationV2, src.Generation())
		require.Equal(t, "v2", src.Generation().String())
	})

	t.Run("v1", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, v1Schema)
		src := f.open()
		require.Equal(t, GenerationV1, src.Generation())
	})

	t.Run("both present prefers v2", func(t *testing.T) {
		t.Parallel()
		f := newFixture(t, v2Schema)
		f.exec(`CREATE TABLE session (id text PRIMARY KEY, project_id text NOT NULL, slug text NOT NULL, directory text NOT NULL, title text NOT NULL, version text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL)`)
		f.exec(`CREATE TABLE message (id text PRIMARY KEY, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL)`)
		f.exec(`CREATE TABLE part (id text PRIMARY KEY, message_id text NOT NULL, session_id text NOT NULL, time_created integer NOT NULL, time_updated integer NOT NULL, data text NOT NULL)`)
		src := f.open()
		require.Equal(t, GenerationV2, src.Generation())
	})

	t.Run("unrelated database", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "other.db")
		conn, err := db.OpenFile(t.Context(), path)
		require.NoError(t, err)
		_, err = conn.ExecContext(t.Context(), `CREATE TABLE notes (id integer PRIMARY KEY)`)
		require.NoError(t, err)
		require.NoError(t, conn.Close())

		_, err = Open(t.Context(), path)
		require.Error(t, err)
		require.Contains(t, err.Error(), "does not look like an OpenCode database")
		require.Contains(t, err.Error(), "notes")
	})

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		_, err := Open(t.Context(), filepath.Join(t.TempDir(), "missing.db"))
		require.Error(t, err)
	})
}

func TestSourceSessionsAndProjectsV2(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v2Schema)
	dir := t.TempDir()
	root := seedV2Conversation(f, dir)
	src := f.open()

	sessions, err := src.Sessions(t.Context())
	require.NoError(t, err)
	require.Len(t, sessions, 2)
	require.Equal(t, root, sessions[0].ID)
	require.Equal(t, "", sessions[0].ParentID)
	require.Equal(t, "prj_1", sessions[0].ProjectID)
	require.Equal(t, dir, sessions[0].Directory)
	require.Equal(t, "Fix the flaky test", sessions[0].Title)
	require.Equal(t, "slug-ses_root", sessions[0].Slug)
	require.InDelta(t, 1.5, sessions[0].Cost, 1e-9)
	require.Equal(t, int64(1_700_000_000_000), sessions[0].Created)
	require.Equal(t, int64(1_700_000_900_000), sessions[0].Updated)
	require.Equal(t, root, sessions[1].ParentID)

	projects, err := src.Projects(t.Context())
	require.NoError(t, err)
	require.Equal(t, dir, projects["prj_1"].Worktree)

	todos, err := src.Todos(t.Context(), root)
	require.NoError(t, err)
	require.Len(t, todos, 3)
	require.Equal(t, "Reproduce failure", todos[0].Content)
	require.Equal(t, "cancelled", todos[2].Status)

	blob, err := src.Blob(t.Context(), "ab12")
	require.NoError(t, err)
	require.NotNil(t, blob)
	require.Equal(t, "image/png", blob.Mime)
	require.Equal(t, []byte("PNGDATA"), blob.Data)

	missing, err := src.Blob(t.Context(), "nope")
	require.NoError(t, err)
	require.Nil(t, missing)
}

func TestSourceMessagesV2MergesParts(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v2Schema)
	dir := t.TempDir()
	root := seedV2Conversation(f, dir)
	// A newer OpenCode build that spilled content into session_part.
	f.partV2("msg_4", root, 1, "text", obj{"type": "text", "text": "second"})
	f.partV2("msg_4", root, 0, "text", obj{"type": "text", "text": "first"})
	src := f.open()

	msgs, err := src.Messages(t.Context(), root)
	require.NoError(t, err)
	require.Len(t, msgs, 8)
	for i, m := range msgs {
		require.Equal(t, int64(i+1), m.Seq)
	}
	require.Equal(t, "user", msgs[0].Type)
	require.Empty(t, msgs[2].Parts)
	require.Len(t, msgs[3].Parts, 2)
	require.Equal(t, int64(0), msgs[3].Parts[0].Ordinal)
	require.JSONEq(t, `{"type":"text","text":"first"}`, string(msgs[3].Parts[0].Data))

	none, err := src.Messages(t.Context(), "ses_missing")
	require.NoError(t, err)
	require.Empty(t, none)
}

func TestSourceMessagesV1AttachesParts(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v1Schema)
	dir := t.TempDir()
	root := seedV1Conversation(f, dir)
	src := f.open()

	sessions, err := src.Sessions(t.Context())
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, "Legacy chat", sessions[0].Title)
	require.Zero(t, sessions[0].Cost, "legacy schema without a cost column reads as zero")

	msgs, err := src.Messages(t.Context(), root)
	require.NoError(t, err)
	require.Len(t, msgs, 5)
	require.Equal(t, "user", msgs[0].Type)
	require.Equal(t, "assistant", msgs[1].Type)
	require.Len(t, msgs[0].Parts, 1)
	require.Len(t, msgs[1].Parts, 7)
	require.Equal(t, "prt_2a", msgs[1].Parts[0].ID)
	require.Equal(t, "prt_2g", msgs[1].Parts[6].ID)

	todos, err := src.Todos(t.Context(), root)
	require.NoError(t, err)
	require.Len(t, todos, 1)
}

func TestSourceNeverWrites(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v2Schema)
	seedV2Conversation(f, t.TempDir())
	src := f.open()
	_, err := src.conn.ExecContext(t.Context(), `DELETE FROM session_v2`)
	require.Error(t, err, "the source connection must be read-only")
}
