package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"

	"github.com/memmaker/gollum/llm"
)

// webSearchURL is LangSearch's web search API.
// https://docs.langsearch.com/api/web-search-api
const webSearchURL = "https://api.langsearch.com/v1/web-search"

// builtinTools names every tool the harness ships, in the order the Tools
// menu lists them. One list: registerTools, the menu and the config defaults
// all read it, so a new built-in cannot be registered but stay invisible.
var builtinTools = []string{
	"user_choice", "bash",
	"outline", "read", "patch", "write", "find_block",
	"remove_file", "rename_file",
	"web_search",
}

// filePathDescription is the description every file-path argument carries.
// "Path to the file" on its own left a model free to invent an absolute path
// out of nothing when it had no idea where it was running, so this spells the
// only two spellings that resolve out loud.
const filePathDescription = "Path to the file, either relative to the working directory named at the top of the system prompt, or absolute (a leading ~ is expanded to the home directory). Nothing else resolves. If you do not know where a file is, find it — do not guess at a path you have not seen in a tool result."

// ToolConfig is one built-in tool's state: whether it is enabled.
type ToolConfig struct {
	Name    string
	Enabled bool
}

// toolEnabled reports whether name should be offered to the model: true
// unless a `tool:` record in cfgs explicitly disables it. Absence means
// enabled — an MCP tool the user has never toggled has no record yet.
func toolEnabled(name string, cfgs []ToolConfig) bool {
	for _, c := range cfgs {
		if c.Name == name {
			return c.Enabled
		}
	}
	return true
}

// registerTools installs the harness tools on a session, but only ones the user
// has enabled. The llm library ships no tools of its own — anything that talks
// to a terminal lives here.
func registerTools(s *llm.Session, toolConfigs []ToolConfig) {
	if toolEnabled("user_choice", toolConfigs) {
		s.AddTool(llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "user_choice",
				Description: "Ask the user to pick one of several options in the terminal. Returns the chosen option, or an error if the user cancels.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"question": map[string]any{
							"type":        "string",
							"description": "Question shown above the options",
						},
						"options": map[string]any{
							"type":        "array",
							"items":       map[string]any{"type": "string"},
							"description": "The options to choose from",
						},
					},
					"required": []string{"question", "options"},
				},
			},
		}, execUserChoice)
	}

	if toolEnabled("bash", toolConfigs) {
		s.AddTool(llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "bash",
				Description: "Execute a shell command in the terminal. Returns the stdout output.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{
							"type":        "string",
							"description": "The shell command to execute",
						},
					},
					"required": []string{"command"},
				},
			},
		}, execBash)
	}

	if toolEnabled("outline", toolConfigs) {
		s.AddTool(llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "outline",
				Description: "Scan a file's shape before reading it: lists just the function/type/class signatures with their line numbers, no bodies, at a fraction of `read`'s context cost. Call this first on any file you haven't read yet — jump to `find_block` or `patch` on the LINE:HASH of the signature you want instead of reading the whole file. Only fall back to `read` for a file too small to bother outlining, or once you already know exactly which lines you need.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file": map[string]any{
							"type":        "string",
							"description": filePathDescription,
						},
					},
					"required": []string{"file"},
				},
			},
		}, execHashlineOutline)
	}

	if toolEnabled("read", toolConfigs) {
		s.AddTool(llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "read",
				Description: "Read a file for editing. Returns one numbered line per line, each with a short hash of its content: `12:a3f1|some code`. Those LINE:HASH anchors are what `patch` edits by. For a file you haven't seen yet, call `outline` first to see its shape for less context — reach for `read` once you know which lines you need, and pass start_line/end_line to fetch just those instead of the whole file.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file": map[string]any{
							"type":        "string",
							"description": filePathDescription,
						},
						"start_line": map[string]any{
							"type":        "integer",
							"description": "First line to read (1-based). Omit to start at the top.",
						},
						"end_line": map[string]any{
							"type":        "integer",
							"description": "Last line to read, inclusive. Omit to read to the end.",
						},
					},
					"required": []string{"file"},
				},
			},
		}, execHashlineRead)
	}

	if toolEnabled("patch", toolConfigs) {
		s.AddTool(llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "patch",
				Description: hashlinePatchDescription,
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file": map[string]any{
							"type":        "string",
							"description": filePathDescription,
						},
						"patch": map[string]any{
							"type":        "string",
							"description": "The operations to apply, in the format described above",
						},
						"dry_run": map[string]any{
							"type":        "boolean",
							"description": "Check the patch and show the result without writing anything",
						},
					},
					"required": []string{"file", "patch"},
				},
			},
		}, execHashlinePatch)
	}

	if toolEnabled("write", toolConfigs) {
		s.AddTool(llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "write",
				Description: "Create a file with the given content. Refuses to touch a file that already exists unless force is set — edit an existing file with `patch` instead. Returns a short confirmation, not the file back — call `read` if you need to see it with LINE:HASH anchors.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file": map[string]any{
							"type":        "string",
							"description": filePathDescription,
						},
						"content": map[string]any{
							"type":        "string",
							"description": "The file's full content",
						},
						"force": map[string]any{
							"type":        "boolean",
							"description": "Replace the file wholesale if it already exists",
						},
					},
					"required": []string{"file", "content"},
				},
			},
		}, execHashlineWrite)
	}

	if toolEnabled("find_block", toolConfigs) {
		s.AddTool(llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "find_block",
				Description: "Show the block of code around one LINE:HASH anchor — the function, branch or loop it sits in — instead of the whole file. Anchors come from read and go stale on every patch.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file": map[string]any{
							"type":        "string",
							"description": filePathDescription,
						},
						"anchor": map[string]any{
							"type":        "string",
							"description": "A LINE:HASH anchor inside the block, as `read` printed it, e.g. `12:a3f1`",
						},
					},
					"required": []string{"file", "anchor"},
				},
			},
		}, execHashlineFindBlock)
	}

	if toolEnabled("remove_file", toolConfigs) {
		s.AddTool(llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "remove_file",
				Description: "Delete a file.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"file": map[string]any{
							"type":        "string",
							"description": filePathDescription,
						},
					},
					"required": []string{"file"},
				},
			},
		}, execHashlineRemove)
	}

	if toolEnabled("rename_file", toolConfigs) {
		s.AddTool(llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "rename_file",
				Description: "Rename or move a file. Refuses to overwrite an existing destination.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"src": map[string]any{
							"type":        "string",
							"description": "Path to move from. " + filePathDescription,
						},
						"dst": map[string]any{
							"type":        "string",
							"description": "Path to move to. " + filePathDescription,
						},
					},
					"required": []string{"src", "dst"},
				},
			},
		}, execHashlineRename)
	}

	if toolEnabled("web_search", toolConfigs) {
		s.AddTool(llm.Tool{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "web_search",
				Description: "Search the web. Returns the raw JSON results.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"query": map[string]any{
							"type":        "string",
							"description": "The search query",
						},
					},
					"required": []string{"query"},
				},
			},
		}, execWebSearch)
	}
}

