package main

import (
	"fmt"
	"os"
	"path/filepath"
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
var keylessProviders = map[string]bool{"lmstudio": true, "omniroute": true}

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
			Provider: provider,
			APIKey:   key,
			Model:    m["model"],
			BaseURL:  m["base_url"],
		})
	}
	if len(cfgs) == 0 {
		return nil, fmt.Errorf("no usable provider in %s (no API key for: %s); set the provider API key in the environment, point `api_key_file` at a key file, or create a .apikey file in the current directory", cfgPath, strings.Join(skipped, ", "))
	}
	return cfgs, nil
}

// SaveDefault makes provider/model the default for the next run: it sets the
// record's `model` field and moves that record first, since the first usable
// record is what starts the session. Records for other providers keep their own
// settings.
func SaveDefault(provider, model string) error {
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
		target = setField(rec, "model", model)
	}
	if target == nil {
		// A provider that is configured only through the environment still gets
		// remembered, as its own record.
		target = rec_files.Record{{Name: "provider", Value: provider}, {Name: "model", Value: model}}
	}
	ordered = append([]rec_files.Record{target}, ordered...)

	// Write through a temp file so a failure cannot leave a half-written config.
	tmp, err := os.CreateTemp(filepath.Dir(cfgPath), ".llm_model.*.rec")
	if err != nil {
		return err
	}
	if err := rec_files.Write(tmp, ordered); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), cfgPath)
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

func expandHome(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
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

// SystemPrompt returns ~/.config/gollum/system.md, or "" if absent.
func SystemPrompt() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, ".config", "gollum", "system.md"))
	if err != nil {
		return ""
	}
	return string(b)
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
