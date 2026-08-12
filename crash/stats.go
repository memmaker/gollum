package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/memmaker/gollum/llm"

	"github.com/memmaker/crash/ui"
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
// total. context is trimmed to contextPreviewLen. toolCalls and prose
// describe what that request's response actually did — a tool-calling leg of
// an agentic turn, a final answer, or (rarely) both.
func (t *tokenStats) addCall(model, context string, toolCalls int, prose bool, u llm.Usage) {
	if t.byModel == nil {
		t.byModel = map[string][]call{}
	}
	t.byModel[model] = append(t.byModel[model], call{
		Usage: u, Context: trimContext(context), ToolCalls: toolCalls, Prose: prose,
	})
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
	for _, m := range models {
		s := t.perModel(m)
		fmt.Fprintf(&b, "## %s\n\n", m)
		fmt.Fprintf(&b, "%d calls · %d in / %d out · %d reasoning · %d total\n\n",
			len(t.byModel[m]), s.InputTokens, s.OutputTokens, s.ReasoningTokens, s.TotalTokens)
		b.WriteString("| # | in | out | reasoning | total | tool calls | prose | context |\n")
		b.WriteString("|--:|---:|----:|----------:|------:|-----------:|:-----:|:--------|\n")
		for i, c := range t.byModel[m] {
			fmt.Fprintf(&b, "| %d | %d | %d | %d | %d | %d | %s | %s |\n",
				i+1, c.Usage.InputTokens, c.Usage.OutputTokens, c.Usage.ReasoningTokens, c.Usage.TotalTokens,
				c.ToolCalls, yesNo(c.Prose), escapePipes(c.Context))
		}
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return "no requests yet"
	}
	return strings.TrimRight(b.String(), "\n")
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
	return ui.Truncate(s, "…", contextPreviewLen)
}

func addUsage(a, b llm.Usage) llm.Usage {
	a.InputTokens += b.InputTokens
	a.OutputTokens += b.OutputTokens
	a.TotalTokens += b.TotalTokens
	a.ReasoningTokens += b.ReasoningTokens
	return a
}
