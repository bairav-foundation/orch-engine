// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bairav-foundation/orch-engine/cli/internal/fit"
)

const gb = int64(1 << 30)

func TestRenderFit(t *testing.T) {
	dev, _ := fit.Manual("12GB")
	paths := []string{"m-Q4_K_M.gguf", "m-Q8_0.gguf", "m-f16.gguf", "m-Q8_0-00001-of-00002.gguf"}
	sizes := []int64{5 * gb, 8 * gb, 15 * gb, 4 * gb}
	var b bytes.Buffer
	renderFit(&b, "org/M-8B-GGUF", dev, fit.Files(paths, sizes, 8e9, dev.Usable, fit.DefaultContext), 8e9, fit.DefaultContext, true)
	out := b.String()
	for _, want := range []string{
		"Sizing for: 12 GB (from --memory)", "org/M-8B-GGUF (8.0B)",
		"✗ too big", "✓ fits",
		"Best for you: m-Q8_0.gguf (Q8_0, near-full quality",
		"bairav model deploy org/M-8B-GGUF --file m-Q8_0.gguf",
		"split into parts",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}

	small, _ := fit.Manual("4GB")
	b.Reset()
	renderFit(&b, "org/M-8B-GGUF", small, fit.Files(paths, sizes, 8e9, small.Usable, fit.DefaultContext), 8e9, fit.DefaultContext, false)
	if !strings.Contains(b.String(), "Nothing here fits 4 GB") {
		t.Errorf("no-fit message missing:\n%s", b.String())
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
