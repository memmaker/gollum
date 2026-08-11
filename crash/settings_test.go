package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestMain gives the whole test binary a home of its own. Several tests reach
// saveSettings through the menu, and settingsPath is absolute — without this
// they rewrite the developer's own ~/.config/gollum/config.rec, silently
// resetting whatever the bare test `tui` has no value for.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "crash-test-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// The settings share config.rec with the provider setup, so a save must not
// eat the provider records — and LoadConfigs must not read the settings record
// as a provider.
func TestSettingsRoundTripKeepsProviders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".config", "gollum")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(cfgDir, "config.rec")
	if err := os.WriteFile(cfgPath, []byte("provider: openai\nmodel: gpt-5-mini\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := LoadSettings(); !reflect.DeepEqual(got, defaults()) {
		t.Fatalf("no settings record: got %+v, want the defaults %+v", got, defaults())
	}
	want := Settings{
		SendTools:  false,
		FullWidth:  true,
		TTSAnswers: true,
		TTSLang:    "en",
		TTSEngines: map[string]string{"de": "tts-de-female-kokoro", "en": "tts-en-male-kokoro"},
	}
	if err := SaveSettings(want); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if got := LoadSettings(); !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}

	body, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "gpt-5-mini") {
		t.Fatalf("provider record lost:\n%s", body)
	}
	// Saving twice must not stack up settings records.
	if err := SaveSettings(want); err != nil {
		t.Fatal(err)
	}
	body, _ = os.ReadFile(cfgPath)
	if n := strings.Count(string(body), "settings: ui"); n != 1 {
		t.Fatalf("%d settings records, want 1:\n%s", n, body)
	}

	// The settings record must not become a bogus provider.
	t.Chdir(t.TempDir()) // no local llm_model.rec, so the home config is used
	t.Setenv("OPENAI_API_KEY", "x")
	cfgs, err := LoadConfigs()
	if err != nil {
		t.Fatalf("LoadConfigs: %v", err)
	}
	if len(cfgs) != 1 || cfgs[0].Model != "gpt-5-mini" {
		t.Fatalf("configs = %+v, want just the openai record", cfgs)
	}
}
