package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/memmaker/gollum/llm"
)

// contextPreviewLen bounds the prompt snippet kept with each call: enough to
// tell two turns apart, short enough to sit in a column.
const contextPreviewLen = 40

// call is one request to a backend: what it cost, and a trimmed reminder of
// the user context it was sent with.
type call struct {
	Usage   llm.Usage
	Context string
}

// tokenStats keeps every per-request token count of this session, grouped by
// the model that produced it, plus a running total so the sidebar never has to
// re-sum the whole history to draw one line.
// ponytail: no mutex — written only from the Bubble Tea update loop.
type tokenStats struct {
	byModel map[string][]call
	sum     llm.Usage // running total over every model
	calls   int
}

// addCall files one request under a model slug and folds it into the running
// total. context is trimmed to contextPreviewLen.
func (t *tokenStats) addCall(model, context string, u llm.Usage) {
	if t.byModel == nil {
		t.byModel = map[string][]call{}
	}
	t.byModel[model] = append(t.byModel[model], call{Usage: u, Context: trimContext(context)})
	t.sum = addUsage(t.sum, u)
	t.calls++
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

// String is the full breakdown: one header per model, sorted by slug, then one
// line per call underneath.
func (t *tokenStats) String() string {
	models := make([]string, 0, len(t.byModel))
	for m := range t.byModel {
		models = append(models, m)
	}
	sort.Strings(models)

	var b strings.Builder
	for _, m := range models {
		s := t.perModel(m)
		fmt.Fprintf(&b, "%s\n", label.Render(m))
		fmt.Fprintf(&b, "%s\n", dim.Render(fmt.Sprintf("%d calls · %d in / %d out · %d reasoning · %d total",
			len(t.byModel[m]), s.InputTokens, s.OutputTokens, s.ReasoningTokens, s.TotalTokens)))
		for i, c := range t.byModel[m] {
			fmt.Fprintf(&b, "%3d  %6d in %6d out %6d reason %7d total  %s\n",
				i+1, c.Usage.InputTokens, c.Usage.OutputTokens, c.Usage.ReasoningTokens, c.Usage.TotalTokens, c.Context)
		}
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return dim.Render("no requests yet")
	}
	return b.String()
}

// trimContext squeezes a prompt onto one short line.
func trimContext(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return ansi.Truncate(s, contextPreviewLen, "…")
}

func addUsage(a, b llm.Usage) llm.Usage {
	a.InputTokens += b.InputTokens
	a.OutputTokens += b.OutputTokens
	a.TotalTokens += b.TotalTokens
	a.ReasoningTokens += b.ReasoningTokens
	return a
}