type userChoiceArgs struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
}

func execUserChoice(ctx context.Context, call llm.ToolCall) (string, error) {
	var args userChoiceArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "", fmt.Errorf("user_choice: parse args: %w", err)
	}
	answer, ok := ask(ctx, args.Question, args.Options)
	if !ok {
		return "", fmt.Errorf("user_choice: cancelled by user")
	}
	return answer, nil
}

type bashArgs struct {
	Command string `json:"command"`
}

func execBash(_ context.Context, call llm.ToolCall) (string, error) {
	var args bashArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "", fmt.Errorf("bash: parse args: %w", err)
	}
	out, err := exec.Command("sh", "-c", args.Command).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("error: %v\noutput: %s", err, string(out)), nil
	}
	return string(out), nil
}

type webSearchArgs struct {
	Query string `json:"query"`
}

func execWebSearch(ctx context.Context, call llm.ToolCall) (string, error) {
	var args webSearchArgs
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		return "", fmt.Errorf("web_search: parse args: %w", err)
	}
	apiKey := os.Getenv("LANGSEARCH_API_KEY")
	if apiKey == "" {
		return "error: LANGSEARCH_API_KEY not set", nil
	}
	body, err := json.Marshal(map[string]any{"query": args.Query})
	if err != nil {
		return "", fmt.Errorf("web_search: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webSearchURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("web_search: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Sprintf("error reading response: %v", err), nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Sprintf("error: status %d: %s", resp.StatusCode, string(out)), nil
	}
	return string(out), nil
}

// ask puts a choice overlay on screen from a tool's goroutine and blocks until
// the user answers — with one of the options, or with something they typed
// instead. The second return is false if they cancelled, which an empty
// answer is not.
//
// The wait ends on ctx too, so ctrl+x gets the turn back from a question
// nobody is going to answer. That withdraws the question as well: an overlay
// outliving the tool call it belongs to is a list where every answer goes
// nowhere.
func ask(ctx context.Context, question string, options []string) (string, bool) {
	if prog == nil {
		return "", false
	}
	reply := make(chan string, 1)
	prog.Send(askMsg{question: question, options: options, reply: reply})
	select {
	case answer, ok := <-reply:
		return answer, ok
	case <-ctx.Done():
		prog.Send(askCancelMsg{reply: reply})
		return "", false
	}
}
