// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bairav-foundation/orch-engine/cli/internal/hf"
)

func m(id string, params int64, likes int, gguf string, tags ...string) hf.DiscoveredModel {
	return hf.DiscoveredModel{ID: id, Params: params, Likes: likes, GGUF: gguf, Tags: tags}
}

type fakeSource map[string][]hf.DiscoveredModel

func (f fakeSource) Discover(_ context.Context, o hf.DiscoverOptions) ([]hf.DiscoveredModel, error) {
	return f[o.Task], nil
}

func ids(c Category) []string {
	var out []string
	for _, e := range c.Models {
		out = append(out, e.ID)
	}
	return out
}

func TestBuildCategories(t *testing.T) {
	src := fakeSource{
		"text": {
			m("Qwen/Qwen3-1.7B", 1_700_000_000, 900, "Qwen/Qwen3-1.7B-GGUF"),
			m("tiny/NoGGUF-1B", 1_000_000_000, 5000, ""), // popular but not runnable: ranks after runnable ones
			m("Qwen/Qwen3-Coder-30B", 30_000_000_000, 700, "unsloth/Qwen3-Coder-30B-GGUF"),
			m("google/gemma-3-12b-it", 12_000_000_000, 800, "ggml-org/gemma-3-12b-it-GGUF"),
			m("sarvamai/sarvam-m", 24_000_000_000, 300, "bartowski/sarvam-m-GGUF", "hi", "ta"),
			m("deepseek-ai/DeepSeek-R1-Distill-Qwen-7B", 7_600_000_000, 600, "unsloth/DeepSeek-R1-Distill-Qwen-7B-GGUF"),
			m("blocked/Bad-3B", 3_000_000_000, 99999, "blocked/Bad-3B-GGUF"),
		},
		"vision":    {m("google/gemma-3-4b-it", 4_000_000_000, 400, "ggml-org/gemma-3-4b-it-GGUF")},
		"embedding": {m("BAAI/bge-m3", 568_000_000, 2000, "")},
	}
	var cur Curation
	cur.Blocks = []string{"blocked/Bad-3B"}
	cur.Pins = append(cur.Pins, struct {
		Category string `yaml:"category"`
		ID       string `yaml:"id"`
		GGUF     string `yaml:"gguf"`
		Note     string `yaml:"note"`
	}{Category: "laptop", ID: "Qwen/Qwen2.5-0.5B-Instruct-GGUF", GGUF: "Qwen/Qwen2.5-0.5B-Instruct-GGUF", Note: "The quickstart model: 400 MB."})

	c, err := Build(context.Background(), src, cur, time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"laptop":    {"Qwen/Qwen2.5-0.5B-Instruct-GGUF", "Qwen/Qwen3-1.7B", "tiny/NoGGUF-1B"},
		"general":   {"google/gemma-3-12b-it", "deepseek-ai/DeepSeek-R1-Distill-Qwen-7B"},
		"capable":   {"Qwen/Qwen3-Coder-30B", "sarvamai/sarvam-m"},
		"coding":    {"Qwen/Qwen3-Coder-30B"},
		"reasoning": {"deepseek-ai/DeepSeek-R1-Distill-Qwen-7B"},
		"indic":     {"sarvamai/sarvam-m"},
		"vision":    {"google/gemma-3-4b-it"},
		"embedding": {"BAAI/bge-m3"},
	}
	for key, w := range want {
		cat, ok := c.Find(key)
		if !ok {
			t.Errorf("category %s missing", key)
			continue
		}
		got := ids(cat)
		if len(got) != len(w) {
			t.Errorf("%s: got %v, want %v", key, got, w)
			continue
		}
		for i := range w {
			if got[i] != w[i] {
				t.Errorf("%s: got %v, want %v", key, got, w)
				break
			}
		}
	}
	lap, _ := c.Find("laptop")
	if !lap.Models[0].Pinned || lap.Models[0].Note == "" {
		t.Errorf("pin lost its note: %+v", lap.Models[0])
	}
	for _, cat := range c.Categories {
		for _, e := range cat.Models {
			if e.ID == "blocked/Bad-3B" {
				t.Errorf("blocked model in %s", cat.Key)
			}
		}
	}
}

func TestFetchCacheAndFallback(t *testing.T) {
	good := Catalog{Version: 1, Categories: []Category{{Key: "laptop", Models: []Entry{{ID: "a/b"}}}}}
	calls := 0
	up := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if !up {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(good)
	}))
	defer srv.Close()
	cache := filepath.Join(t.TempDir(), "catalog.json")
	now := time.Now()

	c, stale, err := Fetch(context.Background(), srv.Client(), srv.URL, cache, now)
	if err != nil || stale || len(c.Categories) != 1 || calls != 1 {
		t.Fatalf("first fetch: %v %v %d", err, stale, calls)
	}
	// Fresh cache: no network.
	if _, _, err := Fetch(context.Background(), srv.Client(), srv.URL, cache, now.Add(time.Hour)); err != nil || calls != 1 {
		t.Fatalf("cached fetch hit the network: %v calls=%d", err, calls)
	}
	// Old cache + server down: stale copy, flagged.
	old := now.Add(-13 * time.Hour)
	_ = os.Chtimes(cache, old, old)
	up = false
	c, stale, err = Fetch(context.Background(), srv.Client(), srv.URL, cache, now)
	if err != nil || !stale || len(c.Categories) != 1 {
		t.Fatalf("stale fallback: %v %v", err, stale)
	}
	// No cache + server down: error.
	if _, _, err := Fetch(context.Background(), srv.Client(), srv.URL, filepath.Join(t.TempDir(), "x.json"), now); err == nil {
		t.Fatal("expected an error with no cache and no server")
	}
}

func TestIsIndicNeedsTwoLanguageTags(t *testing.T) {
	if isIndic(m("org/Model", 1, 1, "", "hi")) {
		t.Error("one stray language tag shouldn't count")
	}
	if !isIndic(m("org/Model", 1, 1, "", "hi", "te")) {
		t.Error("two Indian language tags should count")
	}
	if !isIndic(m("ai4bharat/IndicBERT", 1, 1, "")) {
		t.Error("name match should count")
	}
}
