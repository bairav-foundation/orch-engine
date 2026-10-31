// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

// Package catalog builds and reads the curated model catalog behind
// `bairav model discover`.
//
// Raw "trending" answers "what's popular this week". People deploying a
// model want "what's best for my job, and can I run it". The catalog
// groups first-party open-weight releases by what they're good at (on a
// laptop, general chat, coding, reasoning, Indian languages, vision,
// embeddings), prefers models bairav can serve today, and layers an
// editorial file on top (pins and blocks) so a person stays in the loop.
//
// A scheduled workflow rebuilds it every 12 hours and publishes
// catalog.json as a release asset; the CLI caches it for 12 hours and
// falls back to the live Hugging Face ranking when it can't be fetched.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/bairav-foundation/orch-engine/cli/internal/hf"
)

// DefaultURL is where the published catalog lives (a rolling pre-release,
// so it never becomes the "latest" release that install.sh resolves).
const DefaultURL = "https://github.com/bairav-foundation/orch-engine/releases/download/catalog/catalog.json"

// MaxAge is how long a cached catalog is used before refetching.
const MaxAge = 12 * time.Hour

// PerCategory is how many models each category keeps.
const PerCategory = 5

// Catalog is the published document.
type Catalog struct {
	Version    int        `json:"version"`
	Generated  time.Time  `json:"generated"`
	Categories []Category `json:"categories"`
}

// Category is one "best for" group.
type Category struct {
	Key    string  `json:"key"`
	Title  string  `json:"title"`
	Why    string  `json:"why"`
	Models []Entry `json:"models"`
}

// Entry is one model in a category.
type Entry struct {
	ID        string    `json:"id"`
	GGUF      string    `json:"gguf,omitempty"` // what `bairav model deploy` pulls; "" = not runnable yet
	Params    int64     `json:"params,omitempty"`
	License   string    `json:"license,omitempty"`
	Released  time.Time `json:"released"`
	Likes     int       `json:"likes"`
	Downloads int64     `json:"downloads"`
	Note      string    `json:"note,omitempty"` // editorial, from curated.yaml
	Pinned    bool      `json:"pinned,omitempty"`
}

// Curation is the editorial layer (catalog/curated.yaml in the repo).
type Curation struct {
	// Pins put a model at the top of a category with a note, whatever
	// its trending rank. Category keys as in Categories.
	Pins []struct {
		Category string `yaml:"category"`
		ID       string `yaml:"id"`
		GGUF     string `yaml:"gguf"`
		Note     string `yaml:"note"`
	} `yaml:"pins"`
	// Blocks remove a model (exact id) from every category.
	Blocks []string `yaml:"blocks"`
}

