package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// CompactionPrompt replaces the system prompt for the single call that
// summarizes a conversation grown too long for its context window. It is the
// whole instruction: everything after it is the conversation being summarized,
// not a conversation being continued, and the model must be told that plainly
// or it answers the last question again instead of describing it.
//
// The section list is where the open harnesses that do this well converge —
// Claude Code's /compact, opencode's summarizer, crush's session summaries.
// All of them ask for structure rather than prose, for the same reason: a
// free-form summary keeps what reads well and drops the two things the next
// turn actually needs, which are what was being worked on and what was about
// to happen next.
const CompactionPrompt = `You are summarizing a conversation between a user and an AI assistant so the assistant can keep working after its context window has been cleared. Everything that follows is that conversation, tool calls and tool results included.

Write the summary as the only thing the assistant will still have. Be specific: names, paths, identifiers, signatures, exact values, exact error text. Spend your detail on recent and unfinished work rather than on completeness about what is already settled. Do not address the user, do not comment on the summary itself, and do not state anything the conversation does not.

Use these sections, in this order, dropping any that would be empty:

1. Goal — what the user asked for, in their own terms, including later corrections to it.
2. Context — what has been established: files, paths, identifiers, APIs, data, and what was learned about them.
3. Decisions — what was chosen, why, and what was ruled out.
4. Work done — the changes made and the tool calls that mattered, with their outcomes.
5. Errors — what failed, the exact messages, and what fixed them or failed to.
6. State — what is true right now: finished, half-done, broken.
7. Next — the immediate next step, plus anything asked for and still not done.
8. Open questions — what the assistant needs from the user to continue.`

// compactAsk is the turn that asks for the summary. The instruction is
// already the system prompt, so this only has to be a user turn at all: a
// message list ending on a tool result is not something every backend will
// answer, and Gemini in particular expects a function response to be followed
// by something other than "your turn".
const compactAsk = "Summarize the conversation above as instructed."

// compactedIntro and compactedOutro wrap the summary in the user message that
// replaces the conversation. The outro is what keeps the next turn honest:
// without being told the history is gone, a model happily refers back to a
// file listing it can no longer see.
const compactedIntro = "Summary of the conversation so far, compacted to fit the context window:"

const compactedOutro = "The history this summarizes is gone — the summary above is all that is left of it. Continue the work from here, starting with the next step it names. If anything you need is missing or unclear, ask me rather than acting on a guess."

// compactThreshold is the share of the context window at which a request is
// summarized instead of sent. Not 1.0: the estimate is an estimate, and the
// answer still has to fit alongside the prompt.
const compactThreshold = 0.60

// charsPerToken is the usual rough conversion for English prose and code. It
// is only the starting point — calibrate replaces it with the ratio the
// backend's own tokenizer reported as soon as one request has been counted.
const charsPerToken = 4

// perMessageOverhead is the framing every message costs on top of its text:
// role, delimiters, and the few tokens the chat template adds around it.
const perMessageOverhead = 4

