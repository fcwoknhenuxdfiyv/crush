package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/projects"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/usage"
	"github.com/stretchr/testify/require"
)

// newProjectDB creates a migrated crush.db under dataDir and returns the
// open connection so the test can seed it.
func newProjectDB(t *testing.T, dataDir string) (*sql.DB, session.Service) {
	t.Helper()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return conn, session.NewService(db.New(conn), conn)
}

func TestGatherDailyUsageMergesProjects(t *testing.T) {
	ctx := t.Context()
	loc := time.UTC
	day := time.Date(2026, 10, 27, 12, 0, 0, 0, loc)

	// Project A has the ledger table and records calls on two days.
	dirA := t.TempDir()
	connA, sessionsA := newProjectDB(t, dirA)
	sessA, err := sessionsA.Create(ctx, "a")
	require.NoError(t, err)
	seedEvent(t, connA, sessA.ID, day, 100, 10, 1.0)
	seedEvent(t, connA, sessA.ID, day.AddDate(0, 0, -1), 50, 5, 0.5)
	setSessionCost(t, connA, sessA.ID, 1.5)

	// Project B predates the ledger; its spend is approximate and lands
	// on the session's creation day.
	dirB := t.TempDir()
	connB, sessionsB := newProjectDB(t, dirB)
	sessB, err := sessionsB.Create(ctx, "b")
	require.NoError(t, err)
	setSessionCost(t, connB, sessB.ID, 2.0)
	setSessionCreatedAt(t, connB, sessB.ID, day)
	_, err = connB.ExecContext(ctx, "DROP TABLE usage_events")
	require.NoError(t, err)

	// Project C is unreadable.
	dirC := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dirC, "crush.db"), []byte("not a database"), 0o644))

	targets := []statsTarget{
		{dbPath: filepath.Join(dirA, "crush.db"), projectDir: "/proj/a"},
		{dbPath: filepath.Join(dirB, "crush.db"), projectDir: "/proj/b"},
		{dbPath: filepath.Join(dirC, "crush.db"), projectDir: "/proj/c"},
	}

	out, err := gatherPeriodUsage(ctx, dailyPeriod, targets, time.Time{}, loc)
	require.NoError(t, err)

	require.Len(t, out.Projects, 3)
	require.Equal(t, "/proj/a", out.Projects[0].Path)
	require.True(t, out.Projects[0].Ledger)
	require.False(t, out.Projects[0].Skipped)
	require.Equal(t, "/proj/b", out.Projects[1].Path)
	require.False(t, out.Projects[1].Ledger)
	require.False(t, out.Projects[1].Skipped)
	require.Equal(t, "/proj/c", out.Projects[2].Path)
	require.True(t, out.Projects[2].Skipped)
	require.NotEmpty(t, out.Projects[2].Error)

	require.Len(t, out.Periods, 2)
	require.Equal(t, "2026-10-26", out.Periods[0].Date, "days must be oldest first so the latest sits by the prompt")
	require.Equal(t, int64(1), out.Periods[0].Sessions)
	require.InDelta(t, 0.5, out.Periods[0].Cost, 1e-9)
	require.False(t, out.Periods[0].Approximate)

	require.Equal(t, "2026-10-27", out.Periods[1].Date)
	require.Equal(t, int64(2), out.Periods[1].Sessions)
	require.Equal(t, int64(1), out.Periods[1].Turns)
	require.Equal(t, int64(100), out.Periods[1].InputTokens)
	require.Equal(t, int64(10), out.Periods[1].OutputTokens)
	require.InDelta(t, 3.0, out.Periods[1].Cost, 1e-9)
	require.InDelta(t, 2.0, out.Periods[1].ApproximateCost, 1e-9)
	require.True(t, out.Periods[1].Approximate)

	require.InDelta(t, 3.5, out.Total.Cost, 1e-9)
	require.Equal(t, int64(3), out.Total.Sessions)
	require.True(t, out.Total.Approximate)
}

func TestGatherDailyUsageHonorsSince(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	conn, sessions := newProjectDB(t, dir)
	sess, err := sessions.Create(ctx, "s")
	require.NoError(t, err)

	old := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	seedEvent(t, conn, sess.ID, old, 1, 1, 5)
	seedEvent(t, conn, sess.ID, recent, 1, 1, 1)
	setSessionCost(t, conn, sess.ID, 6)
	setSessionCreatedAt(t, conn, sess.ID, old)

	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	out, err := gatherPeriodUsage(ctx, dailyPeriod, []statsTarget{{dbPath: filepath.Join(dir, "crush.db"), projectDir: "/p"}}, since, time.UTC)
	require.NoError(t, err)
	require.Equal(t, "2026-10-01", out.Since)
	require.Len(t, out.Periods, 1)
	require.Equal(t, "2026-10-27", out.Periods[0].Date)
	require.InDelta(t, 1, out.Periods[0].Cost, 1e-9)
	require.False(t, out.Periods[0].Approximate)
}

