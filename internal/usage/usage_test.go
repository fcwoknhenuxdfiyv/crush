package usage

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

func openTestDB(t *testing.T) (*sql.DB, *db.Queries, session.Service) {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	q := db.New(conn)
	return conn, q, session.NewService(q, conn)
}

func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

func TestRecordPersistsEvent(t *testing.T) {
	t.Parallel()
	conn, q, sessions := openTestDB(t)
	ctx := t.Context()

	sess, err := sessions.Create(ctx, "one")
	require.NoError(t, err)

	at := time.Date(2026, 10, 27, 15, 4, 5, 0, time.UTC)
	svc := &service{q: q, now: fixedClock(at)}
	require.NoError(t, svc.Record(ctx, Event{
		SessionID:           sess.ID,
		Provider:            "copilot",
		Model:               "claude-fable-5.1",
		InputTokens:         100,
		OutputTokens:        20,
		CacheReadTokens:     300,
		CacheCreationTokens: 40,
		Cost:                0.5,
	}))
	require.NoError(t, svc.Record(ctx, Event{
		SessionID: sess.ID,
		Source:    SourceTitle,
		Estimated: true,
	}))

	rows, err := q.ListUsageEventsSince(ctx, 0)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, sess.ID, rows[0].SessionID)
	require.Equal(t, at.Unix(), rows[0].CreatedAt)
	require.Equal(t, int64(100), rows[0].InputTokens)
	require.Equal(t, int64(20), rows[0].OutputTokens)
	require.Equal(t, int64(300), rows[0].CacheReadTokens)
	require.Equal(t, int64(40), rows[0].CacheCreationTokens)
	require.InDelta(t, 0.5, rows[0].Cost, 1e-9)
	require.Equal(t, int64(0), rows[0].Estimated)
	require.Equal(t, int64(1), rows[1].Estimated)

	var sources []string
	all, err := conn.QueryContext(ctx, "SELECT source FROM usage_events ORDER BY id")
	require.NoError(t, err)
	defer all.Close()
	for all.Next() {
		var s string
		require.NoError(t, all.Scan(&s))
		sources = append(sources, s)
	}
	require.NoError(t, all.Err())
	require.Equal(t, []string{string(SourceTurn), string(SourceTitle)}, sources)
}

func TestRecordRequiresSessionID(t *testing.T) {
	t.Parallel()
	_, q, _ := openTestDB(t)
	svc := NewService(q)
	require.Error(t, svc.Record(t.Context(), Event{Cost: 1}))
}

func TestDailyFromDBBucketsEventsByLocalDay(t *testing.T) {
	t.Parallel()
	conn, q, sessions := openTestDB(t)
	ctx := t.Context()

	a, err := sessions.Create(ctx, "a")
	require.NoError(t, err)
	b, err := sessions.Create(ctx, "b")
	require.NoError(t, err)

	// UTC-5: 2026-10-27T03:30Z is still 2026-10-26 locally.
	loc := time.FixedZone("test", -5*60*60)
	lateNight := time.Date(2026, 10, 27, 3, 30, 0, 0, time.UTC)
	morning := time.Date(2026, 10, 27, 14, 0, 0, 0, time.UTC)

	record := func(sessID string, at time.Time, ev Event) {
		t.Helper()
		ev.SessionID = sessID
		svc := &service{q: q, now: fixedClock(at)}
		require.NoError(t, svc.Record(ctx, ev))
	}
	record(a.ID, lateNight, Event{InputTokens: 10, OutputTokens: 1, Cost: 0.10})
	record(a.ID, morning, Event{InputTokens: 20, OutputTokens: 2, CacheReadTokens: 5, Cost: 0.20})
	record(b.ID, morning, Event{InputTokens: 30, OutputTokens: 3, CacheCreationTokens: 7, Cost: 0.30})
	record(b.ID, morning.Add(time.Minute), Event{InputTokens: 1, Estimated: true})

	// Keep the session running totals consistent with the ledger so no
	// remainder is attributed.
	setCost(t, conn, a.ID, 0.30)
	setCost(t, conn, b.ID, 0.30)

	days, hasLedger, err := DailyFromDB(ctx, conn, time.Time{}, loc)
	require.NoError(t, err)
	require.True(t, hasLedger)
	require.Len(t, days, 2)

	require.Equal(t, "2026-10-27", days[0].Date)
	require.Equal(t, int64(2), days[0].Sessions)
	require.Equal(t, int64(3), days[0].Turns)
	require.Equal(t, int64(51), days[0].InputTokens)
	require.Equal(t, int64(5), days[0].OutputTokens)
	require.Equal(t, int64(5), days[0].CacheReadTokens)
	require.Equal(t, int64(7), days[0].CacheCreationTokens)
	require.InDelta(t, 0.50, days[0].Cost, 1e-9)
	require.False(t, days[0].Approximate)

	require.Equal(t, "2026-10-26", days[1].Date)
	require.Equal(t, int64(1), days[1].Sessions)
	require.Equal(t, int64(1), days[1].Turns)
	require.InDelta(t, 0.10, days[1].Cost, 1e-9)
	require.False(t, days[1].Approximate)
}

