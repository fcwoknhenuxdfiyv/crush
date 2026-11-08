package opencode

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/charmbracelet/crush/internal/db"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/usage"
	"github.com/stretchr/testify/require"
)

func openCrushDB(t *testing.T, dataDir string) *sql.DB {
	t.Helper()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { db.Release(dataDir) })
	return conn
}

func TestNewPlanGroupsByDirectoryAndSkipsMissing(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v2Schema)
	existing := t.TempDir()
	other := t.TempDir()
	gone := filepath.Join(t.TempDir(), "gone")
	seedV2Conversation(f, existing)
	f.project("prj_2", other)
	f.sessionV2("ses_other", "prj_2", "", other, "Other", 0, 1_000, 2_000)
	f.project("prj_3", gone)
	f.sessionV2("ses_gone", "prj_3", "", gone, "Gone", 0, 1_000, 2_000)
	f.sessionV2("ses_gone_child", "prj_3", "ses_gone", gone, "Gone child", 0, 1_500, 2_000)
	f.sessionV2("ses_nodir", "prj_3", "", "", "No dir", 0, 1_000, 2_000)
	f.exec(`UPDATE project SET worktree = '' WHERE id = 'prj_3'`)
	f.sessionV2("ses_orphan", "prj_2", "ses_missing_parent", other, "Orphan", 0, 1_000, 2_000)
	// A child whose own directory differs from the parent's still files
	// under the parent.
	f.sessionV2("ses_child2", "prj_1", "ses_root", other, "Elsewhere", 0, 1_700_000_300_000, 1_700_000_400_000)
	src := f.open()

	plan, err := NewPlan(t.Context(), src, PlanOptions{GroupBy: GroupByDirectory})
	require.NoError(t, err)
	require.Equal(t, GenerationV2, plan.Generation)
	require.Equal(t, f.path, plan.Source)
	require.Equal(t, GroupByDirectory, plan.GroupBy)
	require.Len(t, plan.Projects, 2)
	require.Equal(t, 5, plan.Sessions())

	byDir := map[string]*ProjectPlan{}
	for _, p := range plan.Projects {
		byDir[p.Dir] = p
	}
	main := byDir[existing]
	require.NotNil(t, main)
	require.Equal(t, filepath.Join(existing, ".crush"), main.DataDir)
	require.Len(t, main.Roots, 1)
	require.Equal(t, "ses_root", main.Roots[0].ID)
	require.Len(t, main.Children["ses_root"], 2)
	require.Equal(t, "ses_child", main.Children["ses_root"][0].ID)
	require.Equal(t, "ses_child2", main.Children["ses_root"][1].ID)
	require.Equal(t, 3, main.Total)
	require.Nil(t, main.Folded, "directory grouping never folds")

	second := byDir[other]
	require.NotNil(t, second)
	require.Len(t, second.Roots, 2, "orphans become roots")
	require.Equal(t, 2, second.Total)

	require.Len(t, plan.Skipped, 2)
	reasons := map[string]SkippedGroup{}
	for _, s := range plan.Skipped {
		reasons[s.Dir] = s
	}
	require.Equal(t, 2, reasons[gone].Sessions)
	require.Equal(t, "directory does not exist", reasons[gone].Reason)
	require.Equal(t, 1, reasons["(none)"].Sessions)
	require.Equal(t, "no project directory recorded", reasons["(none)"].Reason)
	require.Len(t, plan.Warnings, 1)
	require.Contains(t, plan.Warnings[0], "1 sessions reference a parent")
}

// sharedProjectFixture builds a project used from three directories (its
// worktree, a second checkout and one that has since been deleted) plus a
// couple of sessions under OpenCode's catch-all "global" project. It
// returns the fixture and the directories involved.
func sharedProjectFixture(t *testing.T) (f *fixture, worktree, other, gone, global, globalGone string) {
	t.Helper()
	f = newFixture(t, v2Schema)
	worktree = t.TempDir()
	other = t.TempDir()
	gone = filepath.Join(t.TempDir(), "gone")
	global = t.TempDir()
	globalGone = filepath.Join(t.TempDir(), "gone-too")

	f.project("prj_shared", worktree)
	f.sessionV2("ses_wt", "prj_shared", "", worktree, "In worktree", 0, 3_000, 3_000)
	f.sessionV2("ses_other", "prj_shared", "", other, "In other checkout", 0, 1_000, 9_000)
	f.sessionV2("ses_other_child", "prj_shared", "ses_other", other, "Sub-agent", 0, 1_500, 9_000)
	f.sessionV2("ses_gone", "prj_shared", "", gone, "In deleted checkout", 0, 2_000, 2_000)

	f.project("global", "/")
	f.sessionV2("ses_global", "global", "", global, "Scratch", 0, 1_000, 1_000)
	f.sessionV2("ses_global_gone", "global", "", globalGone, "Old scratch", 0, 1_000, 1_000)
	return f, worktree, other, gone, global, globalGone
}

