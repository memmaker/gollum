package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/memmaker/gollum/llm"
)

func TestSaveDefaultMovesProviderFirst(t *testing.T) {
	t.Chdir(t.TempDir())
	const cfg = "provider: openai\nmodel: gpt-5-mini\napi_key_file: ~/keys/openai\n\n" +
		"provider: omniroute\nmodel: auto/best-coding\nbase_url: http://localhost:20128/v1\n"
	if err := os.WriteFile("llm_model.rec", []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}

	if err := SaveDefault("omniroute", "auto/best-fast", llm.ReasoningEffortLow); err != nil {
		t.Fatal(err)
	}
	cfgs, err := LoadConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfgs) != 1 {
		// openai has no key here, so it is skipped on load but must survive in
		// the file — checked below.
		t.Fatalf("got %d usable configs, want 1: %+v", len(cfgs), cfgs)
	}
	if cfgs[0].Provider != "omniroute" || cfgs[0].Model != "auto/best-fast" {
		t.Fatalf("default is %s/%s, want omniroute/auto/best-fast", cfgs[0].Provider, cfgs[0].Model)
	}
	if cfgs[0].ReasoningEffort != llm.ReasoningEffortLow {
		t.Fatalf("reasoning effort is %q, want low", cfgs[0].ReasoningEffort)
	}
	if cfgs[0].BaseURL != "http://localhost:20128/v1" {
		t.Fatalf("base_url lost: %q", cfgs[0].BaseURL)
	}

	written, err := os.ReadFile("llm_model.rec")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"provider: openai", "model: gpt-5-mini", "api_key_file: ~/keys/openai"} {
		if !strings.Contains(string(written), want) {
			t.Fatalf("rewritten config lost %q:\n%s", want, written)
		}
	}

	// A provider with no record yet gets one.
	if err := SaveDefault("gemini", "gemini-2.0-flash", ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GEMINI_API_KEY", "test-key")
	cfgs, err = LoadConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if cfgs[0].Provider != "gemini" || cfgs[0].Model != "gemini-2.0-flash" {
		t.Fatalf("default is %s/%s, want gemini/gemini-2.0-flash", cfgs[0].Provider, cfgs[0].Model)
	}

	// Back to the model's own default: the field goes away rather than being
	// written empty, and the record it belonged to keeps everything else.
	if err := SaveDefault("omniroute", "auto/best-fast", ""); err != nil {
		t.Fatal(err)
	}
	written, err = os.ReadFile("llm_model.rec")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), "reasoning_effort") {
		t.Fatalf("an empty effort left the field behind:\n%s", written)
	}
	if !strings.Contains(string(written), "base_url: http://localhost:20128/v1") {
		t.Fatalf("clearing the effort lost the rest of the record:\n%s", written)
	}
}

func TestLoadMCPConfigsDefaultsEnabled(t *testing.T) {
	t.Chdir(t.TempDir())
	const cfg = "provider: openai\nmodel: gpt-5-mini\n\n" +
		"mcp: hashline\ncommand: hashline\nargs: mcp\n\n" +
		"mcp: other\ncommand: other-server\nargs: --stdio --verbose\nenabled: off\n"
	if err := os.WriteFile("llm_model.rec", []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}

	mcps, err := LoadMCPConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(mcps) != 2 {
		t.Fatalf("got %d mcp configs, want 2: %+v", len(mcps), mcps)
	}
	if mcps[0].Name != "hashline" || mcps[0].Command != "hashline" ||
		!slices.Equal(mcps[0].Args, []string{"mcp"}) || !mcps[0].Enabled {
		t.Fatalf("hashline config = %+v, want enabled with args [mcp]", mcps[0])
	}
	if mcps[1].Name != "other" || mcps[1].Enabled {
		t.Fatalf("other config = %+v, want enabled=false", mcps[1])
	}
	if !slices.Equal(mcps[1].Args, []string{"--stdio", "--verbose"}) {
		t.Fatalf("other args = %v, want [--stdio --verbose]", mcps[1].Args)
	}

	// A provider record still loads fine alongside mcp records.
	t.Setenv("OPENAI_API_KEY", "test-key")
	cfgs, err := LoadConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfgs) != 1 || cfgs[0].Provider != "openai" {
		t.Fatalf("LoadConfigs got %+v, want just the openai provider", cfgs)
	}
}

func TestSaveMCPEnabledTogglesInPlace(t *testing.T) {
	t.Chdir(t.TempDir())
	const cfg = "provider: openai\nmodel: gpt-5-mini\n\n" +
		"mcp: hashline\ncommand: hashline\nargs: mcp\n"
	if err := os.WriteFile("llm_model.rec", []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}

	if err := SaveMCPEnabled("hashline", false); err != nil {
		t.Fatal(err)
	}
	mcps, err := LoadMCPConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(mcps) != 1 || mcps[0].Enabled {
		t.Fatalf("after disabling, got %+v, want enabled=false", mcps)
	}

	written, err := os.ReadFile("llm_model.rec")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "provider: openai") {
		t.Fatalf("rewritten config lost the provider record:\n%s", written)
	}

	if err := SaveMCPEnabled("hashline", true); err != nil {
		t.Fatal(err)
	}
	mcps, err = LoadMCPConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if !mcps[0].Enabled {
		t.Fatal("re-enabling did not stick")
	}
}

func TestShortenHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	for in, want := range map[string]string{
		home:                        "~",
		filepath.Join(home, "code"): filepath.Join("~", "code"),
		home + "-backup":            home + "-backup", // a sibling, not a child
		"/etc":                      "/etc",
	} {
		if got := shortenHome(in); got != want {
			t.Errorf("shortenHome(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSystemPromptStartsWithEnvironmentLine(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	line, _, _ := strings.Cut(SystemPrompt(), "\n")
	if !strings.HasSuffix(line, " - CWD: "+cwd) || !strings.HasPrefix(line, "OS: "+runtime.GOOS+" - ") {
		t.Fatalf("first line %q does not name %s and %s", line, runtime.GOOS, cwd)
	}
}
