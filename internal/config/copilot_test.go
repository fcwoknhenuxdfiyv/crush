package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/csync"
	"github.com/charmbracelet/crush/internal/env"
	"github.com/charmbracelet/crush/internal/oauth"
	"github.com/stretchr/testify/require"
)

// TestSetProviderAPIKeyCopilotFetchesCatalog proves a Copilot login
// fetches the live model catalog, persists it, and overlays its
// reasoning levels onto the Catwalk models so the effort picker is
// offered for the models that honor it.
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

	originalFetch := fetchCopilotModels
	fetchCopilotModels = func(context.Context, *oauth.Token) ([]catwalk.Model, error) {
		return []catwalk.Model{
			{ID: "gpt-5.4", Name: "GPT-5.4", CanReason: true, ReasoningLevels: []string{"low", "medium", "high"}, DefaultReasoningEffort: "medium"},
		}, nil
	}
	t.Cleanup(func() { fetchCopilotModels = originalFetch })

	store := &ConfigStore{
		config:         &Config{Providers: providers},
		globalDataPath: configPath,
		workingDir:     dir,
	}

	require.NoError(t, store.SetProviderAPIKey(ScopeGlobal, "copilot", token))

	pc, ok := store.Config().Providers.Get("copilot")
	require.True(t, ok)
	require.Equal(t, token.AccessToken, pc.APIKey)
	require.Len(t, pc.CopilotModels, 1, "the live catalog lands in its own field")

	// The write auto-reloads the config, which merges the embedded
	// Catwalk catalog behind the user's models; only the overlay on the
	// user's entries is asserted here.
	var seen int
	for _, m := range pc.Models {
		if m.ID == "gpt-5.4" {
			seen++
			require.True(t, m.CanReason)
			require.Equal(t, []string{"low", "medium", "high"}, m.ReasoningLevels)
			require.Equal(t, "medium", m.DefaultReasoningEffort)
			require.Equal(t, 1.25, m.CostPer1MIn, "Catwalk pricing survives the overlay")
		}
		if m.ID == "gpt-4.1" {
			require.False(t, m.CanReason, "models the listing does not know are untouched")
		}
	}
	require.Equal(t, 1, seen, "the overlay does not duplicate models")

	disk, err := os.ReadFile(store.globalDataPath)
	require.NoError(t, err)
	require.Contains(t, string(disk), "copilot_models", "the fetched catalog is persisted")
}

// TestConfigureProvidersOverlaysCopilotCatalog proves the persisted live
// catalog is overlaid onto the Catwalk models at load time, when the
// provider is signed in with Copilot.
func TestConfigureProvidersOverlaysCopilotCatalog(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	cfg.Providers.Set("copilot", ProviderConfig{
		ID:         "copilot",
		OAuthToken: &oauth.Token{AccessToken: "copilot-at"},
		CopilotModels: []catwalk.Model{
			{ID: "gpt-5.4", CanReason: true, ReasoningLevels: []string{"low", "high"}, DefaultReasoningEffort: "low"},
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
	require.False(t, pc.Models[1].CanReason)
	require.Equal(t, "GitHubCopilotChat/0.32.4", pc.ExtraHeaders["User-Agent"], "the Copilot headers are still applied")
}