// approxTokens estimates what a request would count as, from its own bytes.
// Nothing in this package can tokenize a provider's vocabulary, and asking a
// backend to count for us costs the round trip we are trying to avoid.
func approxTokens(msgs []Message, tools []Tool) int {
	chars := 0
	for _, m := range msgs {
		chars += len(m.Role) + len(m.Content) + len(m.Name) + len(m.ToolCallID) + perMessageOverhead
		for _, tc := range m.ToolCalls {
			chars += len(tc.ID) + len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	// Tool schemas are sent in full with every request and are not small —
	// a handful of MCP servers outweighs the conversation for a while.
	if len(tools) > 0 {
		if b, err := json.Marshal(tools); err == nil {
			chars += len(b)
		}
	}
	return chars / charsPerToken
}

// estimateTokens is approxTokens corrected by whatever the backend last
// reported, so the guess drifts towards this model's real tokenizer instead of
// staying at a constant that is wrong for code, wrong for CJK, and wrong for
// every provider's message framing.
func (s *Session) estimateTokens(msgs []Message, tools []Tool) int {
	n := approxTokens(msgs, tools)
	s.mu.Lock()
	calib := s.calib
	s.mu.Unlock()
	if calib > 0 {
		return int(float64(n) * calib)
	}
	return n
}

// calibrate records how far off approxTokens was for a request the backend
// counted, so the next estimate is scaled by it. Called only with the exact
// messages and tools that produced u — feeding it a different request than
// the one that was billed is how the ratio goes wrong in the direction that
// matters, too low, and stops compaction from ever triggering.
func (s *Session) calibrate(u Usage, msgs []Message, tools []Tool) {
	if u.InputTokens <= 0 {
		return
	}
	approx := approxTokens(msgs, tools)
	if approx <= 0 {
		return
	}
	s.mu.Lock()
	s.calib = float64(u.InputTokens) / float64(approx)
	s.mu.Unlock()
}

// needsCompaction reports whether this request should be summarized before it
// is sent. Stateless sessions are left alone: their transcript is a record the
// user can switch back on, not something the request is built from, and
// throwing it away to shrink a request that does not contain it would be a
// straight loss.
func (s *Session) needsCompaction(msgs []Message, tools []Tool) bool {
	if !s.AutoCompact || s.ContextLimit <= 0 || s.Stateless {
		return false
	}
	if s.estimateTokens(msgs, tools) < int(float64(s.ContextLimit)*compactThreshold) {
		return false
	}
	// A conversation already down to its own summary has nothing left to
	// give up; summarizing it again spends a call to produce the same
	// request. Three is the smallest exchange worth the round trip.
	n := 0
	for _, m := range msgs {
		if m.Role != RoleSystem {
			n++
		}
	}
	return n >= 3
}

// compact summarizes msgs and returns the message list that replaces them:
// the original system messages, then one user message carrying the summary.
// The summarizing call itself runs with CompactionPrompt as its system prompt
// and no tools — it is being asked to describe the conversation, not to
// continue it.
func (s *Session) compact(ctx context.Context, msgs []Message) (compacted []Message, summary string, err error) {
	var system, rest []Message
	for _, m := range msgs {
		if m.Role == RoleSystem {
			system = append(system, m)
		} else {
			rest = append(rest, m)
		}
	}
	if len(rest) == 0 {
		return nil, "", errors.New("llm: nothing to compact")
	}

	req := make([]Message, 0, len(rest)+2)
	req = append(req, Message{Role: RoleSystem, Content: CompactionPrompt})
	req = append(req, rest...)
	req = append(req, Message{Role: RoleUser, Content: compactAsk})

	ctx, cancel := s.withTimeout(ctx)
	defer cancel()
	resp, err := s.client.Chat(ctx, s.request(req, nil))
	if err != nil {
		return nil, "", asTimeout(err)
	}
	s.addUsage(resp.Usage)

	summary = strings.TrimSpace(resp.Message.Content)
	if summary == "" {
		return nil, "", errors.New("llm: compaction produced no summary")
	}
	out := append([]Message{}, system...)
	return append(out, Message{
		Role:    RoleUser,
		Content: compactedIntro + "\n\n" + summary + "\n\n" + compactedOutro,
	}), summary, nil
}

// Compress summarizes the conversation so far and replaces the history with
// that summary, so a long-running session can keep going without an
// ever-growing context. This is what the interactive /compress does; the same
// summarizing runs by itself once a request would cross compactThreshold of
// ContextLimit (see AutoCompact).
func (s *Session) Compress(ctx context.Context) (string, error) {
	// Same bookkeeping a streamed turn does: this is a real call to the
	// backend, and a caller watching Err has no other way to hear that it
	// failed — or that the last turn's failure is no longer the current one.
	s.setErr(nil)
	out, summary, err := s.compact(ctx, s.snapshotHistory())
	if err != nil {
		s.setErr(err)
		return "", err
	}
	s.mu.Lock()
	s.messages = out
	s.mu.Unlock()
	s.logChange(out, true)
	return summary, nil
}
