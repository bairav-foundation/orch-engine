// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package hf

// Discovery for `bairav model discover`: what's new and notable in open
// weights, and whether bairav can run it today.
//
// Raw Hugging Face trending is noisy (uncensored fine-tunes, re-uploads,
// quants of quants), so we keep first-party releases only: a repo whose
// base_model lineage, if any, points at its own org. Each family (e.g.
// Kolibri-1 and Kolibri-1-BF16) is collapsed to its most trending member.
// Then we look up the most-downloaded GGUF quantization of each, since
// that's what `bairav model pull` / `deploy` serve.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Tasks maps the task names accepted by Discover to Hugging Face pipeline tags.
// Coding / reasoning specialisations aren't HF pipeline tags; they come
// later from a curated layer.
var Tasks = map[string]string{
	"text":      "text-generation",
	"vision":    "image-text-to-text",
	"embedding": "feature-extraction",
	"image":     "text-to-image",
	"speech":    "automatic-speech-recognition",
}

// Servable reports whether bairav's llama.cpp serving image can run a task.
func Servable(task string) bool {
	return task == "text" || task == "embedding" || task == "vision"
}

// DiscoveredModel is one first-party open-weight release.
type DiscoveredModel struct {
	ID        string    `json:"id"`
	Likes     int       `json:"likes"`
	Downloads int64     `json:"downloads"`
	Trending  float64   `json:"trendingScore"`
	Created   time.Time `json:"createdAt"`
	Pipeline  string    `json:"pipeline_tag"`
	Tags      []string  `json:"tags"`
	Library   string    `json:"library_name"`

	// Filled by Discover.
	Params int64  `json:"-"` // total parameters from safetensors metadata; 0 = unknown
	GGUF   string `json:"-"` // best GGUF repo to pull; "" = none found
}

// Author is the org/user part of the repo id.
func (m DiscoveredModel) Author() string {
	a, _, _ := strings.Cut(m.ID, "/")
	return a
}

// License is the license tag value ("apache-2.0", "mit", "other", ...).
func (m DiscoveredModel) License() string {
	for _, t := range m.Tags {
		if v, ok := strings.CutPrefix(t, "license:"); ok {
			return v
		}
	}
	return ""
}

// IsGGUF reports whether the repo itself ships GGUF files.
func (m DiscoveredModel) IsGGUF() bool {
	for _, t := range m.Tags {
		if t == "gguf" {
			return true
		}
	}
	return false
}

// DiscoverOptions controls Discover.
type DiscoverOptions struct {
	Task  string // key of Tasks; default "text"
	Sort  string // trending (default) | downloads | likes | new
	Limit int    // default 10
	All   bool   // skip the first-party filter (raw HF ranking)
}

var sortKeys = map[string]string{"trending": "trendingScore", "downloads": "downloads", "likes": "likes", "new": "createdAt"}

// Discover returns the top open-weight models for a task.
func (c *Client) Discover(ctx context.Context, o DiscoverOptions) ([]DiscoveredModel, error) {
	if o.Task == "" {
		o.Task = "text"
	}
	if o.Sort == "" {
		o.Sort = "trending"
	}
	if o.Limit <= 0 {
		o.Limit = 10
	}
	pipeline, ok := Tasks[o.Task]
	if !ok {
		return nil, fmt.Errorf("unknown task %q (use text, vision, embedding, image or speech)", o.Task)
	}
	sortKey, ok := sortKeys[o.Sort]
	if !ok {
		return nil, fmt.Errorf("unknown sort %q (use trending, downloads, likes or new)", o.Sort)
	}

	q := url.Values{}
	q.Set("pipeline_tag", pipeline)
	q.Set("sort", sortKey)
	q.Set("direction", "-1")
	q.Set("limit", "200") // filter down to Limit below
	var raw []DiscoveredModel
	if err := c.getJSON(ctx, "/api/models?"+q.Encode(), &raw); err != nil {
		return nil, err
	}
	picked := raw
	if !o.All {
		picked = FirstParty(raw)
	}
	if len(picked) > o.Limit {
		picked = picked[:o.Limit]
	}

	// Enrich in parallel: params + best GGUF. Failures just leave blanks.
	var wg sync.WaitGroup
	for i := range picked {
		wg.Add(1)
		go func(m *DiscoveredModel) {
			defer wg.Done()
			m.Params = c.params(ctx, m.ID)
			if m.IsGGUF() {
				m.GGUF = m.ID
				return
			}
			m.GGUF = c.bestGGUF(ctx, m.ID, m.Author())
		}(&picked[i])
	}
	wg.Wait()
	return picked, nil
}

var junk = regexp.MustCompile(`(?i)uncensor|abliterat|nsfw|jailbreak|heretic`)

