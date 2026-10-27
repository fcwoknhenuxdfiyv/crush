package copilot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer at-models", r.Header.Get("Authorization"))
		require.Equal(t, "application/json", r.Header.Get("Accept"))
		require.Equal(t, integrationID, r.Header.Get("Copilot-Integration-Id"))

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": [
				{
					"id": "gpt-5.4",
					"name": "GPT-5.4",
					"model_picker_enabled": true,
					"capabilities": {
						"type": "chat",
						"limits": {"max_context_window_tokens": 400000, "max_output_tokens": 128000},
						"supports": {"reasoning_effort": ["low", "medium", "high", "xhigh"], "vision": true}
					}
				},
				{
					"id": "claude-haiku-4.5",
					"name": "Claude Haiku 4.5",
					"model_picker_enabled": true,
					"capabilities": {
						"type": "chat",
						"limits": {"max_context_window_tokens": 200000, "max_output_tokens": 64000},
						"supports": {"vision": true}
					}
				},
				{
					"id": "gpt-4o-mini",
					"name": "GPT-4o mini",
					"model_picker_enabled": false,
					"capabilities": {"type": "chat", "supports": {}}
				},
				{
					"id": "text-embedding-3-small",
					"name": "Embedding V3 small",
					"model_picker_enabled": true,
					"capabilities": {"type": "embeddings", "supports": {"dimensions": true}}
				}
			],
			"object": "list"
		}`))
	}))
	t.Cleanup(server.Close)

	orig := modelsEndpoint
	modelsEndpoint = server.URL
	t.Cleanup(func() { modelsEndpoint = orig })

	models, err := Models(context.Background(), &oauth.Token{AccessToken: "at-models"})
	require.NoError(t, err)

	// The hidden snapshot and the embedding model are dropped; the other
	// two survive in order.
	require.Len(t, models, 2)
	require.Equal(t, "gpt-5.4", models[0].ID)
	require.Equal(t, "GPT-5.4", models[0].Name)
	require.True(t, models[0].CanReason)
	require.Equal(t, []string{"low", "medium", "high", "xhigh"}, models[0].ReasoningLevels)
	require.Equal(t, "medium", models[0].DefaultReasoningEffort)
	require.Equal(t, int64(400000), models[0].ContextWindow)
	require.Equal(t, int64(128000), models[0].DefaultMaxTokens)
	require.True(t, models[0].SupportsImages)

	require.Equal(t, "claude-haiku-4.5", models[1].ID)
	require.False(t, models[1].CanReason)
	require.Empty(t, models[1].ReasoningLevels)
	require.Empty(t, models[1].DefaultReasoningEffort)
	require.True(t, models[1].SupportsImages)
}

func TestModels_Errors(t *testing.T) {
	t.Run("nil token", func(t *testing.T) {
		_, err := Models(context.Background(), nil)
		require.ErrorContains(t, err, "OAuth token")
	})

	t.Run("server error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error": "unauthorized"}`))
		}))
		t.Cleanup(server.Close)

		orig := modelsEndpoint
		modelsEndpoint = server.URL
		t.Cleanup(func() { modelsEndpoint = orig })

		_, err := Models(context.Background(), &oauth.Token{AccessToken: "at"})
		var exchangeErr *oauth.TokenExchangeError
		require.ErrorAs(t, err, &exchangeErr)
		require.Equal(t, http.StatusUnauthorized, exchangeErr.StatusCode)
	})

	t.Run("empty catalog", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data": []}`))
		}))
		t.Cleanup(server.Close)

		orig := modelsEndpoint
		modelsEndpoint = server.URL
		t.Cleanup(func() { modelsEndpoint = orig })

		_, err := Models(context.Background(), &oauth.Token{AccessToken: "at"})
		require.ErrorContains(t, err, "empty")
	})
}

func TestMergeModels(t *testing.T) {
	t.Parallel()

	base := []catwalk.Model{
		{ID: "gpt-5.4", Name: "GPT-5.4", CostPer1MIn: 1.25, ContextWindow: 400000, DefaultMaxTokens: 32000},
		{ID: "gpt-4.1", Name: "GPT-4.1", ContextWindow: 128000},
		{ID: "claude-sonnet-5", Name: "Claude Sonnet 5", CanReason: true, ReasoningLevels: []string{"low"}, DefaultReasoningEffort: "low"},
	}
	live := []catwalk.Model{
		{ID: "gpt-5.4", Name: "GPT-5.4", CanReason: true, ReasoningLevels: []string{"low", "medium", "high"}, DefaultReasoningEffort: "medium", ContextWindow: 128000, DefaultMaxTokens: 128000, SupportsImages: true},
		{ID: "claude-sonnet-5", Name: "Claude Sonnet 5", CanReason: true, ReasoningLevels: []string{"low", "medium", "high", "max"}, DefaultReasoningEffort: "medium"},
		{ID: "gpt-6-astra", Name: "GPT-6 Astra", CanReason: true, ReasoningLevels: []string{"low", "high"}, ContextWindow: 400000},
	}

	merged := MergeModels(base, live)
	require.Len(t, merged, 4)

	// Catwalk pricing and defaults survive; reasoning levels come from
	// the live listing.
	require.Equal(t, "gpt-5.4", merged[0].ID)
	require.Equal(t, 1.25, merged[0].CostPer1MIn)
	require.Equal(t, int64(400000), merged[0].ContextWindow)
	require.Equal(t, int64(32000), merged[0].DefaultMaxTokens)
	require.True(t, merged[0].CanReason)
	require.Equal(t, []string{"low", "medium", "high"}, merged[0].ReasoningLevels)
	require.Equal(t, "medium", merged[0].DefaultReasoningEffort)
	require.True(t, merged[0].SupportsImages)

	// A model the listing does not know is left untouched.
	require.Equal(t, "gpt-4.1", merged[1].ID)
	require.False(t, merged[1].CanReason)

	// A Catwalk default that the listing still honors is kept.
	require.Equal(t, "claude-sonnet-5", merged[2].ID)
	require.Equal(t, []string{"low", "medium", "high", "max"}, merged[2].ReasoningLevels)
	require.Equal(t, "low", merged[2].DefaultReasoningEffort)

	// A model only the listing grants is appended.
	require.Equal(t, "gpt-6-astra", merged[3].ID)
	require.Equal(t, int64(400000), merged[3].ContextWindow)

	require.Equal(t, base, MergeModels(base, nil), "an empty listing is a no-op")
}
