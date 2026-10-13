package main

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/memmaker/gollum/llm"
	rec_files "github.com/memmaker/rec-files"
)

// envKeysByProvider lists the environment variables checked for each provider's
// API key, in order.
var envKeysByProvider = map[string][]string{
	"openai":      {"OPENAI_API_KEY", "OPENAI_API_TOKEN"},
	"huggingface": {"HUGGINGFACE_API_KEY", "HF_API_KEY"},
	"copilot":     {"GITHUB_TOKEN", "COPILOT_TOKEN"},
	"gemini":      {"GEMINI_API_KEY", "GOOGLE_API_KEY"},
	"lmstudio":    {"LMSTUDIO_API_KEY"},
	"omniroute":   {"OMNIROUTE_API_KEY"},
}

// keylessProviders serve locally and need no API key.
var keylessProviders = map[string]bool{"lmstudio": true, "omniroute": true, "ollama": true}

// configPath returns llm_model.rec from the current directory if present,
// otherwise ~/.config/gollum/config.rec, creating a default config if needed.
func configPath() (string, error) {
	if _, err := os.Stat("llm_model.rec"); err == nil {
		// Absolute, so the sidebar and the error messages name a file the
		// reader can actually find — "llm_model.rec" says nothing about which
		// directory won.
		return filepath.Abs("llm_model.rec")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	cfgDir := filepath.Join(home, ".config", "gollum")
	cfgPath := filepath.Join(cfgDir, "config.rec")
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if err := os.MkdirAll(cfgDir, 0700); err != nil {
			return "", fmt.Errorf("error creating config dir: %w", err)
		}
		f, err := os.Create(cfgPath)
		if err != nil {
			return "", fmt.Errorf("error creating config file: %w", err)
		}
		defer f.Close()
		defaultRecs := []rec_files.Record{{
			{Name: "provider", Value: "openai"},
			{Name: "model", Value: "gpt-5-mini"},
		}}
		if err := rec_files.Write(f, defaultRecs); err != nil {
			return "", fmt.Errorf("error writing config: %w", err)
		}
	}
	return cfgPath, nil
}

// LoadConfigs reads every record of the config file. Each record configures one
// provider: fields are `provider` (required), `model` (optional default model),
// `base_url` and `api_key_file` (both optional). API keys are resolved from the
// provider's environment variables, then from `api_key_file`, then — for the
// first record only, for backwards compatibility — from ./.apikey.
//
// Records without a usable API key are skipped (lmstudio needs none).
func LoadConfigs() ([]llm.Config, error) {
	cfgPath, err := configPath()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("error opening config file %s: %w", cfgPath, err)
	}
	recs := rec_files.Read(f)
	f.Close()

	var cfgs []llm.Config
	var skipped []string
	for i, rec := range recs {
		m := rec.ToMap(",")
		if m["settings"] != "" {
			continue // the UI settings record shares this file — see settings.go
		}
		if m["mcp"] != "" {
			continue // an MCP server record shares this file — see mcp.go
		}
		if m["tool"] != "" {
			continue // a built-in tool's toggle record shares this file — see tools.go
		}
		provider := m["provider"]
		if provider == "" {
			provider = "openai"
		}
		key := env(envKeysByProvider[provider]...)
		if key == "" {
			if path := m["api_key_file"]; path != "" {
				if b, err := os.ReadFile(expandHome(path)); err == nil {
					key = strings.TrimSpace(string(b))
				}
			}
		}
		if key == "" && i == 0 {
			if b, err := os.ReadFile(".apikey"); err == nil {
				key = strings.TrimSpace(string(b))
			}
		}
		if key == "" && !keylessProviders[provider] {
			skipped = append(skipped, provider)
			continue
		}
		cfgs = append(cfgs, llm.Config{
			Provider:        provider,
			APIKey:          key,
			Model:           m["model"],
			BaseURL:         m["base_url"],
			ServiceTier:     llm.ServiceTier(m["service_tier"]),
			ReasoningEffort: llm.ReasoningEffort(m["reasoning_effort"]),
			ContextLimit:    intField(m["context"]),
			Temp:            floatField(m["temp"]),
		})
	}
	if len(cfgs) == 0 {
		return nil, fmt.Errorf("no usable provider in %s (no API key for: %s); set the provider API key in the environment, point `api_key_file` at a key file, or create a .apikey file in the current directory", cfgPath, strings.Join(skipped, ", "))
	}
	return cfgs, nil
}

// MCPConfig is one `mcp:` record: a server started as a subprocess and talked
// to over stdio.
type MCPConfig struct {
	Name    string
	Command string
	Args    []string
	Enabled bool
}

// LoadMCPConfigs reads every `mcp:` record from the same config file
// LoadConfigs reads providers from. Enabled defaults true — a record with no
// `enabled` field is on, so a fresh config starts every MCP running.
func LoadMCPConfigs() ([]MCPConfig, error) {
	cfgPath, err := configPath()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("error opening config file %s: %w", cfgPath, err)
	}
	defer f.Close()

	var out []MCPConfig
	for _, rec := range rec_files.Read(f) {
		m := rec.ToMap(",")
		name := m["mcp"]
		if name == "" {
			continue
		}
		out = append(out, MCPConfig{
			Name:    name,
			Command: m["command"],
			Args:    strings.Fields(m["args"]),
			Enabled: boolField(m["enabled"], true),
		})
	}
	return out, nil
}

