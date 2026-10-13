package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/memmaker/gollum/llm"

	markterm "github.com/memmaker/gollum/mark-term"
)

// contextPreviewLen bounds the prompt snippet kept with each call: enough to
// tell two turns apart, short enough to sit in a column.
const contextPreviewLen = 40

// call is one request to a backend: what it cost, and a trimmed reminder of
// the user context it was sent with.
type call struct {
	Usage     llm.Usage
	Context   string
	ToolCalls int  // tool calls this request's response made
	Prose     bool // whether it also carried answer content
	Tier      llm.ServiceTier

	// InputCost and OutputCost are what this one request cost in USD, fixed
	// at the price in effect when it was recorded — a later price refresh
	// does not reprice history. Both are 0 when the model has no known price.
	InputCost, OutputCost float64
}

// tokenStats keeps every per-request token count of this session, grouped by
// the model that produced it, plus a running total so the sidebar never has to
// re-sum the whole history to draw one line.
// ponytail: no mutex — written only from the Bubble Tea update loop.
type tokenStats struct {
	byModel map[string][]call
	sum     llm.Usage // running total over every model
	calls   int
	prices  priceIndex // models.dev cost table, empty when unavailable
	costSum float64    // running total of every call's InputCost+OutputCost, so the sidebar's $ figure is O(1)

	// genTokens and genSeconds accumulate the output tokens and the wall
	// time of every call that reported a generation duration, so the average
	// rate is the ratio of the sums rather than the mean of the per-call
	// rates: a two-token tool leg would otherwise weigh as much as a full
	// answer. lastRate is the most recent timed call on its own.
	genTokens  int
	genSeconds float64
	lastRate   float64
}

// genWindow is the part of a leg whose generation could actually be timed:
// the stretch from its first streamed chunk to its last, and the output
// tokens produced inside it.
//
// The distinction matters because the clock only starts when the first chunk
// lands — everything that chunk carried was generated before that, outside
// the window. Charging those tokens to it is what produced the impossible
// rates: a tool-calling leg arrives as a single aggregated chunk, so its
// window is the few hundred nanoseconds between "first chunk" and "last
// chunk" being the same chunk, and its hundred-odd argument tokens divided by
// that read as hundreds of millions of tokens per second.
type genWindow struct {
	tokens int
	d      time.Duration
}

// addCall files one request under a model slug, prices it against t.prices
// right away, and folds both the tokens and the cost into the running
// totals. context is trimmed to contextPreviewLen. toolCalls and prose
// describe what that request's response actually did — a tool-calling leg of
// an agentic turn, a final answer, or (rarely) both. gen is the timed slice of
// the response's generation; a zero window leaves the rate figures untouched
// rather than dividing by nothing.
func (t *tokenStats) addCall(model, context string, toolCalls int, prose bool, u llm.Usage, tier llm.ServiceTier, gen genWindow) {
	if t.byModel == nil {
		t.byModel = map[string][]call{}
	}
	price, modelID, ok := lookupPrice(t.prices, model)
	// flex is a narrow OpenAI allowlist (see flexEligible): the session's
	// service tier is one setting shared by every model, so switching to a
	// model that was never eligible — a Gemini model through omniroute, say
	// — still carries whatever tier was last set. Recording "flex" for a
	// call that was never actually billed at that tier would just be wrong,
	// not merely unpriced, so it's downgraded to the standard tier here,
	// same eligibility check the cost math and the sidebar heading use. A
	// model absent from the pricing table (ok == false, e.g. an omniroute
	// alias) is unverified, not eligible, so it's downgraded too.
	if tier == llm.ServiceTierFlex && !(ok && flexEligible[modelID]) {
		tier = ""
	}
	c := call{Usage: u, Context: trimContext(context), ToolCalls: toolCalls, Prose: prose, Tier: tier}
	if ok {
		c.InputCost, c.OutputCost = price.cost(u.InputTokens, u.OutputTokens, tier, modelID)
	}
	t.byModel[model] = append(t.byModel[model], c)
	t.sum = addUsage(t.sum, u)
	t.costSum += c.InputCost + c.OutputCost
	t.calls++
	if gen.d > 0 && gen.tokens > 0 {
		t.lastRate = float64(gen.tokens) / gen.d.Seconds()
		t.genTokens += gen.tokens
		t.genSeconds += gen.d.Seconds()
	}
}

