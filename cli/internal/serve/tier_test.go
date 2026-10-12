// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package serve

import "testing"

func TestParseParamSize(t *testing.T) {
	cases := []struct {
		in   string
		want float64
	}{
		{"qwen2.5-0.5b-instruct-q2_k.gguf", 0.5},
		{"Qwen/Qwen2.5-0.5B-Instruct-GGUF", 0.5},
		{"mistral-7b-instruct-v0.2.Q4_K_M.gguf", 7},
		{"Llama-3.2-1B-Instruct.gguf", 1},
		{"Qwen3-30B-A3B-Instruct.gguf", 30}, // first token wins (30B before A3B)
		{"some-model-q4.gguf", 0},           // no <N>B token
		{"model.bin", 0},
		{"", 0},
	}
	for _, c := range cases {
		got := ParseParamSize(c.in)
		if got != c.want {
			t.Errorf("ParseParamSize(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestClassify_TierBoundaries(t *testing.T) {
	cases := []struct {
		filename string
		repo     string
		wantName string
	}{
		{"qwen2.5-0.5b-instruct.gguf", "", "tiny"},
		{"llama-1b.gguf", "", "small"},
		{"mistral-7b.gguf", "", "standard"},
		{"command-r-13b.gguf", "", "large"},
		{"llama-70b.gguf", "", "xlarge"},
		{"unknown.gguf", "", "unknown"},
		// Filename has no size but repo does — fall back to repo.
		{"model-q4.gguf", "Qwen/Qwen2.5-7B-Instruct-GGUF", "standard"},
	}
	for _, c := range cases {
		got := Classify(c.filename, c.repo)
		if got.Name != c.wantName {
			t.Errorf("Classify(%q, %q).Name = %q, want %q",
				c.filename, c.repo, got.Name, c.wantName)
		}
	}
}

func TestSuggestedUpgrade(t *testing.T) {
	if Classify("qwen-0.5b.gguf", "").SuggestedUpgrade() == "" {
		t.Error("tiny tier should suggest an upgrade")
	}
	if Classify("mistral-7b.gguf", "").SuggestedUpgrade() != "" {
		t.Error("standard tier should NOT badger the user with an upgrade")
	}
}

func TestFormatParams(t *testing.T) {
	cases := []struct {
		params float64
		want   string
	}{
		{0, "?"},
		{0.5, "0.5"},
		{7, "7"},
		{27, "27"},
		{70.5, "70.5"},
	}
	for _, c := range cases {
		got := Tier{Params: c.params}.FormatParams()
		if got != c.want {
			t.Errorf("FormatParams(%v) = %q, want %q", c.params, got, c.want)
		}
	}
}
