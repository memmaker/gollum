package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/memmaker/gollum/llm"
)

// flexDiscount is OpenAI's own flat rate for the "flex" service tier: half of
// standard sync pricing, the same discount as the Batch API. models.dev has
// no field for this — its cost entries are all standard-tier — so it's
// applied here instead of looked up.
const flexDiscount = 0.5

// flexEligible lists the models OpenAI actually allows the "flex" tier on.
// Sending service_tier: flex for any other model is either rejected or
// silently ignored by the backend, so a call outside this set is priced at
// standard rate regardless of what the session's tier was set to.
var flexEligible = map[string]bool{
	"gpt-5.6-sol":   true,
	"gpt-5.6-terra": true,
	"gpt-5.6-luna":  true,
}

// pricingURL is models.dev's own machine-readable catalogue: every provider
// it tracks, every model, cost per token and context/output limits.
const pricingURL = "https://models.dev/api.json"

// pricingTTL bounds how long a cached copy is trusted before crash refetches
// it. Prices don't move often enough to justify a network call every start.
const pricingTTL = 24 * time.Hour

// modelPrice is one model's models.dev entry, trimmed to what crash shows:
// USD per 1M tokens, and the limits it's rated for.
type modelPrice struct {
	Input, Output, CacheRead, CacheWrite float64
	Context, MaxOutput                   int

	// LongContextThreshold is the input-token count at and above which a
	// request is billed at LongInput/LongOutput instead of Input/Output —
	// models.dev's "context" cost tier. Zero means the model has none.
	LongContextThreshold  int
	LongInput, LongOutput float64
}

// rate is the USD-per-1M-token price this call is actually billed at: the
// long-context tier if its input crossed LongContextThreshold, halved again
// if it ran on the flex tier and the model allows flex at all (modelID is
// models.dev's own id, not crash's "provider/model" string — flexEligible is
// keyed by that).
func (p modelPrice) rate(inputTokens int, tier llm.ServiceTier, modelID string) (input, output float64) {
	input, output = p.Input, p.Output
	if p.LongContextThreshold > 0 && inputTokens >= p.LongContextThreshold {
		input, output = p.LongInput, p.LongOutput
	}
	if tier == llm.ServiceTierFlex && flexEligible[modelID] {
		input *= flexDiscount
		output *= flexDiscount
	}
	return input, output
}

// cost is what a single call's usage cost at this price, in USD, split into
// its input and output components. ReasoningTokens are already counted
// inside OutputTokens (see llm.Usage's own doc comment), so they are not
// billed a second time.
func (p modelPrice) cost(in, out int, tier llm.ServiceTier, modelID string) (inputCost, outputCost float64) {
	inRate, outRate := p.rate(in, tier, modelID)
	return float64(in) / 1e6 * inRate, float64(out) / 1e6 * outRate
}

// priceIndex mirrors models.dev's own shape: provider id -> model id -> price.
type priceIndex map[string]map[string]modelPrice

// providerAlias maps a gollum provider slug to the models.dev provider id it
// prices under, for the handful of names that differ.
var providerAlias = map[string]string{
	"gemini":  "google",
	"copilot": "github-copilot",
}

// lookupPrice finds the price for m.current, which applyModel builds as
// "<gollum-provider>/<model>". omniroute's own model field is itself
// "<vendor>/<model>" (see llm_model.rec) — that already matches models.dev's
// provider/model shape once the leading "omniroute/" is dropped, so it's
// tried first; every other provider is looked up by its own (aliased) name.
// The models.dev model id is returned alongside the price since callers need
// it for things keyed by that id rather than crash's own "provider/model"
// string — flex eligibility, for one.
func lookupPrice(idx priceIndex, current string) (price modelPrice, modelID string, ok bool) {
	provider, model, ok := strings.Cut(current, "/")
	if !ok {
		return modelPrice{}, "", false
	}
	if vendor, rest, ok := strings.Cut(model, "/"); ok {
		if p, ok := idx[vendor][rest]; ok {
			return p, rest, true
		}
	}
	if alias, ok := providerAlias[provider]; ok {
		provider = alias
	}
	p, ok := idx[provider][model]
	return p, model, ok
}

// pricingCachePath is ~/.config/gollum/models_dev.json, alongside crash's
// other cached/config state.
func pricingCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".config", "gollum", "models_dev.json"), nil
}

// loadPricing returns models.dev's pricing table, cached for pricingTTL. A
// network failure falls back to whatever is on disk however old — a stale
// price beats none. No cache and no network returns an empty index rather
// than an error: pricing is a bonus, not something crash should refuse to
// start over.
func loadPricing(ctx context.Context) priceIndex {
	path, err := pricingCachePath()
	if err != nil {
		return priceIndex{}
	}
	if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) < pricingTTL {
		if idx, err := readPricingCache(path); err == nil {
			return idx
		}
	}
	idx, err := fetchPricing(ctx)
	if err != nil {
		if cached, err := readPricingCache(path); err == nil {
			return cached
		}
		return priceIndex{}
	}
	_ = writePricingCache(path, idx)
	return idx
}

func readPricingCache(path string) (priceIndex, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var idx priceIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, err
	}
	return idx, nil
}

// writePricingCache writes through a temp file in the same directory then
// renames into place, same atomic-write idiom writeRecsAtomically uses for
// crash's other config files.
func writePricingCache(path string, idx priceIndex) error {
	b, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".models_dev-*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// fetchPricing downloads and flattens models.dev's api.json. Its raw shape
// nests far more than crash needs — release dates, modalities, reasoning
// flags and the rest — only cost and limit survive.
func fetchPricing(ctx context.Context) (priceIndex, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pricingURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models.dev: status %d", resp.StatusCode)
	}

	var raw map[string]struct {
		Models map[string]struct {
			Limit struct {
				Context int `json:"context"`
				Output  int `json:"output"`
			} `json:"limit"`
			Cost struct {
				Input      float64 `json:"input"`
				Output     float64 `json:"output"`
				CacheRead  float64 `json:"cache_read"`
				CacheWrite float64 `json:"cache_write"`
				// Tiers carries the higher rate a request is billed at once
				// its input crosses a context-length threshold — OpenAI's
				// long-context pricing. Only the "context" kind is one
				// crash prices by; models.dev's other tier kinds (if any
				// provider ever adds one) are left alone.
				Tiers []struct {
					Input  float64 `json:"input"`
					Output float64 `json:"output"`
					Tier   struct {
						Type string `json:"type"`
						Size int    `json:"size"`
					} `json:"tier"`
				} `json:"tiers"`
			} `json:"cost"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	idx := make(priceIndex, len(raw))
	for provider, p := range raw {
		models := make(map[string]modelPrice, len(p.Models))
		for id, m := range p.Models {
			price := modelPrice{
				Input:      m.Cost.Input,
				Output:     m.Cost.Output,
				CacheRead:  m.Cost.CacheRead,
				CacheWrite: m.Cost.CacheWrite,
				Context:    m.Limit.Context,
				MaxOutput:  m.Limit.Output,
			}
			for _, t := range m.Cost.Tiers {
				if t.Tier.Type == "context" {
					price.LongContextThreshold = t.Tier.Size
					price.LongInput = t.Input
					price.LongOutput = t.Output
					break
				}
			}
			models[id] = price
		}
		idx[provider] = models
	}
	return idx, nil
}