func TestNewPlanGroupsByProject(t *testing.T) {
	t.Parallel()
	f, worktree, other, gone, global, globalGone := sharedProjectFixture(t)
	src := f.open()

	plan, err := NewPlan(t.Context(), src, PlanOptions{})
	require.NoError(t, err)
	require.Equal(t, GroupByProject, plan.GroupBy, "project grouping is the default")
	require.Len(t, plan.Projects, 2)
	require.Equal(t, 5, plan.Sessions())

	byDir := map[string]*ProjectPlan{}
	for _, p := range plan.Projects {
		byDir[p.Dir] = p
	}

	// Every session of the shared project lands in its worktree, even the
	// ones recorded in a directory that no longer exists.
	shared := byDir[worktree]
	require.NotNil(t, shared)
	require.Equal(t, filepath.Join(worktree, ".crush"), shared.DataDir)
	require.Equal(t, 4, shared.Total)
	require.Len(t, shared.Roots, 3)
	require.Equal(t, "ses_other", shared.Roots[0].ID)
	require.Equal(t, "ses_gone", shared.Roots[1].ID)
	require.Equal(t, "ses_wt", shared.Roots[2].ID)
	require.Len(t, shared.Children["ses_other"], 1)
	want := []FoldedDirectory{
		{Dir: other, Sessions: 2, Exists: true},
		{Dir: gone, Sessions: 1, Exists: false},
	}
	sort.Slice(want, func(i, j int) bool { return want[i].Dir < want[j].Dir })
	require.Equal(t, want, shared.Folded)

	// The global project is not a real project, so its sessions stay
	// grouped by directory.
	scratch := byDir[global]
	require.NotNil(t, scratch)
	require.Equal(t, 1, scratch.Total)
	require.Nil(t, scratch.Folded)
	require.Equal(t, []SkippedGroup{{Dir: globalGone, Sessions: 1, Reason: "directory does not exist"}}, plan.Skipped)
}

func TestNewPlanGroupByDirectory(t *testing.T) {
	t.Parallel()
	f, worktree, other, gone, global, globalGone := sharedProjectFixture(t)
	src := f.open()

	plan, err := NewPlan(t.Context(), src, PlanOptions{GroupBy: GroupByDirectory})
	require.NoError(t, err)
	require.Equal(t, GroupByDirectory, plan.GroupBy)
	require.Len(t, plan.Projects, 3)
	require.Equal(t, 4, plan.Sessions())

	totals := map[string]int{}
	for _, p := range plan.Projects {
		totals[p.Dir] = p.Total
		require.Nil(t, p.Folded)
	}
	require.Equal(t, map[string]int{worktree: 1, other: 2, global: 1}, totals)

	skipped := map[string]SkippedGroup{}
	for _, s := range plan.Skipped {
		skipped[s.Dir] = s
	}
	require.Len(t, skipped, 2)
	require.Equal(t, SkippedGroup{Dir: gone, Sessions: 1, Reason: "directory does not exist"}, skipped[gone])
	require.Equal(t, SkippedGroup{Dir: globalGone, Sessions: 1, Reason: "directory does not exist"}, skipped[globalGone])
}

