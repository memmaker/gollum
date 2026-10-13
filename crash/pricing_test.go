package main

import (
	"testing"

	"github.com/memmaker/gollum/llm"
)

func TestLookupPrice(t *testing.T) {
	idx := priceIndex{
		"openai": {"gpt-5.6-luna": modelPrice{Input: 0.2, Output: 1.2, Context: 1050000}},
		"google": {"gemini-3-pro": modelPrice{Input: 2, Output: 12}},
	}

	cases := []struct {
		name      string
		current   string
		wantPrice modelPrice
		wantID    string
		ok        bool
	}{
		{"omniroute embeds vendor/model", "omniroute/openai/gpt-5.6-luna", idx["openai"]["gpt-5.6-luna"], "gpt-5.6-luna", true},
		{"direct provider match", "openai/gpt-5.6-luna", idx["openai"]["gpt-5.6-luna"], "gpt-5.6-luna", true},
		{"aliased provider", "gemini/gemini-3-pro", idx["google"]["gemini-3-pro"], "gemini-3-pro", true},
		{"unknown model", "openai/no-such-model", modelPrice{}, "no-such-model", false},
		{"no slash at all", "justamodel", modelPrice{}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotPrice, gotID, ok := lookupPrice(idx, c.current)
			if ok != c.ok || gotPrice != c.wantPrice || gotID != c.wantID {
				t.Errorf("lookupPrice(%q) = %+v, %q, %v; want %+v, %q, %v", c.current, gotPrice, gotID, ok, c.wantPrice, c.wantID, c.ok)
			}
		})
	}
}

func TestModelPriceCost(t *testing.T) {
	// gpt-5.6-luna's real rates: $0.2/$1.2 base, $0.4/$1.8 over 272000 input
	// tokens, both halved on flex.
	luna := modelPrice{
		Input: 0.2, Output: 1.2,
		LongContextThreshold: 272000, LongInput: 0.4, LongOutput: 1.8,
	}

	cases := []struct {
		name            string
		in, out         int
		tier            llm.ServiceTier
		modelID         string
		wantIn, wantOut float64
	}{
		{"short context, standard tier", 100_000, 500_000, "", "gpt-5.6-luna", 0.02, 0.6},
		{"short context, flex, eligible model", 100_000, 500_000, llm.ServiceTierFlex, "gpt-5.6-luna", 0.01, 0.3},
		{"short context, flex, ineligible model", 100_000, 500_000, llm.ServiceTierFlex, "gpt-5", 0.02, 0.6},
		{"long context, standard tier", 300_000, 500_000, "", "gpt-5.6-luna", 0.12, 0.9},
		{"long context, flex, eligible model", 300_000, 500_000, llm.ServiceTierFlex, "gpt-5.6-luna", 0.06, 0.45},
		{"exactly at threshold counts as long", 272000, 0, "", "gpt-5.6-luna", 0.4 * 272000 / 1e6, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotIn, gotOut := luna.cost(c.in, c.out, c.tier, c.modelID)
			if !closeEnough(gotIn, c.wantIn) || !closeEnough(gotOut, c.wantOut) {
				t.Errorf("cost(%d, %d, %q, %q) = %v, %v; want %v, %v", c.in, c.out, c.tier, c.modelID, gotIn, gotOut, c.wantIn, c.wantOut)
			}
		})
	}
}

func closeEnough(a, b float64) bool {
	d := a - b
	return d < 1e-9 && d > -1e-9
}