// LoadToolConfigs reads every `tool:` record from the config file. Enabled
// defaults true — a fresh config starts every tool enabled.
func LoadToolConfigs() ([]ToolConfig, error) {
	cfgPath, err := configPath()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("error opening config file %s: %w", cfgPath, err)
	}
	defer f.Close()

	seen := make(map[string]bool)
	var out []ToolConfig
	for _, rec := range rec_files.Read(f) {
		m := rec.ToMap(",")
		name := m["tool"]
		if name == "" {
			continue
		}
		seen[name] = true
		out = append(out, ToolConfig{
			Name:    name,
			Enabled: boolField(m["enabled"], true),
		})
	}
	for _, name := range builtinTools {
		if !seen[name] {
			out = append(out, ToolConfig{Name: name, Enabled: true})
		}
	}
	return out, nil
}

// SaveToolEnabled persists one tool's toggle state in place, in its own
// `tool:` record, leaving every other record in the file untouched.
func SaveToolEnabled(name string, enabled bool) error {
	cfgPath, err := configPath()
	if err != nil {
		return err
	}
	f, err := os.Open(cfgPath)
	if err != nil {
		return err
	}
	recs := rec_files.Read(f)
	f.Close()

	found := false
	for i, rec := range recs {
		if rec.ToMap(",")["tool"] == name {
			recs[i] = setField(rec, "enabled", onOff(enabled))
			found = true
		}
	}
	if !found {
		recs = append(recs, rec_files.Record{{Name: "tool", Value: name}, {Name: "enabled", Value: onOff(enabled)}})
	}
	return writeRecsAtomically(cfgPath, recs)
}

// SaveMCPEnabled persists one MCP's toggle state in place, in its own `mcp:`
// record, leaving every other record in the file untouched.
func SaveMCPEnabled(name string, enabled bool) error {
	cfgPath, err := configPath()
	if err != nil {
		return err
	}
	f, err := os.Open(cfgPath)
	if err != nil {
		return err
	}
	recs := rec_files.Read(f)
	f.Close()

	for i, rec := range recs {
		if rec.ToMap(",")["mcp"] == name {
			recs[i] = setField(rec, "enabled", onOff(enabled))
		}
	}
	return writeRecsAtomically(cfgPath, recs)
}

// SaveDefault makes provider/model the default for the next run: it sets the
// record's `model` and `reasoning_effort` fields and moves that record first,
// since the first usable record is what starts the session. Records for other
// providers keep their own settings.
//
// An empty effort means the model's own default, which is the absence of the
// field rather than an empty one — writing `reasoning_effort:` with nothing
// after it would be a value the reader then has to special-case.
func SaveDefault(provider, model string, effort llm.ReasoningEffort) error {
	cfgPath, err := configPath()
	if err != nil {
		return err
	}
	f, err := os.Open(cfgPath)
	if err != nil {
		return err
	}
	recs := rec_files.Read(f)
	f.Close()

	ordered := make([]rec_files.Record, 0, len(recs)+1)
	var target rec_files.Record
	for _, rec := range recs {
		if rec.ToMap(",")["provider"] != provider || target != nil {
			ordered = append(ordered, rec)
			continue
		}
		target = setEffort(setField(rec, "model", model), effort)
	}
	if target == nil {
		// A provider that is configured only through the environment still gets
		// remembered, as its own record.
		target = setEffort(rec_files.Record{{Name: "provider", Value: provider}, {Name: "model", Value: model}}, effort)
	}
	ordered = append([]rec_files.Record{target}, ordered...)
	return writeRecsAtomically(cfgPath, ordered)
}

// writeRecsAtomically writes recs to path through a temp file in the same
// directory, then renames it into place, so a failure partway through never
// leaves a half-written config behind.
func writeRecsAtomically(path string, recs []rec_files.Record) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config.*.rec")
	if err != nil {
		return err
	}
	if err := rec_files.Write(tmp, recs); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// setEffort writes the reasoning effort onto a record, or drops the field when
// the effort is the model's own default — see SaveDefault.
func setEffort(rec rec_files.Record, effort llm.ReasoningEffort) rec_files.Record {
	if effort == "" {
		return slices.DeleteFunc(rec, func(f rec_files.Field) bool { return f.Name == "reasoning_effort" })
	}
	return setField(rec, "reasoning_effort", string(effort))
}

func setField(rec rec_files.Record, name, value string) rec_files.Record {
	for i, field := range rec {
		if field.Name == name {
			rec[i].Value = value
			return rec
		}
	}
	return append(rec, rec_files.Field{Name: name, Value: value})
}

// intField reads a plain integer config field. Anything unparseable — an
// absent field included — is zero, which every caller already treats as
// "not set".
func intField(v string) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// floatField reads a plain decimal config field. Anything unparseable — an
// absent field included, or a negative number — is zero, which every caller
// already treats as "not set".
func floatField(v string) float64 {
	n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

// shortenHome is the inverse of expandHome: it writes a path under the home
// directory back as "~/...". An exact match becomes "~", and a sibling
// directory whose name merely starts with the home path is left alone.
func shortenHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}

// SystemPrompt returns ~/.config/gollum/system.md behind a line naming the
// machine it runs on. Without it a model has nothing to anchor a path to and
// invents one from thin air.
func SystemPrompt() string {
	var body []byte
	if home, err := os.UserHomeDir(); err == nil {
		body, _ = os.ReadFile(filepath.Join(home, ".config", "gollum", "system.md"))
	}
	return environmentLine() + "\n\n" + string(body)
}

// environmentLine is "OS: <os> - USER: <user> - CWD: <cwd>", with "?" for
// anything the OS won't tell us.
func environmentLine() string {
	name := "?"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "?"
	}
	return fmt.Sprintf("OS: %s - USER: %s - CWD: %s", runtime.GOOS, name, cwd)
}

// env returns the first non-empty environment variable among keys.
func env(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