// LoadCuration reads a curated.yaml; a missing file is an empty curation.
func LoadCuration(path string) (Curation, error) {
	var c Curation
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// categoryDef describes how a category is filled.
type categoryDef struct {
	key, title, why string
	task            string                          // hf task the candidates come from
	match           func(m hf.DiscoveredModel) bool // nil = every candidate
}

var (
	codeRe     = regexp.MustCompile(`(?i)cod(e|er|ing|estral)|devstral|starcoder|program`)
	reasonRe   = regexp.MustCompile(`(?i)reason|think|-r1\b|\br1-|qwq|o1-|deepseek-r`)
	indicRe    = regexp.MustCompile(`(?i)indic|sarvam|airavata|krutrim|bharat|hindi|tamil|telugu|kannada|malayalam|marathi|bengali|navarasa|param-?1|ai4bharat`)
	indicLangs = []string{"hi", "ta", "te", "kn", "ml", "mr", "bn", "gu", "pa", "or", "as"}
	paramsUpTo = func(n int64) func(hf.DiscoveredModel) bool {
		return func(m hf.DiscoveredModel) bool { return m.Params > 0 && m.Params <= n }
	}
	paramsAbove = func(n int64) func(hf.DiscoveredModel) bool {
		return func(m hf.DiscoveredModel) bool { return m.Params > n }
	}
)

// Categories in display order.
var Categories = []categoryDef{
	{"laptop", "Runs on a laptop", "Up to 4B parameters: about 3 GB of RAM at 4-bit. Start here.", "text", paramsUpTo(4_500_000_000)},
	{"general", "General chat", "5B to 15B: the sweet spot on a single GPU or a big laptop.", "text",
		func(m hf.DiscoveredModel) bool { return m.Params > 4_500_000_000 && m.Params <= 16_000_000_000 }},
	{"capable", "Most capable", "Over 15B: best answers, needs serious hardware.", "text", paramsAbove(16_000_000_000)},
	{"coding", "Coding", "Models built or tuned for code.", "text", func(m hf.DiscoveredModel) bool { return codeRe.MatchString(m.ID) || hasTag(m, "code") }},
	{"reasoning", "Reasoning", "Think-before-answering models for maths, planning and multi-step work.", "text",
		func(m hf.DiscoveredModel) bool { return reasonRe.MatchString(m.ID) || hasTag(m, "reasoning") }},
	{"indic", "Indian languages", "Models trained for Hindi, Tamil, Telugu and other Indian languages.", "text", isIndic},
	{"vision", "Vision", "Text and images in, text out.", "vision", nil},
	{"embedding", "Embeddings", "For search and RAG.", "embedding", nil},
}

func hasTag(m hf.DiscoveredModel, tag string) bool {
	for _, t := range m.Tags {
		if strings.EqualFold(t, tag) {
			return true
		}
	}
	return false
}

func isIndic(m hf.DiscoveredModel) bool {
	if indicRe.MatchString(m.ID) {
		return true
	}
	n := 0
	for _, l := range indicLangs {
		if hasTag(m, l) {
			n++
		}
	}
	return n >= 2 // one stray language tag isn't enough
}

// Source fetches candidates. *hf.Client satisfies it; tests use a fake.
type Source interface {
	Discover(ctx context.Context, o hf.DiscoverOptions) ([]hf.DiscoveredModel, error)
}

// Build assembles a catalog from live Hugging Face data plus curation.
func Build(ctx context.Context, src Source, cur Curation, now time.Time) (Catalog, error) {
	candidates := map[string][]hf.DiscoveredModel{}
	for _, task := range []string{"text", "vision", "embedding"} {
		seen := map[string]bool{}
		var all []hf.DiscoveredModel
		// Trending finds what's new; downloads keeps proven models in.
		for _, sortBy := range []string{"trending", "downloads"} {
			ms, err := src.Discover(ctx, hf.DiscoverOptions{Task: task, Sort: sortBy, Limit: 40})
			if err != nil {
				return Catalog{}, fmt.Errorf("discover %s/%s: %w", task, sortBy, err)
			}
			for _, m := range ms {
				if !seen[m.ID] {
					seen[m.ID] = true
					all = append(all, m)
				}
			}
		}
		candidates[task] = all
	}
	return assemble(candidates, cur, now), nil
}

// assemble is Build without the network, for tests.
func assemble(candidates map[string][]hf.DiscoveredModel, cur Curation, now time.Time) Catalog {
	blocked := map[string]bool{}
	for _, b := range cur.Blocks {
		blocked[strings.ToLower(b)] = true
	}
	out := Catalog{Version: 1, Generated: now.UTC()}
	for _, def := range Categories {
		cat := Category{Key: def.key, Title: def.title, Why: def.why}
		taken := map[string]bool{}
		for _, p := range cur.Pins {
			if p.Category != def.key || blocked[strings.ToLower(p.ID)] {
				continue
			}
			e := Entry{ID: p.ID, GGUF: p.GGUF, Note: p.Note, Pinned: true}
			for _, m := range candidates[def.task] { // enrich from live data when present
				if strings.EqualFold(m.ID, p.ID) {
					e = fromModel(m)
					e.Note, e.Pinned = p.Note, true
					if p.GGUF != "" {
						e.GGUF = p.GGUF
					}
				}
			}
			cat.Models = append(cat.Models, e)
			taken[strings.ToLower(p.ID)] = true
		}
		var pool []hf.DiscoveredModel
		for _, m := range candidates[def.task] {
			if blocked[strings.ToLower(m.ID)] || taken[strings.ToLower(m.ID)] {
				continue
			}
			if def.match == nil || def.match(m) {
				pool = append(pool, m)
			}
		}
		// Runnable first, then by community signal (likes weigh popularity
		// over time, downloads break ties). Fresh trending order is kept
		// within equal scores by the stable sort.
		sort.SliceStable(pool, func(i, j int) bool {
			ri, rj := pool[i].GGUF != "", pool[j].GGUF != ""
			if ri != rj {
				return ri
			}
			if pool[i].Likes != pool[j].Likes {
				return pool[i].Likes > pool[j].Likes
			}
			return pool[i].Downloads > pool[j].Downloads
		})
		for _, m := range pool {
			if len(cat.Models) >= PerCategory {
				break
			}
			cat.Models = append(cat.Models, fromModel(m))
		}
		if len(cat.Models) > 0 {
			out.Categories = append(out.Categories, cat)
		}
	}
	return out
}

func fromModel(m hf.DiscoveredModel) Entry {
	return Entry{ID: m.ID, GGUF: m.GGUF, Params: m.Params, License: m.License(),
		Released: m.Created, Likes: m.Likes, Downloads: m.Downloads}
}

// CachePath is ~/.bairav/catalog.json.
func CachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".bairav", "catalog.json"), nil
}

// Fetch returns the catalog: from cachePath when it is younger than
// MaxAge, else from url (and refreshes the cache). When the download
// fails, a stale cache is still returned, with stale=true.
func Fetch(ctx context.Context, client *http.Client, url, cachePath string, now time.Time) (c Catalog, stale bool, err error) {
	if cachePath != "" {
		if st, serr := os.Stat(cachePath); serr == nil && now.Sub(st.ModTime()) < MaxAge {
			if c, rerr := readFile(cachePath); rerr == nil {
				return c, false, nil
			}
		}
	}
	c, err = download(ctx, client, url)
	if err == nil {
		if cachePath != "" {
			_ = writeFile(cachePath, c)
		}
		return c, false, nil
	}
	if cachePath != "" {
		if cached, rerr := readFile(cachePath); rerr == nil {
			return cached, true, nil
		}
	}
	return Catalog{}, false, err
}

func download(ctx context.Context, client *http.Client, url string) (Catalog, error) {
	var c Catalog
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
	if err != nil {
		return c, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return c, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c, fmt.Errorf("catalog: %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&c); err != nil {
		return c, fmt.Errorf("catalog: %w", err)
	}
	if c.Version != 1 || len(c.Categories) == 0 {
		return c, fmt.Errorf("catalog: unexpected version %d or empty", c.Version)
	}
	return c, nil
}

func readFile(path string) (Catalog, error) {
	var c Catalog
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

func writeFile(path string, c Catalog) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Write saves a catalog as indented JSON (used by the build workflow).
func Write(path string, c Catalog) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// Find returns the category with key, or false.
func (c Catalog) Find(key string) (Category, bool) {
	for _, cat := range c.Categories {
		if cat.Key == key {
			return cat, true
		}
	}
	return Category{}, false
}

// Keys lists category keys in order.
func Keys() []string {
	out := make([]string, len(Categories))
	for i, d := range Categories {
		out[i] = d.key
	}
	return out
}