// rates returns the output tokens per second of the most recent timed call
// and the session average over every timed call. ok is false until one call
// has been timed — with nothing measured there is no rate to show, which is
// not the same as a rate of zero.
func (t *tokenStats) rates() (last, avg float64, ok bool) {
	if t.genSeconds <= 0 {
		return 0, 0, false
	}
	return t.lastRate, float64(t.genTokens) / t.genSeconds, true
}

// total is the accumulated usage over every model, O(1).
func (t *tokenStats) total() llm.Usage { return t.sum }

// perModel sums one model's requests.
func (t *tokenStats) perModel(model string) llm.Usage {
	var sum llm.Usage
	for _, c := range t.byModel[model] {
		sum = addUsage(sum, c.Usage)
	}
	return sum
}

// totalCost is the accumulated cost over every model, O(1) — see costSum.
func (t *tokenStats) totalCost() float64 { return t.costSum }

// String is the full breakdown as markdown: one heading per model, sorted by
// slug, a summary line, then a table with one row per call. Markdown rather
// than hand-aligned columns so the pager can render it — and so copying a
// row out lands somewhere useful.
func (t *tokenStats) String() string {
	models := make([]string, 0, len(t.byModel))
	for m := range t.byModel {
		models = append(models, m)
	}
	sort.Strings(models)

	var b strings.Builder
	if cost := t.totalCost(); cost > 0 {
		fmt.Fprintf(&b, "**total: $%.4f**\n\n", cost)
	}
	for _, m := range models {
		s := t.perModel(m)
		fmt.Fprintf(&b, "## %s\n\n", m)
		if price, _, ok := lookupPrice(t.prices, m); ok {
			fmt.Fprintf(&b, "%d calls · %d in / %d out (of which %d reasoning) · %d total · %s token max. context size\n\n",
				len(t.byModel[m]), s.InputTokens, s.OutputTokens, s.ReasoningTokens, s.TotalTokens,
				commaInt(price.Context))
		} else {
			fmt.Fprintf(&b, "%d calls · %d in / %d out (of which %d reasoning) · %d total\n\n",
				len(t.byModel[m]), s.InputTokens, s.OutputTokens, s.ReasoningTokens, s.TotalTokens)
		}
		b.WriteString("| # | in | out | reasoning | total | in $ | out $ | tools | prose | tier | context |\n")
		b.WriteString("|--:|---:|----:|----------:|------:|-----:|------:|------:|:-----:|:-----|:--------|\n")
		for i, c := range t.byModel[m] {
			tier := string(c.Tier)
			if tier == "" {
				tier = "standard"
			}
			// 6 decimals, not 4: a single cheap-tier turn is often a few
			// hundredths of a cent, which $%.4f rounds two or more distinct
			// requests down to the same "$0.0001".
			fmt.Fprintf(&b, "| %d | %d | %d | %d | %d | $%.6f | $%.6f | %d | %s | %s | %s |\n",
				i+1, c.Usage.InputTokens, c.Usage.OutputTokens, c.Usage.ReasoningTokens, c.Usage.TotalTokens,
				c.InputCost, c.OutputCost, c.ToolCalls, yesNo(c.Prose), tier, escapePipes(c.Context))
		}
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return "no requests yet"
	}
	return strings.TrimRight(b.String(), "\n")
}

// commaInt renders n with thousands separators, e.g. 1050000 -> "1,050,000".
func commaInt(n int) string {
	s := fmt.Sprintf("%d", n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// lastCall returns the most recently recorded call for a model, so the
// sidebar can show what fraction of its context window that one request used.
func (t *tokenStats) lastCall(model string) (call, bool) {
	cs := t.byModel[model]
	if len(cs) == 0 {
		return call{}, false
	}
	return cs[len(cs)-1], true
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// escapePipes keeps a prompt containing "|" from splitting its own table row.
func escapePipes(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

// trimContext squeezes a prompt onto one short line.
func trimContext(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return markterm.Truncate(s, "…", contextPreviewLen)
}

func addUsage(a, b llm.Usage) llm.Usage {
	a.InputTokens += b.InputTokens
	a.OutputTokens += b.OutputTokens
	a.TotalTokens += b.TotalTokens
	a.ReasoningTokens += b.ReasoningTokens
	return a
}

// callTally is the dim line closing a turn: the backend's total time across
// every request of the turn. Tool execution happens between the requests and
// is counted in none of them, so an agentic turn's tally is smaller than the
// wall time it took on screen — see the ticking elapsed time in the
// statusbar for that.
func callTally(ds []time.Duration) string {
	var total time.Duration
	for _, d := range ds {
		total += d
	}
	return fmt.Sprintf("took %.1fs", total.Seconds())
}