func TestDailyFromDBAttributesRemainderToCreationDay(t *testing.T) {
	t.Parallel()
	conn, q, sessions := openTestDB(t)
	ctx := t.Context()

	parent, err := sessions.Create(ctx, "parent")
	require.NoError(t, err)
	child, err := sessions.CreateTaskSession(ctx, "call-1", parent.ID, "child")
	require.NoError(t, err)
	legacy, err := sessions.Create(ctx, "legacy")
	require.NoError(t, err)

	createdOn := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	later := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	setCreatedAt(t, conn, parent.ID, createdOn)
	setCreatedAt(t, conn, child.ID, createdOn)
	setCreatedAt(t, conn, legacy.ID, createdOn)

	// Parent accrued 1.0 in total; 0.6 of it is explained by the ledger
	// (0.4 on the parent itself, 0.2 on its child), all on a later day.
	record := func(sessID string, at time.Time, cost float64) {
		t.Helper()
		svc := &service{q: q, now: fixedClock(at)}
		require.NoError(t, svc.Record(ctx, Event{SessionID: sessID, Cost: cost}))
	}
	record(parent.ID, later, 0.4)
	record(child.ID, later, 0.2)
	setCost(t, conn, parent.ID, 1.0)
	setCost(t, conn, child.ID, 0.2)
	// Legacy session predates the ledger entirely.
	setCost(t, conn, legacy.ID, 2.5)

	days, hasLedger, err := DailyFromDB(ctx, conn, time.Time{}, time.UTC)
	require.NoError(t, err)
	require.True(t, hasLedger)
	require.Len(t, days, 2)

	require.Equal(t, "2026-10-25", days[0].Date)
	require.InDelta(t, 0.6, days[0].Cost, 1e-9)
	require.InDelta(t, 0, days[0].ApproximateCost, 1e-9)
	require.False(t, days[0].Approximate)
	require.Equal(t, int64(2), days[0].Sessions)
	require.Equal(t, int64(2), days[0].Turns)

	require.Equal(t, "2026-10-20", days[1].Date)
	require.InDelta(t, 2.9, days[1].Cost, 1e-9)
	require.InDelta(t, 2.9, days[1].ApproximateCost, 1e-9)
	require.True(t, days[1].Approximate)
	require.Equal(t, int64(2), days[1].Sessions)
	require.Equal(t, int64(0), days[1].Turns)
}

func TestDailyFromDBHonorsSince(t *testing.T) {
	t.Parallel()
	conn, q, sessions := openTestDB(t)
	ctx := t.Context()

	old, err := sessions.Create(ctx, "old")
	require.NoError(t, err)
	recent, err := sessions.Create(ctx, "recent")
	require.NoError(t, err)

	oldAt := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	recentAt := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	setCreatedAt(t, conn, old.ID, oldAt)
	setCreatedAt(t, conn, recent.ID, recentAt)

	svc := &service{q: q, now: fixedClock(oldAt)}
	require.NoError(t, svc.Record(ctx, Event{SessionID: old.ID, Cost: 1}))
	svc = &service{q: q, now: fixedClock(recentAt)}
	require.NoError(t, svc.Record(ctx, Event{SessionID: recent.ID, Cost: 2}))
	setCost(t, conn, old.ID, 1)
	setCost(t, conn, recent.ID, 2)

	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	days, _, err := DailyFromDB(ctx, conn, since, time.UTC)
	require.NoError(t, err)
	require.Len(t, days, 1)
	require.Equal(t, "2026-10-27", days[0].Date)
	require.InDelta(t, 2, days[0].Cost, 1e-9)
}

