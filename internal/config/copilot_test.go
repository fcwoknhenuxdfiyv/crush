package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/env"
	"github.com/charmbracelet/crush/internal/modelsdev"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/stretchr/testify/require"
)

// stubCopilotModels replaces the Copilot catalog fetch with one that
// returns the given models and counts how often it ran. Each call hands
// out a fresh copy, as the real fetch does, since the store prices the
// result in place.
func stubCopilotModels(t *testing.T, models []catwalk.Model) *int {
	t.Helper()

	originalFetch := fetchCopilotModels
	calls := 0
	fetchCopilotModels = func(context.Context, *oauth.Token) ([]catwalk.Model, error) {
		calls++
		return slices.Clone(models), nil
	}
	t.Cleanup(func() { fetchCopilotModels = originalFetch })
	return &calls
}

// stubCopilotPricing replaces the models.dev fetch with one that returns
// the given prices (or the error) and counts how often it ran.
func stubCopilotPricing(t *testing.T, prices map[string]modelsdev.Cost, err error) *int {
	t.Helper()

	originalFetch := fetchCopilotPricing
	calls := 0
	fetchCopilotPricing = func(_ context.Context, providerID string) (map[string]modelsdev.Cost, error) {
		calls++
		require.Equal(t, copilotPricingProvider, providerID, "prices are looked up under the models.dev key for Copilot")
		return prices, err
	}
	t.Cleanup(func() { fetchCopilotPricing = originalFetch })
	return &calls
}

