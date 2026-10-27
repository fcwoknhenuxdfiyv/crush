package agent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/charmbracelet/crush/internal/usage"
	"github.com/stretchr/testify/require"
)

type fakeUsageRecorder struct {
	mu     sync.Mutex
	events []usage.Event
	err    error
}

func (f *fakeUsageRecorder) Record(_ context.Context, ev usage.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return f.err
}

func TestRecordUsageWithoutRecorderIsNoop(t *testing.T) {
	t.Parallel()

	agent := &sessionAgent{}
	require.NotPanics(t, func() {
		agent.recordUsage(t.Context(), "session-id", Model{}, fantasy.Usage{InputTokens: 1}, 0.1, false, usage.SourceTurn)
	})
}

func TestRecordUsageForwardsEvent(t *testing.T) {
	t.Parallel()

	recorder := &fakeUsageRecorder{}
	agent := &sessionAgent{usage: recorder}
	model := Model{ModelCfg: config.SelectedModel{Provider: "copilot", Model: "claude-fable-5.1"}}
	stepUsage := fantasy.Usage{
		InputTokens:         100,
		OutputTokens:        20,
		CacheReadTokens:     300,
		CacheCreationTokens: 40,
	}

	agent.recordUsage(t.Context(), "session-id", model, stepUsage, 0.42, true, usage.SourceSummarize)

	require.Len(t, recorder.events, 1)
	require.Equal(t, usage.Event{
		SessionID:           "session-id",
		Provider:            "copilot",
		Model:               "claude-fable-5.1",
		Source:              usage.SourceSummarize,
		InputTokens:         100,
		OutputTokens:        20,
		CacheReadTokens:     300,
		CacheCreationTokens: 40,
		Cost:                0.42,
		Estimated:           true,
	}, recorder.events[0])
}

func TestRecordUsageSwallowsRecorderErrors(t *testing.T) {
	t.Parallel()

	recorder := &fakeUsageRecorder{err: errors.New("disk full")}
	agent := &sessionAgent{usage: recorder}
	require.NotPanics(t, func() {
		agent.recordUsage(t.Context(), "session-id", Model{}, fantasy.Usage{}, 0, false, usage.SourceTitle)
	})
	require.Len(t, recorder.events, 1)
}

func TestUpdateSessionUsageReturnsAppliedCost(t *testing.T) {
	t.Parallel()

	model := Model{CatwalkCfg: catwalk.Model{
		CostPer1MIn:        10,
		CostPer1MOut:       20,
		CostPer1MInCached:  5,
		CostPer1MOutCached: 1,
	}}
	stepUsage := fantasy.Usage{
		InputTokens:         1_000_000,
		OutputTokens:        500_000,
		CacheReadTokens:     200_000,
		CacheCreationTokens: 100_000,
	}
	// 10 + 10 + 0.2 + 0.5
	const listCost = 20.7

	t.Run("list price", func(t *testing.T) {
		t.Parallel()
		agent := &sessionAgent{}
		currentSession := &session.Session{ID: "session-id", Cost: 1}
		cost := agent.updateSessionUsage(model, currentSession, stepUsage, nil, false)
		require.InDelta(t, listCost, cost, 1e-9)
		require.InDelta(t, 1+listCost, currentSession.Cost, 1e-9)
	})

	t.Run("provider override", func(t *testing.T) {
		t.Parallel()
		agent := &sessionAgent{}
		currentSession := &session.Session{ID: "session-id"}
		override := 3.5
		cost := agent.updateSessionUsage(model, currentSession, stepUsage, &override, false)
		require.InDelta(t, 3.5, cost, 1e-9)
		require.InDelta(t, 3.5, currentSession.Cost, 1e-9)
	})

	t.Run("flat rate", func(t *testing.T) {
		t.Parallel()
		agent := &sessionAgent{}
		flat := model
		flat.FlatRate = true
		currentSession := &session.Session{ID: "session-id"}
		cost := agent.updateSessionUsage(flat, currentSession, stepUsage, nil, false)
		require.Zero(t, cost)
		require.Zero(t, currentSession.Cost)
	})

	t.Run("estimated", func(t *testing.T) {
		t.Parallel()
		agent := &sessionAgent{}
		currentSession := &session.Session{ID: "session-id"}
		cost := agent.updateSessionUsage(model, currentSession, stepUsage, nil, true)
		require.Zero(t, cost)
		require.Zero(t, currentSession.Cost)
	})
}