func TestStatsDailyJSONOutput(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)

	dataDir := filepath.Join(tmp, "proj", ".crush")
	conn, sessions := newProjectDB(t, dataDir)
	sess, err := sessions.Create(t.Context(), "s")
	require.NoError(t, err)
	at := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	seedEvent(t, conn, sess.ID, at, 10, 2, 0.25)
	setSessionCost(t, conn, sess.ID, 0.25)
	require.NoError(t, projects.Register(filepath.Join(tmp, "proj"), dataDir))

	var b bytes.Buffer
	statsDailyCmd.SetOut(&b)
	statsDailyCmd.SetErr(&b)
	statsDailyCmd.SetContext(context.Background())
	require.NoError(t, statsDailyCmd.Flags().Set("json", "true"))
	require.NoError(t, statsDailyCmd.Flags().Set("days", "0"))
	t.Cleanup(func() {
		statsDailyCmd.Flags().Set("json", "false")
		statsDailyCmd.Flags().Set("days", "30")
	})

	require.NoError(t, statsDailyCmd.RunE(statsDailyCmd, nil))

	var out dailyOutput
	require.NoError(t, json.Unmarshal(b.Bytes(), &out))
	require.Len(t, out.Days, 1)
	require.Equal(t, int64(1), out.Days[0].Turns)
	require.Equal(t, int64(10), out.Days[0].InputTokens)
	require.InDelta(t, 0.25, out.Total.Cost, 1e-9)
	require.Len(t, out.Projects, 1)
	require.True(t, out.Projects[0].Ledger)
}

func TestCompactTokens(t *testing.T) {
	t.Parallel()
	require.Equal(t, "0", compactTokens(0))
	require.Equal(t, "999", compactTokens(999))
	require.Equal(t, "1K", compactTokens(1_000))
	require.Equal(t, "1.5K", compactTokens(1_500))
	require.Equal(t, "2M", compactTokens(2_000_000))
	require.Equal(t, "2.3M", compactTokens(2_345_678))
}

func TestPeriodRowFormatting(t *testing.T) {
	t.Parallel()

	exact := usage.Day{Date: "2026-10-27", Sessions: 2, Turns: 3, InputTokens: 1500, Cost: 1.234}
	require.Equal(t, []string{"2026-10-27", "2", "3", "1.5K", "0", "0", "0", "$1.23"}, periodRow(exact.Date, exact))

	approx := usage.Day{Date: "2026-10-26", Sessions: 1, Cost: 0.5, ApproximateCost: 0.5, Approximate: true}
	require.Equal(t, []string{"2026-10-26", "1", "-", "-", "-", "-", "-", "~$0.50"}, periodRow(approx.Date, approx))
}

func TestPeriodHeaders(t *testing.T) {
	t.Parallel()
	require.Equal(t, "Date", dailyPeriod.headers()[0])
	require.Equal(t, "Month", monthlyPeriod.headers()[0])
	require.Equal(t, dailyPeriod.headers()[1:], monthlyPeriod.headers()[1:])
}

func seedEvent(t *testing.T, conn *sql.DB, sessionID string, at time.Time, in, out int64, cost float64) {
	t.Helper()
	_, err := conn.ExecContext(context.Background(), `
		INSERT INTO usage_events (session_id, provider, model, source, input_tokens, output_tokens, cost, created_at)
		VALUES (?, 'test', 'model', 'turn', ?, ?, ?, ?)`,
		sessionID, in, out, cost, at.Unix())
	require.NoError(t, err)
}

func setSessionCost(t *testing.T, conn *sql.DB, sessionID string, cost float64) {
	t.Helper()
	_, err := conn.ExecContext(context.Background(), "UPDATE sessions SET cost = ? WHERE id = ?", cost, sessionID)
	require.NoError(t, err)
}

func setSessionCreatedAt(t *testing.T, conn *sql.DB, sessionID string, at time.Time) {
	t.Helper()
	_, err := conn.ExecContext(context.Background(), "UPDATE sessions SET created_at = ? WHERE id = ?", at.Unix(), sessionID)
	require.NoError(t, err)
}