func TestDailyFromDBWithoutLedgerTable(t *testing.T) {
	t.Parallel()
	conn, _, sessions := openTestDB(t)
	ctx := t.Context()

	root, err := sessions.Create(ctx, "root")
	require.NoError(t, err)
	child, err := sessions.CreateTaskSession(ctx, "call-9", root.ID, "child")
	require.NoError(t, err)
	free, err := sessions.Create(ctx, "free")
	require.NoError(t, err)

	createdOn := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	setCreatedAt(t, conn, root.ID, createdOn)
	setCreatedAt(t, conn, child.ID, createdOn)
	setCreatedAt(t, conn, free.ID, createdOn)
	// Root already includes the child's cost via the coordinator roll-up.
	setCost(t, conn, root.ID, 1.5)
	setCost(t, conn, child.ID, 0.5)
	setCost(t, conn, free.ID, 0)

	_, err = conn.ExecContext(ctx, "DROP TABLE usage_events")
	require.NoError(t, err)

	days, hasLedger, err := DailyFromDB(ctx, conn, time.Time{}, time.UTC)
	require.NoError(t, err)
	require.False(t, hasLedger)
	require.Len(t, days, 1)
	require.Equal(t, "2026-10-22", days[0].Date)
	require.InDelta(t, 1.5, days[0].Cost, 1e-9)
	require.InDelta(t, 1.5, days[0].ApproximateCost, 1e-9)
	require.True(t, days[0].Approximate)
	require.Equal(t, int64(1), days[0].Sessions)
}

func TestMergeDaysAndTotal(t *testing.T) {
	t.Parallel()
	a := []Day{
		{Date: "2026-10-27", Sessions: 1, Turns: 2, InputTokens: 10, Cost: 1},
		{Date: "2026-10-26", Sessions: 1, Turns: 1, InputTokens: 5, Cost: 0.5, ApproximateCost: 0.5, Approximate: true},
	}
	b := []Day{
		{Date: "2026-10-27", Sessions: 2, Turns: 3, OutputTokens: 7, Cost: 2},
		{Date: "2026-10-28", Sessions: 1, Turns: 1, Cost: 0.25},
	}

	merged := MergeDays(a, b)
	require.Len(t, merged, 3)
	require.Equal(t, "2026-10-28", merged[0].Date)
	require.Equal(t, "2026-10-27", merged[1].Date)
	require.Equal(t, "2026-10-26", merged[2].Date)

	require.Equal(t, int64(3), merged[1].Sessions)
	require.Equal(t, int64(5), merged[1].Turns)
	require.Equal(t, int64(10), merged[1].InputTokens)
	require.Equal(t, int64(7), merged[1].OutputTokens)
	require.InDelta(t, 3, merged[1].Cost, 1e-9)
	require.False(t, merged[1].Approximate)
	require.True(t, merged[2].Approximate)

	total := Total(merged)
	require.Equal(t, "", total.Date)
	require.Equal(t, int64(5), total.Sessions)
	require.Equal(t, int64(7), total.Turns)
	require.InDelta(t, 3.75, total.Cost, 1e-9)
	require.InDelta(t, 0.5, total.ApproximateCost, 1e-9)
	require.True(t, total.Approximate)
}

func TestIsMissingTable(t *testing.T) {
	t.Parallel()
	require.True(t, isMissingTable(sqlError("SQL logic error: no such table: usage_events (1)")))
	require.False(t, isMissingTable(sqlError("no such table: sessions")))
	require.False(t, isMissingTable(nil))
}

type sqlError string

func (e sqlError) Error() string { return string(e) }

func setCost(t *testing.T, conn *sql.DB, sessionID string, cost float64) {
	t.Helper()
	_, err := conn.ExecContext(context.Background(), "UPDATE sessions SET cost = ? WHERE id = ?", cost, sessionID)
	require.NoError(t, err)
}

func setCreatedAt(t *testing.T, conn *sql.DB, sessionID string, at time.Time) {
	t.Helper()
	_, err := conn.ExecContext(context.Background(), "UPDATE sessions SET created_at = ? WHERE id = ?", at.Unix(), sessionID)
	require.NoError(t, err)
}
