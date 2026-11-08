// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package fit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"24": 24 * gib, "24GB": 24 * gib, "24g": 24 * gib, " 24 GiB ": 24 * gib,
		"16384MB": 16 * gib, "0.5TB": 512 * gib, "7.5G": gib * 15 / 2,
	} {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("%q: got %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "lots", "-4GB", "0"} {
		if _, err := ParseSize(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestQuantOf(t *testing.T) {
	for in, want := range map[string]string{
		"qwen2.5-7b-instruct-q4_k_m.gguf":      "Q4_K_M",
		"Meta-Llama-3.1-8B-Instruct-Q8_0.gguf": "Q8_0",
		"gemma-3-27b-it-IQ4_XS.gguf":           "IQ4_XS",
		"Q5_K_M/model-Q5_K_M.gguf":             "Q5_K_M",
		"phi-4-f16.gguf":                       "F16",
		"model-bf16.gguf":                      "BF16",
		"tinyllama.gguf":                       "",
		"Qwen2.5-0.5B-Instruct-Q6_K_L.gguf":    "Q6_K_L",
	} {
		if got := QuantOf(in); got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
}

func TestRecommend(t *testing.T) {
	cases := []struct {
		params  float64 // billions
		have    int64
		quant   string
		verdict Verdict
	}{
		{0.5, 8 * gib, "Q8_0", Fits},   // tiny model: best quality short of F16
		{8, 24 * gib, "Q8_0", Fits},    // 8B on an RTX 4090
		{8, 8 * gib, "Q5_K_M", Fits},   // 8B on an 8 GB card: 5-bit just fits
		{32, 24 * gib, "Q4_K_M", Fits}, // 32B on 24 GB: 4-bit, ~19 GB file
		{70, 24 * gib, "Q2_K", TooBig}, // 70B never fits 24 GB
		{70, 80 * gib, "Q6_K", Fits},   // 70B on an 80 GB H100
	}
	for _, c := range cases {
		p := Recommend(int64(c.params*1e9), c.have, DefaultContext)
		if p.Quant.Name != c.quant || p.Verdict != c.verdict {
			t.Errorf("%vB on %s: got %s %s (needs %s), want %s %s", c.params, GB(c.have), p.Quant.Name, p.Verdict, GB(p.Need), c.quant, c.verdict)
		}
	}
}

func TestJudge(t *testing.T) {
	if Judge(9*gib, 10*gib) != Fits || Judge(95*gib/10, 10*gib) != Tight || Judge(11*gib, 10*gib) != TooBig || Judge(0, gib) != Unknown {
		t.Fatal("judge thresholds")
	}
}

func TestFilesAndBest(t *testing.T) {
	paths := []string{
		"README.md",
		"model-Q2_K.gguf", "model-Q4_K_M.gguf", "model-Q8_0.gguf", "model-f16.gguf",
		"model-Q6_K-00001-of-00002.gguf", "model-Q6_K-00002-of-00002.gguf",
		"model-Q4_0_8_8.gguf", "mmproj-model-f16.gguf",
	}
	sizes := []int64{1, 3 * gib, 5 * gib, 8 * gib, 15 * gib, 3 * gib, 3 * gib, 5*gib + 1, gib}
	files := Files(paths, sizes, 8e9, 12*gib, DefaultContext)
	if len(files) != 6 || files[0].Quant != "F16" || files[len(files)-1].Quant != "Q2_K" {
		t.Fatalf("files: %+v", files)
	}
	var split, legacy *File
	for i := range files {
		switch {
		case files[i].Parts > 1:
			split = &files[i]
		case files[i].Legacy:
			legacy = &files[i]
		}
	}
	if split == nil || split.Quant != "Q6_K" || split.Size != 6*gib || split.Parts != 2 || split.Path != "model-Q6_K-00001-of-00002.gguf" {
		t.Fatalf("split: %+v", split)
	}
	if legacy == nil || legacy.Quant != "Q4_0_8_8" {
		t.Fatalf("legacy: %+v", legacy)
	}
	if b := Best(files); b == nil || b.Path != "model-Q8_0.gguf" {
		t.Fatalf("best on 12 GB: %+v", b)
	}
	// On 9 GB, Q8 doesn't fit; the split Q6 does but can't be served.
	files = Files(paths, sizes, 8e9, 9*gib, DefaultContext)
	if b := Best(files); b == nil || b.Quant != "Q4_K_M" {
		t.Fatalf("best on 9 GB: %+v", b)
	}
	if b := BestAny(files); b == nil || b.Quant != "Q6_K" || b.Servable() {
		t.Fatalf("ideal on 9 GB: %+v", b)
	}
	if Best(Files(paths, sizes, 8e9, 4*gib, DefaultContext)) != nil {
		t.Fatal("nothing fits 4 GB, Best must be nil")
	}
	if b := Best(Files([]string{"x-f16.gguf"}, []int64{gib}, 5e8, 16*gib, DefaultContext)); b == nil || b.Quant != "F16" {
		t.Fatalf("f16 fallback: %+v", b)
	}
	if !LowQuality("Q3_K_M") || LowQuality("Q4_K_M") {
		t.Fatal("LowQuality")
	}
}

func TestDetect(t *testing.T) {
	noMem := func() int64 { return 32 * gib }
	fake := func(answers map[string]string) runner {
		return func(_ context.Context, name string, args ...string) (string, error) {
			if v, ok := answers[name+" "+strings.Join(args, " ")]; ok {
				return v, nil
			}
			return "", errors.New("not found")
		}
	}
	smi := "nvidia-smi --query-gpu=name,memory.total --format=csv,noheader,nounits"

	d := detect(context.Background(), fake(map[string]string{smi: "NVIDIA GeForce RTX 4090, 24564\nNVIDIA GeForce RTX 3060, 12288\n"}), "linux", "amd64", noMem)
	if d.Kind != "nvidia" || d.Count != 2 || !strings.Contains(d.Name, "3060") || d.Usable != 12*gib-gib/2 {
		t.Errorf("nvidia: %+v", d)
	}

	d = detect(context.Background(), fake(map[string]string{
		"sysctl -n hw.memsize":               fmt.Sprint(36 * gib),
		"sysctl -n machdep.cpu.brand_string": "Apple M3 Pro",
	}), "darwin", "arm64", noMem)
	if d.Kind != "apple" || d.Usable < 24*gib || d.Usable > 25*gib || !strings.Contains(d.Describe(), "Apple M3 Pro") {
		t.Errorf("apple: %+v %s", d, d.Describe())
	}

	d = detect(context.Background(), fake(nil), "linux", "amd64", noMem)
	if d.Kind != "cpu" || d.Usable != 24*gib || !strings.Contains(d.Describe(), "CPU only") {
		t.Errorf("cpu: %+v", d)
	}
}
