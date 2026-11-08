// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bairav-foundation/orch-engine/cli/internal/fit"
	"github.com/bairav-foundation/orch-engine/cli/internal/hf"
)

const gb = int64(1 << 30)

func TestRenderFit(t *testing.T) {
	dev, _ := fit.Manual("12GB")
	paths := []string{"m-Q4_K_M.gguf", "m-Q8_0.gguf", "m-f16.gguf"}
	sizes := []int64{5 * gb, 8 * gb, 15 * gb}
	var b bytes.Buffer
	renderFit(&b, "org/M-8B-GGUF", dev, fit.Files(paths, sizes, 8e9, dev.Usable, fit.DefaultContext), 8e9, fit.DefaultContext, nil)
	out := b.String()
	for _, want := range []string{
		"Sizing for: 12 GB (from --memory)", "org/M-8B-GGUF (8.0B)",
		"✗ too big", "✓ fits",
		"Best you can deploy from this repo: m-Q8_0.gguf (Q8_0, near-full quality",
		"bairav model deploy org/M-8B-GGUF --file m-Q8_0.gguf",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	small, _ := fit.Manual("4GB")
	b.Reset()
	renderFit(&b, "org/M-8B-GGUF", small, fit.Files(paths, sizes, 8e9, small.Usable, fit.DefaultContext), 8e9, fit.DefaultContext, nil)
	if !strings.Contains(b.String(), "Nothing here fits 4 GB") {
		t.Errorf("no-fit message missing:\n%s", b.String())
	}
}

// The official Qwen repos split Q4 and up into parts: only Q2/Q3 are
// single files. Say so, and point at single-file repos of the same model.
func TestRenderFitSplitRepo(t *testing.T) {
	dev, _ := fit.Manual("32GB")
	paths := []string{"q-q2_k.gguf", "q-q3_k_m.gguf", "q-q4_k_m-00001-of-00002.gguf", "q-q4_k_m-00002-of-00002.gguf", "q-q8_0-00001-of-00003.gguf", "q-q8_0-00002-of-00003.gguf", "q-q8_0-00003-of-00003.gguf"}
	sizes := []int64{3 * gb, 4 * gb, 3 * gb, 2 * gb, 3 * gb, 3 * gb, 2 * gb}
	files := fit.Files(paths, sizes, 7e9, dev.Usable, fit.DefaultContext)
	if !needsSibling(files) {
		t.Fatal("needsSibling should be true")
	}
	var b bytes.Buffer
	renderFit(&b, "Qwen/Q-7B-GGUF", dev, files, 7e9, fit.DefaultContext, []string{"bartowski/Q-7B-GGUF"})
	out := b.String()
	for _, want := range []string{
		"3 parts, can't serve yet",
		"Best you can deploy from this repo: q-q3_k_m.gguf",
		"Better for your machine: Q8_0 (near-full), but this repo splits it into 3 files",
		"bairav model fit bartowski/Q-7B-GGUF",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestSizingLabel(t *testing.T) {
	dev, _ := fit.Manual("24GB")
	z := &sizing{dev: dev, ctx: fit.DefaultContext}
	if l := z.label("a/7B", "a/7B-GGUF", 7e9); !strings.HasPrefix(l, "✓ Q8_0") {
		t.Errorf("7B on 24 GB: %q", l)
	}
	if l := z.label("a/70B", "a/70B-GGUF", 70e9); !strings.HasPrefix(l, "✗ needs") {
		t.Errorf("70B on 24 GB: %q", l)
	}
	if l := z.label("a/x", "", 0); l != "size unknown" {
		t.Errorf("unknown size: %q", l)
	}
	z.label("a/14B", "a/14B-GGUF", 14e9)
	var b bytes.Buffer
	z.footer(&b)
	if !strings.Contains(b.String(), "Biggest that fits: a/14B") || !strings.Contains(b.String(), "bairav model fit a/14B-GGUF --memory 24GB") {
		t.Errorf("footer: %s", b.String())
	}
}

func TestPickSameModel(t *testing.T) {
	hits := []hf.SearchHit{
		{ID: "Qwen/Qwen2.5-7B-Instruct-GGUF"},
		{ID: "Qwen/Qwen2.5-Coder-7B-Instruct-GGUF"},
		{ID: "bartowski/Qwen2.5-7B-Instruct-abliterated-GGUF"},
		{ID: "bartowski/Qwen2.5-7B-Instruct-GGUF"},
		{ID: "lmstudio-community/Qwen2.5-7B-Instruct-GGUF"},
	}
	got := pickSameModel("Qwen/Qwen2.5-7B-Instruct-GGUF", hits)
	if len(got) != 2 || got[0] != "bartowski/Qwen2.5-7B-Instruct-GGUF" || got[1] != "lmstudio-community/Qwen2.5-7B-Instruct-GGUF" {
		t.Fatalf("got %v", got)
	}
}
