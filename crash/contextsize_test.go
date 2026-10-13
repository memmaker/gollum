package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/memmaker/gollum/llm"
)

// The three sources answer in the order of how much the number was meant for
// this exact model, and a models.dev answer is cached on the way out.
func TestResolveContextSizePrecedence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfgs := []llm.Config{{Provider: "ollama", ContextLimit: 32000}}
	prices := priceIndex{"openai": {"gpt-5-mini": modelPrice{Context: 400000}}}

	if size, src := resolveContextSize("ollama/qwen3:latest", cfgs, prices); size != 32000 || src != "config" {
		t.Errorf("got %d from %q, want 32000 from the provider record", size, src)
	}
	if size, src := resolveContextSize("openai/gpt-5-mini", nil, prices); size != 400000 || src != "models.dev" {
		t.Errorf("got %d from %q, want 400000 from models.dev", size, src)
	}
	// That lookup cached itself, and the cache now answers first — without
	// shadowing the record, which was never cached over.
	if size, src := resolveContextSize("openai/gpt-5-mini", nil, nil); size != 400000 || src != contextMapFile {
		t.Errorf("got %d from %q, want the cached 400000", size, src)
	}
	if size, _ := resolveContextSize("ollama/qwen3:latest", cfgs, prices); size != 32000 {
		t.Errorf("the record was shadowed by the cache: %d", size)
	}

	// What /context writes beats both, since it is the only one the user
	// could have corrected from inside the session.
	if err := saveContextSize("ollama/qwen3:latest", 64000); err != nil {
		t.Fatal(err)
	}
	if size, src := resolveContextSize("ollama/qwen3:latest", cfgs, prices); size != 64000 || src != contextMapFile {
		t.Errorf("got %d from %q, want the 64000 that was set by hand", size, src)
	}

	if size, src := resolveContextSize("nobody/knows", nil, nil); size != 0 || src != "" {
		t.Errorf("got %d from %q, want nothing known", size, src)
	}
}

// The file is meant to be edited by hand, so a junk line costs that line and
// nothing else — and a model slug with a colon in it survives the parse,
// which is the whole reason the separator is two of them.
func TestContextMapParsing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".config", "gollum")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	const file = `# a comment
ollama/qwen3:latest::64000

garbage
openai/gpt-5-mini :: 400000
broken/model::not-a-number
zero/model::0
`
	if err := os.WriteFile(filepath.Join(dir, contextMapFile), []byte(file), 0600); err != nil {
		t.Fatal(err)
	}
	m := loadContextMap()
	if m["ollama/qwen3:latest"] != 64000 {
		t.Errorf("a model slug with a colon did not survive: %+v", m)
	}
	if m["openai/gpt-5-mini"] != 400000 {
		t.Errorf("spaces around the separator broke the parse: %+v", m)
	}
	if len(m) != 2 {
		t.Errorf("unparseable lines were not skipped: %+v", m)
	}

	// A save keeps everything already in the file.
	if err := saveContextSize("new/model", 8000); err != nil {
		t.Fatal(err)
	}
	if m = loadContextMap(); len(m) != 3 || m["ollama/qwen3:latest"] != 64000 || m["new/model"] != 8000 {
		t.Errorf("after saving: %+v", m)
	}
}
