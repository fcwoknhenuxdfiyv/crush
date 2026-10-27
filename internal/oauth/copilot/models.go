package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/charmbracelet/crush/internal/oauth"
)

// APIBaseURL is the Copilot API the Copilot token authorizes.
const APIBaseURL = "https://api.githubcopilot.com"

// modelsEndpoint is a variable so tests can point it at a stub server.
var modelsEndpoint = APIBaseURL + "/models"

// preferredDefaultEffort is used as the default reasoning level when the
// model honors it; the listing carries no default of its own.
const preferredDefaultEffort = "medium"

// modelSupports mirrors the request parameters a model accepts.
type modelSupports struct {
	// ReasoningEffort lists the effort values the model honors.
	ReasoningEffort []string `json:"reasoning_effort"`
	Vision          bool     `json:"vision"`
}

// modelLimits mirrors the token limits of a model.
type modelLimits struct {
	MaxContextWindowTokens int64 `json:"max_context_window_tokens"`
	MaxOutputTokens        int64 `json:"max_output_tokens"`
}

// modelCapabilities mirrors the capabilities block of a model.
type modelCapabilities struct {
	// Type is "chat" for language models; embedding models carry
	// "embeddings".
	Type     string         `json:"type"`
	Supports *modelSupports `json:"supports"`
	Limits   *modelLimits   `json:"limits"`
}

// modelInfo mirrors one entry of the Copilot model listing.
type modelInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// ModelPickerEnabled marks the models the plan lets the user pick;
	// the rest are legacy or internal entries.
	ModelPickerEnabled bool               `json:"model_picker_enabled"`
	Capabilities       *modelCapabilities `json:"capabilities"`
}

type modelsResponse struct {
	Models []modelInfo `json:"data"`
}

// Models fetches the model catalog the Copilot plan grants. Only chat
// models the picker exposes are returned: the listing also carries
// embedding models and retired snapshots.
func Models(ctx context.Context, token *oauth.Token) ([]catwalk.Model, error) {
	if token == nil {
		return nil, fmt.Errorf("an OAuth token is required to list Copilot models")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsEndpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	for k, v := range Headers() {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch Copilot model catalog: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read Copilot model catalog: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &oauth.TokenExchangeError{
			StatusCode: resp.StatusCode,
			Body:       string(body),
		}
	}

	var mr modelsResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		return nil, fmt.Errorf("decode Copilot model catalog: %w", err)
	}

	models := make([]catwalk.Model, 0, len(mr.Models))
	for _, m := range mr.Models {
		if m.ID == "" || !m.ModelPickerEnabled || m.Capabilities == nil || m.Capabilities.Type != "chat" {
			continue
		}
		var (
			levels         []string
			supportsImages bool
			contextWindow  int64
			maxTokens      int64
		)
		if s := m.Capabilities.Supports; s != nil {
			levels = s.ReasoningEffort
			supportsImages = s.Vision
		}
		if l := m.Capabilities.Limits; l != nil {
			contextWindow = l.MaxContextWindowTokens
			maxTokens = l.MaxOutputTokens
		}
		var defaultEffort string
		if slices.Contains(levels, preferredDefaultEffort) {
			defaultEffort = preferredDefaultEffort
		}
		name := m.Name
		if name == "" {
			name = m.ID
		}
		models = append(models, catwalk.Model{
			ID:                     m.ID,
			Name:                   name,
			ContextWindow:          contextWindow,
			DefaultMaxTokens:       maxTokens,
			CanReason:              len(levels) > 0,
			ReasoningLevels:        levels,
			DefaultReasoningEffort: defaultEffort,
			SupportsImages:         supportsImages,
		})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("the Copilot model catalog was empty")
	}
	return models, nil
}

// MergeModels overlays the live Copilot catalog onto the Catwalk one.
// Catwalk carries pricing and tuned defaults the listing lacks, while the
// listing knows which reasoning levels each model honors; models the
// listing grants but Catwalk does not know are appended as-is.
func MergeModels(base, live []catwalk.Model) []catwalk.Model {
	if len(live) == 0 {
		return base
	}
	liveByID := make(map[string]catwalk.Model, len(live))
	for _, m := range live {
		liveByID[m.ID] = m
	}
	merged := make([]catwalk.Model, 0, len(base)+len(live))
	seen := make(map[string]bool, len(base))
	for _, m := range base {
		seen[m.ID] = true
		if l, ok := liveByID[m.ID]; ok {
			if len(l.ReasoningLevels) > 0 {
				m.CanReason = true
				m.ReasoningLevels = l.ReasoningLevels
				if m.DefaultReasoningEffort == "" || !slices.Contains(l.ReasoningLevels, m.DefaultReasoningEffort) {
					m.DefaultReasoningEffort = l.DefaultReasoningEffort
				}
			}
			if l.SupportsImages {
				m.SupportsImages = true
			}
			if m.ContextWindow == 0 {
				m.ContextWindow = l.ContextWindow
			}
			if m.DefaultMaxTokens == 0 {
				m.DefaultMaxTokens = l.DefaultMaxTokens
			}
		}
		merged = append(merged, m)
	}
	for _, m := range live {
		if !seen[m.ID] {
			merged = append(merged, m)
		}
	}
	return merged
}
