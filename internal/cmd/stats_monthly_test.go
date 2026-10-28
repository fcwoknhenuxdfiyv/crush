package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/projects"
	"github.com/stretchr/testify/require"
)

func TestGatherMonthlyUsageMergesProjects(t *testing.T) {
	ctx := t.Context()
	loc := time.UTC
	october := time.Date(2026, 10, 27, 12, 0, 0, 0, loc)
	september := time.Date(2026, 9, 3, 9, 0, 0, 0, loc)

	// Project A records calls in two months; one session spans several
	// October days and must count once for the month.
	dirA := t.TempDir()
	connA, sessionsA := newProjectDB(t, dirA)
	sessA, err := sessionsA.Create(ctx, "a")
	require.NoError(t, err)
	seedEvent(t, connA, sessA.ID, october, 100, 10, 1.0)
	seedEvent(t, connA, sessA.ID, october.AddDate(0, 0, -5), 50, 5, 0.5)
	seedEvent(t, connA, sessA.ID, september, 20, 2, 0.2)
	setSessionCost(t, connA, sessA.ID, 1.7)

	// Project B predates the ledger; its spend is approximate and lands
	// on the session's creation month.
	dirB := t.TempDir()
	connB, sessionsB := newProjectDB(t, dirB)
	sessB, err := sessionsB.Create(ctx, "b")
	require.NoError(t, err)
	setSessionCost(t, connB, sessB.ID, 2.0)
	setSessionCreatedAt(t, connB, sessB.ID, october)
	_, err = connB.ExecContext(ctx, "DROP TABLE usage_events")
	require.NoError(t, err)

	targets := []statsTarget{
		{dbPath: filepath.Join(dirA, "crush.db"), projectDir: "/proj/a"},
		{dbPath: filepath.Join(dirB, "crush.db"), projectDir: "/proj/b"},
	}

	out, err := gatherPeriodUsage(ctx, monthlyPeriod, targets, time.Time{}, loc)
	require.NoError(t, err)

	require.Len(t, out.Projects, 2)
	require.True(t, out.Projects[0].Ledger)
	require.False(t, out.Projects[1].Ledger)

	require.Len(t, out.Periods, 2)
	require.Equal(t, "2026-09", out.Periods[0].Date, "months must be oldest first so the latest sits by the prompt")
	require.Equal(t, int64(1), out.Periods[0].Sessions)
	require.Equal(t, int64(1), out.Periods[0].Turns)
	require.InDelta(t, 0.2, out.Periods[0].Cost, 1e-9)
	require.False(t, out.Periods[0].Approximate)

	require.Equal(t, "2026-10", out.Periods[1].Date)
	require.Equal(t, int64(2), out.Periods[1].Sessions)
	require.Equal(t, int64(2), out.Periods[1].Turns)
	require.Equal(t, int64(150), out.Periods[1].InputTokens)
	require.Equal(t, int64(15), out.Periods[1].OutputTokens)
	require.InDelta(t, 3.5, out.Periods[1].Cost, 1e-9)
	require.InDelta(t, 2.0, out.Periods[1].ApproximateCost, 1e-9)
	require.True(t, out.Periods[1].Approximate)

	require.InDelta(t, 3.7, out.Total.Cost, 1e-9)
	require.Equal(t, int64(3), out.Total.Sessions)
	require.True(t, out.Total.Approximate)
}

func TestGatherMonthlyUsageHonorsSince(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	conn, sessions := newProjectDB(t, dir)
	sess, err := sessions.Create(ctx, "s")
	require.NoError(t, err)

	old := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	seedEvent(t, conn, sess.ID, old, 1, 1, 5)
	seedEvent(t, conn, sess.ID, recent, 1, 1, 1)
	setSessionCost(t, conn, sess.ID, 6)
	setSessionCreatedAt(t, conn, sess.ID, old)

	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	out, err := gatherPeriodUsage(ctx, monthlyPeriod, []statsTarget{{dbPath: filepath.Join(dir, "crush.db"), projectDir: "/p"}}, since, time.UTC)
	require.NoError(t, err)
	require.Equal(t, "2026-09", out.Since)
	require.Len(t, out.Periods, 1)
	require.Equal(t, "2026-10", out.Periods[0].Date)
	require.InDelta(t, 1, out.Periods[0].Cost, 1e-9)
	require.False(t, out.Periods[0].Approximate)
}

