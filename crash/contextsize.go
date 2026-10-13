package main

import (
	"bufio"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/memmaker/gollum/llm"
)

// contextMapFile is the plain-text override and cache for context window
// sizes: one `provider/model::tokens` line each, sitting beside the rest of
// crash's config. It exists because the two other sources are both out of the
// user's hands mid-session — models.dev does not know every model behind an
// omniroute alias or a local gateway, and editing llm_model.rec means editing
// a file to fix a number the running session just got wrong. `/context 64000`
// writes here.
const contextMapFile = "context-sizes.map"

// contextMapSep separates the model from its size. Two colons rather than
// one: model slugs contain colons of their own ("qwen38-iq4:latest").
const contextMapSep = "::"

func contextMapPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, ".config", "gollum", contextMapFile), nil
}

// loadContextMap reads the mapping file. A missing file is an empty map, not
// an error, and so is any line that does not parse — this file is meant to be
// edited by hand, and one bad line should not cost the rest of them.
func loadContextMap() map[string]int {
	out := map[string]int{}
	path, err := contextMapPath()
	if err != nil {
		return out
	}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		model, size, ok := strings.Cut(line, contextMapSep)
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(size))
		if err != nil || n <= 0 {
			continue
		}
		out[strings.TrimSpace(model)] = n
	}
	return out
}

// saveContextSize records one model's window, replacing any line it already
// had. Written through a temp file and renamed, the same way the config
// records are, so an interrupted write never truncates the file.
func saveContextSize(model string, size int) error {
	path, err := contextMapPath()
	if err != nil {
		return err
	}
	m := loadContextMap()
	m[model] = size

	var b strings.Builder
	b.WriteString("# model::context-window-in-tokens — written by /context, cached from models.dev\n")
	for _, name := range slices.Sorted(maps.Keys(m)) {
		fmt.Fprintf(&b, "%s%s%d\n", name, contextMapSep, m[name])
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".context-sizes-*.map")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
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

// resolveContextSize decides how big the current model's context window is,
// from the three places that know something about it, in the order of how
// much the answer was actually meant for this model:
//
//   - the mapping file, which is either what the user typed at `/context` or
//     what a previous run cached there;
//   - the provider's own record in llm_model.rec, for a gateway whose models
//     no catalogue lists;
//   - models.dev, which knows the real models and nothing else.
//
// A models.dev answer is cached into the mapping file on the way out, so the
// next run has it without the lookup — and so there is a line to edit for a
// model whose published window is not the one the gateway in front of it
// actually serves. Nothing is cached over an answer that came from the first
// two sources, which is what keeps the cache from shadowing them later.
//
// A zero size means nobody knows, which switches auto-compaction off rather
// than guessing.
func resolveContextSize(current string, cfgs []llm.Config, prices priceIndex) (size int, source string) {
	if n, ok := loadContextMap()[current]; ok {
		return n, contextMapFile
	}
	provider, _, _ := strings.Cut(current, "/")
	for _, cfg := range cfgs {
		if cfg.Provider == provider && cfg.ContextLimit > 0 {
			return cfg.ContextLimit, "config"
		}
	}
	if price, _, ok := lookupPrice(prices, current); ok && price.Context > 0 {
		_ = saveContextSize(current, price.Context)
		return price.Context, "models.dev"
	}
	return 0, ""
}