// TestSetProviderAPIKeyCopilotFetchesCatalog proves a Copilot login
// fetches the live model catalog, prices it from models.dev, persists
// it, and overlays its reasoning levels and prices onto the Catwalk
// models so the effort picker is offered for the models that honor it
// and session spend stops reading zero.
func TestSetProviderAPIKeyCopilotFetchesCatalog(t *testing.T) {
	// Not parallel: t.Setenv below.
	t.Setenv("CRUSH_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("CRUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	token := &oauth.Token{
		AccessToken:  "copilot-at",
		RefreshToken: "gho-rt",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(time.Hour).Unix(),
	}

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")
	initialConfig := `{
		"providers": {
			"copilot": {
				"id": "copilot",
				"models": [
					{"id": "gpt-5.4", "name": "GPT-5.4", "cost_per_1m_in": 1.25},
					{"id": "claude-sonnet-5", "name": "Claude Sonnet 5"},
					{"id": "gpt-4.1", "name": "GPT-4.1"}
				]
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	providers := csync.NewMap[string, ProviderConfig]()
	require.NoError(t, json.Unmarshal([]byte(initialConfig), &struct {
		Providers *csync.Map[string, ProviderConfig] `json:"providers"`
	}{Providers: providers}))

	stubCopilotModels(t, []catwalk.Model{
		{ID: "gpt-5.4", Name: "GPT-5.4", CanReason: true, ReasoningLevels: []string{"low", "medium", "high"}, DefaultReasoningEffort: "medium"},
		{ID: "claude-sonnet-5", Name: "Claude Sonnet 5"},
	})
	stubCopilotPricing(t, map[string]modelsdev.Cost{
		"gpt-5.4":         {Input: 2.5, Output: 15, CacheRead: 0.25},
		"claude-sonnet-5": {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	}, nil)

	store := &ConfigStore{
		config:         &Config{Providers: providers},
		globalDataPath: configPath,
		workingDir:     dir,
	}

	require.NoError(t, store.SetProviderAPIKey(ScopeGlobal, "copilot", token))

	pc, ok := store.Config().Providers.Get("copilot")
	require.True(t, ok)
	require.Equal(t, token.AccessToken, pc.APIKey)
	require.Len(t, pc.CopilotModels, 2, "the live catalog lands in its own field")
	require.Equal(t, 2.5, pc.CopilotModels[0].CostPer1MIn, "the live catalog is priced from models.dev")
	require.Equal(t, 15.0, pc.CopilotModels[0].CostPer1MOut)
	require.Equal(t, 0.25, pc.CopilotModels[0].CostPer1MOutCached, "a cache read is billed on the cached-output rate")
	require.Equal(t, 3.75, pc.CopilotModels[1].CostPer1MInCached, "a cache write is billed on the cached-input rate")

	// The write auto-reloads the config, which merges the embedded
	// Catwalk catalog behind the user's models; only the overlay on the
	// user's entries is asserted here.
	seen := map[string]int{}
	for _, m := range pc.Models {
		seen[m.ID]++
		switch m.ID {
		case "gpt-5.4":
			require.True(t, m.CanReason)
			require.Equal(t, []string{"low", "medium", "high"}, m.ReasoningLevels)
			require.Equal(t, "medium", m.DefaultReasoningEffort)
			require.Equal(t, 1.25, m.CostPer1MIn, "a price the user set survives the overlay")
			require.Zero(t, m.CostPer1MOut, "a priced model is not patched piecemeal from models.dev")
		case "claude-sonnet-5":
			require.Equal(t, 3.0, m.CostPer1MIn, "an unpriced model takes the models.dev price")
			require.Equal(t, 15.0, m.CostPer1MOut)
			require.Equal(t, 3.75, m.CostPer1MInCached)
			require.Equal(t, 0.3, m.CostPer1MOutCached)
		case "gpt-4.1":
			require.False(t, m.CanReason, "models the listing does not know are untouched")
			require.False(t, modelsdev.HasPricing(m))
		}
	}
	require.Equal(t, 1, seen["gpt-5.4"], "the overlay does not duplicate models")
	require.Equal(t, 1, seen["claude-sonnet-5"])

	disk, err := os.ReadFile(store.globalDataPath)
	require.NoError(t, err)
	require.Contains(t, string(disk), "copilot_models", "the fetched catalog is persisted")
	require.Contains(t, string(disk), `"cost_per_1m_in":2.5`, "the prices are persisted with the catalog")
}

// TestRefetchCopilotModelsKeepsUnpricedCatalog proves an unreachable
// models.dev does not cost the user the catalog: the models still land,
// unpriced, so the reasoning levels are known.
func TestRefetchCopilotModelsKeepsUnpricedCatalog(t *testing.T) {
	// Not parallel: swaps the fetch package variables.
	dir := t.TempDir()
	configPath := filepath.Join(dir, "crush.json")

	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("copilot", ProviderConfig{
		ID:         "copilot",
		OAuthToken: &oauth.Token{AccessToken: "copilot-at", ExpiresAt: time.Now().Add(time.Hour).Unix()},
		Models:     []catwalk.Model{{ID: "gpt-5.4", Name: "GPT-5.4"}},
	})
	store := &ConfigStore{
		config:         &Config{Providers: providers},
		globalDataPath: configPath,
		workingDir:     dir,
	}

	stubCopilotModels(t, []catwalk.Model{
		{ID: "gpt-5.4", Name: "GPT-5.4", CanReason: true, ReasoningLevels: []string{"low", "high"}, DefaultReasoningEffort: "low"},
	})
	stubCopilotPricing(t, nil, errors.New("models.dev is down"))

	store.refetchCopilotModels(t.Context(), ScopeGlobal, true)

	pc, ok := store.Config().Providers.Get("copilot")
	require.True(t, ok)
	require.Len(t, pc.CopilotModels, 1, "the catalog is kept without prices")
	require.False(t, modelsdev.HasPricing(pc.CopilotModels[0]))
	require.True(t, pc.Models[0].CanReason, "the reasoning levels are still overlaid")
	require.False(t, modelsdev.HasPricing(pc.Models[0]))

	disk, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Contains(t, string(disk), "copilot_models", "the unpriced catalog is persisted")
}

// TestRefetchCopilotModelsBackfillsPricing proves a catalog persisted
// before pricing existed (or while models.dev was unreachable) is priced
// on the next model update, once per process: a models.dev that stays
// down is not asked again on every model switch, while a priced catalog
// is never refetched by this path at all.
func TestRefetchCopilotModelsBackfillsPricing(t *testing.T) {
	// Not parallel: swaps the fetch package variables.
	newStore := func(t *testing.T, catalog []catwalk.Model) *ConfigStore {
		t.Helper()
		dir := t.TempDir()
		providers := csync.NewMap[string, ProviderConfig]()
		providers.Set("copilot", ProviderConfig{
			ID:            "copilot",
			OAuthToken:    &oauth.Token{AccessToken: "copilot-at", ExpiresAt: time.Now().Add(time.Hour).Unix()},
			Models:        []catwalk.Model{{ID: "gpt-5.4", Name: "GPT-5.4"}},
			CopilotModels: catalog,
		})
		return &ConfigStore{
			config:         &Config{Providers: providers},
			globalDataPath: filepath.Join(dir, "crush.json"),
			workingDir:     dir,
		}
	}
	unpriced := []catwalk.Model{{ID: "gpt-5.4", Name: "GPT-5.4", CanReason: true, ReasoningLevels: []string{"low", "high"}}}

	t.Run("an unpriced catalog is priced once", func(t *testing.T) {
		store := newStore(t, unpriced)
		modelCalls := stubCopilotModels(t, unpriced)
		priceCalls := stubCopilotPricing(t, map[string]modelsdev.Cost{"gpt-5.4": {Input: 2.5, Output: 15}}, nil)

		store.RefetchCopilotModels(t.Context())
		store.RefetchCopilotModels(t.Context())

		require.Equal(t, 1, *modelCalls, "the catalog is refetched to be priced, once")
		require.Equal(t, 1, *priceCalls)

		pc, ok := store.Config().Providers.Get("copilot")
		require.True(t, ok)
		require.Equal(t, 2.5, pc.CopilotModels[0].CostPer1MIn, "the stored catalog is now priced")
		require.Equal(t, 2.5, pc.Models[0].CostPer1MIn, "the price is overlaid onto the prepared models")
		require.True(t, pc.Models[0].CanReason)
	})

	t.Run("a models.dev outage is retried only on the next run", func(t *testing.T) {
		store := newStore(t, unpriced)
		modelCalls := stubCopilotModels(t, unpriced)
		priceCalls := stubCopilotPricing(t, nil, errors.New("models.dev is down"))

		store.RefetchCopilotModels(t.Context())
		store.RefetchCopilotModels(t.Context())

		require.Equal(t, 1, *modelCalls, "an outage costs one attempt per process")
		require.Equal(t, 1, *priceCalls)

		pc, ok := store.Config().Providers.Get("copilot")
		require.True(t, ok)
		require.Len(t, pc.CopilotModels, 1, "the catalog survives the failed backfill")
	})

	t.Run("a priced catalog is left alone", func(t *testing.T) {
		priced := []catwalk.Model{{ID: "gpt-5.4", Name: "GPT-5.4", CostPer1MIn: 2.5, CostPer1MOut: 15}}
		store := newStore(t, priced)
		modelCalls := stubCopilotModels(t, priced)
		priceCalls := stubCopilotPricing(t, nil, errors.New("must not be called"))

		store.RefetchCopilotModels(t.Context())

		require.Zero(t, *modelCalls)
		require.Zero(t, *priceCalls)
	})

	t.Run("a missing catalog is fetched and priced", func(t *testing.T) {
		store := newStore(t, nil)
		modelCalls := stubCopilotModels(t, unpriced)
		priceCalls := stubCopilotPricing(t, map[string]modelsdev.Cost{"gpt-5.4": {Input: 2.5, Output: 15}}, nil)

		store.RefetchCopilotModels(t.Context())

		require.Equal(t, 1, *modelCalls)
		require.Equal(t, 1, *priceCalls)

		pc, ok := store.Config().Providers.Get("copilot")
		require.True(t, ok)
		require.Equal(t, 15.0, pc.CopilotModels[0].CostPer1MOut)
	})
}

// TestConfigureProvidersOverlaysCopilotCatalog proves the persisted live
// catalog is overlaid onto the Catwalk models at load time, when the
// provider is signed in with Copilot: reasoning levels and the prices
// models.dev supplied both land on the Catwalk entries.
func TestConfigureProvidersOverlaysCopilotCatalog(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	cfg.Providers.Set("copilot", ProviderConfig{
		ID:         "copilot",
		OAuthToken: &oauth.Token{AccessToken: "copilot-at"},
		CopilotModels: []catwalk.Model{
			{ID: "gpt-5.4", CanReason: true, ReasoningLevels: []string{"low", "high"}, DefaultReasoningEffort: "low", CostPer1MIn: 2.5, CostPer1MOut: 15, CostPer1MOutCached: 0.25},
		},
	})

	known := []catwalk.Provider{{
		ID:          catwalk.InferenceProviderCopilot,
		Name:        "GitHub Copilot",
		APIEndpoint: "https://api.githubcopilot.com",
		Type:        catwalk.TypeOpenAICompat,
		Models: []catwalk.Model{
			{ID: "gpt-5.4", Name: "GPT-5.4", ContextWindow: 128000},
			{ID: "gpt-4.1", Name: "GPT-4.1"},
		},
	}}

	testEnv := env.NewFromMap(map[string]string{})
	require.NoError(t, cfg.configureProviders(t.Context(), testStore(cfg), testEnv, NewShellVariableResolver(testEnv), known))

	pc, ok := cfg.Providers.Get("copilot")
	require.True(t, ok)
	require.Len(t, pc.Models, 2)
	require.True(t, pc.Models[0].CanReason)
	require.Equal(t, []string{"low", "high"}, pc.Models[0].ReasoningLevels)
	require.Equal(t, "low", pc.Models[0].DefaultReasoningEffort)
	require.Equal(t, int64(128000), pc.Models[0].ContextWindow, "Catwalk fields survive")
	require.Equal(t, 2.5, pc.Models[0].CostPer1MIn, "the price lands on the Catwalk entry, which has none")
	require.Equal(t, 15.0, pc.Models[0].CostPer1MOut)
	require.Equal(t, 0.25, pc.Models[0].CostPer1MOutCached)
	require.False(t, pc.Models[1].CanReason)
	require.False(t, modelsdev.HasPricing(pc.Models[1]), "models the catalog does not price stay unpriced")
	require.Equal(t, "GitHubCopilotChat/0.32.4", pc.ExtraHeaders["User-Agent"], "the Copilot headers are still applied")
}