func TestNewPlanProjectWorktreeFallback(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v2Schema)
	older := t.TempDir()
	newer := t.TempDir()
	gone := filepath.Join(t.TempDir(), "gone")
	f.project("prj_moved", filepath.Join(t.TempDir(), "old-worktree"))
	f.sessionV2("ses_older", "prj_moved", "", older, "Older", 0, 1_000, 1_000)
	f.sessionV2("ses_newer", "prj_moved", "", newer, "Newer", 0, 1_000, 2_000)
	// Activity in a deleted directory never makes it the destination,
	// however recent.
	f.sessionV2("ses_gone", "prj_moved", "", gone, "Gone", 0, 1_000, 5_000)
	src := f.open()

	plan, err := NewPlan(t.Context(), src, PlanOptions{})
	require.NoError(t, err)
	require.Len(t, plan.Projects, 1)
	require.Empty(t, plan.Skipped)
	proj := plan.Projects[0]
	require.Equal(t, newer, proj.Dir, "falls back to the most recently used directory that still exists")
	require.Equal(t, 3, proj.Total)
	want := []FoldedDirectory{
		{Dir: older, Sessions: 1, Exists: true},
		{Dir: gone, Sessions: 1, Exists: false},
	}
	sort.Slice(want, func(i, j int) bool { return want[i].Dir < want[j].Dir })
	require.Equal(t, want, proj.Folded)
}

func TestNewPlanProjectWithNoSurvivingDirectory(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v2Schema)
	base := t.TempDir()
	goneA := filepath.Join(base, "a")
	goneB := filepath.Join(base, "b")
	f.project("prj_lost", filepath.Join(base, "worktree"))
	f.sessionV2("ses_a", "prj_lost", "", goneA, "A", 0, 1_000, 1_000)
	f.sessionV2("ses_a_child", "prj_lost", "ses_a", goneA, "A child", 0, 1_500, 1_500)
	f.sessionV2("ses_b", "prj_lost", "", goneB, "B", 0, 1_000, 1_000)
	f.sessionV2("ses_nodir", "prj_lost", "", "", "No dir", 0, 1_000, 1_000)
	f.exec(`UPDATE project SET worktree = '' WHERE id = 'prj_lost'`)
	src := f.open()

	plan, err := NewPlan(t.Context(), src, PlanOptions{})
	require.NoError(t, err)
	require.Empty(t, plan.Projects)
	require.Equal(t, []SkippedGroup{
		{Dir: "(none)", Sessions: 1, Reason: "no project directory recorded"},
		{Dir: goneA, Sessions: 2, Reason: "directory does not exist"},
		{Dir: goneB, Sessions: 1, Reason: "directory does not exist"},
	}, plan.Skipped)
}

func TestNewPlanFiltersMatchFoldedDirectories(t *testing.T) {
	t.Parallel()
	f, worktree, other, gone, _, _ := sharedProjectFixture(t)
	src := f.open()

	// Selecting a secondary checkout pulls in the whole project, filed
	// under its worktree.
	for _, filter := range []string{other, gone} {
		plan, err := NewPlan(t.Context(), src, PlanOptions{Projects: []string{filter}})
		require.NoError(t, err)
		require.Len(t, plan.Projects, 1, filter)
		require.Equal(t, worktree, plan.Projects[0].Dir)
		require.Equal(t, 4, plan.Projects[0].Total)
		require.Empty(t, plan.Skipped)
	}

	plan, err := NewPlan(t.Context(), src, PlanOptions{Projects: []string{t.TempDir()}})
	require.NoError(t, err)
	require.Empty(t, plan.Projects)
	require.Empty(t, plan.Skipped)

	// Directory grouping keeps the filter literal.
	plan, err = NewPlan(t.Context(), src, PlanOptions{GroupBy: GroupByDirectory, Projects: []string{other}})
	require.NoError(t, err)
	require.Len(t, plan.Projects, 1)
	require.Equal(t, other, plan.Projects[0].Dir)
	require.Equal(t, 2, plan.Projects[0].Total)
}

func TestParseGrouping(t *testing.T) {
	t.Parallel()
	for input, want := range map[string]Grouping{
		"":          GroupByProject,
		"project":   GroupByProject,
		"directory": GroupByDirectory,
	} {
		got, err := ParseGrouping(input)
		require.NoError(t, err, input)
		require.Equal(t, want, got, input)
	}
	_, err := ParseGrouping("bogus")
	require.EqualError(t, err, `unknown grouping "bogus" (want "project" or "directory")`)
}

