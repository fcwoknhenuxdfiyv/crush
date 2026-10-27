package modelsdev

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"charm.land/catwalk/pkg/catwalk"
)

// APIURL is the models.dev catalog: a single JSON document keyed by
// provider, each carrying its models with pricing in USD per 1M tokens.
const APIURL = "https://models.dev/api.json"

// apiURL is a variable so tests can point it at a stub server.
var apiURL = APIURL

// Cost is what a model costs in USD per 1M tokens.
type Cost struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// IsZero reports whether no rate is set.
func (c Cost) IsZero() bool {
	return c.Input == 0 && c.Output == 0 && c.CacheRead == 0 && c.CacheWrite == 0
}

// modelInfo mirrors the subset of a models.dev model entry that is used.
type modelInfo struct {
	ID   string `json:"id"`
	Cost *Cost  `json:"cost"`
}

// providerInfo mirrors one provider of the models.dev document.
type providerInfo struct {
	Models map[string]modelInfo `json:"models"`
}

// Fetch returns the pricing models.dev publishes for the given provider,
// keyed by model ID. Only that provider is decoded: the document carries
// every provider models.dev knows and most of it is not needed.
func Fetch(ctx context.Context, providerID string) (map[string]Cost, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch models.dev catalog: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch models.dev catalog: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("read models.dev catalog: %w", err)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode models.dev catalog: %w", err)
	}
	raw, ok := doc[providerID]
	if !ok {
		return nil, fmt.Errorf("models.dev has no provider %q", providerID)
	}
	var provider providerInfo
	if err := json.Unmarshal(raw, &provider); err != nil {
		return nil, fmt.Errorf("decode models.dev provider %q: %w", providerID, err)
	}

	prices := make(map[string]Cost, len(provider.Models))
	for key, m := range provider.Models {
		id := m.ID
		if id == "" {
			id = key
		}
		if id == "" || m.Cost == nil || m.Cost.IsZero() {
			continue
		}
		prices[id] = *m.Cost
	}
	if len(prices) == 0 {
		return nil, fmt.Errorf("models.dev lists no prices for provider %q", providerID)
	}
	return prices, nil
}

// Apply fills in the pricing of models that carry none. Models that
// already have any rate set are left alone: a price the catalog or the
// user supplied wins over models.dev.
//
// Catwalk names its cache rates from the request's point of view:
// CostPer1MInCached is what writing a prompt to the cache costs, and
// CostPer1MOutCached is what reading it back costs.
func Apply(models []catwalk.Model, prices map[string]Cost) {
	for i := range models {
		m := &models[i]
		if HasPricing(*m) {
			continue
		}
		cost, ok := prices[m.ID]
		if !ok {
			continue
		}
		m.CostPer1MIn = cost.Input
		m.CostPer1MOut = cost.Output
		m.CostPer1MInCached = cost.CacheWrite
		m.CostPer1MOutCached = cost.CacheRead
	}
}

// HasPricing reports whether any rate is set on the model.
func HasPricing(m catwalk.Model) bool {
	return m.CostPer1MIn != 0 || m.CostPer1MOut != 0 || m.CostPer1MInCached != 0 || m.CostPer1MOutCached != 0
}
