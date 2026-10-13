package main

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	rec_files "github.com/memmaker/rec-files"
)

// sortedKeys gives the languages a stable order for both the file and the menu.
func sortedKeys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }

// Settings are the menu toggles that outlive a session. They live in a record
// of their own in ~/.config/gollum/config.rec, tagged `settings: ui` so the
// provider records in the same file stay untouched.
type Settings struct {
	SendTools bool
	FullWidth bool
	Session   bool // carry the conversation into each request
	// AutoCompact summarizes the conversation when a request would overrun
	// the model's context window, instead of letting the backend refuse it.
	AutoCompact bool
	TTSAnswers  bool
	TTSLang     string            // which engine speaks, by key
	TTSEngines  map[string]string // language key -> engine command
	Timeout     time.Duration     // idle limit before a backend is considered hung
}

// defaultSettings is what a fresh install gets: tools on, output width capped,
// answers not read out loud, timeout at 2 minutes. Its map is never handed out —
// LoadSettings copies it, so a config file cannot edit the defaults of the next call.
var defaultSettings = Settings{
	SendTools:   true,
	FullWidth:   false,
	Session:     true,
	AutoCompact: true,
	TTSLang:     "de",
	TTSEngines:  map[string]string{"de": "tts-de-male-kokoro", "en": "tts-en-male-kokoro"},
	Timeout:     120 * time.Second, // llm.DefaultTimeout
}

// ttsEnginePrefix marks a per-language engine field: `tts_engine_de: NAME`
// adds or overrides the engine for key `de`. Any key works, not just the two
// that ship as defaults.
const ttsEnginePrefix = "tts_engine_"

func defaults() Settings {
	s := defaultSettings
	s.TTSEngines = maps.Clone(defaultSettings.TTSEngines)
	return s
}

// settingsPath is always the user's config, never a project-local
// llm_model.rec: these are preferences, not a project's provider setup.
func settingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "gollum", "config.rec"), nil
}

// LoadSettings reads the settings record. A missing file or record leaves the
// defaults in place, which is not an error.
func LoadSettings() Settings {
	s := defaults()
	path, err := settingsPath()
	if err != nil {
		return s
	}
	f, err := os.Open(path)
	if err != nil {
		return s
	}
	defer f.Close()
	for _, rec := range rec_files.Read(f) {
		m := rec.ToMap(",")
		if m["settings"] == "" {
			continue // a provider record
		}
		s.SendTools = boolField(m["send_tools"], s.SendTools)
		s.FullWidth = boolField(m["full_width"], s.FullWidth)
		s.Session = boolField(m["session"], s.Session)
		s.AutoCompact = boolField(m["auto_compact"], s.AutoCompact)
		s.TTSAnswers = boolField(m["tts_answers"], s.TTSAnswers)
		if v := m["tts_lang"]; v != "" {
			s.TTSLang = v
		}
		if v := m["timeout"]; v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				s.Timeout = d
			}
		}
		for name, v := range m {
			if lang, ok := strings.CutPrefix(name, ttsEnginePrefix); ok && lang != "" && v != "" {
				s.TTSEngines[lang] = v
			}
		}
	}
	return s
}

// SaveSettings writes the settings record back, keeping every other record in
// the file as it is.
func SaveSettings(s Settings) error {
	path, err := settingsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}

	var recs []rec_files.Record
	if f, err := os.Open(path); err == nil {
		for _, rec := range rec_files.Read(f) {
			if rec.ToMap(",")["settings"] == "" {
				recs = append(recs, rec) // provider records survive untouched
			}
		}
		f.Close()
	}
	rec := rec_files.Record{
		{Name: "settings", Value: "ui"},
		{Name: "send_tools", Value: onOff(s.SendTools)},
		{Name: "full_width", Value: onOff(s.FullWidth)},
		{Name: "session", Value: onOff(s.Session)},
		{Name: "auto_compact", Value: onOff(s.AutoCompact)},
		{Name: "tts_answers", Value: onOff(s.TTSAnswers)},
		{Name: "tts_lang", Value: s.TTSLang},
		{Name: "timeout", Value: s.Timeout.String()},
	}
	// Sorted, so the file does not reshuffle itself on every save.
	for _, lang := range sortedKeys(s.TTSEngines) {
		rec = append(rec, rec_files.Field{Name: ttsEnginePrefix + lang, Value: s.TTSEngines[lang]})
	}
	recs = append(recs, rec)
	return writeRecsAtomically(path, recs)
}

// boolField reads "on"/"off" — what the menu labels say — and falls back to
// the current value for anything else, including an absent field.
func boolField(v string, fallback bool) bool {
	switch v {
	case "on", "true", "yes", "1":
		return true
	case "off", "false", "no", "0":
		return false
	}
	return fallback
}
