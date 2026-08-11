package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveDefaultMovesProviderFirst(t *testing.T) {
	t.Chdir(t.TempDir())
	const cfg = "provider: openai\nmodel: gpt-5-mini\napi_key_file: ~/keys/openai\n\n" +
		"provider: omniroute\nmodel: auto/best-coding\nbase_url: http://localhost:20128/v1\n"
	if err := os.WriteFile("llm_model.rec", []byte(cfg), 0600); err != nil {
		t.Fatal(err)
	}

	if err := SaveDefault("omniroute", "auto/best-fast"); err != nil {
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
	if err := SaveDefault("gemini", "gemini-2.0-flash"); err != nil {
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
