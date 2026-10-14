// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/bairav-foundation/orch-engine/cli/internal/catalog"
)

func TestRenderCatalog(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	c := catalog.Catalog{Version: 1, Generated: now.Add(-3 * time.Hour), Categories: []catalog.Category{
		{Key: "laptop", Title: "Runs on a laptop", Why: "Up to 4B.", Models: []catalog.Entry{
			{ID: "Qwen/Qwen2.5-0.5B-Instruct-GGUF", GGUF: "Qwen/Qwen2.5-0.5B-Instruct-GGUF", Pinned: true, Note: "The quickstart model."},
			{ID: "tiny/NoGGUF-1B", Params: 1_000_000_000},
		}},
	}}
	var b strings.Builder
	renderCatalog(&b, c, true, now)
	out := b.String()
	for _, want := range []string{
		"updated 3h ago (couldn't refresh; showing the cached copy)",
		"RUNS ON A LAPTOP  ·  Up to 4B.",
		"Qwen/Qwen2.5-0.5B-Instruct-GGUF ★",
		"The quickstart model.",
		"no GGUF yet",
		"Try one:  bairav model deploy Qwen/Qwen2.5-0.5B-Instruct-GGUF --name my-model",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
