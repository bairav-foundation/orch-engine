// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

// Package fit answers "will this model run on my GPU (or Mac, or CPU)?"
// without anyone doing the math: memory needed = the model file (weights)
// + the KV cache for the context window + a runtime overhead, compared
// with the memory the device can actually give a model.
//
// Every number here is an estimate with headroom, not a guarantee. The
// file size is exact when we know the GGUF file; otherwise it comes from
// the parameter count and the quantisation's bits per weight.
package fit

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

const gib = 1 << 30

// DefaultContext is the context window the estimate assumes (tokens).
const DefaultContext = 8192

// overhead is the runtime's own memory (CUDA/Metal context, buffers).
const overhead = gib * 6 / 10

// Quant is a GGUF quantisation and roughly how many bytes it spends per
// parameter (bits per weight / 8, including the scales).
type Quant struct {
	Name          string
	BytesPerParam float64
	Quality       string // one-word quality note for people who don't know quants
}

// Quants lists the quantisations, best quality first. Recommend walks this list and takes the
// first that fits.
var Quants = []Quant{
	{"F16", 2.0, "full"},
	{"Q8_0", 1.06, "near-full"},
	{"Q6_K", 0.82, "very good"},
	{"Q5_K_M", 0.71, "very good"},
	{"Q4_K_M", 0.60, "good"},
	{"Q3_K_M", 0.48, "noticeably worse"},
	{"Q2_K", 0.37, "poor"},
}

// FileBytes estimates a GGUF file's size from the parameter count.
func FileBytes(params int64, q Quant) int64 {
	return int64(float64(params) * q.BytesPerParam)
}

// KVBytes is a rough KV-cache size for a model of params parameters at
// ctx tokens. Real numbers depend on layers and KV heads (grouped-query
// attention makes modern models much smaller); this stays on the safe
// side of common 0.5B-70B models: ~0.1 GiB per billion parameters at 8k
// tokens, at least 0.25 GiB and at most 3 GiB, scaled by context.
func KVBytes(params int64, ctx int) int64 {
	if ctx <= 0 {
		ctx = DefaultContext
	}
	b := float64(params) / 1e9 * 0.1 * gib
	b = math.Max(b, 0.25*gib)
	b = math.Min(b, 3*gib)
	return int64(b * float64(ctx) / DefaultContext)
}

// Need is the memory a model needs: weights + KV cache + overhead.
func Need(fileBytes, params int64, ctx int) int64 {
	if params <= 0 {
		// Unknown parameter count: infer it from the file at ~Q4.
		params = int64(float64(fileBytes) / 0.6)
	}
	return fileBytes + KVBytes(params, ctx) + overhead
}

// Verdict is how a model fits a device.
type Verdict string

// Verdict values.
const (
	Fits    Verdict = "fits"    // with at least 10% to spare
	Tight   Verdict = "tight"   // fits, but close: a long context or another app may tip it over
	TooBig  Verdict = "too big" // won't fit in memory
	Unknown Verdict = "unknown" // no size to go on
)

// Judge compares what a model needs with what the device has.
func Judge(need, have int64) Verdict {
	switch {
	case need <= 0 || have <= 0:
		return Unknown
	case float64(need) <= 0.9*float64(have):
		return Fits
	case need <= have:
		return Tight
	}
	return TooBig
}

// Pick is a recommended quantisation for a model on a device.
type Pick struct {
	Quant   Quant
	File    int64
	Need    int64
	Verdict Verdict
}

// Recommend returns the best-quality quantisation of a params-sized model
// that fits have with room to spare, or the smallest one with its verdict
// when none does. It starts at Q8_0: F16 is twice the memory for no
// difference most people can measure, and runs slower. Q2/Q3 are only
// offered when nothing better fits.
func Recommend(params, have int64, ctx int) Pick {
	var last Pick
	for _, q := range Quants {
		if q.Name == "F16" {
			continue
		}
		f := FileBytes(params, q)
		p := Pick{Quant: q, File: f, Need: Need(f, params, ctx)}
		p.Verdict = Judge(p.Need, have)
		if p.Verdict == Fits {
			return p
		}
		last = p
	}
	return last
}

// MinNeed is the memory the 4-bit (Q4_K_M) version needs: the practical
// floor for a model, since lower quants lose too much quality.
func MinNeed(params int64, ctx int) int64 {
	return Need(FileBytes(params, Quant{Name: "Q4_K_M", BytesPerParam: 0.60}), params, ctx)
}

// QuantOf reads the quantisation from a GGUF file name, e.g.
// "qwen2.5-7b-instruct-q4_k_m.gguf" → "Q4_K_M". Empty when unknown.
func QuantOf(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	m := quantRe.FindStringSubmatch(strings.ToUpper(name))
	if m == nil {
		return ""
	}
	return m[1]
}

var quantRe = regexp.MustCompile(`(?:^|[-_./])((?:I?Q\d(?:_[0-9A-Z]+)*)|BF16|F16|F32)(?:[-_.]|$)`)

// ParseSize reads a memory size typed by a person: "24", "24GB", "24G",
// "24GiB", "16384MB", "0.5TB". A bare number is GB. GB and GiB are both
// treated as GiB: GPU vendors say "24 GB" and mean 24 GiB.
func ParseSize(s string) (int64, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	t = strings.TrimSuffix(strings.TrimSuffix(t, "IB"), "B")
	mult := float64(gib)
	switch {
	case strings.HasSuffix(t, "T"):
		mult, t = 1024*gib, strings.TrimSuffix(t, "T")
	case strings.HasSuffix(t, "G"):
		t = strings.TrimSuffix(t, "G")
	case strings.HasSuffix(t, "M"):
		mult, t = 1<<20, strings.TrimSuffix(t, "M")
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
	if err != nil || v <= 0 || math.IsInf(v, 0) {
		return 0, fmt.Errorf("can't read %q as a memory size (try 24GB or 16384MB)", s)
	}
	return int64(v * mult), nil
}

// GB formats bytes as a short GiB figure: "0.4 GB", "9 GB", "42 GB".
func GB(b int64) string {
	g := float64(b) / gib
	if g < 10 {
		return strconv.FormatFloat(math.Round(g*10)/10, 'f', -1, 64) + " GB"
	}
	return strconv.Itoa(int(math.Round(g))) + " GB"
}
