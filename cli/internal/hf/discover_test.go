// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package hf

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func dm(id string, trending float64, tags ...string) DiscoveredModel {
	return DiscoveredModel{ID: id, Trending: trending, Tags: tags}
}

// Shapes taken from the real trending list (Oct 2026).
var trendingFixture = []DiscoveredModel{
	dm("orcarouter/OrcaSAQ-2-Cyber-27B-Uncensored-GGUF", 256, "gguf"),
	dm("poolside/Laguna-S-2.1", 240, "license:openmdw-1.1"),
	dm("Aleph-Alpha/Kolibri-1", 173, "base_model:Aleph-Alpha/Kolibri-1-BF16", "base_model:quantized:Aleph-Alpha/Kolibri-1-BF16", "license:apache-2.0"),
	dm("unsloth/Laguna-S-2.1-GGUF", 192, "gguf", "base_model:poolside/Laguna-S-2.1", "base_model:quantized:poolside/Laguna-S-2.1"),
	dm("prism-ml/Bonsai-27B-gguf", 145, "gguf", "base_model:Qwen/Qwen3.6-27B", "base_model:quantized:Qwen/Qwen3.6-27B"),
	dm("poolside/Laguna-S-2.1-NVFP4", 142, "base_model:poolside/Laguna-S-2.1", "base_model:quantized:poolside/Laguna-S-2.1"),
	dm("deepreinforce-ai/Ornith-1.0-35B-GGUF", 45, "gguf", "license:mit"),
	dm("deepreinforce-ai/Ornith-1.0-35B", 35, "license:mit"),
	dm("Nanbeige/Nanbeige4.2-3B", 431, "base_model:Nanbeige/Nanbeige4.2-3B-Base", "base_model:finetune:Nanbeige/Nanbeige4.2-3B-Base"),
	dm("Aleph-Alpha/Kolibri-1-BF16", 20, "license:apache-2.0"),
	dm("Nanbeige/Nanbeige4.2-3B-Base", 40),
	dm("huihui-ai/Huihui-GLM-5.2-abliterated-GGUF", 34, "gguf", "base_model:zai-org/GLM-5.2"),
	dm("badtheorylabs/BTL-3", 60, "base_model:adapter:Qwen/Qwen3.6-27B"),
}

func TestFirstParty(t *testing.T) {
	var got []string
	for _, m := range FirstParty(trendingFixture) {
		got = append(got, m.ID)
	}
	want := []string{
		"poolside/Laguna-S-2.1",                // its NVFP4 sibling folded in; unsloth GGUF is third-party
		"Aleph-Alpha/Kolibri-1",                // beats Kolibri-1-BF16 (same family)
		"deepreinforce-ai/Ornith-1.0-35B-GGUF", // GGUF + base collapse to one row
		"Nanbeige/Nanbeige4.2-3B",              // Base folded in
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FirstParty:\n got  %v\n want %v", got, want)
	}
}

func TestPickGGUF(t *testing.T) {
	hits := []DiscoveredModel{
		dm("someone/X-GGUF", 0), dm("AliceThirty/X-UNCENSORED-GGUF", 0), dm("bartowski/X-GGUF", 0), dm("unsloth/X-GGUF", 0),
	}
	if got := pickGGUF(hits, "org"); got != "unsloth/X-GGUF" {
		t.Errorf("trusted quantizer should win, got %s", got)
	}
	hits = append(hits, dm("org/X-GGUF", 0))
	if got := pickGGUF(hits, "org"); got != "org/X-GGUF" {
		t.Errorf("first-party GGUF should win, got %s", got)
	}
	if got := pickGGUF([]DiscoveredModel{dm("a/X-abliterated-GGUF", 0)}, "org"); got != "" {
		t.Errorf("junk-only should give no GGUF, got %s", got)
	}
}

func TestDiscoverEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/api/models" && q.Get("pipeline_tag") == "text-generation":
			if q.Get("sort") != "trendingScore" {
				t.Errorf("sort = %q", q.Get("sort"))
			}
			_ = json.NewEncoder(w).Encode(trendingFixture)
		case r.URL.Path == "/api/models" && strings.HasPrefix(q.Get("filter"), "base_model:quantized:poolside/Laguna-S-2.1"):
			_ = json.NewEncoder(w).Encode([]DiscoveredModel{dm("unsloth/Laguna-S-2.1-GGUF", 0)})
		case r.URL.Path == "/api/models":
			_, _ = w.Write([]byte("[]"))
		case r.URL.Path == "/api/models/Aleph-Alpha/Kolibri-1":
			_, _ = w.Write([]byte(`{"safetensors":{"total":78103074560}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &Client{Endpoint: srv.URL, HTTPClient: srv.Client()}
	got, err := c.Discover(context.Background(), DiscoverOptions{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("limit not applied: %d", len(got))
	}
	if got[0].GGUF != "unsloth/Laguna-S-2.1-GGUF" {
		t.Errorf("Laguna GGUF = %q", got[0].GGUF)
	}
	if got[1].GGUF != "" || got[1].Params != 78103074560 {
		t.Errorf("Kolibri: GGUF %q params %d (want none, 78B)", got[1].GGUF, got[1].Params)
	}
	if got[2].GGUF != got[2].ID {
		t.Errorf("a GGUF repo should point at itself, got %q", got[2].GGUF)
	}
	if _, err := c.Discover(context.Background(), DiscoverOptions{Task: "music"}); err == nil {
		t.Error("unknown task must error")
	}
}

func TestHumanParams(t *testing.T) {
	for n, want := range map[int64]string{78103074560: "78B", 7_600_000_000: "7.6B", 494_032_768: "494M", 2_400_000_000_000: "2.4T", 0: "?"} {
		if got := HumanParams(n); got != want {
			t.Errorf("HumanParams(%d) = %q, want %q", n, got, want)
		}
	}
}