// FirstParty keeps original releases and collapses families, preserving
// the input order (the API's ranking). A family is the plain
// base_model:<id> root (e.g. Kolibri-1 → Kolibri-1-BF16), or the repo
// itself. A first-party GGUF whose base is also in the list is dropped:
// the base row will point at it as its GGUF anyway.
func FirstParty(in []DiscoveredModel) []DiscoveredModel {
	present := map[string]bool{}
	for _, m := range in {
		present[familyKey(m.ID)] = true
	}
	byFamily := map[string]int{} // family root → index in out
	var out []DiscoveredModel
	for _, m := range in {
		if junk.MatchString(m.ID) || !ownLineage(m) {
			continue
		}
		fam := familyKey(m.ID)
		for _, t := range m.Tags {
			if b, ok := strings.CutPrefix(t, "base_model:"); ok && !strings.Contains(b, ":") {
				fam = familyKey(b)
			}
		}
		if m.IsGGUF() && fam != familyKey(m.ID) && present[fam] {
			continue
		}
		if i, seen := byFamily[fam]; seen {
			if m.Trending > out[i].Trending {
				out[i] = m
			}
			continue
		}
		byFamily[fam] = len(out)
		out = append(out, m)
	}
	return out
}

// familyKey normalises a repo id so "org/X-GGUF" and "org/X" (a
// first-party GGUF often has no base_model tag) land in one family.
func familyKey(id string) string {
	k := strings.ToLower(id)
	for _, suf := range []string{"-gguf", "-instruct", "-it", "-chat"} {
		k = strings.TrimSuffix(k, suf)
	}
	return k
}

// ownLineage: every base_model tag points at the repo's own org.
func ownLineage(m DiscoveredModel) bool {
	for _, t := range m.Tags {
		b, ok := strings.CutPrefix(t, "base_model:")
		if !ok {
			continue
		}
		if i := strings.LastIndex(b, ":"); i >= 0 { // base_model:finetune:<id>
			b = b[i+1:]
		}
		if a, _, _ := strings.Cut(b, "/"); !strings.EqualFold(a, m.Author()) {
			return false
		}
	}
	return true
}

// trustedQuantizers are preferred, in order, when several GGUFs exist.
// The model's own org always wins.
var trustedQuantizers = []string{"ggml-org", "unsloth", "bartowski", "lmstudio-community", "Qwen", "mradermacher"}

func (c *Client) bestGGUF(ctx context.Context, id, author string) string {
	q := url.Values{}
	q.Set("filter", "base_model:quantized:"+id+",gguf")
	q.Set("sort", "downloads")
	q.Set("direction", "-1")
	q.Set("limit", "20")
	var hits []DiscoveredModel
	if err := c.getJSON(ctx, "/api/models?"+q.Encode(), &hits); err != nil || len(hits) == 0 {
		return ""
	}
	return pickGGUF(hits, author)
}

func pickGGUF(hits []DiscoveredModel, author string) string {
	rank := func(h DiscoveredModel) int {
		if junk.MatchString(h.ID) {
			return 1000
		}
		if strings.EqualFold(h.Author(), author) {
			return -1
		}
		for i, q := range trustedQuantizers {
			if strings.EqualFold(h.Author(), q) {
				return i
			}
		}
		return 100 // unknown uploader: still usable, ranked by downloads
	}
	sort.SliceStable(hits, func(i, j int) bool { return rank(hits[i]) < rank(hits[j]) })
	if rank(hits[0]) >= 1000 {
		return ""
	}
	return hits[0].ID
}

func (c *Client) params(ctx context.Context, id string) int64 {
	var r struct {
		Safetensors struct {
			Total int64 `json:"total"`
		} `json:"safetensors"`
	}
	if err := c.getJSON(ctx, "/api/models/"+escapePathSegments(id)+"?expand[]=safetensors", &r); err != nil {
		return 0
	}
	return r.Safetensors.Total
}

func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, strings.TrimRight(c.Endpoint, "/")+path, nil)
	if err != nil {
		return err
	}
	c.applyAuth(req)
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("hugging face: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hugging face: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// HumanParams formats a parameter count: 78103074560 → "78B", 494032768 → "494M".
func HumanParams(n int64) string {
	switch {
	case n <= 0:
		return "?"
	case n >= 1e12:
		return fmt.Sprintf("%.1fT", float64(n)/1e12)
	case n >= 1e10:
		return fmt.Sprintf("%.0fB", float64(n)/1e9)
	case n >= 1e9:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	default:
		return fmt.Sprintf("%.0fM", float64(n)/1e6)
	}
}

// Q4Bytes estimates a Q4_K_M GGUF download: ~4.8 bits per parameter.
func Q4Bytes(params int64) int64 { return params * 6 / 10 }