func TestMonthlySince(t *testing.T) {
	t.Parallel()
	loc := time.FixedZone("test", -5*60*60)
	now := time.Date(2026, 10, 28, 15, 4, 5, 0, loc)

	require.Equal(t, time.Date(2026, 10, 1, 0, 0, 0, 0, loc), monthlySince(now, 1))
	require.Equal(t, time.Date(2026, 8, 1, 0, 0, 0, 0, loc), monthlySince(now, 3))
	require.Equal(t, time.Date(2025, 11, 1, 0, 0, 0, 0, loc), monthlySince(now, 12))

	// Month arithmetic must not spill over on short months: from the 31st
	// the window still starts on the first.
	endOfMarch := time.Date(2026, 3, 31, 23, 0, 0, 0, loc)
	require.Equal(t, time.Date(2026, 2, 1, 0, 0, 0, 0, loc), monthlySince(endOfMarch, 2))
}

func TestStatsMonthlyJSONOutput(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmp)

	dataDir := filepath.Join(tmp, "proj", ".crush")
	conn, sessions := newProjectDB(t, dataDir)
	sess, err := sessions.Create(t.Context(), "s")
	require.NoError(t, err)
	seedEvent(t, conn, sess.ID, time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC), 10, 2, 0.25)
	seedEvent(t, conn, sess.ID, time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC), 30, 4, 0.75)
	setSessionCost(t, conn, sess.ID, 1.0)
	require.NoError(t, projects.Register(filepath.Join(tmp, "proj"), dataDir))

	var b bytes.Buffer
	statsMonthlyCmd.SetOut(&b)
	statsMonthlyCmd.SetErr(&b)
	statsMonthlyCmd.SetContext(context.Background())
	require.NoError(t, statsMonthlyCmd.Flags().Set("json", "true"))
	require.NoError(t, statsMonthlyCmd.Flags().Set("months", "0"))
	t.Cleanup(func() {
		statsMonthlyCmd.Flags().Set("json", "false")
		statsMonthlyCmd.Flags().Set("months", "12")
	})

	require.NoError(t, statsMonthlyCmd.RunE(statsMonthlyCmd, nil))

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b.Bytes(), &raw))
	require.Contains(t, raw, "months")
	require.NotContains(t, raw, "days")

	var out monthlyOutput
	require.NoError(t, json.Unmarshal(b.Bytes(), &out))
	require.Empty(t, out.Since)
	require.Len(t, out.Months, 2)
	require.Equal(t, "2026-09", out.Months[0].Date)
	require.Equal(t, "2026-10", out.Months[1].Date)
	require.Equal(t, int64(1), out.Months[1].Turns)
	require.Equal(t, int64(10), out.Months[1].InputTokens)
	require.InDelta(t, 1.0, out.Total.Cost, 1e-9)
	require.Equal(t, int64(2), out.Total.Sessions, "a session active in two months counts once per month")
	require.Len(t, out.Projects, 1)
	require.True(t, out.Projects[0].Ledger)
}

func TestStatsMonthlyRejectsNegativeMonths(t *testing.T) {
	var b bytes.Buffer
	statsMonthlyCmd.SetOut(&b)
	statsMonthlyCmd.SetErr(&b)
	statsMonthlyCmd.SetContext(context.Background())
	require.NoError(t, statsMonthlyCmd.Flags().Set("months", "-1"))
	t.Cleanup(func() { statsMonthlyCmd.Flags().Set("months", "12") })

	require.ErrorContains(t, statsMonthlyCmd.RunE(statsMonthlyCmd, nil), "--months must be zero or positive")
}
