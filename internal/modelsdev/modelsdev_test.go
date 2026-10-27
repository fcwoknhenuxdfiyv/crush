package modelsdev

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/stretchr/testify/require"
)

const fixture = `{
	"github-copilot": {
		"id": "github-copilot",
		"name": "GitHub Copilot",
		"models": {
			"gpt-5.4": {
				"id": "gpt-5.4",
				"name": "GPT-5.4",
				"cost": {"input": 2.5, "output": 15, "cache_read": 0.25}
			},
			"gpt-6-sol": {
				"id": "gpt-6-sol",
				"cost": {"input": 2, "output": 10, "cache_read": 0.2, "cache_write": 2.5}
			},
			"keyed-only": {
				"cost": {"input": 1, "output": 2}
			},
			"free-model": {
				"id": "free-model",
				"cost": {"input": 0, "output": 0}
			},
			"no-cost": {
				"id": "no-cost"
			}
		}
	},
	"openai": {
		"id": "openai",
		"models": {
			"gpt-5.4": {"id": "gpt-5.4", "cost": {"input": 99, "output": 99}}
		}
	}
}`

func stub(t *testing.T, status int, body string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "application/json", r.Header.Get("Accept"))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)

	orig := apiURL
	apiURL = server.URL
	t.Cleanup(func() { apiURL = orig })
}

func TestFetch(t *testing.T) {
	stub(t, http.StatusOK, fixture)

	prices, err := Fetch(context.Background(), "github-copilot")
	require.NoError(t, err)

	// Only the requested provider is decoded; entries without a usable
	// price are dropped; an entry without an id falls back to its key.
	require.Len(t, prices, 3)
	require.Equal(t, Cost{Input: 2.5, Output: 15, CacheRead: 0.25}, prices["gpt-5.4"])
	require.Equal(t, Cost{Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5}, prices["gpt-6-sol"])
	require.Equal(t, Cost{Input: 1, Output: 2}, prices["keyed-only"])
	require.NotContains(t, prices, "free-model")
	require.NotContains(t, prices, "no-cost")
}

func TestFetch_Errors(t *testing.T) {
	t.Run("unknown provider", func(t *testing.T) {
		stub(t, http.StatusOK, fixture)
		_, err := Fetch(context.Background(), "nope")
		require.ErrorContains(t, err, `no provider "nope"`)
	})

	t.Run("server error", func(t *testing.T) {
		stub(t, http.StatusBadGateway, "")
		_, err := Fetch(context.Background(), "github-copilot")
		require.ErrorContains(t, err, "502")
	})

	t.Run("malformed document", func(t *testing.T) {
		stub(t, http.StatusOK, "{")
		_, err := Fetch(context.Background(), "github-copilot")
		require.ErrorContains(t, err, "decode")
	})

	t.Run("no prices", func(t *testing.T) {
		stub(t, http.StatusOK, `{"github-copilot": {"models": {"x": {"id": "x"}}}}`)
		_, err := Fetch(context.Background(), "github-copilot")
		require.ErrorContains(t, err, "no prices")
	})
}

func TestApply(t *testing.T) {
	t.Parallel()

	models := []catwalk.Model{
		{ID: "gpt-5.4"},
		{ID: "gpt-6-sol", CostPer1MIn: 7},
		{ID: "unknown"},
	}
	Apply(models, map[string]Cost{
		"gpt-5.4":   {Input: 2.5, Output: 15, CacheRead: 0.25, CacheWrite: 3.125},
		"gpt-6-sol": {Input: 2, Output: 10},
	})

	// Unpriced models take the models.dev rates. Catwalk's cached fields
	// are named from the request's side: InCached is the write rate and
	// OutCached the read rate.
	require.Equal(t, 2.5, models[0].CostPer1MIn)
	require.Equal(t, 15.0, models[0].CostPer1MOut)
	require.Equal(t, 3.125, models[0].CostPer1MInCached)
	require.Equal(t, 0.25, models[0].CostPer1MOutCached)
	require.True(t, HasPricing(models[0]))

	// A model that already carries any rate is left alone.
	require.Equal(t, 7.0, models[1].CostPer1MIn)
	require.Zero(t, models[1].CostPer1MOut)

	// A model models.dev does not know stays unpriced.
	require.False(t, HasPricing(models[2]))
}
