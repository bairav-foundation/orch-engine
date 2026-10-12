// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import "testing"

func TestK8sNameFromRepo(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Normal HF repo slugs — noisy suffixes get stripped.
		{"Qwen/Qwen2.5-0.5B-Instruct-GGUF", "qwen2-5-0-5b"},
		{"bartowski/Mistral-7B-Instruct-v0.2-GGUF", "mistral-7b"},
		{"TinyLlama/TinyLlama-1.1B-Chat-v1.0", "tinyllama-1-1b"},
		// Stripping only operates on trailing tokens; "chat" in the
		// middle is part of the actual model name and must survive.
		{"foo/chat-foo-bar", "chat-foo-bar"},
		// Multiple noise tokens trail in sequence — keep stripping.
		{"foo/llama3-8b-instruct-gguf", "llama3-8b"},
		// Gemma's "it" (italian? no, "instruction-tuned") suffix.
		{"google/gemma-2-2b-it", "gemma-2-2b"},
		// Quantization-tag suffixes.
		{"foo/llama-3-70b-awq", "llama-3-70b"},
		// No "/" — use the whole string.
		{"single-segment", "single-segment"},
		// Underscores collapse to hyphens.
		{"foo/bar_baz_qux", "bar-baz-qux"},
		// Repeated separators collapse.
		{"foo/a..b..c", "a-b-c"},
		// Edge: stripping must not leave the name empty — keep at least
		// one token. A repo that's entirely a noise token stays as-is.
		{"foo/gguf", "gguf"},
		// Edge: empty → fallback name.
		{"", "bairav-model"},
		// Edge: only junk → fallback name.
		{"!@#$%^&*()", "bairav-model"},
	}
	for _, c := range cases {
		got := k8sNameFromRepo(c.in)
		if got != c.want {
			t.Errorf("k8sNameFromRepo(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestK8sNameFromRepo_HonoursDNSLimit(t *testing.T) {
	// Build a repo slug whose lastSegment is well over 63 chars.
	huge := "org/" + repeat("a", 100)
	got := k8sNameFromRepo(huge)
	if len(got) > 63 {
		t.Errorf("name length = %d, want <= 63", len(got))
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