func TestNewPlanFiltersAndOverrides(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v2Schema)
	a := t.TempDir()
	b := t.TempDir()
	f.project("prj_a", a)
	f.project("prj_b", b)
	f.sessionV2("ses_a", "prj_a", "", a, "A", 0, 1_000, 2_000)
	f.sessionV2("ses_b", "prj_b", "", b, "B", 0, 1_000, 2_000)
	src := f.open()

	plan, err := NewPlan(t.Context(), src, PlanOptions{Projects: []string{a}})
	require.NoError(t, err)
	require.Len(t, plan.Projects, 1)
	require.Equal(t, a, plan.Projects[0].Dir)
	require.Empty(t, plan.Skipped, "filtered-out projects are not reported as skipped")

	plan, err = NewPlan(t.Context(), src, PlanOptions{DataDir: "/tmp/all"})
	require.NoError(t, err)
	require.Len(t, plan.Projects, 2)
	for _, p := range plan.Projects {
		require.Equal(t, "/tmp/all", p.DataDir)
	}

	plan, err = NewPlan(t.Context(), src, PlanOptions{
		ResolveDataDir: func(_ context.Context, dir string) (string, error) {
			return filepath.Join(dir, "custom"), nil
		},
	})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(a, "custom"), plan.Projects[0].DataDir)

	// Worktree fallback when the session has no directory of its own.
	f.sessionV2("ses_c", "prj_a", "", "", "C", 0, 1_000, 2_000)
	plan, err = NewPlan(t.Context(), src, PlanOptions{Projects: []string{a}})
	require.NoError(t, err)
	require.Len(t, plan.Projects, 1)
	require.Equal(t, 2, plan.Projects[0].Total)
}

func TestImportProjectRoundTrip(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v2Schema)
	dir := t.TempDir()
	root := seedV2Conversation(f, dir)
	src := f.open()

	plan, err := NewPlan(t.Context(), src, PlanOptions{})
	require.NoError(t, err)
	require.Len(t, plan.Projects, 1)
	proj := plan.Projects[0]

	conn := openCrushDB(t, proj.DataDir)
	im := NewImporter(src, ImportOptions{Usage: true})
	res, err := im.ImportProject(t.Context(), proj, conn)
	require.NoError(t, err)
	require.Equal(t, 2, res.Sessions)
	require.Equal(t, 2, res.Imported)
	require.Zero(t, res.AlreadyPresent)
	require.Zero(t, res.Failed)
	require.Equal(t, 9, res.Messages)
	require.Equal(t, 3, res.Events)
	require.Empty(t, res.Warnings)
	require.Equal(t, 1, res.Dropped["idle"])

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	messages := message.NewService(q)

	// The root keeps its OpenCode ID and preserved timestamps; its cost
	// includes the child's.
	got, err := sessions.Get(t.Context(), root)
	require.NoError(t, err)
	require.Equal(t, "Fix the flaky test", got.Title)
	require.Equal(t, int64(7), got.MessageCount)
	require.Equal(t, int64(1_700_000_000), got.CreatedAt)
	require.Equal(t, int64(1_700_000_900), got.UpdatedAt)
	require.InDelta(t, 1.75, got.Cost, 1e-9)
	require.Equal(t, int64(1520), got.PromptTokens)
	require.Equal(t, int64(30), got.CompletionTokens)
	require.Equal(t, "msg_6", got.SummaryMessageID)
	require.Len(t, got.Todos, 2)
	require.Equal(t, "", got.ParentSessionID)

	// The sub-agent session is nested under the task call.
	child, err := sessions.Get(t.Context(), "msg_3$$call_task")
	require.NoError(t, err)
	require.Equal(t, root, child.ParentSessionID)
	require.Equal(t, int64(2), child.MessageCount)
	require.InDelta(t, 0.25, child.Cost, 1e-9)

	roots, err := sessions.List(t.Context())
	require.NoError(t, err)
	require.Len(t, roots, 1)
	require.Equal(t, root, roots[0].ID)

	msgs, err := messages.List(t.Context(), root)
	require.NoError(t, err)
	require.Len(t, msgs, 7)
	require.Equal(t, message.User, msgs[0].Role)
	require.Equal(t, "Please fix the flaky test", msgs[0].Content().Text)
	require.Len(t, msgs[0].BinaryContent(), 1)
	require.Equal(t, message.Assistant, msgs[1].Role)
	require.Len(t, msgs[1].ToolCalls(), 3)
	require.Equal(t, message.Tool, msgs[2].Role)
	require.Len(t, msgs[2].ToolResults(), 3)
	require.Equal(t, message.FinishReasonToolUse, msgs[1].FinishReason())
	require.True(t, msgs[4].IsSummaryMessage)
	require.True(t, msgs[5].Content().Hidden)
	require.Equal(t, message.FinishReasonCanceled, msgs[6].FinishReason())
	for i := 1; i < len(msgs); i++ {
		require.Greater(t, msgs[i].CreatedAt, msgs[i-1].CreatedAt)
	}

	// Spend lands on the days the calls happened, with exact figures.
	days, hasLedger, err := usage.DailyFromDB(t.Context(), conn, time.Time{}, time.UTC)
	require.NoError(t, err)
	require.True(t, hasLedger)
	require.Len(t, days, 1)
	require.Equal(t, time.Unix(1_700_000_007, 0).UTC().Format(usage.DateLayout), days[0].Date)
	require.Equal(t, int64(3), days[0].Turns)
	require.InDelta(t, 1.75, days[0].Cost, 1e-9)
	require.False(t, days[0].Approximate)
	require.Equal(t, int64(2), days[0].Sessions)

	// Foreign keys are enforced again afterwards.
	var fk int
	require.NoError(t, conn.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&fk))
	require.Equal(t, 1, fk)

	// A second run finds everything already present and writes nothing.
	again, err := im.ImportProject(t.Context(), proj, conn)
	require.NoError(t, err)
	require.Zero(t, again.Imported)
	require.Equal(t, 2, again.AlreadyPresent)
	require.Zero(t, again.Messages)
	var count int
	require.NoError(t, conn.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM usage_events").Scan(&count))
	require.Equal(t, 3, count)
	require.NoError(t, conn.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages").Scan(&count))
	require.Equal(t, 9, count)
}

func TestImportProjectV1RoundTrip(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v1Schema)
	dir := t.TempDir()
	root := seedV1Conversation(f, dir)
	src := f.open()

	plan, err := NewPlan(t.Context(), src, PlanOptions{})
	require.NoError(t, err)
	require.Len(t, plan.Projects, 1)
	proj := plan.Projects[0]

	conn := openCrushDB(t, proj.DataDir)
	res, err := NewImporter(src, ImportOptions{Usage: true}).ImportProject(t.Context(), proj, conn)
	require.NoError(t, err)
	require.Equal(t, 1, res.Imported)
	require.Equal(t, 6, res.Messages)
	require.Equal(t, 3, res.Events)

	q := db.New(conn)
	got, err := session.NewService(q, conn).Get(t.Context(), root)
	require.NoError(t, err)
	require.Equal(t, int64(6), got.MessageCount)
	require.InDelta(t, 0.5, got.Cost, 1e-9)
	require.Equal(t, "msg_l4", got.SummaryMessageID)

	msgs, err := message.NewService(q).ListFromSummary(t.Context(), root, "msg_l4")
	require.NoError(t, err)
	require.NotEmpty(t, msgs)
	require.Equal(t, "msg_l4", msgs[0].ID)

	days, _, err := usage.DailyFromDB(t.Context(), conn, time.Time{}, time.UTC)
	require.NoError(t, err)
	require.Len(t, days, 1)
	require.InDelta(t, 0.5, days[0].Cost, 1e-9)
	require.False(t, days[0].Approximate)
}

func TestImportProjectDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v2Schema)
	dir := t.TempDir()
	seedV2Conversation(f, dir)
	src := f.open()

	plan, err := NewPlan(t.Context(), src, PlanOptions{})
	require.NoError(t, err)
	proj := plan.Projects[0]

	res, err := NewImporter(src, ImportOptions{Usage: true, DryRun: true}).ImportProject(t.Context(), proj, nil)
	require.NoError(t, err)
	require.Equal(t, 2, res.Imported)
	require.Equal(t, 9, res.Messages)
	require.Equal(t, 3, res.Events)
	require.NoDirExists(t, proj.DataDir)

	// With a database handle, dry run still reports what is already there.
	conn := openCrushDB(t, proj.DataDir)
	_, err = NewImporter(src, ImportOptions{Usage: true}).ImportProject(t.Context(), proj, conn)
	require.NoError(t, err)
	res, err = NewImporter(src, ImportOptions{Usage: true, DryRun: true}).ImportProject(t.Context(), proj, conn)
	require.NoError(t, err)
	require.Zero(t, res.Imported)
	require.Equal(t, 2, res.AlreadyPresent)
}

func TestImportProjectNoUsage(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v2Schema)
	dir := t.TempDir()
	seedV2Conversation(f, dir)
	src := f.open()

	plan, err := NewPlan(t.Context(), src, PlanOptions{})
	require.NoError(t, err)
	proj := plan.Projects[0]
	conn := openCrushDB(t, proj.DataDir)

	res, err := NewImporter(src, ImportOptions{}).ImportProject(t.Context(), proj, conn)
	require.NoError(t, err)
	require.Zero(t, res.Events)

	// Without ledger rows the spend is still visible, as an approximate
	// figure on the session's creation day.
	days, hasLedger, err := usage.DailyFromDB(t.Context(), conn, time.Time{}, time.UTC)
	require.NoError(t, err)
	require.True(t, hasLedger)
	require.Len(t, days, 1)
	require.InDelta(t, 1.75, days[0].Cost, 1e-9)
	require.True(t, days[0].Approximate)
}

func TestImportProjectSkipsUndecodableSessionAndChildren(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v2Schema)
	dir := t.TempDir()
	f.project("prj_1", dir)
	f.sessionV2("ses_bad", "prj_1", "", dir, "Bad", 0, 1_000, 2_000)
	f.sessionV2("ses_bad_child", "prj_1", "ses_bad", dir, "Bad child", 0, 1_500, 2_000)
	f.sessionV2("ses_ok", "prj_1", "", dir, "Ok", 0, 3_000, 4_000)
	f.exec(`INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data) VALUES ('msg_bad', 'ses_bad', 'user', 1, 1000, 1000, 'not json')`)
	f.messageV2("msg_ok", "ses_ok", "user", 1, 3_000, obj{"text": "hello", "time": obj{"created": 3_000}})
	src := f.open()

	plan, err := NewPlan(t.Context(), src, PlanOptions{})
	require.NoError(t, err)
	proj := plan.Projects[0]
	conn := openCrushDB(t, proj.DataDir)

	res, err := NewImporter(src, ImportOptions{}).ImportProject(t.Context(), proj, conn)
	require.NoError(t, err)
	// Undecodable messages are dropped, not fatal: the session still
	// imports, just without that message.
	require.Equal(t, 3, res.Imported)
	require.Zero(t, res.Failed)
	require.Equal(t, 1, res.Dropped["undecodable"])

	got, err := session.NewService(db.New(conn), conn).Get(t.Context(), "ses_bad")
	require.NoError(t, err)
	require.Zero(t, got.MessageCount)
}

func TestImportProjectPreservesExistingSessions(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v2Schema)
	dir := t.TempDir()
	root := seedV2Conversation(f, dir)
	src := f.open()

	plan, err := NewPlan(t.Context(), src, PlanOptions{})
	require.NoError(t, err)
	proj := plan.Projects[0]
	conn := openCrushDB(t, proj.DataDir)

	// A session with the same ID already exists in Crush with its own data.
	sessions := session.NewService(db.New(conn), conn)
	_, err = conn.ExecContext(t.Context(), `INSERT INTO sessions (id, title, message_count, prompt_tokens, completion_tokens, cost, created_at, updated_at)
		VALUES (?, 'Mine', 0, 0, 0, 9.5, 1, 1)`, root)
	require.NoError(t, err)

	res, err := NewImporter(src, ImportOptions{Usage: true}).ImportProject(t.Context(), proj, conn)
	require.NoError(t, err)
	require.Equal(t, 1, res.AlreadyPresent)
	require.Equal(t, 1, res.Imported, "the child is new and still imports")

	got, err := sessions.Get(t.Context(), root)
	require.NoError(t, err)
	require.Equal(t, "Mine", got.Title)
	require.InDelta(t, 9.5, got.Cost, 1e-9)
	require.Zero(t, got.MessageCount)

	child, err := sessions.Get(t.Context(), "msg_3$$call_task")
	require.NoError(t, err)
	require.Equal(t, root, child.ParentSessionID)
}

func TestImportProjectDataDirIsCreatedByCaller(t *testing.T) {
	t.Parallel()
	f := newFixture(t, v2Schema)
	dir := t.TempDir()
	seedV2Conversation(f, dir)
	src := f.open()

	plan, err := NewPlan(t.Context(), src, PlanOptions{})
	require.NoError(t, err)
	proj := plan.Projects[0]
	_, err = os.Stat(proj.DataDir)
	require.True(t, os.IsNotExist(err), "planning must not create the data directory")

	_, err = NewImporter(src, ImportOptions{}).ImportProject(t.Context(), proj, nil)
	require.Error(t, err)
}
